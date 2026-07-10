package consolidation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LLMClient is a minimal OpenAI-compatible chat-completions client for
// per-cluster labels + the workspace digest. Plain net/http, no SDK —
// matches internal/mentat/client.go. Nil when no URL is configured.
type LLMClient struct {
	baseURL string
	model   string
	apiKey  string
	http    *http.Client
}

// NewLLMClient returns nil when baseURL is empty so the pipeline can
// nil-check and skip the label/digest step cleanly. A 30s timeout keeps
// a stuck LLM from wedging the nightly job.
func NewLLMClient(baseURL, model, apiKey string) *LLMClient {
	if strings.TrimSpace(baseURL) == "" {
		return nil
	}
	return &LLMClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

const labelSystem = "You name a cluster of related work sessions with ONE terse line " +
	"(max 80 chars, no quotes, no trailing punctuation). Reply with only the line."

const digestSystem = "You write one short paragraph describing the current state of a " +
	"software project, given labels of its most recent active work clusters. " +
	"Be concrete and factual. Reply with only the paragraph."

// Label returns a one-line (<=80 char) cluster label from representative
// excerpts. Errors surface to the caller, which decides to skip.
func (c *LLMClient) Label(ctx context.Context, excerpts []string) (string, error) {
	user := "Representative excerpts from the cluster:\n\n" + strings.Join(excerpts, "\n---\n")
	out, err := c.complete(ctx, labelSystem, user)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	if len(line) > 80 {
		line = line[:80]
	}
	return line, nil
}

// Digest returns a project-state paragraph from cluster labels (ordered
// by the caller — recency+confidence).
func (c *LLMClient) Digest(ctx context.Context, labels []string) (string, error) {
	user := "Recent active work clusters (most important first):\n\n- " +
		strings.Join(labels, "\n- ")
	return c.complete(ctx, digestSystem, user)
}

type chatReq struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type chatResp struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

func (c *LLMClient) complete(ctx context.Context, system, user string) (string, error) {
	body, err := json.Marshal(chatReq{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	})
	if err != nil {
		return "", fmt.Errorf("llm marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("llm %d: %s", resp.StatusCode, string(buf))
	}
	var out chatResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("llm decode: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("llm: no choices")
	}
	return out.Choices[0].Message.Content, nil
}
