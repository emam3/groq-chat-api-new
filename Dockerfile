# Build stage
FROM golang:1.24-alpine AS builder

# Install git and build dependencies
RUN apk add --no-cache git

# Set working directory
WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o main .

# Test stage (optional, for running tests in CI)
FROM builder AS tester
RUN go test -v ./...

# Final stage
FROM alpine:3.19

# Install ca-certificates for HTTPS and curl for healthcheck
RUN apk --no-cache add ca-certificates curl

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /app/main .

# Copy any additional required files (if needed)
COPY --from=builder /app/payload.json .
COPY --from=builder /app/targets.txt .

# Expose the application port
EXPOSE 8000

# Set environment variables
ENV GIN_MODE=release

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:8000/health || exit 1


# Run as non-root user for security
RUN adduser -D -g '' appuser
USER appuser

# Command to run the application
CMD ["./main"]
