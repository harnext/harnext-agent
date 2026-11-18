package scan

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const draftReply = `{"manage_url":"%s/cards/new#FAKE-MANAGE-TOKEN","expires_at":"2026-11-19T10:00:00Z"}`

func localClient(url string) Client {
	return Client{BaseURL: url, UserAgent: "harnext-agent/9.9.9", AllowInsecureLocalhost: true}
}

func sendErr(t *testing.T, err error) *Error {
	t.Helper()
	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *scan.Error", err)
	}
	return se
}

func TestCreateDraftSendsTheBodyAsIs(t *testing.T) {
	body := []byte("{\n  \"payload_version\": 1\n}")
	var got struct {
		method, path, ct, ua, auth, cookie string
		body                               []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.ct, got.ua = r.Method, r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("User-Agent")
		got.auth, got.cookie = r.Header.Get("Authorization"), r.Header.Get("Cookie")
		got.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, strings.Replace(draftReply, "%s", "http://"+r.Host, 1))
	}))
	defer srv.Close()

	d, err := CreateDraft(context.Background(), localClient(srv.URL), body)
	if err != nil {
		t.Fatal(err)
	}
	if got.method != "POST" || got.path != "/api/v1/cards/drafts" || got.ct != "application/json" || got.ua != "harnext-agent/9.9.9" {
		t.Errorf("request = %+v", got)
	}
	if got.auth != "" || got.cookie != "" {
		t.Error("an Authorization or Cookie header was sent")
	}
	if string(got.body) != string(body) {
		t.Errorf("body = %q, want the exact bytes", got.body)
	}
	if d.ManageURL != srv.URL+"/cards/new#FAKE-MANAGE-TOKEN" || d.ExpiresAt != "2026-11-19T10:00:00Z" {
		t.Errorf("draft = %+v", d)
	}
}

func TestCreateDraftStatusMapping(t *testing.T) {
	const secret = "sk-FAKE0123456789 hunter2 http://evil.example/x"
	for _, c := range []struct {
		status int
		code   Code
	}{
		{429, CodeRateLimited}, {413, CodeRejected}, {400, CodeRejected}, {503, CodeUnavailable},
		{302, CodeRedirect}, {301, CodeRedirect}, {307, CodeRedirect},
		{500, CodeUnexpected}, {401, CodeUnexpected}, {200, CodeUnexpected}, {202, CodeUnexpected},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "http://evil.example/")
			w.WriteHeader(c.status)
			io.WriteString(w, secret)
		}))
		_, err := CreateDraft(context.Background(), localClient(srv.URL), []byte("{}"))
		srv.Close()
		se := sendErr(t, err)
		if se.Code != c.code || se.Status != c.status {
			t.Errorf("status %d: %+v, want %s", c.status, se, c.code)
		}
		if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "evil") || strings.Contains(err.Error(), srv.URL) {
			t.Errorf("status %d: error carries content: %q", c.status, err)
		}
	}
}

func TestCreateDraftDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := CreateDraft(context.Background(), localClient(srv.URL), []byte("{}"))
	if se := sendErr(t, err); se.Code != CodeRedirect {
		t.Errorf("err = %v", err)
	}
	if hits.Load() != 0 {
		t.Error("the redirect was followed")
	}
}

func TestCreateDraftRejectsLinksOutsideTheServerOrigin(t *testing.T) {
	for name, link := range map[string]string{
		"other host":       "http://evil.example/cards/new#x",
		"other port":       "http://127.0.0.1:1/cards/new#x",
		"https downgrade":  "https://%HOST%/cards/new#x",
		"userinfo":         "http://user:pw@%HOST%/cards/new#x",
		"javascript":       "javascript:alert(1)",
		"relative":         "/cards/new#x",
		"empty":            "",
		"control char":     "http://%HOST%/cards/new#x\x1b[31m",
		"space":            "http://%HOST%/cards/new #x",
		"non ascii":        "http://%HOST%/cards/ü#x",
		"host prefix only": "http://%HOST%.evil.example/cards/new#x",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			l := strings.ReplaceAll(link, "%HOST%", r.Host)
			b, _ := json.Marshal(map[string]string{"manage_url": l, "expires_at": "2026-11-19T10:00:00Z"})
			w.Write(b)
		}))
		_, err := CreateDraft(context.Background(), localClient(srv.URL), []byte("{}"))
		srv.Close()
		if se := sendErr(t, err); se.Code != CodeUnexpected || se.Status != 201 {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestCreateDraftRejectsBrokenSuccessBodies(t *testing.T) {
	for name, reply := range map[string]string{
		"not json":       "<html>",
		"bad expiry":     `{"manage_url":"http://%HOST%/cards/new#x","expires_at":"soon\u001b[2J"}`,
		"missing expiry": `{"manage_url":"http://%HOST%/cards/new#x"}`,
		"oversized":      `{"manage_url":"http://%HOST%/cards/new#x","expires_at":"2026-11-19T10:00:00Z","pad":"` + strings.Repeat("a", 70<<10) + `"}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, strings.ReplaceAll(reply, "%HOST%", r.Host))
		}))
		_, err := CreateDraft(context.Background(), localClient(srv.URL), []byte("{}"))
		srv.Close()
		if se := sendErr(t, err); se.Code != CodeUnexpected {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestCreateDraftServerAddressRules(t *testing.T) {
	for name, c := range map[string]Client{
		"http remote":             {BaseURL: "http://app.example.com", AllowInsecureLocalhost: true},
		"http localhost no flag":  {BaseURL: "http://localhost:3000"},
		"http loopback no flag":   {BaseURL: "http://127.0.0.1:3000"},
		"ftp":                     {BaseURL: "ftp://localhost", AllowInsecureLocalhost: true},
		"empty":                   {},
		"no host":                 {BaseURL: "https://"},
		"userinfo":                {BaseURL: "https://u:p@app.example.com"},
		"query":                   {BaseURL: "https://app.example.com?x=1"},
		"fragment":                {BaseURL: "https://app.example.com#x"},
		"http look-alike of host": {BaseURL: "http://localhost.evil.example", AllowInsecureLocalhost: true},
	} {
		if _, err := CreateDraft(context.Background(), c, []byte("{}")); sendErr(t, err).Code != CodeBadURL {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for _, ok := range []string{"https://app.example.com", "https://app.example.com/", "http://[::1]:3000", "http://localhost:3000"} {
		c := Client{BaseURL: ok, AllowInsecureLocalhost: true}
		if o, err := c.Origin(); err != nil || o == "" {
			t.Errorf("%s: origin %q err %v", ok, o, err)
		}
	}
	if o, _ := (Client{BaseURL: "https://app.example.com/prefix/"}).Origin(); o != "https://app.example.com" {
		t.Errorf("origin = %q", o)
	}
}

func TestCreateDraftNetworkFailuresAndLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listens any more
	_, err := CreateDraft(context.Background(), localClient(url), []byte("{}"))
	if se := sendErr(t, err); se.Code != CodeNetwork || strings.Contains(err.Error(), url) {
		t.Errorf("closed server: %v", err)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer slow.Close()
	c := localClient(slow.URL)
	c.HTTPClient = &http.Client{Timeout: 50 * time.Millisecond}
	if _, err := CreateDraft(context.Background(), c, []byte("{}")); sendErr(t, err).Code != CodeNetwork {
		t.Errorf("timeout: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CreateDraft(ctx, localClient(slow.URL), []byte("{}")); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}

	// Too large for the server: refused without a request.
	var hits atomic.Int32
	count := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer count.Close()
	if _, err := CreateDraft(context.Background(), localClient(count.URL), make([]byte, MaxBodyBytes+1)); sendErr(t, err).Code != CodeRejected {
		t.Errorf("oversized: %v", err)
	}
	if _, err := CreateDraft(context.Background(), localClient(count.URL), nil); sendErr(t, err).Code != CodeRejected {
		t.Errorf("empty: %v", err)
	}
	if hits.Load() != 0 {
		t.Error("an invalid body reached the network")
	}
}

func TestCreateDraftTLSFailureIsAnUnreachableServer(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) }))
	defer srv.Close()
	// The default client does not trust the test certificate.
	_, err := CreateDraft(context.Background(), Client{BaseURL: srv.URL}, []byte("{}"))
	if sendErr(t, err).Code != CodeNetwork {
		t.Errorf("err = %v", err)
	}
}
