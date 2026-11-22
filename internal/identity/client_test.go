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

func goodReport() Report {
	return Report{
		Claude:  ClaudeReport{Found: 2, Matched: true, Label: "c***@gmail.com"},
		Copilot: CopilotReport{Logins: []CopilotLogin{{Login: "octocat", IDHash: hashB, Plan: "pro"}}},
	}
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
	if _, err := c.Report(context.Background(), goodReport()); CodeOf(err) != CodeUnexpectedStatus {
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
	if _, err := c.Report(context.Background(), Report{}); CodeOf(err) != CodeNoToken {
		t.Errorf("report: %v", err)
	}
	if len(f.view().auth) != 0 {
		t.Error("a request was made")
	}
}

func TestReportSendsContractBody(t *testing.T) {
	s := newSigner(t, "k1")
	f := newFakeServer(t, s, t0.Add(time.Minute))
	var got Report
	f.set(func(f *fakeServer) {
		f.onReport = func(rep Report) []Binding {
			got = rep
			return []Binding{{ToolClaudeCode, hashA}}
		}
	})
	c := f.client(t)
	res, err := c.Report(context.Background(), goodReport())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := VerifyEnvelope(res.Envelope, s.keys(), t0.Add(time.Hour), time.Time{})
	if err != nil || len(doc.Bindings) != 1 || doc.Bindings[0].Hash != hashA {
		t.Fatalf("doc = %+v err = %v", doc, err)
	}
	if got.Claude != goodReport().Claude || len(got.Copilot.Logins) != 1 || got.Copilot.Logins[0].Login != "octocat" {
		t.Fatalf("server saw %+v", got)
	}
	v := f.view()
	var body map[string]any
	if err := json.Unmarshal(v.postBody, &body); err != nil || len(body) != 2 {
		t.Fatalf("body = %s", v.postBody)
	}
	// A report without Copilot accounts sends an empty list, not null.
	if _, err := c.Report(context.Background(), Report{Claude: ClaudeReport{Found: 0}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.view().postBody), `"logins":[]`) {
		t.Errorf("body = %s", f.view().postBody)
	}
}

func TestReportRules(t *testing.T) {
	s := newSigner(t, "k1")
	f := newFakeServer(t, s, t0)
	c := f.client(t)

	mk := func(fn func(r *Report)) Report {
		r := goodReport()
		fn(&r)
		return r
	}
	logins := func(n int) []CopilotLogin {
		out := make([]CopilotLogin, n)
		for i := range out {
			out[i] = CopilotLogin{Login: strings.Repeat("a", 64), IDHash: hashB, Plan: "enterprise"}
		}
		return out
	}
	ok := map[string]Report{
		"baseline":           goodReport(),
		"nothing found":      {},
		"found unmatched":    {Claude: ClaudeReport{Found: 3}},
		"label 80 chars":     mk(func(r *Report) { r.Claude.Label = strings.Repeat("a", 80) }),
		"label 1 char":       mk(func(r *Report) { r.Claude.Label = "x" }),
		"masked label":       mk(func(r *Report) { r.Claude.Label = "j***@corp.example" }),
		"label non ascii":    mk(func(r *Report) { r.Claude.Label = "ç***@örnek.com" }),
		"20 claude accounts": mk(func(r *Report) { r.Claude.Found = 20 }),
		"login without id":   mk(func(r *Report) { r.Copilot.Logins = []CopilotLogin{{Login: "never-active"}} }),
		"twenty logins":      mk(func(r *Report) { r.Copilot.Logins = logins(20) }),
		"underscore login":   mk(func(r *Report) { r.Copilot.Logins = []CopilotLogin{{Login: "a_b-C9"}} }),
		"all plans": mk(func(r *Report) {
			r.Copilot.Logins = []CopilotLogin{{Login: "a", Plan: "free"}, {Login: "b", Plan: "business"}}
		}),
	}
	for name, rep := range ok {
		if err := ValidateReport(rep); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	reject := map[string]Report{
		"found negative":      mk(func(r *Report) { r.Claude.Found = -1 }),
		"found 21":            mk(func(r *Report) { r.Claude.Found = 21 }),
		"matched without any": mk(func(r *Report) { r.Claude.Found = 0 }),
		"matched no label":    mk(func(r *Report) { r.Claude.Label = "" }),
		"label unmatched":     mk(func(r *Report) { r.Claude.Matched = false }),
		"label 81 chars":      mk(func(r *Report) { r.Claude.Label = strings.Repeat("a", 81) }),
		"plain email":         mk(func(r *Report) { r.Claude.Label = "jane@gmail.com" }),
		"at sign only":        mk(func(r *Report) { r.Claude.Label = "@" }),
		"newline":             mk(func(r *Report) { r.Claude.Label = "a\nb" }),
		"nul":                 mk(func(r *Report) { r.Claude.Label = "a\x00b" }),
		"line separator":      mk(func(r *Report) { r.Claude.Label = "a\u2028b" }),
		"zero width space":    mk(func(r *Report) { r.Claude.Label = "a\u200bb" }),
		"invalid utf8":        mk(func(r *Report) { r.Claude.Label = "a\xffb" }),
		"41 astral runes":     mk(func(r *Report) { r.Claude.Label = strings.Repeat("\U0001F600", 41) }),
		"21 logins":           mk(func(r *Report) { r.Copilot.Logins = logins(21) }),
		"login empty":         mk(func(r *Report) { r.Copilot.Logins = []CopilotLogin{{Login: ""}} }),
		"login with space":    mk(func(r *Report) { r.Copilot.Logins = []CopilotLogin{{Login: "a b"}} }),
		"login is an email":   mk(func(r *Report) { r.Copilot.Logins = []CopilotLogin{{Login: "a@b.co"}} }),
		"login too long":      mk(func(r *Report) { r.Copilot.Logins = []CopilotLogin{{Login: strings.Repeat("a", 65)}} }),
		"id hash uppercase":   mk(func(r *Report) { r.Copilot.Logins[0].IDHash = strings.ToUpper(hashB) }),
		"id hash short":       mk(func(r *Report) { r.Copilot.Logins[0].IDHash = hashB[:10] }),
		"unknown plan":        mk(func(r *Report) { r.Copilot.Logins[0].Plan = "team" }),
	}
	for name, rep := range reject {
		if CodeOf(ValidateReport(rep)) != CodeInvalidReport {
			t.Errorf("%s: accepted", name)
		}
		if _, err := c.Report(context.Background(), rep); CodeOf(err) != CodeInvalidReport {
			t.Errorf("report %q: code = %q", name, CodeOf(err))
		}
	}
	if n := f.view().posts; n != 0 {
		t.Errorf("%d invalid reports reached the server", n)
	}
	// 40 astral runes (80 UTF-16 units) is still allowed.
	if err := ValidateReport(mk(func(r *Report) { r.Claude.Label = strings.Repeat("\U0001F600", 40) })); err != nil {
		t.Errorf("80 UTF-16 units: %v", err)
	}
	// Error text names no value.
	err := ValidateReport(mk(func(r *Report) { r.Claude.Label = "jane@gmail.com" }))
	if strings.Contains(err.Error(), "jane") {
		t.Error("error carries the label")
	}
	// The largest valid report stays below the server's body limit.
	big := mk(func(r *Report) {
		r.Claude.Label = strings.Repeat("\u20ac", 80)
		r.Copilot.Logins = logins(20)
	})
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
		if _, err := c.Report(context.Background(), goodReport()); CodeOf(err) != want {
			t.Errorf("status %d: code = %q, want %q", status, CodeOf(err), want)
		}
	}
}
