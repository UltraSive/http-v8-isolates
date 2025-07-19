# -------- Build Stage --------
FROM golang:1.21-bookworm AS builder

# Install V8 build dependencies
RUN apt-get update && apt-get install -y \
    git curl gnupg cmake ninja-build pkg-config build-essential \
    python3 python3-pip python3-setuptools \
    libglib2.0-dev libnss3-dev libexpat1-dev \
    && rm -rf /var/lib/apt/lists/*

# Get v8go and download libv8
WORKDIR /app
COPY go.mod ./
copy go.sum ./
COPY main.go ./
RUN go mod download

# Build with CGO enabled
ENV CGO_ENABLED=1
RUN go build -o main .

# -------- Runtime Stage --------
FROM debian:bookworm-slim

# Set default environment variable
ENV PORT=8080

# Install V8 shared libs only (copied from build stage or system install)
RUN apt-get update && apt-get install -y \
    libstdc++6 libnss3 libglib2.0-0 libexpat1 \
    && rm -rf /var/lib/apt/lists/*

# Copy binary and v8 shared libs from builder
COPY --from=builder /app/main /app/main

EXPOSE 8080

# Set entrypoint
ENTRYPOINT ["/app/main"]
