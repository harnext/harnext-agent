package service

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

const maxStatusSize = 64 * 1024

// StatusFile is what the running agent publishes for the status command. It holds counts,
// codes and times only: no paths, no content, no token.
type StatusFile struct {
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Sessions         int    `json:"sessions"` // sessions currently tracked (distinct, not rows)
	UniqueResponses  int64  `json:"unique_responses"`
	SkippedLines     int64  `json:"skipped_lines"`
	AnalyzedTurns    int64  `json:"analyzed_user_turns"`
	UnanalyzedTurns  int64  `json:"unanalyzed_user_turns"`
	WatcherLastError string `json:"watcher_last_error,omitempty"`

	Copilot CopilotStatus `json:"copilot"`
	Config  ConfigStatus  `json:"config"`
	Upload  UploadStatus  `json:"upload"`
	// Backfill is the history import picture (30 days, newest first); see BackfillStatus.Line.
	Backfill BackfillStatus `json:"backfill"`
}

// CopilotStatus holds counts and whether the telemetry file exists; never its path.
type CopilotStatus struct {
	Sessions        int   `json:"sessions"`
	UniqueSpans     int64 `json:"unique_spans"`
	SkippedLines    int64 `json:"skipped_lines"`
	BackgroundSpans int64 `json:"background_spans"`
	File            bool  `json:"file"`
	// FileBytes is the telemetry file size; OwnFile is true when it is the agent's default file, which
	// the agent empties above watcher.DefaultTrimOver. A file at a user-chosen path is only reported.
	FileBytes int64 `json:"file_bytes,omitempty"`
	OwnFile   bool  `json:"own_file,omitempty"`

	ChatSessions     int   `json:"chat_sessions,omitempty"`
	ChatRequests     int64 `json:"chat_requests,omitempty"`
	ChatSkippedLines int64 `json:"chat_skipped_lines,omitempty"`

	// Folder split self-check (counts only): traces tied to a chat request by start time, traces that found none, and how many of
	// the requests with Copilot's own credits had trace credits that matched them (TraceMatches of TraceChecks).
	TracesBound   int64 `json:"traces_bound,omitempty"`
	TracesUnbound int64 `json:"traces_unbound,omitempty"`
	TraceChecks   int64 `json:"trace_checks,omitempty"`
	TraceMatches  int64 `json:"trace_matches,omitempty"`
}

type ConfigStatus struct {
	Loaded          bool      `json:"loaded"`
	Version         int64     `json:"version,omitempty"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	LastCheck       time.Time `json:"last_check,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	UpgradeRequired bool      `json:"upgrade_required,omitempty"`
	TrustedKeys     bool      `json:"trusted_keys"`
}

type UploadStatus struct {
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Accepted    int       `json:"accepted"`
	Rejected    int       `json:"rejected"`
	Deferred    int       `json:"deferred"`
	Invalid     int       `json:"invalid"`
	Unchanged   int       `json:"unchanged"`
}

// WriteStatus replaces the status file atomically.
func (p Paths) WriteStatus(s StatusFile) error {
	data, err := json.Marshal(s)
	if err != nil {
		return errors.New("service: encode status")
	}
	return writeAtomic(p.Dir, p.Status, data)
}

// ReadStatus returns the last published status; os.ErrNotExist-like errors mean none yet.
func (p Paths) ReadStatus() (StatusFile, error) {
	data, err := readLimited(p.Status, maxStatusSize)
	if errors.Is(err, os.ErrNotExist) {
		return StatusFile{}, os.ErrNotExist
	}
	if err != nil {
		return StatusFile{}, errors.New("service: read status")
	}
	var s StatusFile
	if json.Unmarshal(data, &s) != nil {
		return StatusFile{}, errors.New("service: status file is invalid")
	}
	return s, nil
}
