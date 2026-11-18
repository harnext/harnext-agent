package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// epochStateName holds which token this agent state belongs to.
const epochStateName = "token_epoch"

// stateStore is the part of storage.Store the epoch needs.
type stateStore interface {
	LoadState(name string) ([]byte, bool, error)
	SaveState(name string, blob []byte) error
}

type tokenEpoch struct {
	// Fingerprint is a short hash of the token; the token itself is never stored here.
	Fingerprint string `json:"fp"`
	// NotBefore (YYYY-MM-DD, UTC) is the day the token changed; usage of earlier days is not uploaded.
	NotBefore string `json:"not_before,omitempty"`
}

func tokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// resolveTokenEpoch returns the first day whose usage may be uploaded ("" = no limit). A state that never saw a token (first
// run, a new machine, or a wiped state) has no limit, so a first install still imports the last 30 days. When the token is
// not the one the state was last used with, the agent starts clean: history from before today is not sent to whatever
// organization the new token belongs to. The limit stays until the next token change.
func resolveTokenEpoch(db stateStore, token string, now time.Time) (string, error) {
	fp := tokenFingerprint(token)
	var cur tokenEpoch
	blob, found, err := db.LoadState(epochStateName)
	if err != nil {
		return "", err
	}
	if found && json.Unmarshal(blob, &cur) == nil && cur.Fingerprint == fp {
		return cur.NotBefore, nil
	}
	next := tokenEpoch{Fingerprint: fp}
	if found && cur.Fingerprint != "" {
		next.NotBefore = now.UTC().Format(time.DateOnly)
	}
	out, err := json.Marshal(next)
	if err != nil {
		return "", err
	}
	if err := db.SaveState(epochStateName, out); err != nil {
		return "", err
	}
	return next.NotBefore, nil
}
