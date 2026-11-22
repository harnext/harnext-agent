package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

const (
	testToken = "hxt_test_token_value"
	hashA     = "d6efef1a21eecb179c76f57270cfe06ff1c84db42608a7a0f2721bea5846a038"
	hashB     = "a467a71ed5d452565647e3cd1697cd24f69634090cf3b7a2b60b0cc488839a85"
)

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

type signer struct {
	kid  string
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func newSigner(t testing.TB, kid string) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signer{kid: kid, priv: priv, pub: pub}
}

func (s signer) keys() remoteconfig.TrustedKeys { return remoteconfig.TrustedKeys{s.kid: s.pub} }

func (s signer) sign(payload []byte) remoteconfig.Envelope {
	return remoteconfig.Envelope{
		Payload: base64.RawURLEncoding.EncodeToString(payload),
		Sig:     base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.priv, payload)),
		KID:     s.kid,
	}
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// fingerprint is the contract's identity_version rule (docs in the vector file).
func fingerprint(bs []Binding) int64 {
	sorted := append([]Binding(nil), bs...)
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
	sum := h.Sum(nil)
	return int64(binary.BigEndian.Uint64(append([]byte{0, 0}, sum[:6]...)))
}

type jsonBinding struct {
	Tool string `json:"tool"`
	Hash string `json:"hash"`
}

func payloadFor(issued, expires time.Time, bs []Binding) []byte {
	jb := make([]jsonBinding, 0, len(bs))
	for _, b := range bs {
		jb = append(jb, jsonBinding(b))
	}
	p, _ := json.Marshal(map[string]any{
		"payload_version": 2, "identity_version": fingerprint(bs),
		"issued_at": ts(issued), "expires_at": ts(expires), "bindings": jb,
	})
	return p
}

// envelopeAt signs a standard 24 hour document issued at the given time.
func (s signer) envelopeAt(issued time.Time, bs []Binding) remoteconfig.Envelope {
	return s.sign(payloadFor(issued, issued.Add(24*time.Hour), bs))
}

func defaultBindings() []Binding {
	return []Binding{
		{ToolClaudeCode, hashA},
		{ToolVSCodeCopilot, hashB},
	}
}

func bodyOf(t testing.TB, env remoteconfig.Envelope) []byte {
	t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeServer imitates the identity and accounts endpoints: hourly re-signing and ETags from the payload bytes.
type fakeServer struct {
	t   *testing.T
	s   signer
	srv *httptest.Server

	mu          sync.Mutex
	now         time.Time // server clock
	bindings    []Binding
	onReport    func(Report) []Binding
	failStatus  int // non-zero: every request gets this status
	retryAfter  string
	rawResponse []byte // non-nil: served as the 200 body instead of the signed document
	ifNoneMatch []string
	gets, posts int
	agentQuery  []string
	postBody    []byte
	auth        []string
}

func newFakeServer(t *testing.T, s signer, now time.Time) *fakeServer {
	f := &fakeServer{t: t, s: s, now: now, bindings: defaultBindings()}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) set(fn func(f *fakeServer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

type fakeView struct {
	gets, posts int
	ifNoneMatch []string
	agentQuery  []string
	auth        []string
	postBody    []byte
}

// view copies the recorded requests under the lock (the handler runs in another goroutine).
func (f *fakeServer) view() fakeView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeView{f.gets, f.posts, append([]string(nil), f.ifNoneMatch...), append([]string(nil), f.agentQuery...),
		append([]string(nil), f.auth...), append([]byte(nil), f.postBody...)}
}

func (f *fakeServer) doc() ([]byte, string) {
	issued := f.now.Truncate(time.Hour)
	p := payloadFor(issued, issued.Add(24*time.Hour), f.bindings)
	sum := sha256.Sum256(append([]byte(f.s.kid+"\x00"), p...))
	return p, `"` + hex.EncodeToString(sum[:16]) + `"`
}

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	if r.Method == http.MethodGet && r.URL.Path == identityPath {
		f.gets++
		f.ifNoneMatch = append(f.ifNoneMatch, r.Header.Get("If-None-Match"))
		f.agentQuery = append(f.agentQuery, r.URL.Query().Get("agent_version"))
	}
	if f.failStatus != 0 {
		if f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		w.WriteHeader(f.failStatus)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == identityPath:
		payload, etag := f.doc()
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		f.write200(w, payload)
	case r.Method == http.MethodPost && r.URL.Path == accountsPath:
		f.posts++
		body, _ := io.ReadAll(r.Body)
		f.postBody = body
		var req Report
		if json.Unmarshal(body, &req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if f.onReport != nil {
			f.bindings = f.onReport(req)
		}
		payload, _ := f.doc()
		f.write200(w, payload)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeServer) write200(w http.ResponseWriter, payload []byte) {
	w.Header().Set("Content-Type", "application/json")
	if f.rawResponse != nil {
		_, _ = w.Write(f.rawResponse)
		return
	}
	_ = json.NewEncoder(w).Encode(f.s.sign(payload))
}

func (f *fakeServer) client(t *testing.T, opts ...func(*ClientOptions)) *Client {
	t.Helper()
	co := ClientOptions{BaseURL: f.srv.URL, Token: func() string { return testToken }, AllowInsecureLocalhost: true}
	for _, o := range opts {
		o(&co)
	}
	c, err := NewClient(co)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
	// onSleep may cancel the run context; its error is returned from Sleep.
	onSleep func(n int) error
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	n := len(c.sleeps)
	c.mu.Unlock()
	if c.onSleep != nil {
		if err := c.onSleep(n); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (c *fakeClock) sleepLog() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}
