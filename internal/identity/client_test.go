package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func goodAccount() Account {
	return Account{Tool: ToolClaudeCode, Hash: hashA, Label: "c***@gmail.com", Sessions30d: 12}
}

func TestNewClientValidation(t *testing.T) {
	tok := func() string { return testToken }
	cases := []struct {
		name string
		o    ClientOptions
		want Code
	}{
		{"no token func", ClientOptions{BaseURL: "https://x.test"}, CodeNoToken},
		{"http remote", ClientOptions{BaseURL: "http://x.test", Token: tok, AllowInsecureLocalhost: true}, CodeBadURL},
		{"http loopback without flag", ClientOptions{BaseURL: "http://127.0.0.1:1", Token: tok}, CodeBadURL},
		{"user info", ClientOptions{BaseURL: "https://u:p@x.test", Token: tok}, CodeBadURL},
		{"query", ClientOptions{BaseURL: "https://x.test?a=1", Token: tok}, CodeBadURL},
		{"ftp", ClientOptions{BaseURL: "ftp://x.test", Token: tok}, CodeBadURL},
		{"empty", ClientOptions{BaseURL: "", Token: tok}, CodeBadURL},
	}
	for _, c := range cases {
		if _, err := NewClient(c.o); CodeOf(err) != c.want {
			t.Errorf("%s: code = %q", c.name, CodeOf(err))
		}
	}
	for _, ok := range []ClientOptions{
		{BaseURL: "https://x.test/", Token: tok},
		{BaseURL: "http://localhost:3000", Token: tok, AllowInsecureLocalhost: true},
		{BaseURL: "http://[::1]:3000", Token: tok, AllowInsecureLocalhost: true},
	} {
		if _, err := NewClient(ok); err != nil {
			t.Errorf("%s: %v", ok.BaseURL, err)
		}
	}
}

func TestFetchOKAnd304(t *testing.T) {
	s := newSigner(t, "k1")
	f := newFakeServer(t, s, t0.Add(10*time.Minute))
	c := f.client(t, func(o *ClientOptions) { o.AgentVersion = "0.2.4" })

	res, err := c.Fetch(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.NotModified || res.Envelope.KID != "k1" || res.ETag == "" {
		t.Fatalf("res = %+v", res)
	}
	if _, err := VerifyEnvelope(res.Envelope, s.keys(), t0.Add(time.Hour), time.Time{}); err != nil {
		t.Fatalf("served envelope does not verify: %v", err)
	}

	res2, err := c.Fetch(context.Background(), res.ETag)
	if err != nil || !res2.NotModified || res2.ETag != res.ETag {
		t.Fatalf("304: res=%+v err=%v", res2, err)
	}
	// A changed ETag gets the full document.
	res3, err := c.Fetch(context.Background(), `"stale"`)
	if err != nil || res3.NotModified {
		t.Fatalf("stale etag: res=%+v err=%v", res3, err)
	}
	v := f.view()
	if v.ifNoneMatch[0] != "" || v.ifNoneMatch[1] != res.ETag {
		t.Errorf("If-None-Match sent = %q", v.ifNoneMatch)
	}
	if v.agentQuery[0] != "0.2.4" || v.auth[0] != "Bearer "+testToken {
		t.Errorf("query %q auth %q", v.agentQuery[0], v.auth[0])
	}
}

func TestFetchStatusMapping(t *testing.T) {
	s := newSigner(t, "k1")
	f := newFakeServer(t, s, t0)
	c := f.client(t)
	for status, want := range map[int]Code{401: CodeUnauthorized, 429: CodeTransient, 500: CodeTransient, 503: CodeTransient,
		404: CodeUnexpectedStatus, 403: CodeUnexpectedStatus, 302: CodeUnexpectedStatus, 304: CodeUnexpectedStatus, 400: CodeRejected} {
		f.set(func(f *fakeServer) { f.failStatus, f.retryAfter = status, "" })
		if status == 429 {
			f.set(func(f *fakeServer) { f.retryAfter = "120" })
		}
		_, err := c.Fetch(context.Background(), "")
		if CodeOf(err) != want {
			t.Errorf("status %d: code = %q, want %q", status, CodeOf(err), want)
		}
		if status == 429 {
			if e, ok := err.(*Error); !ok || e.RetryAfterSeconds != 120 {
				t.Errorf("retry after = %+v", err)
			}
		}
	}
}

func TestFetchSizeCapAndMalformed(t *testing.T) {
	s := newSigner(t, "k1")
	f := newFakeServer(t, s, t0)
	c := f.client(t, func(o *ClientOptions) { o.MaxBodyBytes = 200 })

	f.set(func(f *fakeServer) {
		f.rawResponse = []byte(`{"payload":"` + strings.Repeat("A", 300) + `","sig":"x","kid":"k1"}`)
	})
	if _, err := c.Fetch(context.Background(), ""); CodeOf(err) != CodeTooLarge {
		t.Errorf("oversize: %v", err)
	}
	f.set(func(f *fakeServer) { f.rawResponse = []byte(`{"nope":1}`) })
	if _, err := c.Fetch(context.Background(), ""); CodeOf(err) != CodeMalformed {
		t.Errorf("shape: %v", err)
	}
	f.set(func(f *fakeServer) { f.rawResponse = []byte(`<html>`) })
	if _, err := c.Fetch(context.Background(), ""); CodeOf(err) != CodeMalformed {
		t.Errorf("html: %v", err)
	}
}

func TestNoRedirectAndNoLeak(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer other.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	c, err := NewClient(ClientOptions{BaseURL: redirector.URL, Token: func() string { return testToken }, AllowInsecureLocalhost: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(context.Background(), ""); CodeOf(err) != CodeUnexpectedStatus {
		t.Errorf("redirect: %v", err)
	}
	if _, err := c.Report(context.Background(), []Account{goodAccount()}); CodeOf(err) != CodeUnexpectedStatus {
		t.Errorf("redirect on report: %v", err)
	}
	if hits.Load() != 0 {
		t.Error("redirect was followed")
	}
}

func TestNetworkErrorsCarryNoURLOrToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, _ := NewClient(ClientOptions{BaseURL: url, Token: func() string { return testToken }, AllowInsecureLocalhost: true})
	_, err := c.Fetch(context.Background(), "")
	if CodeOf(err) != CodeTransient || strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), testToken) {
		t.Errorf("err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Fetch(ctx, ""); err != context.Canceled {
		t.Errorf("cancelled context returned %v", err)
	}
}

func TestEmptyTokenSendsNothing(t *testing.T) {
	s := newSigner(t, "k1")
	f := newFakeServer(t, s, t0)
	c := f.client(t, func(o *ClientOptions) { o.Token = func() string { return "" } })
	if _, err := c.Fetch(context.Background(), ""); CodeOf(err) != CodeNoToken {
		t.Errorf("fetch: %v", err)
	}
	if _, err := c.Report(context.Background(), nil); CodeOf(err) != CodeNoToken {
		t.Errorf("report: %v", err)
	}
	if len(f.view().auth) != 0 {
		t.Error("a request was made")
	}
}

func TestReportSendsContractBody(t *testing.T) {
	s := newSigner(t, "k1")
	f := newFakeServer(t, s, t0.Add(time.Minute))
	f.set(func(f *fakeServer) {
		f.onReport = func(acc []Account) []Binding {
			return []Binding{{acc[0].Tool, acc[0].Hash, routeA}}
		}
	})
	c := f.client(t)
	res, err := c.Report(context.Background(), []Account{goodAccount(), {ToolVSCodeCopilot, hashB, "a<b>&c", 0}})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := VerifyEnvelope(res.Envelope, s.keys(), t0.Add(time.Hour), time.Time{})
	if err != nil || len(doc.Bindings) != 1 || doc.Bindings[0].RouteID != routeA {
		t.Fatalf("doc = %+v err = %v", doc, err)
	}
	v := f.view()
	var body map[string]any
	if err := json.Unmarshal(v.postBody, &body); err != nil || len(body) != 1 {
		t.Fatalf("body = %s", v.postBody)
	}
	first := body["accounts"].([]any)[0].(map[string]any)
	if len(first) != 4 || first["tool"] != ToolClaudeCode || first["hash"] != hashA || first["label"] != "c***@gmail.com" || first["sessions_30d"] != float64(12) {
		t.Errorf("account = %v", first)
	}
	if strings.Contains(string(v.postBody), `\u003c`) {
		t.Error("HTML escaping changed the label bytes")
	}
	if v.auth[0] != "Bearer "+testToken {
		t.Error("report is not authenticated")
	}
}

func TestReportEmptyIsAnArray(t *testing.T) {
	s := newSigner(t, "k1")
	f := newFakeServer(t, s, t0)
	if _, err := f.client(t).Report(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := string(f.view().postBody); got != `{"accounts":[]}` {
		t.Errorf("body = %s", got)
	}
}

func TestReportRules(t *testing.T) {
	s := newSigner(t, "k1")
	f := newFakeServer(t, s, t0)
	c := f.client(t)

	mk := func(fn func(a *Account)) []Account {
		a := goodAccount()
		fn(&a)
		return []Account{a}
	}
	twenty := make([]Account, 20)
	for i := range twenty {
		twenty[i] = goodAccount()
	}
	ok := map[string][]Account{
		"twenty accounts": twenty,
		"label 80 chars":  mk(func(a *Account) { a.Label = strings.Repeat("a", 80) }),
		"label 1 char":    mk(func(a *Account) { a.Label = "x" }),
		"masked label":    mk(func(a *Account) { a.Label = "j***@corp.example" }),
		"star only":       mk(func(a *Account) { a.Label = "***" }),
		"no at sign":      mk(func(a *Account) { a.Label = "work account" }),
		"sessions zero":   mk(func(a *Account) { a.Sessions30d = 0 }),
		"sessions max":    mk(func(a *Account) { a.Sessions30d = 1_000_000 }),
		"label non ascii": mk(func(a *Account) { a.Label = "ç***@örnek.com" }),
		"copilot tool":    mk(func(a *Account) { a.Tool = ToolVSCodeCopilot }),
	}
	for name, acc := range ok {
		if err := ValidateAccounts(acc); err != nil {
			t.Errorf("accept %q: %v", name, err)
		}
	}
	reject := map[string][]Account{
		"twenty one":        append(append([]Account(nil), twenty...), goodAccount()),
		"label empty":       mk(func(a *Account) { a.Label = "" }),
		"label 81 chars":    mk(func(a *Account) { a.Label = strings.Repeat("a", 81) }),
		"plain email":       mk(func(a *Account) { a.Label = "jane@gmail.com" }),
		"at sign only":      mk(func(a *Account) { a.Label = "@" }),
		"email with space":  mk(func(a *Account) { a.Label = "work jane@corp.example" }),
		"newline":           mk(func(a *Account) { a.Label = "a\nb" }),
		"tab":               mk(func(a *Account) { a.Label = "a\tb" }),
		"nul":               mk(func(a *Account) { a.Label = "a\x00b" }),
		"line separator":    mk(func(a *Account) { a.Label = "a\u2028b" }),
		"zero width space":  mk(func(a *Account) { a.Label = "a\u200bb" }),
		"invalid utf8":      mk(func(a *Account) { a.Label = "a\xffb" }),
		"41 astral runes":   mk(func(a *Account) { a.Label = strings.Repeat("\U0001F600", 41) }),
		"unknown tool":      mk(func(a *Account) { a.Tool = "cursor" }),
		"hash uppercase":    mk(func(a *Account) { a.Hash = strings.ToUpper(a.Hash) }),
		"hash short":        mk(func(a *Account) { a.Hash = a.Hash[:10] }),
		"sessions negative": mk(func(a *Account) { a.Sessions30d = -1 }),
		"sessions too many": mk(func(a *Account) { a.Sessions30d = 1_000_001 }),
	}
	for name, acc := range reject {
		if CodeOf(ValidateAccounts(acc)) != CodeInvalidReport {
			t.Errorf("reject %q: not rejected", name)
		}
		if _, err := c.Report(context.Background(), acc); CodeOf(err) != CodeInvalidReport {
			t.Errorf("report %q: code = %q", name, CodeOf(err))
		}
	}
	if n := f.view().posts; n != 0 {
		t.Errorf("%d invalid reports reached the server", n)
	}
	// 40 astral runes (80 UTF-16 units) is still allowed.
	if err := ValidateAccounts(mk(func(a *Account) { a.Label = strings.Repeat("\U0001F600", 40) })); err != nil {
		t.Errorf("80 UTF-16 units: %v", err)
	}
	// Error text names no value.
	err := ValidateAccounts(mk(func(a *Account) { a.Label = "jane@gmail.com" }))
	if strings.Contains(err.Error(), "jane") {
		t.Error("error carries the label")
	}
	// The largest valid report stays below the server's body limit.
	big := make([]Account, 20)
	for i := range big {
		big[i] = Account{ToolVSCodeCopilot, hashB, strings.Repeat("\u20ac", 80), 1_000_000}
	}
	if _, err := c.Report(context.Background(), big); err != nil {
		t.Errorf("largest report: %v", err)
	}
	if n := len(f.view().postBody); n >= MaxReportBodyBytes {
		t.Errorf("largest body is %d bytes", n)
	}
}

func TestReportServerErrors(t *testing.T) {
	s := newSigner(t, "k1")
	f := newFakeServer(t, s, t0)
	c := f.client(t)
	for status, want := range map[int]Code{400: CodeRejected, 413: CodeRejected, 401: CodeUnauthorized, 429: CodeTransient, 503: CodeTransient, 304: CodeUnexpectedStatus} {
		f.set(func(f *fakeServer) { f.failStatus = status })
		if _, err := c.Report(context.Background(), []Account{goodAccount()}); CodeOf(err) != want {
			t.Errorf("status %d: code = %q, want %q", status, CodeOf(err), want)
		}
	}
}
