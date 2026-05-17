package mneme

import (
	"strings"
	"testing"
)

// validateTurnReconstruction is the pure, DB-free pre-flight check that
// RememberWithTurns runs before any embedding or insert work. These tests
// exhaust the failure modes we've hand-verified matter (char-offset misaligns,
// non-reconstructing content, etc.) so production can't regress them.

func TestValidateTurnReconstruction_HappyPath(t *testing.T) {
	session := "USER: hi there\nASSISTANT: hello!"
	turns := []TurnInput{
		{Role: "user", Content: "USER: hi there", CharStart: 0, CharEnd: 14},
		{Role: "assistant", Content: "\nASSISTANT: hello!", CharStart: 14, CharEnd: 32},
	}
	if err := validateTurnReconstruction(session, turns); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestValidateTurnReconstruction_SingleTurn(t *testing.T) {
	session := "The capital of France is Paris."
	turns := []TurnInput{
		{Role: "assistant", Content: "The capital of France is Paris.", CharStart: 0, CharEnd: 31},
	}
	if err := validateTurnReconstruction(session, turns); err != nil {
		t.Fatalf("single-turn reconstruction should succeed, got %v", err)
	}
}

func TestValidateTurnReconstruction_NegativeCharStart(t *testing.T) {
	session := "hello"
	turns := []TurnInput{
		{Role: "user", Content: "hello", CharStart: -1, CharEnd: 5},
	}
	err := validateTurnReconstruction(session, turns)
	if err == nil {
		t.Fatal("expected error for negative char_start, got nil")
	}
	if !strings.Contains(err.Error(), "negative char_start") {
		t.Fatalf("error %q should mention negative char_start", err)
	}
}

func TestValidateTurnReconstruction_EmptySpan(t *testing.T) {
	session := "hello"
	turns := []TurnInput{
		{Role: "user", Content: "", CharStart: 2, CharEnd: 2},
	}
	err := validateTurnReconstruction(session, turns)
	if err == nil {
		t.Fatal("expected error for empty char span, got nil")
	}
	if !strings.Contains(err.Error(), "empty char span") {
		t.Fatalf("error %q should mention empty char span", err)
	}
}

func TestValidateTurnReconstruction_CharEndOutOfRange(t *testing.T) {
	session := "hello"
	turns := []TurnInput{
		{Role: "user", Content: "hello!", CharStart: 0, CharEnd: 6},
	}
	err := validateTurnReconstruction(session, turns)
	if err == nil {
		t.Fatal("expected error for char_end past end, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds session length") {
		t.Fatalf("error %q should mention exceeding session length", err)
	}
}

func TestValidateTurnReconstruction_ContentMismatch(t *testing.T) {
	session := "hello world"
	turns := []TurnInput{
		{Role: "user", Content: "hello WORLD", CharStart: 0, CharEnd: 11},
	}
	err := validateTurnReconstruction(session, turns)
	if err == nil {
		t.Fatal("expected error for content/slice mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error %q should mention content mismatch", err)
	}
}

func TestValidateTurnReconstruction_GapBetweenTurns(t *testing.T) {
	// Valid slices per-turn but gap between turn0 and turn1 means overall
	// reconstruction doesn't equal session_text.
	session := "ABCDEF"
	turns := []TurnInput{
		{Role: "user", Content: "AB", CharStart: 0, CharEnd: 2},
		{Role: "user", Content: "EF", CharStart: 4, CharEnd: 6},
	}
	err := validateTurnReconstruction(session, turns)
	if err == nil {
		t.Fatal("expected error for gap between turns, got nil")
	}
	if !strings.Contains(err.Error(), "does not equal session_text") {
		t.Fatalf("error %q should mention reconstruction mismatch", err)
	}
}

func TestValidateTurnReconstruction_OverlappingTurns(t *testing.T) {
	session := "ABCDEF"
	turns := []TurnInput{
		{Role: "user", Content: "ABCD", CharStart: 0, CharEnd: 4},
		{Role: "user", Content: "CDEF", CharStart: 2, CharEnd: 6},
	}
	err := validateTurnReconstruction(session, turns)
	if err == nil {
		t.Fatal("expected error for overlapping turns, got nil")
	}
	// The overlap produces a too-long reconstruction.
	if !strings.Contains(err.Error(), "does not equal session_text") {
		t.Fatalf("error %q should mention reconstruction mismatch", err)
	}
}

func TestValidateTurnReconstruction_UnicodeRoundtrip(t *testing.T) {
	// Char offsets are byte offsets in Go strings; validate that multi-byte
	// characters (emoji, CJK) don't break the reconstruction check.
	session := "hello 世界"
	// "hello " is 6 bytes; "世界" is 6 bytes (3 bytes per CJK char in UTF-8).
	turns := []TurnInput{
		{Role: "user", Content: "hello ", CharStart: 0, CharEnd: 6},
		{Role: "user", Content: "世界", CharStart: 6, CharEnd: 12},
	}
	if err := validateTurnReconstruction(session, turns); err != nil {
		t.Fatalf("unicode reconstruction should succeed, got %v", err)
	}
}

func TestAllowedTurnRoles_Coverage(t *testing.T) {
	// Guard against accidental divergence from the CHECK constraint in
	// pg_ghola/src/schema.rs create_sub_mnemes_table. If the schema adds
	// a role, we want to notice here.
	want := []string{"user", "assistant", "system", "tool"}
	for _, r := range want {
		if !allowedTurnRoles[r] {
			t.Errorf("expected role %q to be allowed", r)
		}
	}
	if len(allowedTurnRoles) != len(want) {
		t.Errorf("allowedTurnRoles has %d entries; schema CHECK has %d. "+
			"Keep these in sync or update both together.",
			len(allowedTurnRoles), len(want))
	}
}
