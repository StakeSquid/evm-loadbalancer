# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy source code
COPY . .

# Download dependencies
RUN go mod download

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o main .

# Final stage
FROM alpine:latest

WORKDIR /root/

# Install runtime dependencies
RUN apk --no-cache add ca-certificates

# Copy the binary from builder
COPY --from=builder /app/main .

# Expose port (adjust as needed)
EXPOSE 8080

# Run the binary
CMD ["./main"]