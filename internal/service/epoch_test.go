package service

import (
	"strings"
	"testing"
	"time"
)

type memState map[string][]byte

func (m memState) LoadState(name string) ([]byte, bool, error) {
	b, ok := m[name]
	return b, ok, nil
}

func (m memState) SaveState(name string, blob []byte) error {
	m[name] = append([]byte(nil), blob...)
	return nil
}

// The first token a state sees (new machine or wiped state) has no limit: the last 30 days are imported. A different token
// starts clean from that day, and the limit survives restarts with the same token.
func TestResolveTokenEpoch(t *testing.T) {
	db := memState{}
	day1 := time.Date(2026, 11, 15, 22, 30, 0, 0, time.UTC)
	day2 := day1.Add(48 * time.Hour)

	if got, err := resolveTokenEpoch(db, "hxt_first-token", day1); err != nil || got != "" {
		t.Fatalf("first run: %q %v", got, err)
	}
	if got, _ := resolveTokenEpoch(db, "hxt_first-token", day2); got != "" {
		t.Fatalf("same token must stay unlimited: %q", got)
	}
	if got, _ := resolveTokenEpoch(db, "hxt_second-token", day2); got != "2026-11-17" {
		t.Fatalf("changed token: %q", got)
	}
	if got, _ := resolveTokenEpoch(db, "hxt_second-token", day2.Add(72*time.Hour)); got != "2026-11-17" {
		t.Fatalf("the limit must persist for the same token: %q", got)
	}
	if got, _ := resolveTokenEpoch(db, "hxt_third-token", day2.Add(96*time.Hour)); got != "2026-11-21" {
		t.Fatalf("another change moves the limit: %q", got)
	}
}

func TestTokenEpochNeverStoresTheToken(t *testing.T) {
	db := memState{}
	const token = "hxt_super-secret-value"
	if _, err := resolveTokenEpoch(db, token, time.Now()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(db[epochStateName]), "secret") {
		t.Fatalf("token leaked into state: %s", db[epochStateName])
	}
}

// A damaged record is treated as no record: no limit, and it is rewritten.
func TestResolveTokenEpochDamagedRecord(t *testing.T) {
	db := memState{epochStateName: []byte("not json")}
	if got, err := resolveTokenEpoch(db, "hxt_a", time.Now()); err != nil || got != "" {
		t.Fatalf("%q %v", got, err)
	}
	if got, _ := resolveTokenEpoch(db, "hxt_a", time.Now()); got != "" {
		t.Fatalf("rewritten record must match: %q", got)
	}
}
