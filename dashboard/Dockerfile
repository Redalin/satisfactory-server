# Stage 1: Build the Go application
FROM golang:1.23-alpine AS builder

WORKDIR /src

# Copy go mod file
COPY go.mod ./

# Copy source code and embedded assets
COPY . .

# Build a statically linked binary with stripped debug symbols
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /satisfactory-dashboard .

# Stage 2: Minimal runtime image
FROM alpine:3.20

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# Copy compiled binary from builder
COPY --from=builder /satisfactory-dashboard /app/satisfactory-dashboard

# Default environment variables
ENV PORT=8080 \
    SERVER_API_URL=https://satisfactory-server:7777 \
    TARGET_CONTAINER=satisfactory-server \
    POLL_INTERVAL=5

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider "http://127.0.0.1:${PORT:-8080}/healthz" || exit 1

ENTRYPOINT ["/app/satisfactory-dashboard"]

