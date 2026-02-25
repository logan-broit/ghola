FROM --platform=linux/amd64 golang:1.24-alpine AS builder

WORKDIR /build

# Inject corporate CA bundle before any network operations (MITM proxy)
COPY ca-bundle.pem /usr/local/share/ca-certificates/switch-ca.crt
RUN cat /usr/local/share/ca-certificates/switch-ca.crt >> /etc/ssl/certs/ca-certificates.crt \
    && apk add --no-cache ca-certificates \
    && update-ca-certificates

COPY go.mod ./
RUN go mod download

COPY cmd/ cmd/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o ch-web ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot-amd64

COPY --from=builder /build/ch-web /app/ch-web

EXPOSE 8080

ENTRYPOINT ["/app/ch-web"]
