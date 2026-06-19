# syntax=docker/dockerfile:1

# Build all three static, CGO-free binaries from source.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
RUN go build -ldflags="-s -w" -o /out/ghtraffic . \
 && go build -ldflags="-s -w" -o /out/ghpush ./cmd/ghpush \
 && go build -ldflags="-s -w" -o /out/scheduler ./cmd/scheduler

# Minimal runtime: no shell, no package manager, non-root by default.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/ghtraffic /out/ghpush /out/scheduler /
VOLUME ["/data"]
USER nonroot:nonroot
ENTRYPOINT ["/scheduler"]
