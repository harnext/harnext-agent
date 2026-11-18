package remoteconfig

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "hxt_TESTTOKEN-not-a-real-secret"

// scenario is a configurable stand-in for GET /api/v1/agent-config.
type scenario struct {
	mu         sync.Mutex
	status     int
	env        Envelope
	etag       string
	retryAfter string
	upgrade    bool
	body       string // overrides the envelope when set
	hits       int
	lastINM    string
	lastAuth   string
	lastQuery  string
}

func (s *scenario) set(f func(*scenario)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *scenario) snapshot() (hits int, inm, auth, query string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits, s.lastINM, s.lastAuth, s.lastQuery
}

func (s *scenario) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits++
	s.lastINM = r.Header.Get("If-None-Match")
	s.lastAuth = r.Header.Get("Authorization")
	s.lastQuery = r.URL.RawQuery
	if s.etag != "" {
		w.Header().Set("ETag", s.etag)
	}
	if s.retryAfter != "" {
		w.Header().Set("Retry-After", s.retryAfter)
	}
	if s.upgrade {
		w.Header().Set("X-Harnext-Upgrade", "required")
	}
	switch s.status {
	case http.StatusOK:
		w.Header().Set("Content-Type", "application/json")
		if s.body != "" {
			_, _ = w.Write([]byte(s.body))
			return
		}
		_, _ = w.Write(mustJSONNoT(s.env))
	default:
		w.WriteHeader(s.status)
	}
}

func mustJSONNoT(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()
	c, err := NewClient(ClientOptions{
		BaseURL: url, Token: func() string { return testToken }, AgentVersion: "0.1.0-test",
		AllowInsecureLocalhost: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientFetch(t *testing.T) {
	env := vectorEnvelope(t, "valid")
	sc := &scenario{status: 200, env: env, etag: `"abc"`}
	srv := httptest.NewServer(sc)
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	res, err := c.Fetch(context.Background(), "")
	if err != nil || res.NotModified || res.Envelope != env || res.ETag != `"abc"` || res.UpgradeRequired {
		t.Fatalf("200: %+v %v", res, err)
	}
	hits, inm, auth, query := sc.snapshot()
	if hits != 1 || inm != "" || auth != "Bearer "+testToken || query != "agent_version=0.1.0-test" {
		t.Errorf("request: hits=%d inm=%q auth=%q query=%q", hits, inm, auth, query)
	}

	sc.set(func(s *scenario) { s.status = 304; s.upgrade = true })
	res, err = c.Fetch(context.Background(), `"abc"`)
	if err != nil || !res.NotModified || !res.UpgradeRequired {
		t.Fatalf("304: %+v %v", res, err)
	}
	if _, inm, _, _ := sc.snapshot(); inm != `"abc"` {
		t.Errorf("If-None-Match = %q", inm)
	}
	// A 304 without a conditional request is a protocol violation.
	if _, err := c.Fetch(context.Background(), ""); CodeOf(err) != CodeUnexpectedStatus {
		t.Errorf("unconditional 304: %v", err)
	}
}

func TestClientErrorMapping(t *testing.T) {
	sc := &scenario{}
	srv := httptest.NewServer(sc)
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	tests := []struct {
		name       string
		status     int
		retryAfter string
		want       Code
		wantRetry  int
	}{
		{"unauthorized", 401, "", CodeUnauthorized, 0},
		{"forbidden", 403, "", CodeUnexpectedStatus, 0},
		{"not found", 404, "", CodeUnexpectedStatus, 0},
		{"rate limited", 429, "60", CodeTransient, 60},
		{"rate limited without header", 429, "", CodeTransient, 0},
		{"rate limited absurd header is capped", 429, "999999", CodeTransient, 3600},
		{"rate limited bad header", 429, "soon", CodeTransient, 0},
		{"server error", 500, "", CodeTransient, 0},
		{"unavailable", 503, "", CodeTransient, 0},
		{"redirect is not followed", 302, "", CodeUnexpectedStatus, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc.set(func(s *scenario) { s.status, s.retryAfter = tt.status, tt.retryAfter })
			_, err := c.Fetch(context.Background(), `"x"`)
			if CodeOf(err) != tt.want {
				t.Fatalf("code = %q, want %q", CodeOf(err), tt.want)
			}
			var e *Error
			if !errors.As(err, &e) || e.RetryAfterSeconds != tt.wantRetry {
				t.Errorf("retry-after = %+v, want %d", e, tt.wantRetry)
			}
			assertNoLeak(t, err, srv.URL)
		})
	}
}

func assertNoLeak(t *testing.T, err error, extras ...string) {
	t.Helper()
	if err == nil {
		return
	}
	for _, bad := range append([]string{testToken, "agent_version", "127.0.0.1", "http"}, extras...) {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("error text %q leaks %q", err.Error(), bad)
		}
	}
}

func TestClientBodyLimitAndBadBody(t *testing.T) {
	sc := &scenario{status: 200}
	srv := httptest.NewServer(sc)
	defer srv.Close()
	c, err := NewClient(ClientOptions{BaseURL: srv.URL, Token: func() string { return testToken }, AllowInsecureLocalhost: true, MaxBodyBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}

	sc.set(func(s *scenario) { s.body = `{"payload":"` + strings.Repeat("A", 2000) + `","sig":"x","kid":"k"}` })
	if _, err := c.Fetch(context.Background(), ""); CodeOf(err) != CodeTooLarge {
		t.Errorf("oversized body: %v", err)
	}
	sc.set(func(s *scenario) { s.body = `not json SECRET-BODY-FRAGMENT` })
	_, err = c.Fetch(context.Background(), "")
	if CodeOf(err) != CodeMalformed || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("bad body: %v", err)
	}
	sc.set(func(s *scenario) { s.body = `{"payload":"a","sig":"b"}` })
	if _, err := c.Fetch(context.Background(), ""); CodeOf(err) != CodeMalformed {
		t.Errorf("missing kid: %v", err)
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	var otherHits atomic.Int32
	var otherAuth atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		otherAuth.Store(r.Header.Get("Authorization"))
	}))
	defer other.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	c := newTestClient(t, redirector.URL)
	_, err := c.Fetch(context.Background(), "")
	if CodeOf(err) != CodeUnexpectedStatus {
		t.Errorf("redirect: %v", err)
	}
	if otherHits.Load() != 0 {
		t.Errorf("redirect target received %d requests (auth=%v)", otherHits.Load(), otherAuth.Load())
	}
}

func TestClientNetworkErrorIsContentFree(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := newTestClient(t, url)
	_, err := c.Fetch(context.Background(), "")
	if CodeOf(err) != CodeTransient {
		t.Fatalf("got %v", err)
	}
	assertNoLeak(t, err, url)
}

func TestClientContextCancel(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	c := newTestClient(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Fetch(ctx, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v", err)
	}
}

func TestClientHTTPTimeoutIsEnforced(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	c, err := NewClient(ClientOptions{BaseURL: srv.URL, Token: func() string { return testToken }, AllowInsecureLocalhost: true,
		HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(context.Background(), ""); CodeOf(err) != CodeTransient {
		t.Errorf("got %v", err)
	}
	// A caller-supplied client without a timeout still gets one.
	c2, _ := NewClient(ClientOptions{BaseURL: srv.URL, Token: func() string { return "x" }, AllowInsecureLocalhost: true, HTTPClient: &http.Client{}})
	if c2.http.Timeout != DefaultHTTPTimeout {
		t.Errorf("timeout = %v", c2.http.Timeout)
	}
}

func TestNewClientURLRules(t *testing.T) {
	tok := func() string { return "t" }
	tests := []struct {
		name  string
		url   string
		local bool
		ok    bool
	}{
		{"https", "https://harnext.example", false, true},
		{"https with prefix", "https://harnext.example/app/", false, true},
		{"http remote", "http://harnext.example", false, false},
		{"http remote with dev flag", "http://harnext.example", true, false},
		{"http localhost without flag", "http://localhost:3000", false, false},
		{"http localhost with flag", "http://localhost:3000", true, true},
		{"http 127.0.0.1 with flag", "http://127.0.0.1:3000", true, true},
		{"http ipv6 loopback with flag", "http://[::1]:3000", true, true},
		{"http lookalike host", "http://localhost.evil.example", true, false},
		{"https with query", "https://harnext.example?x=1", false, false},
		{"https with user info", "https://user:pw@harnext.example", false, false},
		{"https with fragment", "https://harnext.example/#x", false, false},
		{"ftp", "ftp://harnext.example", false, false},
		{"no scheme", "harnext.example", false, false},
		{"empty", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClient(ClientOptions{BaseURL: tt.url, Token: tok, AllowInsecureLocalhost: tt.local})
			if (err == nil) != tt.ok {
				t.Errorf("err = %v, want ok=%v", err, tt.ok)
			}
			if err != nil && tt.url != "" {
				assertNoLeak(t, err, tt.url)
			}
		})
	}
	if _, err := NewClient(ClientOptions{BaseURL: "https://harnext.example"}); CodeOf(err) != CodeNoToken {
		t.Errorf("missing token source: %v", err)
	}
}

func TestClientEmptyTokenSendsNothing(t *testing.T) {
	sc := &scenario{status: 200}
	srv := httptest.NewServer(sc)
	defer srv.Close()
	c, _ := NewClient(ClientOptions{BaseURL: srv.URL, Token: func() string { return "" }, AllowInsecureLocalhost: true})
	if _, err := c.Fetch(context.Background(), ""); CodeOf(err) != CodeNoToken {
		t.Errorf("got %v", err)
	}
	if hits, _, _, _ := sc.snapshot(); hits != 0 {
		t.Error("request sent without a token")
	}
}

func TestClientIgnoresHostileHeadersAndVersion(t *testing.T) {
	sc := &scenario{status: 200, env: vectorEnvelope(t, "valid")}
	srv := httptest.NewServer(sc)
	defer srv.Close()
	c, _ := NewClient(ClientOptions{BaseURL: srv.URL, Token: func() string { return testToken }, AllowInsecureLocalhost: true, AgentVersion: "1.0 & evil=1"})
	// The request side is what the client controls: hostile conditional header and version are dropped.
	res, err := c.Fetch(context.Background(), "bad\r\nX-Injected: 1")
	if err != nil {
		t.Fatal(err)
	}
	_, inm, _, query := sc.snapshot()
	if inm != "" || query != "" || res.ETag != "" {
		t.Errorf("inm=%q query=%q etag=%q", inm, query, res.ETag)
	}
}
