package identity

import (
	"encoding/json"
	"math"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

const (
	PayloadVersion = 1
	MaxBindings    = 256
	MaxTTL         = 24 * time.Hour

	ToolClaudeCode    = "claude_code"
	ToolVSCodeCopilot = "vscode_copilot"

	maxIdentityVersion = 1<<48 - 1
)

var (
	timestampRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$`)
	routeIDRE   = regexp.MustCompile(`^(?:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}|00000000-0000-0000-0000-000000000000|ffffffff-ffff-ffff-ffff-ffffffffffff)$`)
)

// Binding maps one account (tool + account hash) to the server-side route id, which is opaque to the agent.
type Binding struct {
	Tool    string
	Hash    string
	RouteID string
}

// Document is a verified identity document.
type Document struct {
	// IdentityVersion is a content fingerprint of the bindings: compare for equality only, never for order.
	IdentityVersion int64
	IssuedAt        time.Time
	ExpiresAt       time.Time
	Bindings        []Binding
}

type rawDocument struct {
	PayloadVersion  *float64       `json:"payload_version"`
	IdentityVersion *float64       `json:"identity_version"`
	IssuedAt        *string        `json:"issued_at"`
	ExpiresAt       *string        `json:"expires_at"`
	Bindings        *[]*rawBinding `json:"bindings"`
}

type rawBinding struct {
	Tool    *string `json:"tool"`
	Hash    *string `json:"hash"`
	RouteID *string `json:"route_id"`
}

// Verify parses a JSON envelope body and verifies it. See VerifyEnvelope.
func Verify(body []byte, keys remoteconfig.TrustedKeys, now, notBefore time.Time) (Document, error) {
	env, err := remoteconfig.ParseEnvelope(body)
	if err != nil {
		return Document{}, err
	}
	return VerifyEnvelope(env, keys, now, notBefore)
}

// VerifyEnvelope runs the contract's checks in order and stops at the first failure: malformed, unknown_kid, bad_signature,
// invalid_payload, expired (now >= expires_at), rollback (issued_at < notBefore; a zero notBefore disables it).
func VerifyEnvelope(env remoteconfig.Envelope, keys remoteconfig.TrustedKeys, now, notBefore time.Time) (Document, error) {
	payload, err := remoteconfig.VerifySigned(env, keys)
	if err != nil {
		return Document{}, err
	}
	doc, ok := parsePayload(payload)
	if !ok {
		return Document{}, newError(CodeInvalidPayload)
	}
	if !now.Before(doc.ExpiresAt) {
		return Document{}, newError(CodeExpired)
	}
	if doc.IssuedAt.Before(notBefore) {
		return Document{}, newError(CodeRollback)
	}
	return doc, nil
}

// parsePayload applies contracts/agent-identity.v1.schema.json. Like the schema and its TypeScript mirror it ignores unknown
// fields; a format change that must not be ignored bumps payload_version.
func parsePayload(p []byte) (Document, bool) {
	if !utf8.Valid(p) {
		return Document{}, false
	}
	var raw rawDocument
	if json.Unmarshal(p, &raw) != nil {
		return Document{}, false
	}
	if raw.PayloadVersion == nil || *raw.PayloadVersion != PayloadVersion {
		return Document{}, false
	}
	if raw.IdentityVersion == nil || *raw.IdentityVersion != math.Trunc(*raw.IdentityVersion) ||
		*raw.IdentityVersion < 0 || *raw.IdentityVersion > maxIdentityVersion {
		return Document{}, false
	}
	issued, ok := parseTimestamp(raw.IssuedAt)
	if !ok {
		return Document{}, false
	}
	expires, ok := parseTimestamp(raw.ExpiresAt)
	if !ok {
		return Document{}, false
	}
	if span := expires.Sub(issued); span <= 0 || span > MaxTTL {
		return Document{}, false
	}
	if raw.Bindings == nil || len(*raw.Bindings) > MaxBindings {
		return Document{}, false
	}
	doc := Document{IdentityVersion: int64(*raw.IdentityVersion), IssuedAt: issued, ExpiresAt: expires, Bindings: make([]Binding, 0, len(*raw.Bindings))}
	type key struct{ tool, hash string }
	seen := make(map[key]struct{}, len(*raw.Bindings))
	for _, b := range *raw.Bindings {
		if b == nil || b.Tool == nil || b.Hash == nil || b.RouteID == nil {
			return Document{}, false
		}
		if !validTool(*b.Tool) || !validHash(*b.Hash) || !routeIDRE.MatchString(*b.RouteID) {
			return Document{}, false
		}
		k := key{*b.Tool, *b.Hash}
		if _, dup := seen[k]; dup {
			return Document{}, false
		}
		seen[k] = struct{}{}
		doc.Bindings = append(doc.Bindings, Binding{Tool: *b.Tool, Hash: *b.Hash, RouteID: *b.RouteID})
	}
	return doc, true
}

// parseTimestamp accepts only a UTC ('Z') RFC 3339 date-time; the calendar is validated by time.Parse.
func parseTimestamp(s *string) (time.Time, bool) {
	if s == nil || !timestampRE.MatchString(*s) {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func validTool(s string) bool { return s == ToolClaudeCode || s == ToolVSCodeCopilot }

func validHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
