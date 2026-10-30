# syntax=docker/dockerfile:1

# build stage
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /k8s-job-cleaner .

# final stage
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /k8s-job-cleaner /k8s-job-cleaner
USER nonroot:nonroot
ENTRYPOINT ["/k8s-job-cleaner"]
LABEL org.opencontainers.image.title="k8s-job-cleaner-go" \
      org.opencontainers.image.source="https://github.com/knpavankumar/k8s-job-cleaner-go" \
      org.opencontainers.image.authors="pavankumarkn@gmail.com"
