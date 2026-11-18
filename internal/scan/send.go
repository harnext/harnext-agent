package scan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	draftsPath = "/api/v1/cards/drafts"
	// MaxBodyBytes is the largest card body the server accepts.
	MaxBodyBytes     = maxCardBytes
	sendTimeout      = 20 * time.Second
	maxResponseBytes = 64 << 10
	maxLinkLen       = 2048
)

// Client sends a card draft. There is no account and no token: no Authorization header is ever set.
type Client struct {
	BaseURL   string // https://host[/prefix]; no query, fragment or user info
	UserAgent string
	// AllowInsecureLocalhost permits http:// for localhost, 127.0.0.1 and ::1 (development only).
	AllowInsecureLocalhost bool
	HTTPClient             *http.Client
}

// Code names why a send failed.
type Code string

const (
	CodeBadURL      Code = "bad_url"
	CodeRateLimited Code = "rate_limited"
	CodeRejected    Code = "rejected"
	CodeUnavailable Code = "unavailable"
	CodeNetwork     Code = "network"
	CodeRedirect    Code = "redirect"
	CodeUnexpected  Code = "unexpected"
)

// Error is content-free: no URL, response body or header is kept. Status is 0 when there was no response.
type Error struct {
	Code   Code
	Status int
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("scan: %s (HTTP %d)", e.Code, e.Status)
	}
	return "scan: " + string(e.Code)
}

// Draft is the private link of a created card draft.
type Draft struct {
	ManageURL string
	ExpiresAt string // RFC 3339, UTC
}

// Origin checks the server address and returns it as scheme://host[:port].
func (c Client) Origin() (string, error) {
	u, err := c.base()
	if err != nil {
		return "", err
	}
	return u.Scheme + "://" + u.Host, nil
}

func (c Client) base() (*url.URL, error) {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, &Error{Code: CodeBadURL}
	}
	if !schemeAllowed(u, c.AllowInsecureLocalhost) {
		return nil, &Error{Code: CodeBadURL}
	}
	return u, nil
}

func schemeAllowed(u *url.URL, insecureLocalhost bool) bool {
	switch u.Scheme {
	case "https":
		return true
	case "http":
		return insecureLocalhost && isLoopbackHost(u.Hostname())
	}
	return false
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// CreateDraft posts body (the card JSON, sent byte for byte) once and returns the private manage link. Redirects are
// never followed, and the link is accepted only when it points at the server the body was sent to.
func CreateDraft(ctx context.Context, c Client, body []byte) (Draft, error) {
	base, err := c.base()
	if err != nil {
		return Draft{}, err
	}
	if len(body) == 0 || len(body) > MaxBodyBytes {
		return Draft{}, &Error{Code: CodeRejected}
	}
	hc := &http.Client{}
	if c.HTTPClient != nil {
		cp := *c.HTTPClient
		hc = &cp
	}
	if hc.Timeout <= 0 || hc.Timeout > sendTimeout {
		hc.Timeout = sendTimeout
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	endpoint := strings.TrimRight(base.String(), "/") + draftsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Draft{}, &Error{Code: CodeBadURL}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	resp, err := hc.Do(req)
	if err != nil {
		// *url.Error embeds the request URL; drop it.
		if ctx.Err() != nil {
			return Draft{}, ctx.Err()
		}
		return Draft{}, &Error{Code: CodeNetwork}
	}
	defer resp.Body.Close()

	switch s := resp.StatusCode; {
	case s == http.StatusCreated:
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if err != nil {
			if ctx.Err() != nil {
				return Draft{}, ctx.Err()
			}
			return Draft{}, &Error{Code: CodeNetwork}
		}
		d, ok := parseDraft(raw, base, c.AllowInsecureLocalhost)
		if !ok {
			return Draft{}, &Error{Code: CodeUnexpected, Status: s}
		}
		return d, nil
	case s == http.StatusTooManyRequests:
		return Draft{}, &Error{Code: CodeRateLimited, Status: s}
	case s == http.StatusBadRequest, s == http.StatusRequestEntityTooLarge:
		return Draft{}, &Error{Code: CodeRejected, Status: s}
	case s == http.StatusServiceUnavailable:
		return Draft{}, &Error{Code: CodeUnavailable, Status: s}
	case s >= 300 && s < 400:
		return Draft{}, &Error{Code: CodeRedirect, Status: s}
	}
	return Draft{}, &Error{Code: CodeUnexpected, Status: resp.StatusCode}
}

// parseDraft accepts the link only on the origin the body was sent to, and a plain printable ASCII value, so a hostile
// or broken server can neither show a phishing link nor write terminal control sequences.
func parseDraft(raw []byte, base *url.URL, insecureLocalhost bool) (Draft, bool) {
	var r struct {
		ManageURL string `json:"manage_url"`
		ExpiresAt string `json:"expires_at"`
	}
	if json.Unmarshal(raw, &r) != nil || len(r.ManageURL) == 0 || len(r.ManageURL) > maxLinkLen || !printableASCII(r.ManageURL) {
		return Draft{}, false
	}
	u, err := url.Parse(r.ManageURL)
	if err != nil || u.User != nil || u.Host == "" || !schemeAllowed(u, insecureLocalhost) {
		return Draft{}, false
	}
	if u.Scheme != base.Scheme || !strings.EqualFold(u.Host, base.Host) {
		return Draft{}, false
	}
	exp, err := time.Parse(time.RFC3339, r.ExpiresAt)
	if err != nil {
		return Draft{}, false
	}
	return Draft{ManageURL: r.ManageURL, ExpiresAt: exp.UTC().Format(time.RFC3339)}, true
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
