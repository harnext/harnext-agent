// Package account identifies which Claude Code account a config directory belongs to. The e-mail is read locally, hashed at
// once and never stored, logged or sent; only the hash and a masked label leave this package.
package account

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxConfigSize = 64 << 20
	maxDomainLen  = 60
	// maxVisibleLocal is the number of leading characters of the local part that the masked label keeps.
	maxVisibleLocal = 3
)

// Hash must stay equal to the server's public.account_email_hash.
func Hash(email string) string {
	sum := sha256.Sum256([]byte("harnext:acct:v1:" + strings.ToLower(strings.Trim(email, " "))))
	return hex.EncodeToString(sum[:])
}

// MaskLabel returns "dev***@example.test": the first up to three characters of the local part, never more than half of it, then
// "***" and the domain. It never contains the full local part (except for a one-character local part), and returns "" for input
// that is not an e-mail. The label is for the user to recognise their own account in the panel; it is not a secret and is not
// used to match accounts (the hash does that).
func MaskLabel(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return ""
	}
	local, domain := []rune(email[:at]), []rune(email[at+1:])
	if len(domain) > maxDomainLen {
		domain = domain[:maxDomainLen]
	}
	return string(local[:visibleLocal(len(local))]) + "***@" + string(domain)
}

// visibleLocal is how many leading runes of a local part of n runes stay visible: three, but at most half, and at least one.
func visibleLocal(n int) int {
	return min(maxVisibleLocal, max(1, n/2))
}

// ClaudeEmail reads oauthAccount.emailAddress from the global config of a Claude Code config directory.
func ClaudeEmail(configDir, home string) (email string, ok bool) {
	configDir, home = filepath.Clean(configDir), filepath.Clean(home)
	path := filepath.Join(configDir, ".claude.json")
	if configDir == filepath.Join(home, ".claude") {
		path = filepath.Join(home, ".claude.json")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxConfigSize {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	email, ok = findEmail(io.LimitReader(f, maxConfigSize))
	if !ok {
		return "", false
	}
	return email, true
}

// findEmail walks the JSON as a token stream so unrelated values are skipped without being held in memory.
func findEmail(r io.Reader) (string, bool) {
	dec := json.NewDecoder(r)
	if !isDelim(dec, '{') {
		return "", false
	}
	for dec.More() {
		key, ok := nextString(dec)
		if !ok {
			return "", false
		}
		if key != "oauthAccount" {
			if !skipValue(dec) {
				return "", false
			}
			continue
		}
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		if d, isD := tok.(json.Delim); !isD || d != '{' {
			if isD && !skipRest(dec, d) {
				return "", false
			}
			continue
		}
		return accountEmail(dec)
	}
	return "", false
}

// accountEmail reads the members of an already opened oauthAccount object.
func accountEmail(dec *json.Decoder) (string, bool) {
	var email string
	for dec.More() {
		key, ok := nextString(dec)
		if !ok {
			return "", false
		}
		if key != "emailAddress" {
			if !skipValue(dec) {
				return "", false
			}
			continue
		}
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		switch v := tok.(type) {
		case string:
			email = v
		case json.Delim:
			if !skipRest(dec, v) {
				return "", false
			}
		}
	}
	email = strings.ToLower(strings.TrimSpace(email))
	return email, email != ""
}

func isDelim(dec *json.Decoder, want json.Delim) bool {
	tok, err := dec.Token()
	d, ok := tok.(json.Delim)
	return err == nil && ok && d == want
}

func nextString(dec *json.Decoder) (string, bool) {
	tok, err := dec.Token()
	s, ok := tok.(string)
	return s, err == nil && ok
}

func skipValue(dec *json.Decoder) bool {
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	if d, ok := tok.(json.Delim); ok {
		return skipRest(dec, d)
	}
	return true
}

// skipRest consumes the remainder of a container whose opening delimiter was just read.
func skipRest(dec *json.Decoder, open json.Delim) bool {
	if open != '{' && open != '[' {
		return false
	}
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
	}
	return true
}
