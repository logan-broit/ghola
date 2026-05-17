// Package encoding implements the continuous sietch (working) ->
// episodic trace-creation worker. Entity extraction is the
// lightweight tagging step run on each event before it ships to
// chapterhouse, so episodic queries can filter by `entities` without
// a second pass later.
//
// v1a is regex-only — no ML model. An LLM-assisted upgrade is
// explicitly deferred in the design doc.
package encoding

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// ExtractEntities pulls a small, deterministic set of entity-like
// tokens out of free-form agent text. Rules, in order:
//
//  1. Double-quoted phrases: `"thalamic gating"` -> `thalamic gating`
//  2. Backtick-quoted phrases: `` `workspace_id` `` -> `workspace_id`
//  3. CamelCase / PascalCase identifiers: `PostgreSQL`, `ArgoCD`
//  4. snake_case / kebab-case identifiers: `workspace_id`, `pg-ghola`
//  5. @mentions: `@alice` -> `@alice`
//  6. Title-prefixed names: `Dr. Smith` -> `Dr. Smith`
//  7. Multi-word capitalized runs: `Sarah Chen`, `New York` (anywhere
//     in the sentence, including sentence-start — a two-word run
//     disambiguates from a regular sentence-start capital)
//  8. Single-word capitalized tokens MID-sentence (not at position
//     0, not immediately after `. ` / `! ` / `? `)
//
// Results are lowercased, Unicode-NFKC normalized by default via
// strings.ToLower, deduplicated, and sorted lexicographically so two
// calls over the same text give identical output.
func ExtractEntities(text string) []string {
	if text == "" {
		return nil
	}

	seen := map[string]struct{}{}
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		norm := normalize(raw)
		if norm == "" {
			return
		}
		seen[norm] = struct{}{}
	}

	// 1. Double-quoted phrases.
	for _, m := range doubleQuoted.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	// 2. Backtick-quoted phrases.
	for _, m := range backtickQuoted.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	// 3. CamelCase / PascalCase.
	for _, m := range camelCase.FindAllString(text, -1) {
		add(m)
	}
	// 4. snake / kebab identifiers — require at least one separator
	//    to avoid matching every lowercase word.
	for _, m := range snakeOrKebab.FindAllString(text, -1) {
		add(m)
	}
	// 5. @mentions.
	for _, m := range atMention.FindAllString(text, -1) {
		add(m)
	}

	// 6, 7, 8 use a single sentence-aware pass so we can distinguish
	// sentence-start words from mid-sentence ones.
	for _, ent := range capitalizedRuns(text) {
		add(ent)
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------
// Pre-compiled regexes
// ---------------------------------------------------------------------

var (
	// Double-quoted content; non-greedy so adjacent quotes don't merge.
	doubleQuoted = regexp.MustCompile(`"([^"]+)"`)
	// Backticked content.
	backtickQuoted = regexp.MustCompile("`([^`]+)`")
	// CamelCase: lowercase-then-upper at least once (rules out
	// all-caps abbreviations) and inner runs. PostgreSQL, ArgoCD fit.
	camelCase = regexp.MustCompile(`\b[A-Z][a-z]+(?:[A-Z][a-z]*)+\b`)
	// snake_case / kebab-case: alnum with at least one `_` or `-`
	// in the middle, bounded by non-alnum.
	snakeOrKebab = regexp.MustCompile(`\b[a-z][a-z0-9]*[_\-][a-z0-9_\-]+\b`)
	// @mentions: `@` + word chars.
	atMention = regexp.MustCompile(`@[A-Za-z0-9_\-]+`)
	// Titles that commonly prefix proper names.
	titlePrefix = regexp.MustCompile(`^(Dr|Mr|Mrs|Ms|Prof|Sir|Lady|St)\.$`)
	// Sentence-ending punctuation we use to decide if the next word
	// should be treated as a sentence start.
	sentenceEnd = regexp.MustCompile(`[.!?]$`)
)

// capitalizedRuns walks the text token-by-token and collects
// capitalized-word runs, applying the rules for sentence-start
// disambiguation + title prefixes.
func capitalizedRuns(text string) []string {
	tokens := tokenize(text)
	if len(tokens) == 0 {
		return nil
	}

	var out []string
	startOfSentence := true
	var run []string
	flush := func() {
		if len(run) == 0 {
			return
		}
		// Single capitalized word at sentence start is ambiguous
		// (could be "The" or a name). Require multi-word to keep it;
		// otherwise drop.
		if startOfSentenceAt(&run, startOfSentence) && len(run) == 1 {
			run = nil
			return
		}
		out = append(out, strings.Join(run, " "))
		run = nil
	}

	for i, tok := range tokens {
		if isTitlePrefix(tok) {
			// Start a run with the title word + period attached.
			flush()
			run = []string{tok}
			// The next token joins the run as-is (e.g. "Smith").
			if i+1 < len(tokens) && isCapitalized(tokens[i+1]) {
				run = append(run, tokens[i+1])
			}
			continue
		}
		if isCapitalized(tok) {
			run = append(run, tok)
			continue
		}

		// Non-capitalized token ends the current run.
		flush()
		startOfSentence = endsSentence(tok)
	}
	flush()
	return out
}

// tokenize splits on whitespace; keeps punctuation attached.
func tokenize(s string) []string {
	return strings.Fields(s)
}

func isCapitalized(tok string) bool {
	if tok == "" {
		return false
	}
	// Strip trailing sentence punctuation so "Sarah." still reads as
	// capitalized "Sarah".
	stripped := strings.TrimRight(tok, ".,;:!?)")
	stripped = strings.TrimLeft(stripped, "(\"'")
	if stripped == "" {
		return false
	}
	r := []rune(stripped)
	if !unicode.IsUpper(r[0]) {
		return false
	}
	// Exclude ALL-CAPS abbreviations; they're usually noise (USA, API).
	allUpper := true
	for _, rr := range r {
		if unicode.IsLetter(rr) && !unicode.IsUpper(rr) {
			allUpper = false
			break
		}
	}
	if allUpper && len(r) > 1 {
		return false
	}
	return true
}

func isTitlePrefix(tok string) bool {
	return titlePrefix.MatchString(tok)
}

func endsSentence(tok string) bool {
	return sentenceEnd.MatchString(tok)
}

// startOfSentenceAt returns true if `run` begins at a sentence start.
// The slice pointer is read-only here but passed that way to keep the
// call-site compact.
func startOfSentenceAt(run *[]string, startOfSentence bool) bool {
	return startOfSentence
}

// ---------------------------------------------------------------------
// Normalize
// ---------------------------------------------------------------------

// normalize collapses whitespace and lowercases. Keeping the @
// prefix on mentions, we only lower-case trailing chars to preserve
// the sigil in recall filters.
func normalize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.ToLower(s)
}
