FROM golang:1.22-alpine AS go-builder

WORKDIR /app/backend
RUN apk add --no-cache git
COPY backend/ .
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -o server ./cmd/server

FROM node:18-alpine AS node-builder

WORKDIR /app/frontend
COPY frontend/package.json ./
RUN npm install --legacy-peer-deps
COPY frontend/ .
RUN npm run build

FROM alpine:latest

RUN apk add --no-cache ca-certificates rclone bash curl tzdata nginx supervisor

# Setup nginx
RUN mkdir -p /run/nginx /var/log/nginx
COPY frontend/nginx.conf /etc/nginx/nginx.conf

# Copy backend binary
WORKDIR /app
COPY --from=go-builder /app/backend/server /app/server

# Copy frontend build
COPY --from=node-builder /app/frontend/build /usr/share/nginx/html

# Create directories
RUN mkdir -p /app/data /app/logs

# Setup supervisord
COPY supervisord.conf /etc/supervisor/conf.d/supervisord.conf

EXPOSE 80

CMD ["/usr/bin/supervisord", "-c", "/etc/supervisor/conf.d/supervisord.conf"]
