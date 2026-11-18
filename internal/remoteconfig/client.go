package remoteconfig

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	configPath          = "/api/v1/agent-config"
	DefaultHTTPTimeout  = 15 * time.Second
	DefaultMaxBodyBytes = 128 * 1024
	maxRetryAfter       = time.Hour
	maxETagBytes        = 128
)

// ClientOptions configures a Client.
type ClientOptions struct {
	BaseURL      string        // https://host[/prefix]; no query, fragment or user info
	Token        func() string // returns the agent token; called per request, never logged
	AgentVersion string        // optional; sent as agent_version when it looks like a version
	// AllowInsecureLocalhost permits http:// for localhost, 127.0.0.1 and ::1 (development only).
	AllowInsecureLocalhost bool
	HTTPClient             *http.Client // optional; Timeout and redirect policy are enforced on a copy
	MaxBodyBytes           int64        // default DefaultMaxBodyBytes
}

// Client performs conditional GETs of the signed configuration. It makes no other requests.
type Client struct {
	endpoint     string
	token        func() string
	agentVersion string
	http         *http.Client
	maxBody      int64
}

// FetchResult is the outcome of a successful Fetch.
type FetchResult struct {
	// Envelope is set for a 200 response; it is NOT yet verified.
	Envelope Envelope
	// NotModified is true for a 304: the caller keeps its current verified configuration.
	NotModified bool
	ETag        string
	// UpgradeRequired reflects X-Harnext-Upgrade: required.
	UpgradeRequired bool
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
	endpoint := strings.TrimRight(u.String(), "/") + configPath

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
	return &Client{endpoint: endpoint, token: opts.Token, agentVersion: opts.AgentVersion, http: hc, maxBody: maxBody}, nil
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Fetch requests the configuration. ifNoneMatch may be empty. Errors are *Error values:
// CodeUnauthorized (401), CodeTransient (429, 5xx, network), CodeUnexpectedStatus,
// CodeTooLarge, CodeMalformed (body is not an envelope), CodeNoToken. A cancelled context
// returns the context error unchanged.
func (c *Client) Fetch(ctx context.Context, ifNoneMatch string) (*FetchResult, error) {
	tok := c.token()
	if tok == "" {
		return nil, newError(CodeNoToken)
	}
	target := c.endpoint
	if validAgentVersion(c.agentVersion) {
		target += "?agent_version=" + url.QueryEscape(c.agentVersion)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, newError(CodeBadURL)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
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

	res := &FetchResult{UpgradeRequired: resp.Header.Get("X-Harnext-Upgrade") == "required"}
	if et := resp.Header.Get("ETag"); validETag(et) {
		res.ETag = et
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, newError(CodeTransient)
		}
		if int64(len(body)) > c.maxBody {
			return nil, newError(CodeTooLarge)
		}
		env, err := ParseEnvelope(body)
		if err != nil {
			return nil, err
		}
		res.Envelope = env
		return res, nil
	case resp.StatusCode == http.StatusNotModified:
		if !validETag(ifNoneMatch) {
			return nil, newError(CodeUnexpectedStatus)
		}
		res.NotModified = true
		return res, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, newError(CodeUnauthorized)
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
