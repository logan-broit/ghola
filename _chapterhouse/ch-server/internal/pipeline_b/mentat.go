package pipeline_b

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

// MentatClient wraps a vLLM / OpenAI-compatible chat.completions
// endpoint. Distill takes a batch of turn snippets that triggered the
// same entity pattern and asks the model to emit a single-mneme JSON
// object describing the recurring concept.
type MentatClient struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
}

// Mneme is the distilled output. Matches the semantic.mnemes shape —
// Pipeline B stage 3 converts this into an insert / update. Memory
// types mirror pg_ghola's semantic enum.
type Mneme struct {
	Concept    string   `json:"concept"`
	Content    string   `json:"content"`
	MemoryType string   `json:"memory_type"`
	Entities   []string `json:"entities"`
}

// DistillInput bundles the per-pattern context handed to the model.
// Turns are the raw text snippets (ordered) from episodic.events that
// mentioned E1 and E2 together.
type DistillInput struct {
	E1, E2 string
	Turns  []string
}

// promptTemplate is the strict JSON prompt. The model MUST reply with
// exactly one JSON object and nothing else; we parse directly.
const promptTemplate = `You distill recurring cross-session patterns into a single long-term memory.

Input: multiple short turns that all mention the entities %q and %q. Your job is to synthesize the common underlying knowledge.

Output JSON — exactly one object, no markdown, no prose:
{"concept": "<short noun phrase, 2-6 words>",
 "content": "<1-3 sentence factual statement>",
 "memory_type": "factual|procedural|episodic",
 "entities": ["<entity>", ...]}

Turns:
%s

JSON:`

// Distill calls the LLM and returns the parsed Mneme. Malformed JSON
// is an error — callers must log & drop, never insert.
func (c *MentatClient) Distill(ctx context.Context, in DistillInput) (*Mneme, error) {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 60 * time.Second}
	}

	var turnsBuf strings.Builder
	for i, t := range in.Turns {
		fmt.Fprintf(&turnsBuf, "%d. %s\n", i+1, t)
	}
	prompt := fmt.Sprintf(promptTemplate, in.E1, in.E2, turnsBuf.String())

	reqBody := map[string]any{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0,
		"max_tokens":  512,
	}
	raw, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+"/v1/chat/completions",
		bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mentat call: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mentat status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var chat struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &chat); err != nil {
		return nil, fmt.Errorf("decode chat: %w", err)
	}
	if len(chat.Choices) == 0 {
		return nil, fmt.Errorf("mentat: no choices")
	}

	return parseMneme(chat.Choices[0].Message.Content)
}

// parseMneme extracts a Mneme from the model's reply text. Tolerates
// leading/trailing whitespace but rejects anything that isn't a
// complete well-formed JSON object.
func parseMneme(content string) (*Mneme, error) {
	s := strings.TrimSpace(content)

	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object in reply: %q", s)
	}
	var m Mneme
	if err := json.Unmarshal([]byte(s[start:end+1]), &m); err != nil {
		return nil, fmt.Errorf("decode mneme: %w", err)
	}
	if m.Concept == "" || m.Content == "" {
		return nil, fmt.Errorf("mneme missing concept/content: %+v", m)
	}
	switch m.MemoryType {
	case "factual", "procedural", "episodic":
	default:
		return nil, fmt.Errorf("mneme invalid memory_type %q", m.MemoryType)
	}
	return &m, nil
}
