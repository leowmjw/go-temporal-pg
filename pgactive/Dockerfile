FROM golang:1.21-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git gcc musl-dev

# Set working directory
WORKDIR /app

# Copy go.mod and go.sum files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy the source code
COPY . .

# Build the server binary
RUN CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo -o pgactive-server ./cmd/server
# Build the CLI binary
RUN CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo -o pgactive-tools ./cmd/pgactive-tools

# Use a smaller image for the final container
FROM alpine:3.18

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata

# Set working directory
WORKDIR /app

# Create data directory
RUN mkdir -p /app/data

# Copy binaries from builder stage
COPY --from=builder /app/pgactive-server /app/pgactive-server
COPY --from=builder /app/pgactive-tools /app/pgactive-tools

# Copy config files
COPY config/ /app/config/

# Expose the port the server listens on
EXPOSE 8080

# Set environment variables
ENV PGACTIVE_TOOLS_CONFIG=/app/config/tools.yaml

# Default command to run the server
CMD ["/app/pgactive-server"]
