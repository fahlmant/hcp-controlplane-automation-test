package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go/aws"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	capav1beta2 "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {

	scheme := runtime.NewScheme()
	if err := capav1beta2.AddToScheme(scheme); err != nil {
		// TODO handle err better
		return
	}

	// Get kubeclient for management cluster API
	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	// creates the clientset
	k8sClient, err := client.New(kubeConfig, client.Options{Scheme: scheme})
	if err != nil {
		panic(err.Error())
	}

	// Make sure token file is present since token-minter may take some time to populate
	retries := 0
	for retries < 10 {
		_, err := os.Stat("/var/run/secrets/openshift/serviceaccount/token")
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				retries += 1
				time.Sleep(time.Duration(retries) * time.Second)
				continue
			}
			fmt.Printf("err: %+v\n", err)
			return
		}
		break
	}

	// Get cluster ID from Pod ENV
	clusterID := os.Getenv("CLUSTER_ID")
	if clusterID == "" {
		fmt.Println("Could not get cluster ID. Is it set in Pod's ENV?")
		return
	}

	// Get cluster namespace from Pod ENV
	clusterNamespace := os.Getenv("CLUSTER_NAMESPACE")
	if clusterNamespace == "" {
		fmt.Println("Could not get cluster namespace. Is it set in Pod's ENV?")
		return
	}

	// Setup AWS SDK
	// Get AWS Region from Pod ENV
	awsRegion := os.Getenv("AWS_REGION")
	if awsRegion == "" {
		// Default to us-east-1
		awsRegion = "us-east-1"
	}

	// Load credentials from the config file and set the region
	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(awsRegion))
	if err != nil {
		fmt.Printf("err: %+v\n", err)
		return
	}

	// Create an EC2 client
	ec2Client := ec2.NewFromConfig(cfg)

	// Get all red-hat-managed instances for the cluster from the dataplane AWS account
	allInstances, err := getAllRedHatManagedInstances(ec2Client, clusterID)
	if err != nil {
		fmt.Printf("err: %+v\n", err)
		return
	}

	// Get worker Machine CRs
	awsmachines := &capav1beta2.AWSMachineList{}
	if err := k8sClient.List(context.TODO(), awsmachines, &client.ListOptions{Namespace: clusterNamespace}, client.MatchingLabels{
		"cluster.x-k8s.io/cluster-name": clusterID,
	}); err != nil {
		fmt.Printf("err: %+v\n", err)
		return
	}

	expectedInstances := map[string]bool{}
	for _, awsmachine := range awsmachines.Items {
		if awsmachine.Spec.InstanceID != nil {
			expectedInstances[*awsmachine.Spec.InstanceID] = true
		}
	}

	// Compare CRs to AWS instances
	leakedInstances := []string{}
	for _, instance := range allInstances {
		if _, ok := expectedInstances[instance]; !ok {
			leakedInstances = append(leakedInstances, instance)
		}
	}

	if len(leakedInstances) > 0 {
		fmt.Printf("Terminating instsances: %+v\n", leakedInstances)
		_, err = ec2Client.TerminateInstances(context.TODO(), &ec2.TerminateInstancesInput{
			InstanceIds: leakedInstances,
		})
		if err != nil {
			fmt.Printf("err: %+v\n", err)
			return
		}
	} else {
		fmt.Println(("No instances to terminate!"))
	}

	fmt.Println("All done!")
	for {

	}
}

func getRedHatManagedEC2Reservations(ec2Client *ec2.Client, token string, clusterId string) (ec2.DescribeInstancesOutput, error) {

	output, err := ec2Client.DescribeInstances(context.TODO(), &ec2.DescribeInstancesInput{
		NextToken: &token,
		Filters: []types.Filter{
			{
				// We don't want to terminate pending because they would just be started and might not have been picked up by the MC yet.
				// Importantly we also don't want to count terminated instances as leaked, as it causes confusion.
				Name:   aws.String("instance-state-name"),
				Values: []string{string(types.InstanceStateNameRunning), string(types.InstanceStateNameStopped)},
			},
			{
				Name:   aws.String("tag:red-hat-managed"),
				Values: []string{"true"},
			},
			{
				Name:   aws.String(fmt.Sprintf("tag:sigs.k8s.io/cluster-api-provider-aws/cluster/%s", clusterId)),
				Values: []string{"owned"},
			},
		},
	})
	if err != nil {
		return ec2.DescribeInstancesOutput{}, err
	}

	return *output, nil
}

func getAllRedHatManagedInstances(ec2Client *ec2.Client, clusterId string) ([]string, error) {

	allInstances := []string{}

	output, err := getRedHatManagedEC2Reservations(ec2Client, "", clusterId)
	if err != nil {
		return []string{}, err
	}

	for _, reservation := range output.Reservations {
		for _, instanceId := range reservation.Instances {
			allInstances = append(allInstances, *instanceId.InstanceId)
		}
	}

	nextToken := output.NextToken
	for nextToken != nil {
		getRedHatManagedEC2Reservations(ec2Client, *nextToken, clusterId)
		for _, reservation := range output.Reservations {
			for _, instanceId := range reservation.Instances {
				allInstances = append(allInstances, *instanceId.InstanceId)
			}
		}

		nextToken = output.NextToken
	}

	return allInstances, nil
}
