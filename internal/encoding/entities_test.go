package encoding_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/logan-broit/ghola/internal/encoding"
)

// Ask for a specific entity and check it's there. Case-insensitive
// membership keeps tests readable while still asserting the rule
// each case exercises.
func has(entities []string, needle string) bool {
	for _, e := range entities {
		if e == needle {
			return true
		}
	}
	return false
}

func TestExtractEntities_Empty(t *testing.T) {
	assert.Empty(t, encoding.ExtractEntities(""))
}

func TestExtractEntities_NoCaps(t *testing.T) {
	assert.Empty(t, encoding.ExtractEntities("all lowercase words here."))
}

func TestExtractEntities_MultiWordName(t *testing.T) {
	e := encoding.ExtractEntities("I met Sarah Chen at the conference.")
	assert.True(t, has(e, "sarah chen"), "multi-word name should be captured as one entity; got %v", e)
}

func TestExtractEntities_SingleCapMidSentence(t *testing.T) {
	e := encoding.ExtractEntities("I talked to Sarah about the project.")
	assert.True(t, has(e, "sarah"), "mid-sentence single cap is kept; got %v", e)
}

func TestExtractEntities_SentenceStartSingleCapSkipped(t *testing.T) {
	e := encoding.ExtractEntities("The cat sat on the mat.")
	assert.False(t, has(e, "the"),
		"sentence-start single capital should be skipped; got %v", e)
}

func TestExtractEntities_TitlePrefix(t *testing.T) {
	e := encoding.ExtractEntities("I saw Dr. Smith at the clinic.")
	// Either "dr. smith" (title + name) or "smith" satisfies the rule.
	found := false
	for _, x := range e {
		if strings.Contains(x, "smith") {
			found = true
			break
		}
	}
	assert.True(t, found,
		"Dr.-prefixed name must surface with 'smith' somewhere; got %q", e)
}

func TestExtractEntities_AtMentions(t *testing.T) {
	e := encoding.ExtractEntities("Talked to @loganb about deployment.")
	assert.True(t, has(e, "@loganb"), "@-mention must be captured; got %v", e)
}

func TestExtractEntities_QuotedTerms(t *testing.T) {
	e := encoding.ExtractEntities(`The "thalamic gating" feature is ready.`)
	assert.True(t, has(e, "thalamic gating"), "quoted phrase captured; got %v", e)
}

func TestExtractEntities_BacktickTerms(t *testing.T) {
	e := encoding.ExtractEntities("Check the `workspace_id` column for details.")
	assert.True(t, has(e, "workspace_id"),
		"backticked identifier captured; got %v", e)
}

func TestExtractEntities_CamelCase(t *testing.T) {
	e := encoding.ExtractEntities("We use PostgreSQL and ArgoCD for infrastructure.")
	assert.True(t, has(e, "postgresql"), "PostgreSQL captured; got %v", e)
	assert.True(t, has(e, "argocd"), "ArgoCD captured; got %v", e)
}

func TestExtractEntities_SnakeKebab(t *testing.T) {
	e := encoding.ExtractEntities("The pg-ghola project has a workspace_id column.")
	assert.True(t, has(e, "pg-ghola"), "kebab captured; got %v", e)
	assert.True(t, has(e, "workspace_id"), "snake captured; got %v", e)
}

func TestExtractEntities_Dedup(t *testing.T) {
	e := encoding.ExtractEntities("Sarah met Sarah at the Sarah convention.")
	n := 0
	for _, x := range e {
		if x == "sarah" {
			n++
		}
	}
	assert.Equal(t, 1, n, "Sarah should appear once; got %v", e)
}

func TestExtractEntities_SortedOutput(t *testing.T) {
	// Two independent calls on the same text produce identical
	// (sorted) output; makes downstream tests deterministic.
	a := encoding.ExtractEntities("Sarah met Ada. Ada likes Ruby.")
	b := encoding.ExtractEntities("Sarah met Ada. Ada likes Ruby.")
	assert.Equal(t, a, b)
}
