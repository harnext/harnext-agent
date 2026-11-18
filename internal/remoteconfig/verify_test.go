package remoteconfig

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const vectorPath = "../../contracts/testdata/agent-config.v1.vector.json"

type vectorCase struct {
	Name     string   `json:"name"`
	Envelope Envelope `json:"envelope"`
	Last     int64    `json:"last_accepted_config_version"`
	Expect   struct {
		OK            bool   `json:"ok"`
		ConfigVersion int64  `json:"config_version"`
		Reason        string `json:"reason"`
	} `json:"expect"`
}

type vectorFile struct {
	VerifyAt    string            `json:"verify_at"`
	TrustedKeys map[string]string `json:"trusted_keys"`
	Cases       []vectorCase      `json:"cases"`
}

func loadVector(t *testing.T) (vectorFile, TrustedKeys, time.Time) {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash(vectorPath))
	if err != nil {
		t.Fatalf("cannot read the shared test vector: %v", err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	keys := TrustedKeys{}
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

func vectorEnvelope(t *testing.T, name string) Envelope {
	t.Helper()
	v, _, _ := loadVector(t)
	for _, c := range v.Cases {
		if c.Name == name {
			return c.Envelope
		}
	}
	t.Fatalf("vector case %q not found", name)
	return Envelope{}
}

func TestVector(t *testing.T) {
	v, keys, at := loadVector(t)
	wantNames := []string{"valid", "payload_tampered", "sig_tampered", "wrong_key", "unknown_kid", "expired",
		"rollback_v2_accepted", "rollback_v1_rejected", "malformed_base64", "malformed_padding"}
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
			cfg, err := Verify(c.Envelope, keys, at, c.Last)
			if c.Expect.OK {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				if cfg.ConfigVersion != c.Expect.ConfigVersion {
					t.Errorf("config_version = %d, want %d", cfg.ConfigVersion, c.Expect.ConfigVersion)
				}
				return
			}
			if got := CodeOf(err); string(got) != c.Expect.Reason {
				t.Errorf("code = %q (err %v), want %q", got, err, c.Expect.Reason)
			}
			if cfg != nil {
				t.Error("config returned alongside an error")
			}
		})
	}
}

func TestVectorValidContent(t *testing.T) {
	_, keys, at := loadVector(t)
	cfg, err := Verify(vectorEnvelope(t, "valid"), keys, at, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := cfg.Friction
	if f.DetectorVersion != 7 || f.RepeatMinTurns != 2 || f.RepeatJaccard != 0.75 || f.ErrorPasteMinShare != 0.5 {
		t.Errorf("friction = %+v", f)
	}
	if len(f.Patterns) != 3 || f.Patterns[0].Category != CategoryErrorReport || !f.Patterns[1].WholeMessage || f.Patterns[2].Lang != "tr" {
		t.Errorf("patterns = %+v", f.Patterns)
	}
	if len(f.Negators) != 1 || f.Negators[0].Position != "before" {
		t.Errorf("negators = %+v", f.Negators)
	}
	if len(cfg.Topics.Labels) != 9 || cfg.Topics.Fallback != "other" || cfg.Topics.MinScore != 1 {
		t.Errorf("topics = %+v", cfg.Topics)
	}
	if cfg.ExpiresAt != time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC) {
		t.Errorf("expires_at = %v", cfg.ExpiresAt)
	}
}

// testSigner signs with a throwaway key generated at run time; no private key is ever stored.
type testSigner struct {
	kid  string
	priv ed25519.PrivateKey
	keys TrustedKeys
}

func newTestSigner(t *testing.T, kid string) *testSigner {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testSigner{kid: kid, priv: priv, keys: TrustedKeys{kid: pub}}
}

func (s *testSigner) sign(payload []byte) Envelope {
	return Envelope{
		Payload: base64.RawURLEncoding.EncodeToString(payload),
		Sig:     base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.priv, payload)),
		KID:     s.kid,
	}
}

func (s *testSigner) signJSON(t *testing.T, v any) Envelope {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return s.sign(raw)
}

var testNow = time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)

// validPayload returns a fresh, valid payload map that tests mutate.
func validPayload(version int) map[string]any {
	return map[string]any{
		"payload_version": 3,
		"config_version":  version,
		"issued_at":       "2026-11-01T00:00:00.000Z",
		"expires_at":      "2026-12-01T00:00:00.000Z",
		"friction": map[string]any{
			"detector_version":      1,
			"repeat_min_turns":      3,
			"repeat_jaccard":        0.8,
			"error_paste_min_share": 0.6,
			"feedback_patterns":     []any{map[string]any{"category": "error_report", "lang": "en", "pattern": "pattern one", "whole_message": false}},
			"negators":              []any{map[string]any{"lang": "en", "word": "not", "position": "before"}},
		},
		"topics": map[string]any{
			"labels":            []any{"feature", "test", "other"},
			"extension_signals": map[string]any{".go": []any{map[string]any{"label": "feature", "weight": 1}}},
			"tool_signals":      map[string]any{"Edit": []any{map[string]any{"label": "feature", "weight": 1}}},
			"keyword_weights":   map[string]any{"test": []any{map[string]any{"term": "unit test", "weight": 2}}},
			"min_score":         2,
			"fallback":          "other",
		},
		"usage": map[string]any{"context_bands": []any{100000, 200000}},
	}
}

func fric(p map[string]any) map[string]any   { return p["friction"].(map[string]any) }
func topics(p map[string]any) map[string]any { return p["topics"].(map[string]any) }

func TestVerifyOrderAndBoundaries(t *testing.T) {
	s := newTestSigner(t, "k1")
	other := newTestSigner(t, "k1")

	t.Run("valid with unknown fields ignored", func(t *testing.T) {
		p := validPayload(3)
		p["future_field"] = map[string]any{"x": 1}
		fric(p)["future"] = true
		p["min_agent_version"] = "1.2.3"
		cfg, err := Verify(s.signJSON(t, p), s.keys, testNow, 3)
		if err != nil || cfg.ConfigVersion != 3 || cfg.MinAgentVersion != "1.2.3" {
			t.Fatalf("cfg=%+v err=%v", cfg, err)
		}
	})
	t.Run("expiry boundary", func(t *testing.T) {
		env := s.signJSON(t, validPayload(1))
		exp := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
		if _, err := Verify(env, s.keys, exp.Add(-time.Nanosecond), 0); err != nil {
			t.Errorf("just before expiry: %v", err)
		}
		if _, err := Verify(env, s.keys, exp, 0); CodeOf(err) != CodeExpired {
			t.Errorf("at expiry: %v", err)
		}
	})
	t.Run("rollback boundary", func(t *testing.T) {
		env := s.signJSON(t, validPayload(5))
		if _, err := Verify(env, s.keys, testNow, 5); err != nil {
			t.Errorf("equal version must be accepted: %v", err)
		}
		if _, err := Verify(env, s.keys, testNow, 6); CodeOf(err) != CodeRollback {
			t.Errorf("lower version: %v", err)
		}
	})
	t.Run("order: malformed before unknown_kid", func(t *testing.T) {
		env := s.signJSON(t, validPayload(1))
		env.KID = "unknown"
		env.Sig = "AAAA"
		if _, err := Verify(env, s.keys, testNow, 0); CodeOf(err) != CodeMalformed {
			t.Errorf("got %v", err)
		}
	})
	t.Run("order: unknown_kid before bad_signature", func(t *testing.T) {
		env := other.signJSON(t, validPayload(1))
		env.KID = "unknown"
		if _, err := Verify(env, s.keys, testNow, 0); CodeOf(err) != CodeUnknownKID {
			t.Errorf("got %v", err)
		}
	})
	t.Run("order: bad_signature before invalid_payload", func(t *testing.T) {
		env := other.sign([]byte("not json"))
		if _, err := Verify(env, s.keys, testNow, 0); CodeOf(err) != CodeBadSignature {
			t.Errorf("got %v", err)
		}
	})
	t.Run("order: invalid_payload before expired", func(t *testing.T) {
		p := validPayload(1)
		p["payload_version"] = 4
		p["expires_at"] = "2026-11-02T00:00:00.000Z"
		if _, err := Verify(s.signJSON(t, p), s.keys, testNow, 0); CodeOf(err) != CodeInvalidPayload {
			t.Errorf("got %v", err)
		}
	})
	t.Run("order: expired before rollback", func(t *testing.T) {
		p := validPayload(1)
		p["expires_at"] = "2026-11-02T00:00:00.000Z"
		if _, err := Verify(s.signJSON(t, p), s.keys, testNow, 9); CodeOf(err) != CodeExpired {
			t.Errorf("got %v", err)
		}
	})
	t.Run("no trusted keys verifies nothing", func(t *testing.T) {
		if _, err := Verify(s.signJSON(t, validPayload(1)), TrustedKeys{}, testNow, 0); CodeOf(err) != CodeUnknownKID {
			t.Errorf("got %v", err)
		}
		if _, err := Verify(s.signJSON(t, validPayload(1)), nil, testNow, 0); CodeOf(err) != CodeUnknownKID {
			t.Errorf("nil keys: %v", err)
		}
	})
}

func TestVerifyMalformed(t *testing.T) {
	s := newTestSigner(t, "k1")
	good := s.signJSON(t, validPayload(1))
	bigPayload := make([]byte, MaxPayloadBytes+1)
	for i := range bigPayload {
		bigPayload[i] = ' '
	}
	atMax := append([]byte(`{}`), make([]byte, MaxPayloadBytes-2)...)
	for i := 2; i < len(atMax); i++ {
		atMax[i] = ' '
	}

	tests := []struct {
		name string
		env  Envelope
		want Code
	}{
		{"padding in payload", Envelope{good.Payload + "=", good.Sig, "k1"}, CodeMalformed},
		{"plus in payload", Envelope{"ab+d", good.Sig, "k1"}, CodeMalformed},
		{"slash in payload", Envelope{"ab/d", good.Sig, "k1"}, CodeMalformed},
		{"space in payload", Envelope{good.Payload[:8] + " " + good.Payload[8:], good.Sig, "k1"}, CodeMalformed},
		{"newline in payload", Envelope{good.Payload[:8] + "\n" + good.Payload[8:], good.Sig, "k1"}, CodeMalformed},
		{"CRLF at end of payload", Envelope{good.Payload + "\r\n", good.Sig, "k1"}, CodeMalformed},
		{"length 1 mod 4", Envelope{"abcde", good.Sig, "k1"}, CodeMalformed},
		{"padding in sig", Envelope{good.Payload, good.Sig + "=", "k1"}, CodeMalformed},
		{"short signature", Envelope{good.Payload, base64.RawURLEncoding.EncodeToString(make([]byte, 63)), "k1"}, CodeMalformed},
		{"long signature", Envelope{good.Payload, base64.RawURLEncoding.EncodeToString(make([]byte, 65)), "k1"}, CodeMalformed},
		{"empty signature", Envelope{good.Payload, "", "k1"}, CodeMalformed},
		{"payload over 64 KiB", s.sign(bigPayload), CodeMalformed},
		{"empty kid", Envelope{good.Payload, good.Sig, ""}, CodeMalformed},
		{"kid with space", Envelope{good.Payload, good.Sig, "k 1"}, CodeMalformed},
		{"kid too long", Envelope{good.Payload, good.Sig, "abcdefghijklmnopqrstuvwxyzabcdefg"}, CodeMalformed},
		{"payload exactly 64 KiB is not malformed", s.sign(atMax), CodeInvalidPayload},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Verify(tt.env, s.keys, testNow, 0)
			if CodeOf(err) != tt.want {
				t.Errorf("got %v, want %s", err, tt.want)
			}
		})
	}

	t.Run("non-canonical trailing bits", func(t *testing.T) {
		// "QQ" decodes to "A" canonically; "QR" has a non-zero trailing bit.
		if _, ok := decodeBase64URLStrict("QQ"); !ok {
			t.Fatal("canonical form rejected")
		}
		if _, ok := decodeBase64URLStrict("QR"); ok {
			t.Error("non-canonical trailing bits accepted")
		}
		env := Envelope{Payload: "QR", Sig: good.Sig, KID: "k1"}
		if _, err := Verify(env, s.keys, testNow, 0); CodeOf(err) != CodeMalformed {
			t.Errorf("got %v", err)
		}
	})
}

func TestVerifyInvalidPayload(t *testing.T) {
	s := newTestSigner(t, "k1")
	manyPatterns := make([]any, 257)
	for i := range manyPatterns {
		manyPatterns[i] = map[string]any{"category": "correction", "lang": "en", "pattern": "pattern " + string(rune('a'+i%26)) + string(rune('a'+i/26%26))}
	}
	pat := func(category, lang, pattern string) []any {
		return []any{map[string]any{"category": category, "lang": lang, "pattern": pattern}}
	}
	sig1 := func(label string, w any) []any { return []any{map[string]any{"label": label, "weight": w}} }

	mutations := map[string]func(p map[string]any){
		"payload_version 1":       func(p map[string]any) { p["payload_version"] = 1 },
		"payload_version 2":       func(p map[string]any) { p["payload_version"] = 2 },
		"payload_version 4":       func(p map[string]any) { p["payload_version"] = 4 },
		"usage missing":           func(p map[string]any) { delete(p, "usage") },
		"context_bands missing":   func(p map[string]any) { p["usage"] = map[string]any{} },
		"context_bands unsorted":  func(p map[string]any) { p["usage"] = map[string]any{"context_bands": []any{200000, 100000}} },
		"context_bands duplicate": func(p map[string]any) { p["usage"] = map[string]any{"context_bands": []any{100000, 100000}} },
		"context_band too small":  func(p map[string]any) { p["usage"] = map[string]any{"context_bands": []any{999}} },
		"context_band too large":  func(p map[string]any) { p["usage"] = map[string]any{"context_bands": []any{10000001}} },
		"context_band fractional": func(p map[string]any) { p["usage"] = map[string]any{"context_bands": []any{1500.5}} },
		"context_bands too many": func(p map[string]any) {
			p["usage"] = map[string]any{"context_bands": []any{1000, 2000, 3000, 4000, 5000, 6000, 7000, 8000, 9000}}
		},
		"payload_version missing":   func(p map[string]any) { delete(p, "payload_version") },
		"payload_version string":    func(p map[string]any) { p["payload_version"] = "3" },
		"config_version 0":          func(p map[string]any) { p["config_version"] = 0 },
		"config_version fractional": func(p map[string]any) { p["config_version"] = 1.5 },
		"config_version string":     func(p map[string]any) { p["config_version"] = "2" },
		"issued_at with offset":     func(p map[string]any) { p["issued_at"] = "2026-11-01T00:00:00.000+00:00" },
		"issued_at date only":       func(p map[string]any) { p["issued_at"] = "2026-11-01" },
		"issued_at invalid date":    func(p map[string]any) { p["issued_at"] = "2026-02-30T00:00:00.000Z" },
		"issued_at missing":         func(p map[string]any) { delete(p, "issued_at") },
		"expires before issued":     func(p map[string]any) { p["expires_at"] = "2026-10-01T00:00:00.000Z" },
		"expires equals issued":     func(p map[string]any) { p["expires_at"] = p["issued_at"] },
		"lifetime over 90 days":     func(p map[string]any) { p["expires_at"] = "2027-02-01T00:00:00.000Z" },
		"min_agent_version bad":     func(p map[string]any) { p["min_agent_version"] = "1.2" },
		"friction missing":          func(p map[string]any) { delete(p, "friction") },
		"topics missing":            func(p map[string]any) { delete(p, "topics") },
		"detector_version 0":        func(p map[string]any) { fric(p)["detector_version"] = 0 },
		"detector_version missing":  func(p map[string]any) { delete(fric(p), "detector_version") },
		"repeat turns 1":            func(p map[string]any) { fric(p)["repeat_min_turns"] = 1 },
		"repeat turns 21":           func(p map[string]any) { fric(p)["repeat_min_turns"] = 21 },
		"jaccard 0.4":               func(p map[string]any) { fric(p)["repeat_jaccard"] = 0.4 },
		"jaccard 1":                 func(p map[string]any) { fric(p)["repeat_jaccard"] = 1 },
		"jaccard missing":           func(p map[string]any) { delete(fric(p), "repeat_jaccard") },
		"paste share 0.2":           func(p map[string]any) { fric(p)["error_paste_min_share"] = 0.2 },
		"paste share missing":       func(p map[string]any) { delete(fric(p), "error_paste_min_share") },
		"old weights only":          func(p map[string]any) { delete(fric(p), "feedback_patterns"); fric(p)["weights"] = map[string]any{} },
		"patterns missing":          func(p map[string]any) { delete(fric(p), "feedback_patterns") },
		"patterns empty":            func(p map[string]any) { fric(p)["feedback_patterns"] = []any{} },
		"patterns over 256":         func(p map[string]any) { fric(p)["feedback_patterns"] = manyPatterns },
		"pattern uppercase":         func(p map[string]any) { fric(p)["feedback_patterns"] = pat("error_report", "en", "Wrong") },
		"pattern regex chars":       func(p map[string]any) { fric(p)["feedback_patterns"] = pat("error_report", "en", "wr.ng") },
		"pattern unbalanced":        func(p map[string]any) { fric(p)["feedback_patterns"] = pat("error_report", "en", "(wrong") },
		"pattern empty alt":         func(p map[string]any) { fric(p)["feedback_patterns"] = pat("error_report", "en", "(a||b)") },
		"pattern non-ascii":         func(p map[string]any) { fric(p)["feedback_patterns"] = pat("error_report", "tr", "çalışmıyor") },
		"pattern 1 char":            func(p map[string]any) { fric(p)["feedback_patterns"] = pat("error_report", "en", "a") },
		"pattern trailing space":    func(p map[string]any) { fric(p)["feedback_patterns"] = pat("error_report", "en", "wrong ") },
		"pattern bad lang":          func(p map[string]any) { fric(p)["feedback_patterns"] = pat("error_report", "de", "falsch") },
		"pattern bad category":      func(p map[string]any) { fric(p)["feedback_patterns"] = pat("anger", "en", "wrong") },
		"pattern whole string": func(p map[string]any) {
			fric(p)["feedback_patterns"] = []any{map[string]any{"category": "error_report", "lang": "en", "pattern": "wrong", "whole_message": "yes"}}
		},
		"negators missing": func(p map[string]any) { delete(fric(p), "negators") },
		"negator bad position": func(p map[string]any) {
			fric(p)["negators"] = []any{map[string]any{"lang": "en", "word": "not", "position": "around"}}
		},
		"negator uppercase": func(p map[string]any) {
			fric(p)["negators"] = []any{map[string]any{"lang": "en", "word": "Not", "position": "before"}}
		},
		"labels empty":               func(p map[string]any) { topics(p)["labels"] = []any{} },
		"labels unknown":             func(p map[string]any) { topics(p)["labels"] = []any{"feature", "bogus", "other"} },
		"labels duplicate":           func(p map[string]any) { topics(p)["labels"] = []any{"feature", "feature", "other"} },
		"fallback not in labels":     func(p map[string]any) { topics(p)["fallback"] = "docs" },
		"signal label not in labels": func(p map[string]any) { topics(p)["extension_signals"] = map[string]any{".go": sig1("docs", 1)} },
		"keyword label not in labels": func(p map[string]any) {
			topics(p)["keyword_weights"] = map[string]any{"docs": []any{map[string]any{"term": "readme", "weight": 1}}}
		},
		"extension key without separator": func(p map[string]any) { topics(p)["extension_signals"] = map[string]any{"go": sig1("feature", 1)} },
		"extension key uppercase":         func(p map[string]any) { topics(p)["extension_signals"] = map[string]any{".GO": sig1("feature", 1)} },
		"tool key with space":             func(p map[string]any) { topics(p)["tool_signals"] = map[string]any{"Bad Name": sig1("feature", 1)} },
		"signal weight 0":                 func(p map[string]any) { topics(p)["tool_signals"] = map[string]any{"Edit": sig1("feature", 0)} },
		"signal weight 11":                func(p map[string]any) { topics(p)["tool_signals"] = map[string]any{"Edit": sig1("feature", 11)} },
		"signal list empty":               func(p map[string]any) { topics(p)["tool_signals"] = map[string]any{"Edit": []any{}} },
		"min_score 101":                   func(p map[string]any) { topics(p)["min_score"] = 101 },
		"min_score negative":              func(p map[string]any) { topics(p)["min_score"] = -1 },
		"keyword term regex": func(p map[string]any) {
			topics(p)["keyword_weights"] = map[string]any{"test": []any{map[string]any{"term": "te.*st", "weight": 1}}}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p := validPayload(1)
			mutate(p)
			if _, err := Verify(s.signJSON(t, p), s.keys, testNow, 0); CodeOf(err) != CodeInvalidPayload {
				t.Errorf("got %v, want invalid_payload", err)
			}
		})
	}

	raw := map[string][]byte{
		"invalid UTF-8":         []byte("{\"payload_version\":1,\"x\":\"\xff\"}"),
		"not JSON":              []byte("not json"),
		"empty":                 {},
		"array":                 []byte("[]"),
		"trailing data":         []byte(`{"payload_version":1} {}`),
		"null":                  []byte("null"),
		"BOM prefix":            append([]byte("\xef\xbb\xbf"), mustJSON(t, validPayload(1))...),
		"duplicate trailing {}": append(mustJSON(t, validPayload(1)), '}'),
	}
	for name, b := range raw {
		t.Run("raw "+name, func(t *testing.T) {
			if _, err := Verify(s.sign(b), s.keys, testNow, 0); CodeOf(err) != CodeInvalidPayload {
				t.Errorf("got %v, want invalid_payload", err)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestErrorsCarryNoContent(t *testing.T) {
	const secret = "SECRET-FRAGMENT"
	s := newTestSigner(t, "k1")
	p := validPayload(1)
	fric(p)["feedback_patterns"] = []any{map[string]any{"category": "error_report", "lang": "en", "pattern": secret}} // uppercase: invalid
	_, err := Verify(s.signJSON(t, p), s.keys, testNow, 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got != "remoteconfig: invalid_payload" {
		t.Errorf("error text = %q", got)
	}
}

func TestParseTrustedKeys(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	b64 := base64.StdEncoding.EncodeToString(pub)
	b64b := base64.StdEncoding.EncodeToString(pub2)

	keys, err := ParseTrustedKeys("")
	if err != nil || len(keys) != 0 {
		t.Errorf("empty: %v %v", keys, err)
	}
	keys, err = ParseTrustedKeys(" k1:" + b64 + " , k2:" + b64b + " ")
	if err != nil || len(keys) != 2 || !keys["k1"].Equal(pub) || !keys["k2"].Equal(pub2) {
		t.Errorf("two keys: %v %v", keys, err)
	}
	for name, in := range map[string]string{
		"no colon":       "k1" + b64,
		"bad kid":        "k 1:" + b64,
		"empty kid":      ":" + b64,
		"short key":      "k1:" + base64.StdEncoding.EncodeToString(pub[:16]),
		"unpadded":       "k1:" + base64.RawStdEncoding.EncodeToString(pub),
		"url alphabet":   "k1:" + base64.URLEncoding.EncodeToString(pub) + "x",
		"duplicate kid":  "k1:" + b64 + ",k1:" + b64b,
		"trailing comma": "k1:" + b64 + ",",
	} {
		if _, err := ParseTrustedKeys(in); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// Run with: go test -ldflags "-X github.com/harnext/harnext-agent/internal/remoteconfig.trustedKeys=ci-key:<base64 pub>" -run FromLdflags
func TestBuildTrustedKeysFromLdflags(t *testing.T) {
	if trustedKeys == "" {
		t.Skip("trustedKeys not injected via -ldflags -X")
	}
	keys, err := BuildTrustedKeys()
	if err != nil || len(keys) == 0 {
		t.Fatalf("injected keys not usable: %d keys, err %v", len(keys), err)
	}
}

// The default build has no trusted keys: nothing verifies and friction stays null.
func TestBuildDefaultsToNoTrustedKeys(t *testing.T) {
	if trustedKeys != "" {
		t.Skip("trustedKeys injected via -ldflags")
	}
	keys, err := BuildTrustedKeys()
	if err != nil || len(keys) != 0 {
		t.Fatalf("keys=%v err=%v", keys, err)
	}
	m := NewManager(ManagerOptions{Fetcher: failingFetcher{t}, Keys: keys})
	if err := m.Refresh(context.Background()); CodeOf(err) != CodeNoTrustedKeys {
		t.Errorf("got %v", err)
	}
	if _, ok := m.Current(); ok {
		t.Error("configuration available without trusted keys")
	}
}

type failingFetcher struct{ t *testing.T }

func (f failingFetcher) Fetch(context.Context, string) (*FetchResult, error) {
	f.t.Error("network used although nothing could be verified")
	return nil, newError(CodeTransient)
}

func TestParseEnvelope(t *testing.T) {
	good := `{"payload":"a","sig":"b","kid":"c","extra":1}`
	if env, err := ParseEnvelope([]byte(good)); err != nil || env != (Envelope{"a", "b", "c"}) {
		t.Errorf("good: %+v %v", env, err)
	}
	for name, in := range map[string]string{
		"empty":         ``,
		"array":         `[]`,
		"missing kid":   `{"payload":"a","sig":"b"}`,
		"number sig":    `{"payload":"a","sig":1,"kid":"c"}`,
		"null payload":  `{"payload":null,"sig":"b","kid":"c"}`,
		"trailing junk": `{"payload":"a","sig":"b","kid":"c"} x`,
	} {
		if _, err := ParseEnvelope([]byte(in)); CodeOf(err) != CodeMalformed {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestValidPattern(t *testing.T) {
	for _, p := range []string{"ab", "that's wrong", "re-do it", "a|b", "tests? (still )?(fail|failing)", "calis(miyor|madi)"} {
		if !ValidPattern(p) {
			t.Errorf("ValidPattern(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"", "a", "Wrong", "(a+)+", ".*", "[a-z]", "a{2}", "\\d", "^a", "a$", "()", "(a|)", "(|a)", "a||b", "(ab", "ab)", "?ab", "a ?b", " ab", "ab ", "ab|", "(?:ab)", "çalış", "hâlâ"} {
		if ValidPattern(p) {
			t.Errorf("ValidPattern(%q) = true, want false", p)
		}
	}
}
