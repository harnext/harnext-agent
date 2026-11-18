package service

import (
	"encoding/json"
	"time"

	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/storage"
)

// PreviewRows returns the rows the agent would upload and the first day it may upload, read from the saved state. It never
// writes: the token epoch is only looked at, not stored. A state that was never run or cannot be read yields no rows.
func PreviewRows(db *storage.Store, token string, now time.Time) (rows []aggregate.Row, notBeforeDay string, err error) {
	claude := aggregate.New()
	if blob, ok, e := db.LoadState(stateName); e != nil {
		return nil, "", e
	} else if ok {
		var st aggregate.State
		if json.Unmarshal(blob, &st) == nil && claude.Restore(st) == nil {
			rows = append(rows, claude.Rows()...)
		}
	}
	cop := aggregate.NewCopilot()
	if blob, ok, e := db.LoadState(copilotStateName); e != nil {
		return nil, "", e
	} else if ok {
		var st aggregate.CopilotState
		if json.Unmarshal(blob, &st) == nil && cop.Restore(st) == nil {
			rows = append(rows, cop.Rows()...)
		}
	}
	if token == "" {
		return rows, "", nil
	}
	notBeforeDay, err = peekTokenEpoch(db, token, now)
	return rows, notBeforeDay, err
}

// peekTokenEpoch is resolveTokenEpoch without saving: what the next agent start would use.
func peekTokenEpoch(db stateStore, token string, now time.Time) (string, error) {
	var cur tokenEpoch
	blob, found, err := db.LoadState(epochStateName)
	if err != nil {
		return "", err
	}
	if found && json.Unmarshal(blob, &cur) == nil && cur.Fingerprint == tokenFingerprint(token) {
		return cur.NotBefore, nil
	}
	if found && cur.Fingerprint != "" {
		return now.UTC().Format(time.DateOnly), nil
	}
	return "", nil
}
