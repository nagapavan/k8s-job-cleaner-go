#build stage
FROM golang:alpine AS builder
RUN apk add --no-cache git
WORKDIR /go/src/app
ENV GO111MODULE=on
COPY . .
RUN go get -d -v ./...
RUN go install -v ./...
RUN go build main.go

#final stage
FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /go/src/app .
ENTRYPOINT ./main
LABEL Name=k8s-job-cleaner-go Version=0.0.1 Maintainer=pavankumarkn@gmail.com
