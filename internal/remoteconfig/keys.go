package remoteconfig

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
)

// trustedKeys is set at build time and is empty by default, so an unconfigured build
// verifies nothing and friction stays null (safe default):
//
//	go build -ldflags "-X github.com/harnext/harnext-agent/internal/remoteconfig.trustedKeys=kid1:BASE64PUB[,kid2:BASE64PUB]"
//
// Public keys are not secrets; the private key must never be in this repository or binary.
var trustedKeys = ""

// TrustedKeys maps a key id to its Ed25519 public key.
type TrustedKeys map[string]ed25519.PublicKey

// BuildTrustedKeys parses the keys injected at build time.
func BuildTrustedKeys() (TrustedKeys, error) { return ParseTrustedKeys(trustedKeys) }

// ParseTrustedKeys parses "kid:base64pub,kid:base64pub". Keys are raw 32-byte Ed25519
// public keys in standard padded base64. An empty string yields an empty set.
func ParseTrustedKeys(s string) (TrustedKeys, error) {
	keys := TrustedKeys{}
	s = strings.TrimSpace(s)
	if s == "" {
		return keys, nil
	}
	for _, item := range strings.Split(s, ",") {
		kid, b64, ok := strings.Cut(strings.TrimSpace(item), ":")
		if !ok || !validKID(kid) {
			return nil, errors.New("remoteconfig: invalid trusted key entry")
		}
		raw, err := base64.StdEncoding.Strict().DecodeString(b64)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, errors.New("remoteconfig: invalid trusted key entry")
		}
		if _, dup := keys[kid]; dup {
			return nil, errors.New("remoteconfig: duplicate trusted key id")
		}
		keys[kid] = ed25519.PublicKey(raw)
	}
	return keys, nil
}

func validKID(s string) bool {
	if len(s) < 1 || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}
