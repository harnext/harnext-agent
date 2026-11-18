package remoteconfig

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"time"
)

const (
	// MaxPayloadBytes bounds the decoded payload (contract: 64 KiB).
	MaxPayloadBytes = 64 * 1024
	signatureBytes  = ed25519.SignatureSize
)

// Envelope is the signed wire form {payload, sig, kid}. payload and sig are base64url
// (RFC 4648 section 5, no padding, strict). The signature covers the DECODED payload bytes.
type Envelope struct {
	Payload string `json:"payload"`
	Sig     string `json:"sig"`
	KID     string `json:"kid"`
}

// ParseEnvelope decodes the JSON envelope; any shape problem is CodeMalformed.
func ParseEnvelope(body []byte) (Envelope, error) {
	var raw struct {
		Payload *string `json:"payload"`
		Sig     *string `json:"sig"`
		KID     *string `json:"kid"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Payload == nil || raw.Sig == nil || raw.KID == nil {
		return Envelope{}, newError(CodeMalformed)
	}
	return Envelope{Payload: *raw.Payload, Sig: *raw.Sig, KID: *raw.KID}, nil
}

// Verify runs the contract's checks in order and stops at the first failure:
// malformed, unknown_kid, bad_signature, invalid_payload, expired (now >= expires_at),
// rollback (config_version < lastAccepted). On success the verified Config is returned.
func Verify(env Envelope, keys TrustedKeys, now time.Time, lastAccepted int64) (*Config, error) {
	payload, sig, err := decodeEnvelope(env)
	if err != nil {
		return nil, err
	}
	pub, ok := keys[env.KID]
	if !ok {
		return nil, newError(CodeUnknownKID)
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, payload, sig) {
		return nil, newError(CodeBadSignature)
	}
	cfg, ok := parsePayload(payload)
	if !ok {
		return nil, newError(CodeInvalidPayload)
	}
	if !now.Before(cfg.ExpiresAt) {
		return nil, newError(CodeExpired)
	}
	if cfg.ConfigVersion < lastAccepted {
		return nil, newError(CodeRollback)
	}
	return cfg, nil
}

// VerifySigned checks only the signature of an envelope (kid known, Ed25519 over the decoded payload) and returns the
// verified payload bytes. Other signed documents (the agent release notice) use it; Verify adds the configuration rules.
func VerifySigned(env Envelope, keys TrustedKeys) ([]byte, error) {
	payload, sig, err := decodeEnvelope(env)
	if err != nil {
		return nil, err
	}
	pub, ok := keys[env.KID]
	if !ok {
		return nil, newError(CodeUnknownKID)
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, payload, sig) {
		return nil, newError(CodeBadSignature)
	}
	return payload, nil
}

func decodeEnvelope(env Envelope) (payload, sig []byte, err error) {
	if !validKID(env.KID) {
		return nil, nil, newError(CodeMalformed)
	}
	payload, ok := decodeBase64URLStrict(env.Payload)
	if !ok || len(payload) > MaxPayloadBytes {
		return nil, nil, newError(CodeMalformed)
	}
	sig, ok = decodeBase64URLStrict(env.Sig)
	if !ok || len(sig) != signatureBytes {
		return nil, nil, newError(CodeMalformed)
	}
	return payload, sig, nil
}

// decodeBase64URLStrict rejects everything but the unpadded URL alphabet. The explicit
// alphabet check is needed because encoding/base64 silently skips CR and LF.
func decodeBase64URLStrict(s string) ([]byte, bool) {
	if len(s)%4 == 1 {
		return nil, false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return nil, false
		}
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, false
	}
	return b, true
}
