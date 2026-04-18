package mcp

import (
	"strings"
	"testing"
)

// parseTurnsArg is the MCP-layer normalizer that takes the raw JSON-decoded
// `turns` argument and produces typed TurnInput values with computed char
// offsets. It is the first validation gate before RememberWithTurns sees
// the data.

func TestParseTurnsArg_HappyPath(t *testing.T) {
	raw := []any{
		map[string]any{"role": "user", "content": "USER: hi"},
		map[string]any{"role": "assistant", "content": "\nASSISTANT: hello"},
	}
	turns, err := parseTurnsArg(raw)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(turns))
	}
	if turns[0].CharStart != 0 || turns[0].CharEnd != 8 {
		t.Errorf("turn 0 offsets wrong: [%d:%d]", turns[0].CharStart, turns[0].CharEnd)
	}
	if turns[1].CharStart != 8 || turns[1].CharEnd != 25 {
		t.Errorf("turn 1 offsets wrong: [%d:%d]", turns[1].CharStart, turns[1].CharEnd)
	}
	if turns[1].Role != "assistant" {
		t.Errorf("turn 1 role wrong: %q", turns[1].Role)
	}
}

func TestParseTurnsArg_NotArray(t *testing.T) {
	_, err := parseTurnsArg("not an array")
	if err == nil || !strings.Contains(err.Error(), "must be an array") {
		t.Fatalf("expected 'must be an array' error, got %v", err)
	}
}

func TestParseTurnsArg_EmptyArray(t *testing.T) {
	_, err := parseTurnsArg([]any{})
	if err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("expected 'at least one' error, got %v", err)
	}
}

func TestParseTurnsArg_TurnNotObject(t *testing.T) {
	_, err := parseTurnsArg([]any{"not an object"})
	if err == nil || !strings.Contains(err.Error(), "turn 0 is not an object") {
		t.Fatalf("expected non-object error, got %v", err)
	}
}

func TestParseTurnsArg_MissingRole(t *testing.T) {
	raw := []any{
		map[string]any{"content": "hello"},
	}
	_, err := parseTurnsArg(raw)
	if err == nil || !strings.Contains(err.Error(), "missing role") {
		t.Fatalf("expected missing role error, got %v", err)
	}
}

func TestParseTurnsArg_MissingContent(t *testing.T) {
	raw := []any{
		map[string]any{"role": "user"},
	}
	_, err := parseTurnsArg(raw)
	if err == nil || !strings.Contains(err.Error(), "missing content") {
		t.Fatalf("expected missing content error, got %v", err)
	}
}

func TestParseTurnsArg_EmptyContent(t *testing.T) {
	raw := []any{
		map[string]any{"role": "user", "content": ""},
	}
	_, err := parseTurnsArg(raw)
	if err == nil || !strings.Contains(err.Error(), "content is empty") {
		t.Fatalf("expected empty content error, got %v", err)
	}
}

func TestParseTurnsArg_CumulativeOffsets(t *testing.T) {
	// Three turns; offsets should be strictly increasing and contiguous.
	raw := []any{
		map[string]any{"role": "user", "content": "AB"},
		map[string]any{"role": "assistant", "content": "CDEF"},
		map[string]any{"role": "user", "content": "G"},
	}
	turns, err := parseTurnsArg(raw)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	expect := [][2]int{{0, 2}, {2, 6}, {6, 7}}
	for i, want := range expect {
		if turns[i].CharStart != want[0] || turns[i].CharEnd != want[1] {
			t.Errorf("turn %d got [%d:%d], want [%d:%d]",
				i, turns[i].CharStart, turns[i].CharEnd, want[0], want[1])
		}
	}
}

func TestParseTurnsArg_UnicodeContent(t *testing.T) {
	// Go len() on a string gives byte count, which matches how the mneme
	// validator expects char offsets. Multi-byte chars should produce
	// byte-accurate offsets.
	raw := []any{
		map[string]any{"role": "user", "content": "hello "},
		map[string]any{"role": "assistant", "content": "世界"},
	}
	turns, err := parseTurnsArg(raw)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if turns[0].CharEnd != 6 {
		t.Errorf("turn 0 end: got %d, want 6", turns[0].CharEnd)
	}
	if turns[1].CharStart != 6 || turns[1].CharEnd != 12 {
		t.Errorf("turn 1: got [%d:%d], want [6:12]",
			turns[1].CharStart, turns[1].CharEnd)
	}
}
