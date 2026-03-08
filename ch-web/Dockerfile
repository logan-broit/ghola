FROM golang:1.24-alpine AS builder

WORKDIR /build
COPY go.mod ./
COPY cmd/ cmd/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o ch-web ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot-amd64

COPY --from=builder /build/ch-web /app/ch-web

EXPOSE 8080

ENTRYPOINT ["/app/ch-web"]
