package mcp

import (
	"net/http"
	"testing"
)

// TestProxyTimeout_ConsolidatePathGetsLongDeadline pins Finding 1b's
// timeout-selection helper on the MCP->daemon hop: the consolidate route
// runs the full episodic->semantic batch (tens of seconds to minutes) and
// must get the generous per-request deadline, while every other route
// keeps the default.
func TestProxyTimeout_ConsolidatePathGetsLongDeadline(t *testing.T) {
	if got := proxyTimeout(consolidateWorkspacePath); got != consolidateProxyTimeout {
		t.Fatalf("consolidate path timeout = %s, want %s", got, consolidateProxyTimeout)
	}
	if got := proxyTimeout("/v1/recall"); got != defaultProxyTimeout {
		t.Fatalf("default path timeout = %s, want %s", got, defaultProxyTimeout)
	}
}

// TestClientForPath_ConsolidateUsesUncappedClient proves the mechanism: a
// shared client with a 30s Timeout would clip the request before its 10m
// context deadline could fire, so the consolidate route must be served by a
// client with no Timeout cap (the context deadline is then the only bound).
func TestClientForPath_ConsolidateUsesUncappedClient(t *testing.T) {
	h := newHandler(Config{
		BaseURL:    "http://localhost:7421",
		HTTPClient: &http.Client{Timeout: defaultProxyTimeout},
	})

	if got := h.clientForPath(consolidateWorkspacePath).Timeout; got != 0 {
		t.Fatalf("consolidate client Timeout = %s, want 0 (uncapped)", got)
	}
	if got := h.clientForPath("/v1/recall").Timeout; got != defaultProxyTimeout {
		t.Fatalf("default client Timeout = %s, want %s", got, defaultProxyTimeout)
	}
}
