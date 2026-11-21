package service

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/account"
	"github.com/harnext/harnext-agent/internal/identity"
	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

const (
	acctMainEmail    = "main.user@example.test"
	acctWorkEmail    = "work.user@example.test"
	acctCopilotEmail = "copilot.user@example.test"
	acctRouteMain    = "11111111-1111-4111-8111-111111111111"
	acctRouteWork    = "22222222-2222-4222-8222-222222222222"
	acctRouteCop     = "33333333-3333-4333-8333-333333333333"
	acctKID          = "ktest"
)

// acctCloud is a fake server for ingest, the signed identity document and the account report.
type acctCloud struct {
	srv  *httptest.Server
	rec  *ingestRecorder
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey

	mu             sync.Mutex
	bindings       []identity.Binding
	identityStatus int                // non-zero: the identity endpoint answers with it
	signWith       ed25519.PrivateKey // nil: the trusted key
	identityGets   int
	reports        [][]byte
	other          []string // every non-ingest request: URL and body
}

func newAcctCloud(t *testing.T) *acctCloud {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := &acctCloud{rec: &ingestRecorder{}, pub: pub, priv: priv}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ingest", c.rec.handler)
	mux.HandleFunc("/api/v1/agent-identity", c.handleIdentity)
	mux.HandleFunc("/api/v1/agent-accounts", c.handleAccounts)
	c.srv = httptest.NewServer(mux)
	t.Cleanup(c.srv.Close)
	return c
}

func (c *acctCloud) keys() remoteconfig.TrustedKeys { return remoteconfig.TrustedKeys{acctKID: c.pub} }

func (c *acctCloud) setBindings(bs ...identity.Binding) {
	c.mu.Lock()
	c.bindings = bs
	c.mu.Unlock()
}

func (c *acctCloud) set(fn func(c *acctCloud)) {
	c.mu.Lock()
	fn(c)
	c.mu.Unlock()
}

func acctFingerprint(bs []identity.Binding) int64 {
	sorted := append([]identity.Binding(nil), bs...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Tool != b.Tool {
			return a.Tool < b.Tool
		}
		if a.Hash != b.Hash {
			return a.Hash < b.Hash
		}
		return a.RouteID < b.RouteID
	})
	h := sha256.New()
	h.Write([]byte("harnext:identity:v1\n"))
	for _, b := range sorted {
		fmt.Fprintf(h, "%s\t%s\t%s\n", b.Tool, b.Hash, b.RouteID)
	}
	return int64(binary.BigEndian.Uint64(append([]byte{0, 0}, h.Sum(nil)[:6]...)))
}

// envelope signs the current bindings; the caller holds c.mu.
func (c *acctCloud) envelope() []byte {
	type jb struct {
		Tool    string `json:"tool"`
		Hash    string `json:"hash"`
		RouteID string `json:"route_id"`
	}
	list := make([]jb, 0, len(c.bindings))
	for _, b := range c.bindings {
		list = append(list, jb{b.Tool, b.Hash, b.RouteID})
	}
	issued := time.Now().UTC().Truncate(time.Hour)
	const layout = "2006-01-02T15:04:05.000Z"
	payload, _ := json.Marshal(map[string]any{
		"payload_version": 1, "identity_version": acctFingerprint(c.bindings),
		"issued_at": issued.Format(layout), "expires_at": issued.Add(24 * time.Hour).Format(layout), "bindings": list,
	})
	key := c.priv
	if c.signWith != nil {
		key = c.signWith
	}
	body, _ := json.Marshal(remoteconfig.Envelope{
		Payload: base64.RawURLEncoding.EncodeToString(payload),
		Sig:     base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload)),
		KID:     acctKID,
	})
	return body
}

func (c *acctCloud) handleIdentity(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.identityGets++
	c.other = append(c.other, r.URL.String())
	switch {
	case c.identityStatus != 0:
		w.WriteHeader(c.identityStatus)
	case r.Header.Get("Authorization") != "Bearer "+testToken:
		w.WriteHeader(http.StatusUnauthorized)
	default:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(c.envelope())
	}
}

func (c *acctCloud) handleAccounts(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.other = append(c.other, r.URL.String()+" "+string(body))
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	c.reports = append(c.reports, body)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(c.envelope())
}

func (c *acctCloud) reportList() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.reports...)
}

func (c *acctCloud) gets() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.identityGets
}

// reportedAccounts is every account of every report received so far.
func (c *acctCloud) reportedAccounts(t *testing.T) []identity.Account {
	t.Helper()
	var out []identity.Account
	for _, b := range c.reportList() {
		var req struct {
			Accounts []identity.Account `json:"accounts"`
		}
		if err := json.Unmarshal(b, &req); err != nil {
			t.Fatalf("report body: %v", err)
		}
		out = append(out, req.Accounts...)
	}
	return out
}

type acctLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *acctLog) logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *acctLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

type acctHarness struct {
	cloud    *acctCloud
	paths    Paths
	home     string
	chatRoot string
	otel     string
	log      *acctLog
}

func newAcctHarness(t *testing.T) *acctHarness {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HARNEXT_NO_AUTO_UPDATE", "1")
	h := &acctHarness{cloud: newAcctCloud(t), home: t.TempDir(), log: &acctLog{},
		chatRoot: filepath.Join(t.TempDir(), "workspaceStorage"), otel: filepath.Join(t.TempDir(), "telemetry", "otel.jsonl")}
	h.paths = PathsIn(filepath.Join(t.TempDir(), "agent"))
	if err := os.MkdirAll(filepath.Dir(h.otel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := h.paths.SaveSettings(Settings{ServerURL: h.cloud.srv.URL, AllowInsecureLocalhost: true, CopilotOTelPath: h.otel}); err != nil {
		t.Fatal(err)
	}
	if err := h.paths.SaveToken(testToken); err != nil {
		t.Fatal(err)
	}
	return h
}

func acctBinding(tool, email, route string) identity.Binding {
	return identity.Binding{Tool: tool, Hash: account.Hash(email), RouteID: route}
}

// claudeAccount writes a fake global config for one Claude config directory and returns the directory.
func (h *acctHarness) claudeAccount(t *testing.T, dir, email string) string {
	t.Helper()
	cfg := filepath.Join(h.home, dir, ".claude.json")
	if dir == ".claude" {
		cfg = filepath.Join(h.home, ".claude.json") // the default directory keeps its config next to it
	}
	body := fmt.Sprintf(`{"numStartups":3,"oauthAccount":{"accountUuid":"fake-uuid","emailAddress":%q}}`, email)
	if err := os.MkdirAll(filepath.Join(h.home, dir, "projects", "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(h.home, dir, "projects", "proj")
}

func acctTranscript(id string, at time.Time) string {
	user := fmt.Sprintf(`{"type":"user","sessionId":%q,"uuid":"u_%s","cwd":"/home/dev/example-project","timestamp":%q,"message":{"role":"user","content":"please fix the failing test"}}`,
		id, id, at.Add(-time.Second).UTC().Format("2006-01-02T15:04:05.000Z"))
	asst := fmt.Sprintf(`{"type":"assistant","sessionId":%q,"requestId":"req_%s","uuid":"a_%s","cwd":"/home/dev/example-project","timestamp":%q,`+
		`"message":{"id":"msg_%s","model":"claude-sonnet-5","content":[{"type":"text","text":"SECRET-ANSWER-TEXT"}],"usage":{"input_tokens":10,"output_tokens":100}}}`,
		id, id, id, at.UTC().Format("2006-01-02T15:04:05.000Z"), id)
	return user + "\n" + asst + "\n"
}

func (h *acctHarness) session(t *testing.T, projectDir, id string) {
	t.Helper()
	appendFile(t, filepath.Join(projectDir, id+".jsonl"), acctTranscript(id, time.Now().Add(-10*time.Minute)))
}

// start runs the real agent loop with the production gate; periods are short and uploads wait for a wake-up.
func (h *acctHarness) start(t *testing.T, keys remoteconfig.TrustedKeys) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{
			Paths: h.paths, Version: "0.1.0", Home: h.home, ChatRoots: []string{h.chatRoot}, Keys: keys, Logf: h.log.logf,
			FlushInterval: 30 * time.Millisecond, UploadPeriod: time.Hour, IdentityPeriod: 50 * time.Millisecond,
			AccountCheckPeriod: 50 * time.Millisecond, AccountReportEvery: time.Hour, AccountReportGap: 50 * time.Millisecond,
		})
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("Run did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// uploaded maps each uploaded session id to its account hash.
func (h *acctHarness) uploaded(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, b := range h.cloud.rec.all() {
		var env struct {
			Sessions []struct {
				ID   string `json:"session_external_id"`
				Hash string `json:"account_hash"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal([]byte(b), &env); err != nil {
			t.Fatal(err)
		}
		for _, s := range env.Sessions {
			out[s.ID] = s.Hash
		}
	}
	return out
}

func (h *acctHarness) status(t *testing.T) StatusFile {
	t.Helper()
	s, err := h.paths.ReadStatus()
	if err != nil {
		return StatusFile{}
	}
	return s
}

// noClearIdentity fails when a clear e-mail address (its local part) shows up in anything that left the machine, in the status file
// or in the log.
func (h *acctHarness) noClearIdentity(t *testing.T) {
	t.Helper()
	raw, _ := os.ReadFile(h.paths.Status)
	all := []string{string(raw), h.log.text()}
	all = append(all, h.cloud.rec.all()...)
	h.cloud.mu.Lock()
	all = append(all, h.cloud.other...)
	h.cloud.mu.Unlock()
	for _, text := range all {
		for _, bad := range []string{"main.user", "work.user", "copilot.user", testToken, h.home} {
			if strings.Contains(text, bad) {
				t.Fatalf("clear identity %q found in:\n%s", bad, text)
			}
		}
	}
	for _, b := range h.cloud.rec.all() {
		if strings.Contains(b, "@") {
			t.Fatalf("ingest body holds an e-mail sign: %s", b)
		}
	}
}

func TestAccountsEndToEnd(t *testing.T) {
	h := newAcctHarness(t)
	main := h.claudeAccount(t, ".claude", acctMainEmail)
	work := h.claudeAccount(t, ".claude-work", acctWorkEmail)
	h.session(t, main, "sess-main")
	h.session(t, work, "sess-work")
	h.cloud.setBindings(acctBinding(identity.ToolClaudeCode, acctMainEmail, acctRouteMain))
	mainHash, workHash := account.Hash(acctMainEmail), account.Hash(acctWorkEmail)

	h.start(t, h.cloud.keys())

	// (a) Only the bound account's session is uploaded, with its own account hash.
	eventually(t, "bound session uploaded", func() bool { return h.uploaded(t)["sess-main"] != "" })
	if got := h.uploaded(t)["sess-main"]; got != mainHash {
		t.Fatalf("account_hash = %q, want %q", got, mainHash)
	}

	// (b) The unbound account's session stays here and is reported with a masked label and its session count.
	eventually(t, "unbound account reported", func() bool { return len(h.cloud.reportedAccounts(t)) > 0 })
	accts := h.cloud.reportedAccounts(t)
	if len(accts) != 1 || accts[0] != (identity.Account{Tool: identity.ToolClaudeCode, Hash: workHash, Label: "w***@example.test", Sessions30d: 1}) {
		t.Fatalf("reported accounts = %+v", accts)
	}
	eventually(t, "status names the held back account", func() bool {
		s := h.status(t)
		return s.Identity.State == IdentityOK && s.Accounts.Skipped["account_unbound"] == 1 && s.Accounts.Sendable == 1 && len(s.Accounts.Pending) == 1
	})
	time.Sleep(300 * time.Millisecond)
	if _, sent := h.uploaded(t)["sess-work"]; sent {
		t.Fatal("the unbound account's session was uploaded")
	}
	if n := len(h.cloud.reportList()); n != 1 {
		t.Fatalf("%d reports, want exactly 1 (unchanged candidates are not reported again within the refresh period)", n)
	}

	// (f) The status file lists the skip reasons and holds no clear e-mail.
	s := h.status(t)
	if p := s.Accounts.Pending[0]; p.Label != "w***@example.test" || p.Sessions != 1 || p.Tool != identity.ToolClaudeCode {
		t.Fatalf("pending = %+v", p)
	}
	if s.Identity.Bindings != 1 || s.Upload.Skipped != 1 {
		t.Fatalf("identity %+v, upload %+v", s.Identity, s.Upload)
	}
	h.noClearIdentity(t)

	// (c) A binding that appears later makes the held session upload without a restart.
	h.cloud.setBindings(
		acctBinding(identity.ToolClaudeCode, acctMainEmail, acctRouteMain),
		acctBinding(identity.ToolClaudeCode, acctWorkEmail, acctRouteWork),
	)
	eventually(t, "session uploaded after its account was bound", func() bool { return h.uploaded(t)["sess-work"] != "" })
	if got := h.uploaded(t)["sess-work"]; got != workHash {
		t.Fatalf("account_hash = %q, want %q", got, workHash)
	}
	eventually(t, "nothing pending any more", func() bool {
		s := h.status(t)
		return s.Accounts.Sendable == 2 && len(s.Accounts.Pending) == 0 && len(s.Accounts.Skipped) == 0
	})
	h.noClearIdentity(t)
}

// Without a valid signed identity nothing leaves the machine and nothing is reported.
func TestAccountsFailClosedWithoutValidIdentity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(c *acctCloud)
		keys  func(c *acctCloud) remoteconfig.TrustedKeys
		state string
		asked bool
	}{
		{"unreachable", func(c *acctCloud) { c.identityStatus = http.StatusInternalServerError }, (*acctCloud).keys, IdentityPending, true},
		{"bad signature", func(c *acctCloud) {
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			c.signWith = other
		}, (*acctCloud).keys, IdentityPending, true},
		{"no trusted keys", func(*acctCloud) {}, func(*acctCloud) remoteconfig.TrustedKeys { return remoteconfig.TrustedKeys{} }, IdentityNoTrustedKeys, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAcctHarness(t)
			main := h.claudeAccount(t, ".claude", acctMainEmail)
			h.session(t, main, "sess-main")
			h.cloud.setBindings(acctBinding(identity.ToolClaudeCode, acctMainEmail, acctRouteMain))
			h.cloud.set(tc.setup)

			h.start(t, tc.keys(h.cloud))
			eventually(t, "session read and held back", func() bool {
				s := h.status(t)
				return s.Identity.State == tc.state && s.Accounts.Skipped["identity_pending"] == 1
			})
			time.Sleep(400 * time.Millisecond)
			if n := len(h.cloud.rec.all()); n != 0 {
				t.Fatalf("%d uploads without a valid identity", n)
			}
			if n := len(h.cloud.reportList()); n != 0 {
				t.Fatalf("%d account reports without a valid identity", n)
			}
			if tc.asked && h.cloud.gets() == 0 {
				t.Fatal("the identity was never requested")
			}
			if s := h.status(t); s.Accounts.Sendable != 0 || s.Identity.Bindings != 0 {
				t.Fatalf("status = %+v / %+v", s.Identity, s.Accounts)
			}
			if tc.name == "no trusted keys" && h.cloud.gets() != 0 {
				t.Fatal("identity requested without trusted keys")
			}
			if tc.name == "no trusted keys" && !strings.Contains(h.log.text(), "no trusted keys") {
				t.Fatalf("no log line about the missing keys:\n%s", h.log.text())
			}
			h.noClearIdentity(t)
		})
	}
}

// A Copilot session belongs to the git user.email of its workspace folder.
func TestAccountsCopilotMatchedByGitEmail(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		bound  bool
		want   string // skip reason when nothing is uploaded
	}{
		{"bound git email", "[user]\n\temail = " + acctCopilotEmail + "\n", true, ""},
		{"unbound git email", "[user]\n\temail = " + acctCopilotEmail + "\n", false, "account_unbound"},
		{"no git email", "[core]\n\tbare = false\n", true, "git_email_missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAcctHarness(t)
			repo := filepath.Join(t.TempDir(), "repo")
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{"config": tc.config, "HEAD": "ref: refs/heads/main\n"} {
				if err := os.WriteFile(filepath.Join(repo, ".git", name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			(&chatHarness{otel: h.otel, chatRoot: h.chatRoot}).writeFiles(t)
			uri := filepath.ToSlash(repo)
			if !strings.HasPrefix(uri, "/") {
				uri = "/" + uri
			}
			ws := filepath.Join(h.chatRoot, "hash0001", "workspace.json")
			if err := os.WriteFile(ws, []byte(fmt.Sprintf(`{"folder":%q}`, "file://"+uri)), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.bound {
				h.cloud.setBindings(acctBinding(identity.ToolVSCodeCopilot, acctCopilotEmail, acctRouteCop))
			} else {
				h.cloud.setBindings(acctBinding(identity.ToolVSCodeCopilot, "someone.else@example.test", acctRouteCop))
			}

			h.start(t, h.cloud.keys())
			if tc.want == "" {
				eventually(t, "copilot session uploaded", func() bool {
					s := latestSession(t, h.cloud.rec)
					return s != nil && inputOf(s) == 600
				})
				if got := latestSession(t, h.cloud.rec)["account_hash"]; got != account.Hash(acctCopilotEmail) {
					t.Fatalf("account_hash = %v", got)
				}
			} else {
				eventually(t, "copilot session held back", func() bool { return h.status(t).Accounts.Skipped[tc.want] == 1 })
				time.Sleep(300 * time.Millisecond)
				if n := len(h.cloud.rec.all()); n != 0 {
					t.Fatalf("%d uploads, want none", n)
				}
				if tc.want == "account_unbound" {
					eventually(t, "copilot account reported", func() bool { return len(h.cloud.reportedAccounts(t)) > 0 })
					accts := h.cloud.reportedAccounts(t)
					if len(accts) != 1 || accts[0].Tool != identity.ToolVSCodeCopilot || accts[0].Label != "c***@example.test" || accts[0].Sessions30d != 1 {
						t.Fatalf("reported accounts = %+v", accts)
					}
				}
			}
			h.noClearIdentity(t)
			if bytes.Contains([]byte(h.log.text()), []byte("example.test")) {
				t.Fatalf("log holds a domain:\n%s", h.log.text())
			}
		})
	}
}
