# Build stage
FROM golang:1.21-alpine AS builder

# Install dependencies
RUN apk add --no-cache git ca-certificates tzdata

# Copy source code and dependencies
WORKDIR /app

# Copy all source code at once
COPY . .

# Update go.sum and download dependencies
RUN go mod tidy
RUN go mod download && go mod verify

# Compile the application
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /dockermetrics .

# Final image stage
FROM alpine:3.19

# Install dependencies and configure timezone
RUN apk add --no-cache tzdata ca-certificates && \
    cp /usr/share/zoneinfo/Europe/Moscow /etc/localtime && \
    echo "Europe/Moscow" > /etc/timezone

# Create an unprivileged user
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

# Copy the compiled application
COPY --from=builder /dockermetrics /app/dockermetrics

# Switch to the unprivileged user
USER appuser

# Expose port
EXPOSE 8000

# Set environment variables
ENV EXPORTER_PORT=8000 \
    TZ=Europe/Moscow

# Run the application
ENTRYPOINT ["/app/dockermetrics"]
