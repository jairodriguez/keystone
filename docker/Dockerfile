# Build stage
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /build

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)" \
    -o keystone ./cmd/keystone

# Final stage - scratch for minimal size
FROM scratch

# Import CA certificates from builder
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# Copy binary
COPY --from=builder /build/keystone /keystone

# Copy example config (user should mount their own)
COPY --from=builder /build/config/keystone.example.yaml /config/keystone.example.yaml

# Non-root user
USER 65534:65534

EXPOSE 8080

ENV KEYSTONE_CONFIG=/config/keystone.yaml

ENTRYPOINT ["/keystone"]