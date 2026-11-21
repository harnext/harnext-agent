package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

const vectorPath = "../../contracts/testdata/agent-identity.v1.vector.json"

type vectorCase struct {
	Name     string                `json:"name"`
	Envelope remoteconfig.Envelope `json:"envelope"`
	Last     *string               `json:"last_accepted_issued_at"`
	Expect   struct {
		OK              bool          `json:"ok"`
		IdentityVersion int64         `json:"identity_version"`
		IssuedAt        string        `json:"issued_at"`
		Bindings        []jsonBinding `json:"bindings"`
		Reason          string        `json:"reason"`
	} `json:"expect"`
}

type vectorFile struct {
	VerifyAt    string            `json:"verify_at"`
	TrustedKeys map[string]string `json:"trusted_keys"`
	Cases       []vectorCase      `json:"cases"`
}

func loadVector(t *testing.T) (vectorFile, remoteconfig.TrustedKeys, time.Time) {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash(vectorPath))
	if err != nil {
		t.Fatalf("cannot read the shared test vector: %v", err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	keys := remoteconfig.TrustedKeys{}
	for kid, b64 := range v.TrustedKeys {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			t.Fatalf("vector key %q is invalid", kid)
		}
		keys[kid] = ed25519.PublicKey(raw)
	}
	at, err := time.Parse(time.RFC3339, v.VerifyAt)
	if err != nil {
		t.Fatal(err)
	}
	return v, keys, at
}

func TestVector(t *testing.T) {
	v, keys, at := loadVector(t)
	wantNames := []string{"valid", "valid_empty_bindings", "valid_same_issued_at_accepted", "payload_tampered", "sig_tampered",
		"wrong_key", "unknown_kid", "expired", "rollback_older_issued_at", "wrong_payload_version", "ttl_too_long",
		"unknown_tool", "malformed_base64", "malformed_padding"}
	if len(v.Cases) != len(wantNames) {
		t.Fatalf("vector has %d cases, want %d", len(v.Cases), len(wantNames))
	}
	seen := map[string]bool{}
	for _, c := range v.Cases {
		seen[c.Name] = true
	}
	for _, n := range wantNames {
		if !seen[n] {
			t.Errorf("vector case %q missing", n)
		}
	}
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var notBefore time.Time
			if c.Last != nil {
				var err error
				if notBefore, err = time.Parse(time.RFC3339, *c.Last); err != nil {
					t.Fatal(err)
				}
			}
			doc, err := Verify(bodyOf(t, c.Envelope), keys, at, notBefore)
			if !c.Expect.OK {
				if got := CodeOf(err); string(got) != c.Expect.Reason {
					t.Errorf("code = %q (err %v), want %q", got, err, c.Expect.Reason)
				}
				if len(doc.Bindings) != 0 || !doc.IssuedAt.IsZero() {
					t.Error("document returned alongside an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("want ok, got %v", err)
			}
			if doc.IdentityVersion != c.Expect.IdentityVersion {
				t.Errorf("identity_version = %d, want %d", doc.IdentityVersion, c.Expect.IdentityVersion)
			}
			if want, _ := time.Parse(time.RFC3339Nano, c.Expect.IssuedAt); !doc.IssuedAt.Equal(want) {
				t.Errorf("issued_at = %v, want %v", doc.IssuedAt, want)
			}
			if len(doc.Bindings) != len(c.Expect.Bindings) {
				t.Fatalf("bindings = %+v, want %+v", doc.Bindings, c.Expect.Bindings)
			}
			for i, b := range doc.Bindings {
				if b != Binding(c.Expect.Bindings[i]) {
					t.Errorf("binding %d = %+v, want %+v", i, b, c.Expect.Bindings[i])
				}
			}
			if got := fingerprint(doc.Bindings); got != doc.IdentityVersion {
				t.Errorf("fingerprint rule gives %d, document says %d", got, doc.IdentityVersion)
			}
		})
	}
}

func TestVerifyBodyShape(t *testing.T) {
	_, keys, at := loadVector(t)
	for name, body := range map[string]string{"empty": "", "array": "[]", "missing sig": `{"payload":"e30","kid":"test-key-1"}`, "null": "null"} {
		if _, err := Verify([]byte(body), keys, at, time.Time{}); CodeOf(err) != CodeMalformed {
			t.Errorf("%s: code = %q", name, CodeOf(err))
		}
	}
}

func TestPayloadRules(t *testing.T) {
	s := newSigner(t, "k1")
	now := t0.Add(30 * time.Minute)
	good := func() map[string]any {
		return map[string]any{"payload_version": 1, "identity_version": 5, "issued_at": ts(t0), "expires_at": ts(t0.Add(24 * time.Hour)),
			"bindings": []any{map[string]any{"tool": ToolClaudeCode, "hash": hashA, "route_id": routeA}}}
	}
	binding := func(tool, hash, route string) map[string]any {
		return map[string]any{"tool": tool, "hash": hash, "route_id": route}
	}
	many := func(n int) []any {
		out := make([]any, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, binding(ToolClaudeCode, fmt.Sprintf("%064x", i), routeA))
		}
		return out
	}
	mutate := func(fn func(m map[string]any)) map[string]any {
		m := good()
		fn(m)
		return m
	}

	accept := map[string]map[string]any{
		"baseline":              good(),
		"unknown field ignored": mutate(func(m map[string]any) { m["extra"] = true }),
		"unknown binding field": mutate(func(m map[string]any) {
			m["bindings"] = []any{map[string]any{"tool": ToolClaudeCode, "hash": hashA, "route_id": routeA, "x": 1}}
		}),
		"ttl exactly 24h":       mutate(func(m map[string]any) { m["expires_at"] = ts(t0.Add(24 * time.Hour)) }),
		"no fractional seconds": mutate(func(m map[string]any) { m["issued_at"] = "2026-11-21T12:00:00Z" }),
		"256 bindings":          mutate(func(m map[string]any) { m["bindings"] = many(256) }),
		"nil route id": mutate(func(m map[string]any) {
			m["bindings"] = []any{binding(ToolClaudeCode, hashA, "00000000-0000-0000-0000-000000000000")}
		}),
		"same hash other tool": mutate(func(m map[string]any) {
			m["bindings"] = []any{binding(ToolClaudeCode, hashA, routeA), binding(ToolVSCodeCopilot, hashA, routeB)}
		}),
		"identity_version max":  mutate(func(m map[string]any) { m["identity_version"] = int64(1<<48 - 1) }),
		"identity_version zero": mutate(func(m map[string]any) { m["identity_version"] = 0 }),
		"uppercase uuid": mutate(func(m map[string]any) {
			m["bindings"] = []any{binding(ToolClaudeCode, hashA, "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA")}
		}),
		"leap day": mutate(func(m map[string]any) {
			m["issued_at"] = "2028-02-29T00:00:00.000Z"
			m["expires_at"] = "2028-03-01T00:00:00.000Z"
		}),
	}
	for name, doc := range accept {
		p, _ := json.Marshal(doc)
		nowFor := now
		if name == "leap day" {
			nowFor = time.Date(2028, 2, 29, 1, 0, 0, 0, time.UTC)
		}
		if _, err := VerifyEnvelope(s.sign(p), s.keys(), nowFor, time.Time{}); err != nil {
			t.Errorf("accept %q: %v", name, err)
		}
	}

	reject := map[string]map[string]any{
		"payload_version 2":       mutate(func(m map[string]any) { m["payload_version"] = 2 }),
		"payload_version string":  mutate(func(m map[string]any) { m["payload_version"] = "1" }),
		"payload_version null":    mutate(func(m map[string]any) { m["payload_version"] = nil }),
		"payload_version absent":  mutate(func(m map[string]any) { delete(m, "payload_version") }),
		"identity_version neg":    mutate(func(m map[string]any) { m["identity_version"] = -1 }),
		"identity_version frac":   mutate(func(m map[string]any) { m["identity_version"] = 1.5 }),
		"identity_version big":    mutate(func(m map[string]any) { m["identity_version"] = int64(1 << 48) }),
		"identity_version absent": mutate(func(m map[string]any) { delete(m, "identity_version") }),
		"issued_at offset":        mutate(func(m map[string]any) { m["issued_at"] = "2026-11-21T12:00:00+00:00" }),
		"issued_at no zone":       mutate(func(m map[string]any) { m["issued_at"] = "2026-11-21T12:00:00" }),
		"issued_at date only":     mutate(func(m map[string]any) { m["issued_at"] = "2026-11-21" }),
		"issued_at feb 30":        mutate(func(m map[string]any) { m["issued_at"] = "2026-02-30T00:00:00Z" }),
		"issued_at not leap": mutate(func(m map[string]any) {
			m["issued_at"] = "2027-02-29T00:00:00Z"
			m["expires_at"] = "2027-03-01T00:00:00Z"
		}),
		"issued_at lowercase z": mutate(func(m map[string]any) { m["issued_at"] = "2026-11-21T12:00:00z" }),
		"issued_at number":      mutate(func(m map[string]any) { m["issued_at"] = 1 }),
		"expires_at absent":     mutate(func(m map[string]any) { delete(m, "expires_at") }),
		"expires equals issued": mutate(func(m map[string]any) { m["expires_at"] = m["issued_at"] }),
		"expires before issued": mutate(func(m map[string]any) { m["expires_at"] = ts(t0.Add(-time.Hour)) }),
		"ttl 24h plus 1s": mutate(func(m map[string]any) {
			m["expires_at"] = t0.Add(24*time.Hour + time.Second).Format("2006-01-02T15:04:05Z")
		}),
		"bindings absent": mutate(func(m map[string]any) { delete(m, "bindings") }),
		"bindings null":   mutate(func(m map[string]any) { m["bindings"] = nil }),
		"bindings object": mutate(func(m map[string]any) { m["bindings"] = map[string]any{} }),
		"257 bindings":    mutate(func(m map[string]any) { m["bindings"] = many(257) }),
		"null binding":    mutate(func(m map[string]any) { m["bindings"] = []any{nil} }),
		"unknown tool":    mutate(func(m map[string]any) { m["bindings"] = []any{binding("cursor", hashA, routeA)} }),
		"empty tool":      mutate(func(m map[string]any) { m["bindings"] = []any{binding("", hashA, routeA)} }),
		"hash uppercase":  mutate(func(m map[string]any) { m["bindings"] = []any{binding(ToolClaudeCode, strings.ToUpper(hashA), routeA)} }),
		"hash short":      mutate(func(m map[string]any) { m["bindings"] = []any{binding(ToolClaudeCode, hashA[:63], routeA)} }),
		"hash non hex": mutate(func(m map[string]any) {
			m["bindings"] = []any{binding(ToolClaudeCode, strings.Repeat("g", 64), routeA)}
		}),
		"route empty":    mutate(func(m map[string]any) { m["bindings"] = []any{binding(ToolClaudeCode, hashA, "")} }),
		"route not uuid": mutate(func(m map[string]any) { m["bindings"] = []any{binding(ToolClaudeCode, hashA, "route-1")} }),
		"route bad variant": mutate(func(m map[string]any) {
			m["bindings"] = []any{binding(ToolClaudeCode, hashA, "11111111-1111-4111-c111-111111111111")}
		}),
		"route absent": mutate(func(m map[string]any) { m["bindings"] = []any{map[string]any{"tool": ToolClaudeCode, "hash": hashA}} }),
		"duplicate tool+hash": mutate(func(m map[string]any) {
			m["bindings"] = []any{binding(ToolClaudeCode, hashA, routeA), binding(ToolClaudeCode, hashA, routeB)}
		}),
	}
	for name, doc := range reject {
		p, _ := json.Marshal(doc)
		_, err := VerifyEnvelope(s.sign(p), s.keys(), now, time.Time{})
		if CodeOf(err) != CodeInvalidPayload {
			t.Errorf("reject %q: code = %q", name, CodeOf(err))
		}
	}

	raw := map[string][]byte{
		"not json":      []byte("not json"),
		"array":         []byte("[]"),
		"null":          []byte("null"),
		"trailing data": append(func() []byte { p, _ := json.Marshal(good()); return p }(), []byte(" {}")...),
		"invalid utf8":  []byte("{\"payload_version\":1,\"x\":\"\xff\"}"),
		"empty payload": {},
	}
	for name, p := range raw {
		_, err := VerifyEnvelope(s.sign(p), s.keys(), now, time.Time{})
		if CodeOf(err) != CodeInvalidPayload {
			t.Errorf("raw %q: code = %q", name, CodeOf(err))
		}
	}
}

func TestExpiryBoundary(t *testing.T) {
	s := newSigner(t, "k1")
	env := s.envelopeAt(t0, defaultBindings())
	exp := t0.Add(24 * time.Hour)
	if _, err := VerifyEnvelope(env, s.keys(), exp.Add(-time.Nanosecond), time.Time{}); err != nil {
		t.Errorf("one ns before expiry: %v", err)
	}
	if _, err := VerifyEnvelope(env, s.keys(), exp, time.Time{}); CodeOf(err) != CodeExpired {
		t.Errorf("at expiry: %v", err)
	}
	if _, err := VerifyEnvelope(env, s.keys(), exp.Add(time.Hour), time.Time{}); CodeOf(err) != CodeExpired {
		t.Errorf("after expiry: %v", err)
	}
}

func TestRollbackFloor(t *testing.T) {
	s := newSigner(t, "k1")
	env := s.envelopeAt(t0, defaultBindings())
	now := t0.Add(time.Minute)
	if _, err := VerifyEnvelope(env, s.keys(), now, t0); err != nil {
		t.Errorf("issued_at == floor must pass: %v", err)
	}
	if _, err := VerifyEnvelope(env, s.keys(), now, t0.Add(time.Hour)); CodeOf(err) != CodeRollback {
		t.Errorf("older than floor: %v", err)
	}
	if _, err := VerifyEnvelope(env, s.keys(), now, t0.Add(-time.Hour)); err != nil {
		t.Errorf("newer than floor: %v", err)
	}
}

func TestSignatureAndKeys(t *testing.T) {
	a, b := newSigner(t, "k1"), newSigner(t, "k1")
	env := a.envelopeAt(t0, defaultBindings())
	now := t0.Add(time.Minute)
	if _, err := VerifyEnvelope(env, b.keys(), now, time.Time{}); CodeOf(err) != CodeBadSignature {
		t.Errorf("wrong key: %v", err)
	}
	if _, err := VerifyEnvelope(env, remoteconfig.TrustedKeys{}, now, time.Time{}); CodeOf(err) != CodeUnknownKID {
		t.Errorf("no keys: %v", err)
	}
	tampered := env
	tampered.Payload = base64.RawURLEncoding.EncodeToString([]byte(`{"payload_version":1}`))
	if _, err := VerifyEnvelope(tampered, a.keys(), now, time.Time{}); CodeOf(err) != CodeBadSignature {
		t.Errorf("tampered payload: %v", err)
	}
}

func TestErrorsCarryNoContent(t *testing.T) {
	s := newSigner(t, "k1")
	p, _ := json.Marshal(map[string]any{"payload_version": 1, "secret_marker": "SECRET-DOC-CONTENT"})
	_, err := VerifyEnvelope(s.sign(p), s.keys(), t0, time.Time{})
	if err == nil || err.Error() != "identity: invalid_payload" {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), hashA) {
		t.Error("error text carries document content")
	}
}
