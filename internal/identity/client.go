package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

const (
	identityPath = "/api/v1/agent-identity"
	accountsPath = "/api/v1/agent-accounts"

	DefaultHTTPTimeout  = 15 * time.Second
	DefaultMaxBodyBytes = 128 * 1024 // envelope with the maximum 64 KiB payload is about 88 KiB
	maxRetryAfter       = time.Hour
	maxETagBytes        = 128

	MaxAccountsPerReport = 20
	MaxReportBodyBytes   = 16 * 1024
	MaxLabelLength       = 80
	MaxSessions30d       = 1_000_000
)

// Account is one locally detected tool account. Hash is the 64-hex account hash; Label is a masked display label.
type Account struct {
	Tool        string `json:"tool"`
	Hash        string `json:"hash"`
	Label       string `json:"label"`
	Sessions30d int    `json:"sessions_30d"`
}

// ClientOptions configures a Client.
type ClientOptions struct {
	BaseURL      string        // https://host[/prefix]; no query, fragment or user info
	Token        func() string // returns the agent token; called per request, never logged
	AgentVersion string        // optional; sent as agent_version on the GET when it looks like a version
	// AllowInsecureLocalhost permits http:// for localhost, 127.0.0.1 and ::1 (development only).
	AllowInsecureLocalhost bool
	HTTPClient             *http.Client // optional; Timeout and redirect policy are enforced on a copy
	MaxBodyBytes           int64        // response cap, default DefaultMaxBodyBytes
}

// Client performs the two identity requests. It makes no other requests.
type Client struct {
	identityURL  string
	accountsURL  string
	token        func() string
	agentVersion string
	http         *http.Client
	maxBody      int64
}

// FetchResult is the outcome of a successful request.
type FetchResult struct {
	// Envelope is set unless NotModified; it is NOT yet verified.
	Envelope remoteconfig.Envelope
	// NotModified is true for a 304: the caller keeps its cached document (the 304 renews nothing).
	NotModified bool
	ETag        string
}

// NewClient validates the options. Errors are content-free.
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.Token == nil {
		return nil, newError(CodeNoToken)
	}
	u, err := url.Parse(opts.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, newError(CodeBadURL)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !opts.AllowInsecureLocalhost || !isLoopbackHost(u.Hostname()) {
			return nil, newError(CodeBadURL)
		}
	default:
		return nil, newError(CodeBadURL)
	}
	base := strings.TrimRight(u.String(), "/")

	hc := &http.Client{}
	if opts.HTTPClient != nil {
		cp := *opts.HTTPClient
		hc = &cp
	}
	if hc.Timeout <= 0 {
		hc.Timeout = DefaultHTTPTimeout
	}
	// No redirects at all: the Authorization header must never follow a redirect to another host.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	return &Client{identityURL: base + identityPath, accountsURL: base + accountsPath, token: opts.Token,
		agentVersion: opts.AgentVersion, http: hc, maxBody: maxBody}, nil
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Fetch requests the identity document. ifNoneMatch may be empty. Errors are *Error values (CodeUnauthorized, CodeTransient,
// CodeUnexpectedStatus, CodeTooLarge, CodeMalformed, CodeNoToken); a cancelled context returns the context error unchanged.
func (c *Client) Fetch(ctx context.Context, ifNoneMatch string) (*FetchResult, error) {
	target := c.identityURL
	if validAgentVersion(c.agentVersion) {
		target += "?agent_version=" + url.QueryEscape(c.agentVersion)
	}
	return c.do(ctx, http.MethodGet, target, nil, ifNoneMatch)
}

// Report sends the accounts and returns the server's signed envelope (not yet verified). The report is validated first and
// is not sent when it breaks a contract rule (CodeInvalidReport).
func (c *Client) Report(ctx context.Context, accounts []Account) (*FetchResult, error) {
	if err := ValidateAccounts(accounts); err != nil {
		return nil, err
	}
	list := accounts
	if list == nil {
		list = []Account{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(struct {
		Accounts []Account `json:"accounts"`
	}{list}) != nil {
		return nil, newError(CodeInvalidReport)
	}
	body := bytes.TrimRight(buf.Bytes(), "\n")
	if len(body) > MaxReportBodyBytes {
		return nil, newError(CodeInvalidReport)
	}
	return c.do(ctx, http.MethodPost, c.accountsURL, body, "")
}

func (c *Client) do(ctx context.Context, method, target string, body []byte, ifNoneMatch string) (*FetchResult, error) {
	tok := c.token()
	if tok == "" {
		return nil, newError(CodeNoToken)
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return nil, newError(CodeBadURL)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if validETag(ifNoneMatch) {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// *url.Error embeds the request URL; drop it.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, newError(CodeTransient)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, newError(CodeTransient)
		}
		if int64(len(data)) > c.maxBody {
			return nil, newError(CodeTooLarge)
		}
		env, err := remoteconfig.ParseEnvelope(data)
		if err != nil {
			return nil, err
		}
		res := &FetchResult{Envelope: env}
		if et := resp.Header.Get("ETag"); method == http.MethodGet && validETag(et) {
			res.ETag = et
		}
		return res, nil
	case resp.StatusCode == http.StatusNotModified:
		if method != http.MethodGet || !validETag(ifNoneMatch) {
			return nil, newError(CodeUnexpectedStatus)
		}
		return &FetchResult{NotModified: true, ETag: ifNoneMatch}, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, newError(CodeUnauthorized)
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge:
		return nil, newError(CodeRejected)
	case resp.StatusCode == http.StatusTooManyRequests:
		e := newError(CodeTransient)
		e.RetryAfterSeconds = parseRetryAfter(resp.Header.Get("Retry-After"))
		return nil, e
	case resp.StatusCode >= 500:
		return nil, newError(CodeTransient)
	default:
		return nil, newError(CodeUnexpectedStatus)
	}
}

// ValidateAccounts applies the account report contract plus one client-side rule: a label that contains '@' must be masked
// ('*'), so a plain e-mail address never leaves the machine. Errors are CodeInvalidReport and name no field or value.
func ValidateAccounts(accounts []Account) error {
	if len(accounts) > MaxAccountsPerReport {
		return newError(CodeInvalidReport)
	}
	for _, a := range accounts {
		if !validTool(a.Tool) || !validHash(a.Hash) || a.Sessions30d < 0 || a.Sessions30d > MaxSessions30d || !validLabel(a.Label) {
			return newError(CodeInvalidReport)
		}
	}
	return nil
}

func validLabel(s string) bool {
	if s == "" || !utf8.ValidString(s) {
		return false
	}
	units := 0 // UTF-16 units: the server counts string length that way
	for _, r := range s {
		if unicode.In(r, unicode.C, unicode.Zl, unicode.Zp) {
			return false
		}
		if r >= 0x10000 {
			units += 2
		} else {
			units++
		}
	}
	if units > MaxLabelLength {
		return false
	}
	return !strings.Contains(s, "@") || strings.Contains(s, "*")
}

func parseRetryAfter(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	if max := int(maxRetryAfter / time.Second); n > max {
		return max
	}
	return n
}

// validETag accepts printable ASCII only, so a hostile header cannot inject anything.
func validETag(s string) bool {
	if s == "" || len(s) > maxETagBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func validAgentVersion(s string) bool {
	if s == "" || len(s) > 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '+' || c == '_') {
			return false
		}
	}
	return true
}
