package sietch

import (
	"encoding/json"
	"regexp"
	"time"

	"github.com/logan-broit/ghola/internal/core"
)

// Bound bytes before transferring a raw payload out of SQLite. Oversized or
// unrecognized metadata is unavailable; the event's searchable text is unchanged.
const recallProvenanceSQL = `CASE WHEN length(CAST(raw_event AS BLOB)) <= 16384 THEN raw_event ELSE '' END`

var (
	provenanceUUID     = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	provenanceEntry    = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
	provenanceActor    = regexp.MustCompile(`^[a-zA-Z0-9_.:/-]{1,128}$`)
	provenanceRevision = regexp.MustCompile(`(?i)^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

func recallProvenance(raw string) *core.RecallProvenance {
	var p core.RecallProvenance
	if len(raw) > 16384 || json.Unmarshal([]byte(raw), &p) != nil || p.Producer != "pi-ghola/1" {
		return nil
	}
	p.Actor = provenanceMatch(p.Actor, provenanceActor)
	// The current producer only emits unverified. Never promote an arbitrary
	// stored "verified" string into an apparent service-backed attestation.
	if p.Verification != "unverified" {
		p.Verification = ""
	}
	p.CapturedAt = provenanceTime(p.CapturedAt)
	if pi := p.Pi; pi != nil {
		pi.SessionID = provenanceMatch(pi.SessionID, provenanceUUID)
		pi.EntryID = provenanceMatch(pi.EntryID, provenanceEntry)
		pi.ParentEntryID = provenanceMatch(pi.ParentEntryID, provenanceEntry)
		pi.PreviousEpisodeID = provenanceMatch(pi.PreviousEpisodeID, provenanceUUID)
		pi.Timestamp = provenanceTime(pi.Timestamp)
		switch pi.StopReason {
		case "stop", "length", "toolUse", "error", "aborted", "unknown":
		default:
			pi.StopReason = ""
		}
		if *pi == (core.RecallPiSource{}) {
			p.Pi = nil
		}
	}
	if repo := p.Repository; repo != nil {
		repo.Revision = provenanceMatch(repo.Revision, provenanceRevision)
		if *repo == (core.RecallRepository{}) {
			p.Repository = nil
		}
	}
	return &p
}

func provenanceMatch(value string, pattern *regexp.Regexp) string {
	if !pattern.MatchString(value) {
		return ""
	}
	return value
}

func provenanceTime(value string) string {
	if len(value) > 64 {
		return ""
	}
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return ""
	}
	return value
}
