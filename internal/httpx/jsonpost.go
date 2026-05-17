// Package httpx has shared HTTP-client glue for the ghola daemon's
// outbound calls. PostJSON encodes a request, sets Content-Type, sets
// an optional Bearer token, executes the request, checks the status,
// and decodes the response — collapsing boilerplate that was open-coded
// in internal/truthsayer and internal/chapterhouse.
//
// The chapterhouse client still defines its own do() because it
// constructs a richer *chapterhouse.StatusError on non-2xx. Truthsayer
// and any future plain-JSON clients use this helper.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// PostJSON POSTs body (JSON-encoded) to url with optional bearer auth,
// and decodes a 2xx response into out (if non-nil). Non-2xx becomes a
// formatted error carrying status + truncated body.
//
// `verb` is a short label used in error messages (e.g. "rerank",
// "health"). It surfaces as "rerank: 503 Service Unavailable: ...".
func PostJSON(ctx context.Context, c *http.Client, url, bearer, verb string, body, out any) error {
	buf := new(bytes.Buffer)
	if body != nil {
		if err := json.NewEncoder(buf).Encode(body); err != nil {
			return fmt.Errorf("%s encode: %w", verb, err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, buf)
	if err != nil {
		return fmt.Errorf("%s build: %w", verb, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", verb, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %d %s: %s",
			verb, resp.StatusCode, http.StatusText(resp.StatusCode),
			strings.TrimSpace(string(payload)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s decode: %w", verb, err)
	}
	return nil
}
