package model

import (
	"time"

	"github.com/google/uuid"
)

// MemoryTier represents the tier classification of a memory block.
type MemoryTier string

const (
	TierCore  MemoryTier = "core"
	TierIndex MemoryTier = "index"
	TierState MemoryTier = "state"
)

// Valid returns true if the tier is valid.
func (t MemoryTier) Valid() bool {
	switch t {
	case TierCore, TierIndex, TierState:
		return true
	}
	return false
}

// JournalEntryType represents the type of a journal entry.
type JournalEntryType string

const (
	EntryConversation JournalEntryType = "conversation"
	EntryInsight      JournalEntryType = "insight"
	EntryEvent        JournalEntryType = "event"
	EntryTask         JournalEntryType = "task"
	EntryReflection   JournalEntryType = "reflection"
	EntryDecision     JournalEntryType = "decision"
	EntrySolution     JournalEntryType = "solution"
)

// Valid returns true if the entry type is valid.
func (t JournalEntryType) Valid() bool {
	switch t {
	case EntryConversation, EntryInsight, EntryEvent, EntryTask,
		EntryReflection, EntryDecision, EntrySolution:
		return true
	}
	return false
}

// User represents a system user.
type User struct {
	ID          uuid.UUID      `json:"id"`
	Username    string         `json:"username"`
	Email       string         `json:"email,omitempty"`
	DisplayName string         `json:"display_name,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	ModifiedAt  time.Time      `json:"modified_at"`
}

// MemoryBlock represents a named memory block with versioning.
type MemoryBlock struct {
	ID         int64      `json:"id"`
	GUID       uuid.UUID  `json:"guid"`
	UserID     uuid.UUID  `json:"user_id"`
	Name       string     `json:"name"`
	Tier       MemoryTier `json:"tier"`
	Value      string     `json:"value"`
	Version    int        `json:"version"`
	SortOrder  int        `json:"sort_order"`
	CreatedAt  time.Time  `json:"created_at"`
	ModifiedAt time.Time  `json:"modified_at"`
}

// JournalEntry represents a timestamped journal entry.
type JournalEntry struct {
	ID           int64            `json:"id"`
	GUID         uuid.UUID        `json:"guid"`
	UserID       uuid.UUID        `json:"user_id"`
	EntryType    JournalEntryType `json:"entry_type"`
	Content      string           `json:"content"`
	Metadata     map[string]any   `json:"metadata,omitempty"`
	VectorID     *uuid.UUID       `json:"vector_id,omitempty"`
	SupersededBy *uuid.UUID       `json:"superseded_by,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
	ModifiedAt   time.Time        `json:"modified_at"`
}

// GitCommit represents git commit metadata.
type GitCommit struct {
	ID            int64     `json:"id"`
	GUID          uuid.UUID `json:"guid"`
	UserID        uuid.UUID `json:"user_id"`
	CommitHash    string    `json:"commit_hash"`
	CommitMessage string    `json:"commit_message"`
	AuthorName    string    `json:"author_name,omitempty"`
	AuthorEmail   string    `json:"author_email,omitempty"`
	CommittedAt   time.Time `json:"committed_at"`
	CreatedAt     time.Time `json:"created_at"`
}

// AuditLogEntry represents an audit log entry.
type AuditLogEntry struct {
	ID           int64          `json:"id"`
	GUID         uuid.UUID      `json:"guid"`
	UserID       *uuid.UUID     `json:"user_id,omitempty"`
	Action       string         `json:"action"`
	ResourceType string         `json:"resource_type"`
	ResourceID   string         `json:"resource_id,omitempty"`
	Details      map[string]any `json:"details,omitempty"`
	IPAddress    string         `json:"ip_address,omitempty"`
	UserAgent    string         `json:"user_agent,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
}

// MemoryContext represents the full context loaded for a session.
type MemoryContext struct {
	Core     []MemoryBlock `json:"core"`
	Index    []MemoryBlock `json:"index"`
	State    []MemoryBlock `json:"state"`
	LoadedAt time.Time     `json:"loaded_at"`
}

// SearchResult represents a search result with score.
type SearchResult struct {
	ID        uuid.UUID      `json:"id"`
	Type      string         `json:"type"` // "memory_block" or "journal"
	EntryType string         `json:"entry_type,omitempty"`
	Content   string         `json:"content"`
	Score     float32        `json:"score"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// VectorPoint represents a point in the vector database.
type VectorPoint struct {
	ID         uuid.UUID      `json:"id"`
	UserID     uuid.UUID      `json:"user_id"`
	Type       string         `json:"type"`
	EntryType  string         `json:"entry_type,omitempty"`
	Content    string         `json:"content"`
	Vector     []float32      `json:"-"`
	Superseded bool           `json:"superseded"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}
