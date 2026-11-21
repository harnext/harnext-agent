package service

import (
	"encoding/json"

	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/storage"
)

// PreviewRows returns the rows the agent would upload, read from the saved state. It never writes. A state that was never
// run or cannot be read yields no rows.
func PreviewRows(db *storage.Store) (rows []aggregate.Row, err error) {
	claude := aggregate.New()
	if blob, ok, e := db.LoadState(stateName); e != nil {
		return nil, e
	} else if ok {
		var st aggregate.State
		if json.Unmarshal(blob, &st) == nil && claude.Restore(st) == nil {
			rows = append(rows, claude.Rows()...)
		}
	}
	cop := aggregate.NewCopilot()
	if blob, ok, e := db.LoadState(copilotStateName); e != nil {
		return nil, e
	} else if ok {
		var st aggregate.CopilotState
		if json.Unmarshal(blob, &st) == nil && cop.Restore(st) == nil {
			rows = append(rows, cop.Rows()...)
		}
	}
	return rows, nil
}
