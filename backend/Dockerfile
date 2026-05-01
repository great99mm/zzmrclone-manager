FROM golang:1.22-alpine AS builder

WORKDIR /app

RUN apk add --no-cache git

# Copy ALL source code first (go mod tidy needs .go files to analyze imports)
COPY . .

# Generate go.sum by analyzing actual imports in source files
RUN go mod tidy

# Download all dependencies with verified checksums
RUN go mod download

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -o server ./cmd/server

FROM alpine:latest

RUN apk add --no-cache ca-certificates rclone bash curl tzdata

WORKDIR /app

COPY --from=builder /app/server /app/server

RUN mkdir -p /app/data /app/logs

EXPOSE 7070

CMD ["./server"]
