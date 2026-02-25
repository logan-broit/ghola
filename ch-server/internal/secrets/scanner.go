package secrets

import (
	"fmt"
	"strings"
)

// Finding represents a detected secret in the input text.
type Finding struct {
	RuleID      string
	Description string
	Match       string // redacted match
}

// Scanner checks text for common secret patterns.
type Scanner struct {
	rules []rule
}

// New creates a Scanner with the default set of secret detection rules.
func New() *Scanner {
	return &Scanner{
		rules: defaultRules(),
	}
}

// Scan checks the input text against all rules and returns any findings.
// Returns nil if no secrets are detected.
func (s *Scanner) Scan(text string) []Finding {
	var findings []Finding
	for _, r := range s.rules {
		if loc := r.pattern.FindStringIndex(text); loc != nil {
			matched := text[loc[0]:loc[1]]
			findings = append(findings, Finding{
				RuleID:      r.id,
				Description: r.description,
				Match:       redact(matched),
			})
		}
	}
	return findings
}

// FormatError produces a user-facing error message from findings.
func FormatError(findings []Finding) string {
	if len(findings) == 0 {
		return ""
	}
	var parts []string
	for _, f := range findings {
		parts = append(parts, fmt.Sprintf("- %s (%s)", f.Description, f.Match))
	}
	return fmt.Sprintf(
		"Memory rejected: potential secret(s) detected. Do not store credentials in memory.\n%s",
		strings.Join(parts, "\n"),
	)
}

// redact shows the first 4 and last 4 characters, replacing the middle with asterisks.
func redact(s string) string {
	if len(s) <= 12 {
		return "****"
	}
	return s[:4] + "****" + s[len(s)-4:]
}
