package cli

import (
	"encoding/json"
	"errors"

	"github.com/harnext/harnext-agent/internal/aggregate"
)

// rowsFromSnapshot rebuilds rows from the saved aggregator state; ok=false yields none.
func rowsFromSnapshot(blob []byte, ok bool) ([]aggregate.Row, error) {
	if !ok {
		return nil, nil
	}
	var st aggregate.State
	agg := aggregate.New()
	if json.Unmarshal(blob, &st) != nil || agg.Restore(st) != nil {
		return nil, errors.New("the saved state is unreadable; start the agent to rebuild it")
	}
	return agg.Rows(), nil
}
