package consolidation

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// ParseWorkspaces parses a CSV of workspace UUIDs, dropping blanks and
// unparseable entries. Empty input yields nil (kill-switch: no
// workspaces => consolidation disabled).
func ParseWorkspaces(csv string) []uuid.UUID {
	var out []uuid.UUID
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := uuid.Parse(part)
		if err != nil {
			continue
		}
		out = append(out, id)
	}
	return out
}

// NextRunDelay returns the duration from now until the next occurrence
// of hour:00 in now's location (today if still upcoming, else tomorrow).
func NextRunDelay(now time.Time, hour int) time.Duration {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next.Sub(now)
}
