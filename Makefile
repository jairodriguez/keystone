# Keystone Makefile

.PHONY: build test lint vet tidy docker clean run help

BINARY_NAME=keystone
VERSION?=dev
COMMIT?=$(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME?=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS=-ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)"

GO_FILES=$(shell find . -name '*.go' -not -path './vendor/*' -not -path '*/testdata/*')

## Build the binary
build:
	go build $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/keystone

## Run with example config (requires env vars)
run: build
	cp config/keystone.example.yaml config/keystone.yaml
	bin/$(BINARY_NAME)

## Run tests
test:
	go test -v -race -count=1 ./...

## Run tests with coverage
test-coverage:
	go test -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

## Run linter (requires golangci-lint)
lint:
	golangci-lint run ./...

## Run go vet
vet:
	go vet ./...

## Tidy dependencies
tidy:
	go mod tidy

## Format code
fmt:
	gofmt -w $(GO_FILES)

## Build Docker image
docker:
	docker build -t keystone:$(VERSION) -f docker/Dockerfile .

## Build multi-arch Docker image
docker-multi:
	docker buildx build --platform linux/amd64,linux/arm64 -t keystone:$(VERSION) -f docker/Dockerfile --push .

## Clean build artifacts
clean:
	rm -rf bin/ coverage.out coverage.html

## Install development tools
install-tools:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	go install golang.org/x/tools/cmd/goimports@latest

## Generate mocks (if using mockery)
generate:
	go generate ./...

## Run with race detector
race:
	go run -race ./cmd/keystone

## Quick development cycle
dev: tidy fmt vet test build

## Show help
help:
	@echo "Keystone - Session-sticky AI API Proxy"
	@echo ""
	@echo "Targets:"
	@echo "  build          - Build the binary"
	@echo "  test           - Run tests"
	@echo "  test-coverage  - Run tests with coverage report"
	@echo "  lint           - Run golangci-lint"
	@echo "  vet            - Run go vet"
	@echo "  tidy           - Tidy dependencies"
	@echo "  fmt            - Format code"
	@echo "  docker         - Build Docker image"
	@echo "  clean          - Clean build artifacts"
	@echo "  install-tools  - Install development tools"
	@echo "  dev            - Quick dev cycle (tidy, fmt, vet, test, build)"
	@echo "  help           - Show this help"

# Default target
default: help