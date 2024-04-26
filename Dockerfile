FROM golang:1.22

COPY . .
RUN GOOS=linux GOARCH=amd64 go build .

ENTRYPOINT ["./cronjob-control-plane-operator-test"]