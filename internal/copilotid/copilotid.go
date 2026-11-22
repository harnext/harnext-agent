// Package copilotid finds which GitHub account VS Code Copilot Chat uses. VS Code does not store the account's e-mail
// address, only its user name; the numeric user id and the Copilot plan are known only for the account that is active. Only
// a few well-known keys of the editor's own state database are read (a private copy is opened, nothing is written); tokens in
// the OS keychain are never touched. The numeric id is hashed at once; the user name is a display label.
package copilotid

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	hashPrefix   = "harnext:gh:v1:"
	maxKnown     = 20
	maxEntries   = 64
	maxValueSize = 64 << 10
)

var loginRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// IDHash is the account hash of a GitHub numeric user id; it must stay equal to what the server stores for a selected account.
func IDHash(id string) string {
	sum := sha256.Sum256([]byte(hashPrefix + strings.TrimSpace(id)))
	return hex.EncodeToString(sum[:])
}

// ValidLogin reports whether s looks like a GitHub user name.
func ValidLogin(s string) bool { return loginRE.MatchString(s) }

// PlanFromSKU maps the Copilot SKU VS Code caches to a plan name the server knows; "" when it is not recognised.
func PlanFromSKU(sku string) string {
	s := strings.ToLower(sku)
	switch {
	case s == "":
		return ""
	case strings.Contains(s, "enterprise"):
		return "enterprise"
	case strings.Contains(s, "business"):
		return "business"
	case strings.Contains(s, "free"):
		return "free"
	case strings.Contains(s, "subscriber"), strings.Contains(s, "pro"):
		return "pro"
	}
	return ""
}

// State is what the editor's state database says right now.
type State struct {
	Active   string   // user name of the account Copilot uses; "" when none
	ActiveID string   // numeric user id of the active account; "" when unknown
	Plan     string   // plan of the active account; "" when unknown
	Known    []string // every user name that used Copilot Chat on this machine (includes Active)
}

// Info is what is known about one account; both fields are empty for an account that was never active while observed.
type Info struct {
	IDHash string `json:"h,omitempty"`
	Plan   string `json:"p,omitempty"`
}

// Login is one discovered account.
type Login struct {
	Login string
	Info
}

type entry struct {
	Login string    `json:"l"`
	From  time.Time `json:"t"`
}

// Timeline remembers which account was active when, from the observations of State. It holds user names, hashes and plans only.
type Timeline struct {
	mu      sync.Mutex
	entries []entry
	info    map[string]Info
	known   map[string]struct{}
}

// Observe records st at now. Call it at start and periodically.
func (t *Timeline) Observe(st State, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.info == nil {
		t.info, t.known = map[string]Info{}, map[string]struct{}{}
	}
	active := st.Active
	if !ValidLogin(active) {
		active = ""
	}
	if n := len(t.entries); n == 0 || t.entries[n-1].Login != active {
		if n >= maxEntries {
			t.entries = t.entries[1:]
		}
		t.entries = append(t.entries, entry{Login: active, From: now.UTC()})
	}
	add := func(l string) {
		if ValidLogin(l) && len(t.known) < maxKnown {
			t.known[l] = struct{}{}
		}
	}
	for _, l := range st.Known {
		add(l)
	}
	if active != "" {
		add(active)
		in := t.info[active]
		if st.ActiveID != "" {
			in.IDHash = IDHash(st.ActiveID)
		}
		if st.Plan != "" {
			in.Plan = st.Plan
		}
		t.info[active] = in
	}
}

// LoginAt is the account that was active at the given time. Before the first observation it is only known when exactly one
// account has ever been seen on the machine; otherwise false (the session is not attributed).
func (t *Timeline) LoginAt(at time.Time) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.entries) == 0 {
		return "", false
	}
	if at.Before(t.entries[0].From) {
		if len(t.known) == 1 {
			for l := range t.known {
				return l, true
			}
		}
		return "", false
	}
	login := ""
	for _, e := range t.entries {
		if e.From.After(at) {
			break
		}
		login = e.Login
	}
	return login, login != ""
}

// HashAt is the account hash of the account active at the given time.
func (t *Timeline) HashAt(at time.Time) (hash, login string, ok bool) {
	login, ok = t.LoginAt(at)
	if !ok {
		return "", "", false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.info[login].IDHash
	return h, login, h != ""
}

// Logins lists every discovered account, accounts with a known id first, then by user name.
func (t *Timeline) Logins() []Login {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Login, 0, len(t.known))
	for l := range t.known {
		out = append(out, Login{Login: l, Info: t.info[l]})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.IDHash != "") != (b.IDHash != "") {
			return a.IDHash != ""
		}
		return a.Login < b.Login
	})
	return out
}

type snapshot struct {
	V       int             `json:"v"`
	Entries []entry         `json:"entries"`
	Info    map[string]Info `json:"info"`
	Known   []string        `json:"known"`
}

// Snapshot is the JSON the service stores between runs.
func (t *Timeline) Snapshot() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := snapshot{V: 1, Entries: t.entries, Info: t.info, Known: make([]string, 0, len(t.known))}
	for l := range t.known {
		s.Known = append(s.Known, l)
	}
	sort.Strings(s.Known)
	b, _ := json.Marshal(s)
	return b
}

// Restore loads a Snapshot; invalid data leaves the timeline empty.
func (t *Timeline) Restore(data []byte) error {
	var s snapshot
	if len(data) > 256<<10 || json.Unmarshal(data, &s) != nil || s.V != 1 || len(s.Entries) > maxEntries || len(s.Known) > maxKnown {
		return errInvalid
	}
	info, known := map[string]Info{}, map[string]struct{}{}
	for _, e := range s.Entries {
		if e.Login != "" && !ValidLogin(e.Login) {
			return errInvalid
		}
	}
	for l, in := range s.Info {
		if !ValidLogin(l) || (in.IDHash != "" && len(in.IDHash) != 64) {
			return errInvalid
		}
		info[l] = in
	}
	for _, l := range s.Known {
		if !ValidLogin(l) {
			return errInvalid
		}
		known[l] = struct{}{}
	}
	t.mu.Lock()
	t.entries, t.info, t.known = s.Entries, info, known
	t.mu.Unlock()
	return nil
}
