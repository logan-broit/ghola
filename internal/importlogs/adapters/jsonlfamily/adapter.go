// Package jsonlfamily is the bootstrap-import Adapter for claude-code's
// JSON-event-per-line session logs. The "Family" name reflects intent:
// openclaw / pi-mono / other JSONL agent tools may follow the same outer
// envelope (type + sessionId + timestamp; user/assistant carry a nested
// message.content with chapterhouse-style content blocks — text,
// thinking, tool_use, tool_result), but only claude-code data has been
// inspected and verified. Confirm the field shape against a sample of
// any other tool's output before pointing this adapter at it.
//
// Sample lines observed at ~/.claude/projects/<slug>/<sessionId>.jsonl
// (claude-code, fields trimmed for brevity):
//
//	{"type":"file-history-snapshot","messageId":"...","snapshot":{...}}
//	{"type":"user","sessionId":"<uuid>","timestamp":"2026-04-20T04:51:42.595Z",
//	  "cwd":"/path/to/project","gitBranch":"main",
//	  "message":{"role":"user","content":"what is the docker start command..."}}
//	{"type":"assistant","sessionId":"<uuid>","timestamp":"...",
//	  "message":{"role":"assistant","content":[
//	    {"type":"tool_use","name":"Bash","input":{"command":"docker ps -a"}}]}}
//	{"type":"user","sessionId":"<uuid>","timestamp":"...",
//	  "message":{"role":"user","content":[
//	    {"type":"tool_result","tool_use_id":"...","content":"NAMES IMAGE STATUS..."}]}}
//	{"type":"assistant","sessionId":"<uuid>","timestamp":"...",
//	  "message":{"role":"assistant","content":[
//	    {"type":"text","text":"To start the existing stopped container..."}]}}
//
// Outer-type values seen across real data: user, assistant, system,
// attachment, file-history-snapshot, permission-mode, last-prompt,
// queue-operation, progress, custom-title, agent-name. Only user,
// assistant, system map to chapterhouse's normalized type set; the
// rest are dropped (debug-logged). A "user" event whose nested
// message.content is an array containing tool_result blocks is
// re-classified as a tool_result event so chapterhouse sees the
// canonical (user, assistant, tool_result, system) sequence rather
// than two consecutive "user" turns wrapping a tool exchange.
package jsonlfamily

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/logan-broit/ghola/internal/importlogs"
)

const (
	sourceTool = "claude-code"
	adapterKey = "jsonl-family"
)

// Adapter implements importlogs.Adapter for the jsonl-family of tools.
// Verified against claude-code only; openclaw / pi-mono are aspirational
// targets (see package doc) and have not been tested.
type Adapter struct {
	hostname string
	logger   *slog.Logger
}

// New returns an Adapter populated with the host's name; that value
// is stamped onto every NormalizedSession this adapter parses, so the
// SourceMachine column in chapterhouse reflects where the import ran.
func New() *Adapter {
	logger := slog.Default()
	h, err := os.Hostname()
	if err != nil || h == "" {
		// Don't silently mark every imported session as "unknown" —
		// loud-fail on the operator's terminal so a misconfigured host
		// is noticed before it pollutes the SourceMachine column for
		// the entire bootstrap import.
		logger.Warn("jsonlfamily: os.Hostname() failed; SourceMachine will be 'unknown' for every imported session",
			"err", err)
		h = "unknown"
	}
	return &Adapter{
		hostname: h,
		logger:   logger,
	}
}

// Name is the registry key; matches the --source=<kind>:<path> token.
func (a *Adapter) Name() string { return adapterKey }

// Walk yields every *.jsonl file under root. Recursive so per-project
// slug directories nest cleanly under e.g. ~/.claude/projects/.
func (a *Adapter) Walk(root string) iter.Seq[importlogs.SessionFile] {
	return func(yield func(importlogs.SessionFile) bool) {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				a.logger.Debug("walk error", "path", path, "error", err.Error())
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				a.logger.Debug("stat error", "path", path, "error", err.Error())
				return nil
			}
			if !yield(importlogs.SessionFile{Path: path, RawSize: info.Size()}) {
				return filepath.SkipAll
			}
			return nil
		})
	}
}

// rawEvent is the subset of the on-disk envelope this adapter inspects.
// We unmarshal once into this and once into a generic map only when we
// need fields outside the envelope (rare).
type rawEvent struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	Timestamp string          `json:"timestamp"`
	Cwd       string          `json:"cwd"`
	GitBranch string          `json:"gitBranch"`
	Message   json.RawMessage `json:"message"`
}

// nestedMessage is the user/assistant payload's `message` object.
// Content can be either a string (legacy) or a list of blocks.
type nestedMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

// Parse reads one session file end-to-end and returns a
// NormalizedSession. It never returns a *NormalizedSession with zero
// events — that case is reported as an error so the ingestor can
// count it as failed rather than silently shipping an empty session.
func (a *Adapter) Parse(sf importlogs.SessionFile) (*importlogs.NormalizedSession, error) {
	f, err := os.Open(sf.Path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", sf.Path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", sf.Path, err)
	}
	mtime := info.ModTime()

	var (
		events       []importlogs.NormalizedEvent
		sessionIDs   = map[string]struct{}{}
		cwd          string
		gitBranch    string
		earliestTS   time.Time
		latestTS     time.Time
		rawForHash   []byte
		syntheticIdx int
	)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // claude-code lines can be large
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Accumulate the raw bytes for the DeriveSessionID fallback.
		rawForHash = append(rawForHash, line...)
		rawForHash = append(rawForHash, '\n')

		var ev rawEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			a.logger.Debug("json parse error", "path", sf.Path, "error", err.Error())
			continue
		}
		if ev.SessionID != "" {
			sessionIDs[ev.SessionID] = struct{}{}
		}
		if cwd == "" && ev.Cwd != "" {
			cwd = ev.Cwd
		}
		if gitBranch == "" && ev.GitBranch != "" {
			gitBranch = ev.GitBranch
		}

		ts := parseTimestamp(ev.Timestamp)

		switch ev.Type {
		case "user":
			emitted := a.emitUserOrToolResult(ev.Message, ts, mtime, &syntheticIdx)
			for _, e := range emitted {
				e.Text = stripNUL(e.Text)
				events = append(events, e)
				trackTSRange(&earliestTS, &latestTS, e.Timestamp)
			}
		case "assistant":
			text := stripNUL(flattenAssistantContent(ev.Message))
			if text == "" {
				continue
			}
			t := resolveTS(ts, mtime, &syntheticIdx)
			events = append(events, importlogs.NormalizedEvent{
				Type:      "assistant",
				Text:      text,
				Timestamp: t,
			})
			trackTSRange(&earliestTS, &latestTS, t)
		case "system":
			// Emit system events even when their decoded Text is empty.
			// They carry no replayable text, but downstream metrics
			// (turn density, session length) count events; dropping
			// them would understate session activity and skew the
			// per-session feature signals replay relies on. The cost
			// is a few zero-text rows per session — cheap.
			t := resolveTS(ts, mtime, &syntheticIdx)
			events = append(events, importlogs.NormalizedEvent{
				Type:      "system",
				Text:      "",
				Timestamp: t,
			})
			trackTSRange(&earliestTS, &latestTS, t)
		default:
			a.logger.Debug("dropping unmapped event type",
				"path", sf.Path, "type", ev.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", sf.Path, err)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("no normalizable events in %s", sf.Path)
	}

	sessionID := pickSessionID(sessionIDs, sf.Path, rawForHash)

	// Defensive: if every event lacks a timestamp we still want a
	// monotonic StartedAt anchored at mtime.
	if earliestTS.IsZero() {
		earliestTS = mtime
	}
	endedAt := latestTS
	var endedAtPtr *time.Time
	if !endedAt.IsZero() && endedAt.After(earliestTS) {
		endedAtPtr = &endedAt
	}

	out := &importlogs.NormalizedSession{
		SourceTool:    sourceTool,
		SourceMachine: a.hostname,
		SessionID:     sessionID,
		StartedAt:     earliestTS,
		EndedAt:       endedAtPtr,
		AgentKind:     sourceTool,
		Events:        events,
	}
	if cwd != "" {
		out.Cwd = &cwd
	}
	if gitBranch != "" {
		out.GitBranch = &gitBranch
	}
	return out, nil
}

// emitUserOrToolResult turns one outer-type=="user" envelope into
// either a single "user" event (string content) or a fan-out of
// "tool_result" + "user" events (array content where blocks are
// classified individually). The synthetic-timestamp counter is
// shared across the fan-out so events keep a stable relative order.
func (a *Adapter) emitUserOrToolResult(msg json.RawMessage, ts time.Time, mtime time.Time, idx *int) []importlogs.NormalizedEvent {
	var nm nestedMessage
	if len(msg) == 0 || json.Unmarshal(msg, &nm) != nil {
		return nil
	}
	// String content path: wrap the whole thing as a user event.
	var asString string
	if json.Unmarshal(nm.Content, &asString) == nil {
		t := resolveTS(ts, mtime, idx)
		return []importlogs.NormalizedEvent{{
			Type:      "user",
			Text:      asString,
			Timestamp: t,
		}}
	}
	// Array content path: classify per block.
	var blocks []contentBlock
	if err := json.Unmarshal(nm.Content, &blocks); err != nil {
		return nil
	}
	var out []importlogs.NormalizedEvent
	var userTextParts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				userTextParts = append(userTextParts, b.Text)
			}
		case "tool_result":
			t := resolveTS(ts, mtime, idx)
			out = append(out, importlogs.NormalizedEvent{
				Type:      "tool_result",
				Text:      flattenToolResultContent(b.Content),
				Timestamp: t,
			})
		case "image":
			// Images don't survive into chapterhouse's text-only
			// episodic store; surface a placeholder so the turn count
			// stays accurate.
			userTextParts = append(userTextParts, "[image]")
		default:
			a.logger.Debug("dropping unknown user content block",
				"block_type", b.Type)
		}
	}
	if len(userTextParts) > 0 {
		t := resolveTS(ts, mtime, idx)
		out = append(out, importlogs.NormalizedEvent{
			Type:      "user",
			Text:      strings.Join(userTextParts, "\n"),
			Timestamp: t,
		})
	}
	return out
}

// flattenAssistantContent turns the assistant's message.content
// (string OR list of {text, thinking, tool_use} blocks) into a single
// string. We keep tool_use as a one-line summary so the conversation
// reads sensibly; thinking blocks are included because they're part
// of the assistant's contribution and downstream consumers can strip
// them if they want.
func flattenAssistantContent(msg json.RawMessage) string {
	var nm nestedMessage
	if len(msg) == 0 || json.Unmarshal(msg, &nm) != nil {
		return ""
	}
	var asString string
	if json.Unmarshal(nm.Content, &asString) == nil {
		return asString
	}
	var blocks []contentBlock
	if err := json.Unmarshal(nm.Content, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case "thinking":
			if b.Thinking != "" {
				parts = append(parts, b.Thinking)
			}
		case "tool_use":
			parts = append(parts, fmt.Sprintf("[tool_use:%s %s]", b.Name, string(b.Input)))
		}
	}
	return strings.Join(parts, "\n")
}

// flattenToolResultContent normalizes a tool_result's content (either
// a string or an array of {type:"text",text:"..."} blocks) into a
// single string.
func flattenToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return asString
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// parseTimestamp parses an ISO-8601 timestamp; returns zero time on
// failure so the caller can fall back to a synthetic.
func parseTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// resolveTS returns ts when set; otherwise a synthetic timestamp
// derived from mtime + idx*1s. The counter mutates so consecutive
// timestamp-less events stay strictly ordered.
func resolveTS(ts, mtime time.Time, idx *int) time.Time {
	if !ts.IsZero() {
		return ts
	}
	out := mtime.Add(time.Duration(*idx) * time.Second)
	*idx++
	return out
}

func trackTSRange(earliest, latest *time.Time, t time.Time) {
	if t.IsZero() {
		return
	}
	if earliest.IsZero() || t.Before(*earliest) {
		*earliest = t
	}
	if latest.IsZero() || t.After(*latest) {
		*latest = t
	}
}

// pickSessionID prefers the in-event sessionId when every event with
// one agreed; falls back to DeriveSessionID(name, raw) otherwise.
//
// Quirk worth knowing: claude-code's subagent / tool-results JSONLs
// live under a per-session subdirectory and ALL re-use the parent's
// sessionId. Treating them as the same chapterhouse session would
// collapse the parent + every child into one upsert. When we detect
// the on-disk pattern <parent-uuid>/{subagents,tool-results}/<file>
// we derive a stable child UUID from (parentSessionID, filename) so
// each child becomes its own session while staying linked to the
// parent through the deterministic namespace.
func pickSessionID(seen map[string]struct{}, path string, raw []byte) uuid.UUID {
	if len(seen) == 1 {
		var parsed uuid.UUID
		var ok bool
		for k := range seen {
			if id, err := uuid.Parse(k); err == nil {
				parsed = id
				ok = true
			}
		}
		if ok {
			parentDir := filepath.Base(filepath.Dir(path))
			grandDir := filepath.Base(filepath.Dir(filepath.Dir(path)))
			if (parentDir == "subagents" || parentDir == "tool-results") && uuidLike(grandDir) {
				return uuid.NewSHA1(parsed, []byte(parentDir+"/"+filepath.Base(path)))
			}
			return parsed
		}
	}
	return importlogs.DeriveSessionID(filepath.Base(path), raw)
}

// uuidLike reports whether s parses as a UUID.
func uuidLike(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// stripNUL removes literal U+0000 code points from s. Postgres's
// text type rejects them outright, and a single embedded NUL in a
// 5000-line session would otherwise sink the whole import — usually
// an artifact of upstream binary tool output that leaked into a
// JSON-escaped backslash-u-0000 escape.
func stripNUL(s string) string {
	if !strings.ContainsRune(s, 0) {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}
