.PHONY: all server service test dev-up dev-down clean smoke-predictive smoke-predictive-down

# Smoke-test stack uses an isolated Compose project + alternate host
# ports so it never collides with the dev stack. Override on the make
# command line if these conflict locally.
SMOKE_PROJECT       ?= predictive-smoke
SMOKE_POSTGRES_PORT ?= 15432
SMOKE_EMBEDDING_PORT ?= 18082
SMOKE_MENTAT_PORT   ?= 18084
SMOKE_CH_PORT       ?= 18080
SMOKE_GHOLA_PORT    ?= 17421
SMOKE_COMPOSE = cd deploy/docker-compose && \
    POSTGRES_PORT=$(SMOKE_POSTGRES_PORT) \
    EMBEDDING_PORT=$(SMOKE_EMBEDDING_PORT) \
    MENTAT_PORT=$(SMOKE_MENTAT_PORT) \
    CHAPTERHOUSE_PORT=$(SMOKE_CH_PORT) \
    GHOLA_PORT=$(SMOKE_GHOLA_PORT) \
    docker compose -p $(SMOKE_PROJECT) \
        -f docker-compose.yml -f docker-compose.smoke.yml

SERVER_DIR  := _chapterhouse/ch-server
SERVICE_BIN := ghola
MCP_BIN     := ghola-mcp

all: server service

server:
	cd $(SERVER_DIR) && go build ./...

service:
	go build -o $(SERVICE_BIN) ./cmd/ghola
	go build -o $(MCP_BIN) ./cmd/ghola-mcp

test:
	cd $(SERVER_DIR) && go test ./...
	go test ./...

dev-up:
	cd deploy/docker-compose && docker compose up -d

dev-down:
	cd deploy/docker-compose && docker compose down

clean:
	rm -f $(SERVICE_BIN) $(MCP_BIN)
	cd $(SERVER_DIR) && go clean

# Full-stack smoke for the predictive-replay v1a vertical slice.
# Brings up an isolated Compose project (postgres + guild-stub +
# mentat + ch-init + chapterhouse + ghola), waits for ghola,
# chapterhouse and mentat to be healthy, runs the e2e test, then tears
# the project down. The isolated project name + alternate ports leave
# any long-running dev stack untouched.
#
# The e2e drives ghola's HTTP API end-to-end (session_start ->
# record -> session_end), which is the only flow that catches bugs
# like the missing-ended_at one in core.Consolidate — direct chapter-
# house ingest bypasses ghola entirely and silently masks the issue.
smoke-predictive:
	$(SMOKE_COMPOSE) up -d --build postgres guild mentat ch-init chapterhouse ghola
	@echo "==> waiting for chapterhouse + mentat + ghola health"
	@deadline=$$(($$(date +%s) + 180)); \
	while [ $$(date +%s) -lt $$deadline ]; do \
	  if curl -fsS http://localhost:$(SMOKE_CH_PORT)/health >/dev/null 2>&1 && \
	     curl -fsS http://localhost:$(SMOKE_MENTAT_PORT)/v1/health >/dev/null 2>&1 && \
	     curl -fsS http://localhost:$(SMOKE_GHOLA_PORT)/health >/dev/null 2>&1; then \
	    echo "    healthy"; break; \
	  fi; sleep 2; \
	done; \
	if [ $$(date +%s) -ge $$deadline ]; then \
	  echo "    health check timed out" >&2; \
	  $(SMOKE_COMPOSE) logs --tail=80 chapterhouse mentat ghola >&2 || true; \
	  $(SMOKE_COMPOSE) down -v; \
	  exit 1; \
	fi
	@set -e; \
	rc=0; \
	( cd $(SERVER_DIR) && \
	  GHOLA_URL=http://localhost:$(SMOKE_GHOLA_PORT) \
	  CHAPTERHOUSE_URL=http://localhost:$(SMOKE_CH_PORT) \
	  MENTAT_URL=http://localhost:$(SMOKE_MENTAT_PORT) \
	  POSTGRES_PORT=$(SMOKE_POSTGRES_PORT) \
	  go test -tags=e2e ./test/e2e/ -run TestPredictiveReplayVerticalSlice -v -count=1 -timeout=180s ) || rc=$$?; \
	$(SMOKE_COMPOSE) down -v; \
	exit $$rc

# Tear down the smoke stack only — useful when the test was interrupted
# and left containers behind.
smoke-predictive-down:
	$(SMOKE_COMPOSE) down -v
