package repository

import (
	"fmt"
	"strconv"
	"strings"
)

// vectorLiteralOrNil returns a pgvector text literal like "[1,2,3]"
// or nil (maps to SQL NULL) when the slice is empty.
func vectorLiteralOrNil(v []float64) any {
	if len(v) == 0 {
		return nil
	}
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = strconv.FormatFloat(x, 'f', -1, 64)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// vectorLiteralFloat32 mirrors vectorLiteralOrNil but for []float32.
// Returns the pgvector text literal "[1,2,3]"; the caller is
// responsible for not passing an empty slice (see QueryMnemesByEmbedding).
func vectorLiteralFloat32(v []float32) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = strconv.FormatFloat(float64(x), 'f', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// parseVectorLiteral turns pgvector's text representation ("[1,2,3]")
// back into []float32. We read vectors as text for the same reason the
// ingest path writes them as text: pgx has no native pgvector codec
// without the pgvector-go driver, and the text path is fast enough for
// session-sized event sets.
func parseVectorLiteral(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("malformed vector literal: %q", s)
	}
	body := s[1 : len(s)-1]
	if body == "" {
		return []float32{}, nil
	}
	parts := strings.Split(body, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("parse component %d: %w", i, err)
		}
		out[i] = float32(f)
	}
	return out, nil
}
