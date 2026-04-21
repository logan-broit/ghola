package core

import (
	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"
)

// NewID returns a ULID packed into the UUID string format.
//
// ULIDs are 128 bits with a 48-bit millisecond timestamp prefix + 80
// bits of entropy, so they compare chronologically at both the byte
// and string levels. Packing them into a uuid.UUID keeps them
// compatible with every Postgres `uuid` column in the chapterhouse
// and pg_ghola schemas — no migration needed, no Go type changes.
// The wire format (standard 36-char UUID hex+dashes) is what every
// client library already expects.
//
// Postgres's uuid type compares as 16-byte little-endian... actually
// bytewise, unsigned — which for ULIDs means older events sort
// before newer ones. Downstream, that lets Pipeline A's sietch
// watermark use plain `WHERE id > ?` without the rowid detour, and
// Pipeline B's episodic scan over a time window is a straight id
// range query.
func NewID() string {
	// ulid.Make uses the package's default monotonic entropy source,
	// so back-to-back calls within the same millisecond still produce
	// strictly-ordered ids (the entropy counter increments).
	u := ulid.Make()
	var uu uuid.UUID
	copy(uu[:], u[:])
	return uu.String()
}
