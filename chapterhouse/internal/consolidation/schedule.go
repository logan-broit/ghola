package consolidation

import (
	"log/slog"
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

// ClampHour bounds a configured CONSOLIDATE_HOUR into [0,23]. time.Date
// (used by NextRunDelay) SILENTLY normalizes an out-of-range hour — 24 rolls
// to the next day's 00:00, -1 to the previous day's 23:00 — quietly shifting
// the nightly run to an unintended time. Clamping keeps the scheduled hour
// explicit and logs the correction. A nil log skips the warning.
func ClampHour(hour int, log *slog.Logger) int {
	if hour >= 0 && hour <= 23 {
		return hour
	}
	clamped := hour
	if clamped < 0 {
		clamped = 0
	} else if clamped > 23 {
		clamped = 23
	}
	if log != nil {
		log.Warn("CONSOLIDATE_HOUR out of range; clamped to [0,23]",
			slog.Int("given", hour), slog.Int("clamped", clamped))
	}
	return clamped
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
