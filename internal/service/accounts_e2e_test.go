package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
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
	"github.com/harnext/harnext-agent/internal/copilotid"
	"github.com/harnext/harnext-agent/internal/identity"
	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

const (
	acctMainEmail  = "main.user@example.test"
	acctWorkEmail  = "work.user@example.test"
	acctOtherEmail = "other.user@example.test"
	// Two Harnext accounts on one machine: personal (main) and company (work).
	tokMain = "hxt_" + "mainmainmainmainmainmainmainmainmainmain0001"
	tokWork = "hxt_" + "workworkworkworkworkworkworkworkworkwork0002"
	acctKID = "ktest"
)

// acctCloud is a fake server for ingest, the signed identity document and the account report. Every Harnext account (token)
// has its own bindings, as on the real server.
type acctCloud struct {
	srv  *httptest.Server
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey

	mu             sync.Mutex
	bindings       map[string][]identity.Binding // by token
	identityStatus int                           // non-zero: the identity endpoint answers with it
	signWith       ed25519.PrivateKey            // nil: the trusted key
	identityGets   int
	reports        map[string][][]byte // by token
	ingest         []ingestCall
	other          []string // every non-ingest request: URL and body
}

type ingestCall struct{ token, body string }

func newAcctCloud(t *testing.T) *acctCloud {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := &acctCloud{pub: pub, priv: priv, bindings: map[string][]identity.Binding{}, reports: map[string][][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ingest", c.handleIngest)
	mux.HandleFunc("/api/v1/agent-identity", c.handleIdentity)
	mux.HandleFunc("/api/v1/agent-accounts", c.handleAccounts)
	c.srv = httptest.NewServer(mux)
	t.Cleanup(c.srv.Close)
	return c
}

func (c *acctCloud) keys() remoteconfig.TrustedKeys { return remoteconfig.TrustedKeys{acctKID: c.pub} }

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (c *acctCloud) setBindings(token string, bs ...identity.Binding) {
	c.mu.Lock()
	c.bindings[token] = bs
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
		return a.Hash < b.Hash
	})
	h := sha256.New()
	h.Write([]byte("harnext:identity:v2\n"))
	for _, b := range sorted {
		fmt.Fprintf(h, "%s\t%s\n", b.Tool, b.Hash)
	}
	return int64(binary.BigEndian.Uint64(append([]byte{0, 0}, h.Sum(nil)[:6]...)))
}

// envelope signs the bindings of one token; the caller holds c.mu.
func (c *acctCloud) envelope(token string) []byte {
	type jb struct {
		Tool string `json:"tool"`
		Hash string `json:"hash"`
	}
	bs := c.bindings[token]
	list := make([]jb, 0, len(bs))
	for _, b := range bs {
		list = append(list, jb{b.Tool, b.Hash})
	}
	issued := time.Now().UTC().Truncate(time.Hour)
	const layout = "2006-01-02T15:04:05.000Z"
	payload, _ := json.Marshal(map[string]any{
		"payload_version": 2, "identity_version": acctFingerprint(bs),
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

func (c *acctCloud) known(token string) bool { return token == tokMain || token == tokWork }

func (c *acctCloud) handleIngest(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.ingest = append(c.ingest, ingestCall{bearer(r), string(b)})
	c.mu.Unlock()
	var env struct{ Sessions []json.RawMessage }
	_ = json.Unmarshal(b, &env)
	fmt.Fprintf(w, `{"accepted":%d,"rejected":0,"errors":[]}`, len(env.Sessions))
}

func (c *acctCloud) handleIdentity(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.identityGets++
	c.other = append(c.other, r.URL.String())
	tok := bearer(r)
	switch {
	case c.identityStatus != 0:
		w.WriteHeader(c.identityStatus)
	case !c.known(tok):
		w.WriteHeader(http.StatusUnauthorized)
	default:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(c.envelope(tok))
	}
}

func (c *acctCloud) handleAccounts(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.other = append(c.other, r.URL.String()+" "+string(body))
	tok := bearer(r)
	if r.Method != http.MethodPost || !c.known(tok) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	c.reports[tok] = append(c.reports[tok], body)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(c.envelope(tok))
}

func (c *acctCloud) reportList(token string) []identity.Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []identity.Report
	for _, b := range c.reports[token] {
		var rep identity.Report
		if err := json.Unmarshal(b, &rep); err != nil {
			panic("report body: " + err.Error())
		}
		out = append(out, rep)
	}
	return out
}

func (c *acctCloud) reportCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.reports {
		n += len(r)
	}
	return n
}

func (c *acctCloud) gets() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.identityGets
}

func (c *acctCloud) uploads() []ingestCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ingestCall(nil), c.ingest...)
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
	vscode   string // VS Code globalStorage directory
	log      *acctLog
}

func newAcctHarness(t *testing.T, tokens ...string) *acctHarness {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HARNEXT_NO_AUTO_UPDATE", "1")
	h := &acctHarness{cloud: newAcctCloud(t), home: t.TempDir(), log: &acctLog{},
		chatRoot: filepath.Join(t.TempDir(), "workspaceStorage"), otel: filepath.Join(t.TempDir(), "telemetry", "otel.jsonl"),
		vscode: filepath.Join(t.TempDir(), "globalStorage")}
	h.paths = PathsIn(filepath.Join(t.TempDir(), "agent"))
	if err := os.MkdirAll(filepath.Dir(h.otel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := h.paths.SaveSettings(Settings{ServerURL: h.cloud.srv.URL, AllowInsecureLocalhost: true, CopilotOTelPath: h.otel}); err != nil {
		t.Fatal(err)
	}
	if len(tokens) == 0 {
		tokens = []string{tokMain}
	}
	for _, tok := range tokens {
		if err := h.paths.SaveToken(tok); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func claudeBinding(email string) identity.Binding {
	return identity.Binding{Tool: identity.ToolClaudeCode, Hash: account.Hash(email)}
}

func copilotBinding(githubID string) identity.Binding {
	return identity.Binding{Tool: identity.ToolVSCodeCopilot, Hash: copilotid.IDHash(githubID)}
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

// vscodeState writes the editor's state database: the active GitHub account (login, numeric id) and the other known logins.
func (h *acctHarness) vscodeState(t *testing.T, active, id string, others ...string) {
	t.Helper()
	if err := os.MkdirAll(h.vscode, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(h.vscode, "state.vscdb")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	put := func(k, v string) {
		if _, err := db.Exec(`INSERT INTO ItemTable VALUES (?, ?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	usage := `[{"extensionId":"github.copilot-chat"}]`
	put("github.copilot-github", active)
	put("github-"+active+"-usages", usage)
	for _, o := range others {
		put("github-"+o+"-usages", usage)
	}
	put("defaultAccount.cachedPolicyData", fmt.Sprintf(`{"accountPolicyData":{"accountId":%q}}`, id))
	put("extensionsAssignmentFilterProvider.copilotSku", "copilot_for_business_seat_quota")
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
			Paths: h.paths, Version: "0.1.0", Home: h.home, ChatRoots: []string{h.chatRoot}, CopilotStateDirs: []string{h.vscode}, Keys: keys, Logf: h.log.logf,
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

type uploadedSession struct{ hash, token string }

// uploaded maps each uploaded session id to its account hash and the token that carried it.
func (h *acctHarness) uploaded(t *testing.T) map[string]uploadedSession {
	t.Helper()
	out := map[string]uploadedSession{}
	for _, call := range h.cloud.uploads() {
		var env struct {
			Sessions []struct {
				ID   string `json:"session_external_id"`
				Hash string `json:"account_hash"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal([]byte(call.body), &env); err != nil {
			t.Fatal(err)
		}
		for _, s := range env.Sessions {
			out[s.ID] = uploadedSession{s.Hash, call.token}
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

// harnextStatus is the gate picture of one stored account.
func (h *acctHarness) harnextStatus(t *testing.T, token string) (HarnextAccountStatus, bool) {
	t.Helper()
	for _, a := range h.status(t).Accounts.Harnext {
		if a.ID == AccountID(token) {
			return a, true
		}
	}
	return HarnextAccountStatus{}, false
}

// noClearIdentity fails when a clear e-mail address (its local part), a token, a GitHub user name or the numeric GitHub id shows up in
// anything that left the machine, in the status file or in the log (a GitHub user name only goes to the accounts endpoint).
func (h *acctHarness) noClearIdentity(t *testing.T) {
	t.Helper()
	raw, _ := os.ReadFile(h.paths.Status)
	all := []string{string(raw), h.log.text()}
	for _, c := range h.cloud.uploads() {
		all = append(all, c.body)
	}
	h.cloud.mu.Lock()
	all = append(all, h.cloud.other...)
	h.cloud.mu.Unlock()
	for _, text := range all {
		for _, bad := range []string{"main.user", "work.user", "other.user", tokMain, tokWork, h.home, "4242"} {
			if strings.Contains(text, bad) {
				t.Fatalf("clear identity %q found in:\n%s", bad, text)
			}
		}
	}
	for _, c := range h.cloud.uploads() {
		if strings.Contains(c.body, "@") || strings.Contains(c.body, "octocat") {
			t.Fatalf("ingest body holds an e-mail sign or a GitHub name: %s", c.body)
		}
	}
	for _, text := range []string{string(raw), h.log.text()} {
		if strings.Contains(text, "octocat") {
			t.Fatalf("GitHub user name found in:\n%s", text)
		}
	}
}

// Two Harnext accounts on one machine: each collects the Claude Code account that signs in with its e-mail, and a third Claude
// Code account that matches neither stays on the machine and is only counted.
func TestAccountsEndToEnd(t *testing.T) {
	h := newAcctHarness(t, tokMain, tokWork)
	main := h.claudeAccount(t, ".claude", acctMainEmail)
	work := h.claudeAccount(t, ".claude-work", acctWorkEmail)
	other := h.claudeAccount(t, ".claude-other", acctOtherEmail)
	h.session(t, main, "sess-main")
	h.session(t, work, "sess-work")
	h.session(t, other, "sess-other")
	h.cloud.setBindings(tokMain, claudeBinding(acctMainEmail))
	h.cloud.setBindings(tokWork, claudeBinding(acctWorkEmail))

	h.start(t, h.cloud.keys())

	// Each session leaves with its own account hash, carried by the token of the Harnext account that owns it.
	eventually(t, "both matched sessions uploaded", func() bool {
		u := h.uploaded(t)
		return u["sess-main"].hash != "" && u["sess-work"].hash != ""
	})
	u := h.uploaded(t)
	if got := u["sess-main"]; got.hash != account.Hash(acctMainEmail) || got.token != tokMain {
		t.Fatalf("main session: %+v", got)
	}
	if got := u["sess-work"]; got.hash != account.Hash(acctWorkEmail) || got.token != tokWork {
		t.Fatalf("work session: %+v", got)
	}

	// The unmatched account's session stays here; nothing about it is reported except a count.
	eventually(t, "reports sent", func() bool {
		return len(h.cloud.reportList(tokMain)) > 0 && len(h.cloud.reportList(tokWork)) > 0
	})
	for tok, label := range map[string]string{tokMain: "mai***@example.test", tokWork: "wor***@example.test"} {
		rep := h.cloud.reportList(tok)[0]
		if rep.Claude.Found != 3 || !rep.Claude.Matched || rep.Claude.Label != label {
			t.Fatalf("report of %s = %+v", AccountID(tok), rep.Claude)
		}
	}
	eventually(t, "status names the held back session", func() bool {
		s := h.status(t)
		return s.Accounts.Skipped["no_matching_account"] == 1 && s.Accounts.Sendable == 2 && len(s.Accounts.Harnext) == 2 && s.Accounts.ClaudeFound == 3
	})
	time.Sleep(300 * time.Millisecond)
	if _, sent := h.uploaded(t)["sess-other"]; sent {
		t.Fatal("the unmatched account's session was uploaded")
	}
	for _, id := range []string{AccountID(tokMain), AccountID(tokWork)} {
		var found bool
		for _, a := range h.status(t).Accounts.Harnext {
			if a.ID == id {
				found = a.Identity.State == IdentityOK && a.ClaudeMatched
			}
		}
		if !found {
			t.Fatalf("status of %s: %+v", id, h.status(t).Accounts)
		}
	}
	h.noClearIdentity(t)
}

// A Harnext account that is removed stops being used; the other keeps working.
func TestAccountsRevokedTokenDoesNotBlockTheOther(t *testing.T) {
	h := newAcctHarness(t, tokMain, tokWork)
	main := h.claudeAccount(t, ".claude", acctMainEmail)
	work := h.claudeAccount(t, ".claude-work", acctWorkEmail)
	h.session(t, main, "sess-main")
	h.session(t, work, "sess-work")
	h.cloud.setBindings(tokMain, claudeBinding(acctMainEmail))
	h.cloud.setBindings(tokWork, claudeBinding(acctWorkEmail))
	// The server no longer accepts the work token: every request with it is refused with 401.
	srv := h.cloud.srv.Config.Handler
	h.cloud.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) == tokWork {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		srv.ServeHTTP(w, r)
	})

	h.start(t, h.cloud.keys())
	eventually(t, "main session uploaded", func() bool { return h.uploaded(t)["sess-main"].hash != "" })
	time.Sleep(300 * time.Millisecond)
	if _, sent := h.uploaded(t)["sess-work"]; sent {
		t.Fatal("a session was uploaded with a refused token")
	}
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
			h.cloud.setBindings(tokMain, claudeBinding(acctMainEmail))
			h.cloud.set(tc.setup)

			h.start(t, tc.keys(h.cloud))
			eventually(t, "session read and held back", func() bool {
				a, ok := h.harnextStatus(t, tokMain)
				return ok && a.Identity.State == tc.state && h.status(t).Accounts.Skipped["identity_pending"] == 1
			})
			time.Sleep(400 * time.Millisecond)
			if n := len(h.cloud.uploads()); n != 0 {
				t.Fatalf("%d uploads without a valid identity", n)
			}
			if n := h.cloud.reportCount(); n != 0 {
				t.Fatalf("%d account reports without a valid identity", n)
			}
			if tc.asked && h.cloud.gets() == 0 {
				t.Fatal("the identity was never requested")
			}
			if s := h.status(t); s.Accounts.Sendable != 0 {
				t.Fatalf("status = %+v", s.Accounts)
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

// A Copilot session belongs to the GitHub account VS Code used when it ran, and goes out only when the user selected that account.
func TestAccountsCopilotSelectedGitHubAccount(t *testing.T) {
	for _, tc := range []struct {
		name     string
		editor   bool   // the editor's state database exists
		selected string // numeric GitHub id the Harnext account selected ("" = nothing selected)
		want     string // skip reason when nothing is uploaded
	}{
		{"selected account", true, "4242", ""},
		{"another account selected", true, "9999", "no_matching_account"},
		{"nothing selected", true, "", "no_matching_account"},
		{"no editor state", false, "4242", "copilot_account_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAcctHarness(t)
			(&chatHarness{otel: h.otel, chatRoot: h.chatRoot}).writeFiles(t)
			if tc.editor {
				h.vscodeState(t, "octocat", "4242")
			}
			// Claude Code is bound so that the identity document is valid; Copilot only when something is selected.
			bs := []identity.Binding{claudeBinding(acctMainEmail)}
			if tc.selected != "" {
				bs = append(bs, copilotBinding(tc.selected))
			}
			h.cloud.setBindings(tokMain, bs...)

			h.start(t, h.cloud.keys())
			if tc.want == "" {
				eventually(t, "copilot session uploaded", func() bool {
					s := latestSession(t, &ingestRecorder{bodies: bodiesOf(h.cloud.uploads())})
					return s != nil && inputOf(s) == 600
				})
				got := latestSession(t, &ingestRecorder{bodies: bodiesOf(h.cloud.uploads())})
				if got["account_hash"] != copilotid.IDHash("4242") {
					t.Fatalf("account_hash = %v", got["account_hash"])
				}
				if calls := h.cloud.uploads(); calls[len(calls)-1].token != tokMain {
					t.Fatal("uploaded with the wrong token")
				}
			} else {
				eventually(t, "copilot session held back", func() bool { return h.status(t).Accounts.Skipped[tc.want] == 1 })
				time.Sleep(300 * time.Millisecond)
				if n := len(h.cloud.uploads()); n != 0 {
					t.Fatalf("%d uploads, want none", n)
				}
			}
			// The GitHub accounts found are offered to the Harnext account only while none is selected (a selection made for an
			// account that is not on this machine leaves nothing to offer here).
			if tc.editor && tc.selected == "" {
				eventually(t, "github account reported", func() bool { return len(h.cloud.reportList(tokMain)) > 0 })
				var logins []identity.CopilotLogin
				for _, rep := range h.cloud.reportList(tokMain) {
					logins = append(logins, rep.Copilot.Logins...)
				}
				if len(logins) == 0 || logins[0].Login != "octocat" || logins[0].IDHash != copilotid.IDHash("4242") || logins[0].Plan != "business" {
					t.Fatalf("reported logins = %+v", logins)
				}
			}
			h.noClearIdentity(t)
		})
	}
}

func bodiesOf(calls []ingestCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.body
	}
	return out
}
