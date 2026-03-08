# Build stage
FROM --platform=linux/amd64 golang:1.24-alpine AS builder

WORKDIR /build

RUN apk add --no-cache git ca-certificates

# Copy go mod files first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build binaries
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o ch-server ./cmd/api
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o ch-reindex ./cmd/reindex
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o ch-init ./cmd/init

# Runtime stage
FROM --platform=linux/amd64 alpine:3.21

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# Copy binaries and migrations
COPY --from=builder /build/ch-server /app/ch-server
COPY --from=builder /build/ch-reindex /app/ch-reindex
COPY --from=builder /build/ch-init /app/ch-init
COPY --from=builder /build/db/migrations /app/db/migrations

# Create non-root user
RUN addgroup -g 1000 ch && adduser -u 1000 -G ch -s /bin/sh -D ch
RUN chown -R ch:ch /app
USER ch

EXPOSE 8080

ENTRYPOINT ["/app/ch-server"]
