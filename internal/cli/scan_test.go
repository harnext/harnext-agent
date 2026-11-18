package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/scan"
)

const (
	scanSecretPrompt = "my secret sk-FAKE0123456789abcdef and DB_PASSWORD=hunter2-fake please"
	scanSecretTool   = "API_KEY=fake-api-key-value"
	scanFakeUser     = "fakeuser-x"
	scanManageToken  = "FAKE-MANAGE-TOKEN-0123456789"
)

// scanEnv is an isolated machine: a fake home, an agent directory and one Claude Code transcript with fake secrets in it.
type scanEnv struct {
	root, home, agentHome, data string
}

func newScanEnv(t *testing.T, withData bool) *scanEnv {
	t.Helper()
	root := t.TempDir()
	e := &scanEnv{root: root, home: filepath.Join(root, "Users", scanFakeUser), agentHome: filepath.Join(root, "agent-home"), data: filepath.Join(root, "data")}
	for _, d := range []string{e.home, e.agentHome, filepath.Join(e.data, "claude", "proj")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", e.home)
	t.Setenv("USERPROFILE", e.home)
	t.Setenv("HARNEXT_HOME", e.agentHome)

	if withData {
		b, err := os.ReadFile(filepath.FromSlash("../../testdata/claude-code/session-basic.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		day := time.Now().UTC().AddDate(0, 0, -2).Format(time.DateOnly)
		cwd, _ := json.Marshal(filepath.Join(e.home, "work", "plain"))
		s := strings.NewReplacer(
			"2026-01-01", day,
			"/home/dev/example-project", strings.Trim(string(cwd), `"`),
			"fixture prompt one", scanSecretPrompt,
			"fixture file content", scanSecretTool,
		).Replace(string(b))
		if err := os.WriteFile(filepath.Join(e.data, "claude", "proj", "s.jsonl"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	oldOptions, oldTTY := scanOptions, scanTTY
	t.Cleanup(func() { scanOptions, scanTTY = oldOptions, oldTTY })
	scanOptions = func() scan.Options {
		return scan.Options{
			ClaudeRoot: filepath.Join(e.data, "claude"), CopilotOTelPath: filepath.Join(e.data, "otel.jsonl"),
			ChatRoots: []string{filepath.Join(e.data, "chat")},
		}
	}
	scanTTY = func() (io.ReadCloser, error) { return nil, errors.New("no terminal") }
	return e
}

// answers makes the terminal return the given text; the returned counter says how often it was opened.
func answers(t *testing.T, text string) *int {
	t.Helper()
	opened := new(int)
	scanTTY = func() (io.ReadCloser, error) {
		*opened++
		return io.NopCloser(strings.NewReader(text)), nil
	}
	return opened
}

type draftServer struct {
	*httptest.Server
	mu      sync.Mutex
	bodies  [][]byte
	agents  []string
	authHdr []string
}

func (d *draftServer) hits() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.bodies)
}

// newDraftServer answers every POST with status; 201 carries a manage link on the same host unless link is set.
func newDraftServer(t *testing.T, status int, link string) *draftServer {
	t.Helper()
	d := &draftServer{}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.bodies = append(d.bodies, b)
		d.agents = append(d.agents, r.Header.Get("User-Agent"))
		d.authHdr = append(d.authHdr, r.Header.Get("Authorization"))
		d.mu.Unlock()
		if status != http.StatusCreated {
			w.Header().Set("Location", "http://evil.example/redirect")
			w.WriteHeader(status)
			io.WriteString(w, "server says hunter2-fake sk-FAKE0123456789abcdef http://evil.example/leak")
			return
		}
		l := link
		if l == "" {
			l = "http://" + r.Host + "/cards/new#" + scanManageToken
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"manage_url": l, "expires_at": "2026-11-19T10:00:00Z"})
	}))
	t.Cleanup(d.Close)
	return d
}

func runScan(t *testing.T, serverDefault string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	root := New(Env{Paths: newPaths(t), Version: "9.9.9", In: strings.NewReader(""), Out: &out, Err: &errb, ServerDefault: serverDefault})
	root.SetArgs(append([]string{"scan"}, args...))
	err := root.Execute()
	return result{out.String(), errb.String(), err}
}

func dev(srv *draftServer, args ...string) []string {
	return append([]string{"--server", srv.URL, "--dev-allow-insecure-localhost"}, args...)
}

func treeOf(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		lines = append(lines, fmt.Sprintf("%s|%v|%d|%d", rel, d.IsDir(), info.Size(), info.ModTime().UnixNano()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

func TestScanHappyPathSendsExactlyWhatIsShown(t *testing.T) {
	e := newScanEnv(t, true)
	srv := newDraftServer(t, http.StatusCreated, "")
	opened := answers(t, "y\nn\ny\ny\n")

	before := [3]string{treeOf(t, e.home), treeOf(t, e.agentHome), treeOf(t, e.data)}
	r := runScan(t, "", dev(srv)...)
	if r.failure != nil {
		t.Fatalf("failure = %v\nstderr = %s", r.failure, r.err)
	}
	if srv.hits() != 1 || *opened != 1 {
		t.Fatalf("hits=%d tty opened=%d", srv.hits(), *opened)
	}
	body := srv.bodies[0]
	if !strings.Contains(r.err, string(body)+"\n") {
		t.Fatalf("the sent body is not shown byte for byte.\nbody:\n%s\nstderr:\n%s", body, r.err)
	}
	var p scan.CardPayload
	if err := json.Unmarshal(body, &p); err != nil || p.PayloadVersion != 1 || p.AgentVersion != "9.9.9" || p.Sessions != 1 || len(p.Usage) == 0 {
		t.Fatalf("body = %s err=%v", body, err)
	}
	if bytes.Contains(body, []byte(`"folders"`)) {
		t.Error("folders were sent although the answer was no")
	}
	if srv.agents[0] != "harnext-agent/9.9.9" || srv.authHdr[0] != "" {
		t.Errorf("user agent %q, authorization %q", srv.agents[0], srv.authHdr[0])
	}
	link := srv.URL + "/cards/new#" + scanManageToken
	if !strings.Contains(r.out, "Open this private link to finish: "+link+"\n") ||
		!strings.Contains(r.out, "It expires 2026-11-19T10:00:00Z. Anyone with this link can publish or discard your card, so keep it to yourself. Nothing is public until you publish.") {
		t.Errorf("stdout = %q", r.out)
	}
	if strings.Count(r.out+r.err, scanManageToken) != 1 {
		t.Error("the private link is printed more than once")
	}
	for _, q := range []string{"Create a shareable card from this summary? Only numbers are sent, no prompts, replies or code. [y/N]",
		"Also send your top project names so you can choose to show them on the card? [y/N]",
		"Send exactly this to " + srv.URL + "? [y/N]",
		"Reading the last 30 days of Claude Code and VS Code Copilot history on this computer (nothing is sent yet)..."} {
		if !strings.Contains(r.err, q) {
			t.Errorf("stderr lacks %q", q)
		}
	}
	if !strings.Contains(r.out, "Harnext scan: last 30 days") {
		t.Errorf("summary missing: %q", r.out)
	}
	if after := [3]string{treeOf(t, e.home), treeOf(t, e.agentHome), treeOf(t, e.data)}; after != before {
		t.Errorf("the scan changed the file system:\nbefore: %v\nafter: %v", before, after)
	}
}

func TestScanFolderAnswerAddsFolders(t *testing.T) {
	newScanEnv(t, true)
	srv := newDraftServer(t, http.StatusCreated, "")
	answers(t, "y\nyes\nY\n")
	if r := runScan(t, "", dev(srv)...); r.failure != nil {
		t.Fatal(r.failure, r.err)
	}
	var p scan.CardPayload
	if err := json.Unmarshal(srv.bodies[0], &p); err != nil || len(p.Folders) == 0 || p.Folders[0].Project == "" {
		t.Fatalf("body = %s err=%v", srv.bodies[0], err)
	}
}

func TestScanOnlyAnExplicitYesAgrees(t *testing.T) {
	newScanEnv(t, true)
	for _, tc := range []struct {
		name, text string
		want       int
	}{
		{"upper Y", "Y\nn\nY\n", 1}, {"YES", "YES\nn\nyes\n", 1}, {"spaces", "  y  \nn\n y\n", 1},
		{"empty first", "\n", 0}, {"n first", "n\ny\ny\n", 0}, {"other first", "yep\ny\ny\n", 0}, {"y at end of input", "y", 0},
		{"empty last", "y\nn\n\n", 0}, {"n last", "y\nn\nn\n", 0}, {"eof before last", "y\nn\n", 0}, {"y without newline last", "y\nn\ny", 0},
	} {
		srv := newDraftServer(t, http.StatusCreated, "")
		answers(t, tc.text)
		r := runScan(t, "", dev(srv)...)
		if r.failure != nil || srv.hits() != tc.want {
			t.Errorf("%s: hits=%d want %d failure=%v", tc.name, srv.hits(), tc.want, r.failure)
		}
		if tc.want == 0 && strings.Contains(r.out, "private link") {
			t.Errorf("%s: a link was printed", tc.name)
		}
	}
}

func TestScanJSONSendsNothingAndAsksNothing(t *testing.T) {
	newScanEnv(t, true)
	srv := newDraftServer(t, http.StatusCreated, "")
	opened := answers(t, "y\ny\ny\n")
	r := runScan(t, "", dev(srv, "--json")...)
	if r.failure != nil || srv.hits() != 0 || *opened != 0 {
		t.Fatalf("failure=%v hits=%d tty=%d", r.failure, srv.hits(), *opened)
	}
	if !json.Valid([]byte(r.out)) || !strings.HasPrefix(r.out, "{") || !strings.HasSuffix(r.out, "}\n") || strings.Contains(r.out, "Harnext scan") {
		t.Fatalf("stdout is not only the card JSON:\n%s", r.out)
	}
	if strings.Contains(r.out, `"folders"`) || strings.Contains(r.err, "[y/N]") {
		t.Error("folders or a question without being asked")
	}
	// With --with-folders the project names are in the JSON; no server is needed in this mode.
	r = runScan(t, "", "--json", "--with-folders")
	if r.failure != nil || !strings.Contains(r.out, `"folders"`) || !json.Valid([]byte(r.out)) {
		t.Fatalf("failure=%v out=%s", r.failure, r.out)
	}
}

func TestScanNoCardOnlyPrintsTheSummary(t *testing.T) {
	newScanEnv(t, true)
	srv := newDraftServer(t, http.StatusCreated, "")
	opened := answers(t, "y\ny\ny\n")
	r := runScan(t, "", dev(srv, "--no-card")...)
	if r.failure != nil || srv.hits() != 0 || *opened != 0 {
		t.Fatalf("failure=%v hits=%d tty=%d", r.failure, srv.hits(), *opened)
	}
	if !strings.Contains(r.out, "Harnext scan: last 30 days") || strings.Contains(r.out+r.err, "[y/N]") || strings.Contains(r.out+r.err, "card") {
		t.Errorf("out=%q err=%q", r.out, r.err)
	}
}

func TestScanWithoutHistoryOffersNoCard(t *testing.T) {
	newScanEnv(t, false)
	srv := newDraftServer(t, http.StatusCreated, "")
	opened := answers(t, "y\ny\ny\n")
	for _, args := range [][]string{dev(srv), dev(srv, "--yes"), dev(srv, "--json"), dev(srv, "--no-card")} {
		r := runScan(t, "", args...)
		if r.failure != nil || r.out != "" || !strings.Contains(r.err, "No Claude Code or VS Code Copilot history found in the last 30 days.") {
			t.Errorf("%v: out=%q err=%q failure=%v", args, r.out, r.err, r.failure)
		}
	}
	if srv.hits() != 0 || *opened != 0 {
		t.Errorf("hits=%d tty=%d", srv.hits(), *opened)
	}
}

func TestScanWithoutTerminalAndWithoutYesCreatesNoCard(t *testing.T) {
	newScanEnv(t, true) // the terminal cannot be opened
	srv := newDraftServer(t, http.StatusCreated, "")
	r := runScan(t, "", dev(srv)...)
	if r.failure != nil || srv.hits() != 0 {
		t.Fatalf("failure=%v hits=%d", r.failure, srv.hits())
	}
	if !strings.Contains(r.err, "Run again with --yes to create a card without prompts (it prints exactly what it sends), or use --json to look at the data first.") {
		t.Errorf("stderr = %q", r.err)
	}
	if !strings.Contains(r.out, "Harnext scan: last 30 days") || strings.Contains(r.out, "private link") {
		t.Errorf("stdout = %q", r.out)
	}
}

func TestScanYesSkipsQuestionsButShowsTheData(t *testing.T) {
	newScanEnv(t, true)
	srv := newDraftServer(t, http.StatusCreated, "")
	opened := answers(t, "n\nn\nn\n")
	r := runScan(t, "", dev(srv, "--yes")...)
	if r.failure != nil || srv.hits() != 1 || *opened != 0 {
		t.Fatalf("failure=%v hits=%d tty=%d err=%s", r.failure, srv.hits(), *opened, r.err)
	}
	if strings.Contains(r.err, "[y/N]") || !strings.Contains(r.err, string(srv.bodies[0])+"\n") || bytes.Contains(srv.bodies[0], []byte(`"folders"`)) {
		t.Errorf("stderr = %q", r.err)
	}

	srv2 := newDraftServer(t, http.StatusCreated, "")
	if r := runScan(t, "", dev(srv2, "--yes", "--with-folders")...); r.failure != nil || !bytes.Contains(srv2.bodies[0], []byte(`"folders"`)) {
		t.Fatalf("failure=%v body=%s", r.failure, srv2.bodies)
	}
	// Without a terminal --yes still works.
	scanTTY = func() (io.ReadCloser, error) { return nil, errors.New("no terminal") }
	srv3 := newDraftServer(t, http.StatusCreated, "")
	if r := runScan(t, "", dev(srv3, "--yes")...); r.failure != nil || srv3.hits() != 1 {
		t.Fatalf("failure=%v hits=%d", r.failure, srv3.hits())
	}
}

func TestScanErrorMapping(t *testing.T) {
	for _, c := range []struct {
		status int
		want   string
	}{
		{429, "Too many requests from this network. Try again in an hour."},
		{413, "The server rejected this summary."},
		{400, "The server rejected this summary."},
		{503, "The service is not available right now. Try again later."},
		{302, "Unexpected redirect; refusing to follow."},
		{500, "Unexpected response from the server (HTTP 500)."},
		{404, "Unexpected response from the server (HTTP 404)."},
	} {
		newScanEnv(t, true)
		srv := newDraftServer(t, c.status, "")
		r := runScan(t, "", dev(srv, "--yes")...)
		if r.failure == nil || r.failure.Error() != c.want {
			t.Errorf("status %d: failure = %v, want %q", c.status, r.failure, c.want)
		}
		assertNoLeak(t, fmt.Sprintf("status %d", c.status), r, srv.URL)
	}

	// A server that is gone.
	newScanEnv(t, true)
	srv := newDraftServer(t, http.StatusCreated, "")
	srv.Close()
	r := runScan(t, "", dev(srv, "--yes")...)
	if r.failure == nil || r.failure.Error() != "Could not reach the server." {
		t.Errorf("closed server: failure = %v", r.failure)
	}
	assertNoLeak(t, "closed server", r, srv.URL)
}

// assertNoLeak: nothing from the response, no redirect target, no private link and no server URL beyond the prompt.
func assertNoLeak(t *testing.T, name string, r result, serverURL string) {
	t.Helper()
	all := r.out + r.err
	if r.failure != nil {
		all += r.failure.Error()
	}
	for _, bad := range []string{"hunter2", "sk-FAKE", "evil.example", "leak", "private link", scanManageToken} {
		if strings.Contains(all, bad) {
			t.Errorf("%s: output contains %q", name, bad)
		}
	}
	if r.failure != nil && strings.Contains(r.failure.Error(), serverURL) {
		t.Errorf("%s: the error carries the server URL", name)
	}
}

func TestScanDoesNotFollowRedirects(t *testing.T) {
	newScanEnv(t, true)
	target := newDraftServer(t, http.StatusCreated, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	r := runScan(t, "", "--server", srv.URL, "--dev-allow-insecure-localhost", "--yes")
	if r.failure == nil || r.failure.Error() != "Unexpected redirect; refusing to follow." || target.hits() != 0 {
		t.Errorf("failure=%v target hits=%d", r.failure, target.hits())
	}
}

func TestScanRejectsALinkToAnotherOrigin(t *testing.T) {
	for name, link := range map[string]string{
		"other host":  "http://evil.example/cards/new#" + scanManageToken,
		"other port":  "http://127.0.0.1:1/cards/new#" + scanManageToken,
		"javascript":  "javascript:alert(1)",
		"credentials": "http://user:pw@127.0.0.1/cards/new#" + scanManageToken,
	} {
		newScanEnv(t, true)
		srv := newDraftServer(t, http.StatusCreated, link)
		r := runScan(t, "", dev(srv, "--yes")...)
		if r.failure == nil || r.failure.Error() != "Unexpected response from the server (HTTP 201)." {
			t.Errorf("%s: failure = %v", name, r.failure)
		}
		if strings.Contains(r.out+r.err, "evil.example") || strings.Contains(r.out+r.err, scanManageToken) {
			t.Errorf("%s: the link was printed", name)
		}
	}
}

func TestScanServerAddressRules(t *testing.T) {
	newScanEnv(t, true)
	srv := newDraftServer(t, http.StatusCreated, "")
	opened := answers(t, "y\ny\ny\n")
	for name, args := range map[string][]string{
		"http remote":          {"--server", "http://app.example.com", "--yes"},
		"http remote with dev": {"--server", "http://app.example.com", "--dev-allow-insecure-localhost", "--yes"},
		"http loopback no dev": {"--server", srv.URL, "--yes"},
		"ftp":                  {"--server", "ftp://localhost", "--dev-allow-insecure-localhost", "--yes"},
		"userinfo":             {"--server", "https://user:pw@app.example.com", "--yes"},
	} {
		r := runScan(t, "", args...)
		if r.failure == nil || !strings.Contains(r.failure.Error(), "server address must be https://host") {
			t.Errorf("%s: failure = %v", name, r.failure)
			continue
		}
		if strings.Contains(r.failure.Error(), "pw@") {
			t.Errorf("%s: the error echoes the address", name)
		}
	}
	if srv.hits() != 0 || *opened != 0 {
		t.Errorf("hits=%d tty=%d: a bad address must fail before anything is read or asked", srv.hits(), *opened)
	}
}

func TestScanNeedsAServerAddressWhenTheBuildHasNone(t *testing.T) {
	newScanEnv(t, true)
	r := runScan(t, "", "--yes")
	if r.failure == nil || r.failure.Error() != "--server is required (this build has no default server)" {
		t.Errorf("failure = %v", r.failure)
	}
	// --no-card and --json need no server.
	for _, flag := range []string{"--no-card", "--json"} {
		if r := runScan(t, "", flag); r.failure != nil {
			t.Errorf("%s: %v", flag, r.failure)
		}
	}
	// The compiled-in default is used when --server is absent.
	srv := newDraftServer(t, http.StatusCreated, "")
	if r := runScan(t, srv.URL, "--dev-allow-insecure-localhost", "--yes"); r.failure != nil || srv.hits() != 1 {
		t.Errorf("failure=%v hits=%d", r.failure, srv.hits())
	}
}

func TestScanFakeSecretsNeverLeave(t *testing.T) {
	e := newScanEnv(t, true)
	srv := newDraftServer(t, http.StatusCreated, "")
	answers(t, "y\ny\ny\n")
	r := runScan(t, "", dev(srv)...)
	if r.failure != nil || srv.hits() != 1 {
		t.Fatalf("failure=%v hits=%d", r.failure, srv.hits())
	}
	forbidden := []string{
		"hunter2", "sk-FAKE", "DB_PASSWORD", "API_KEY", "fake-api-key-value", "fixture answer", "fixture title", "main.go",
		scanFakeUser, e.root, filepath.ToSlash(e.root), "example-project",
	}
	for name, out := range map[string]string{"request body": string(srv.bodies[0]), "stdout": r.out, "stderr": r.err} {
		for _, bad := range forbidden {
			if strings.Contains(out, bad) {
				t.Errorf("%s contains %q", name, bad)
			}
		}
	}
	// The project name is part of the local summary and of the opted-in folders, nothing else about the path.
	if !strings.Contains(r.out, "plain") {
		t.Errorf("the local summary lacks the project name:\n%s", r.out)
	}
}
