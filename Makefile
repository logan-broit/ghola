.PHONY: all extension server service clients test dev-up dev-down clean

EXT_DIR     := extension
SERVER_DIR  := _chapterhouse/ch-server
SERVICE_BIN := ghola
MCP_BIN     := ghola-mcp

all: extension server service

extension:
	cd $(EXT_DIR) && cargo pgrx package

server:
	cd $(SERVER_DIR) && go build ./...

service:
	go build -o $(SERVICE_BIN) ./cmd/ghola
	go build -o $(MCP_BIN) ./cmd/ghola-mcp

clients:
	cd clients/pi-mono-ext && npm run build

test:
	cd $(EXT_DIR) && cargo pgrx test pg16
	cd $(SERVER_DIR) && go test ./...
	go test ./...

dev-up:
	cd deploy/docker-compose && docker compose up -d

dev-down:
	cd deploy/docker-compose && docker compose down

clean:
	rm -f $(SERVICE_BIN) $(MCP_BIN)
	cd $(EXT_DIR) && cargo clean
	cd $(SERVER_DIR) && go clean
