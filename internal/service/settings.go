// Package service wires the agent: settings and token files, the write-behind offset store,
// single-instance lock, status file and the run loop that connects watcher, aggregator,
// remote configuration, SQLite buffer and uploader.
package service

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxTokenLen     = 128
	maxSettingsSize = 16 * 1024
	envHome         = "HARNEXT_HOME"
)

// defaultServerURL is set for release builds: -ldflags "-X github.com/harnext/harnext-agent/internal/service.defaultServerURL=https://..."
var defaultServerURL = ""

// DefaultServerURL is the server compiled into this build, or "".
func DefaultServerURL() string { return defaultServerURL }

// Settings are the non-secret local settings.
type Settings struct {
	ServerURL string `json:"server_url"`
	// AllowInsecureLocalhost permits http:// for loopback hosts only (development).
	AllowInsecureLocalhost bool `json:"allow_insecure_localhost,omitempty"`
	// CopilotOTelPath overrides the VS Code Copilot OTel JSONL file; absolute, no "~" expansion.
	CopilotOTelPath string `json:"copilot_otel_path,omitempty"`
}

const maxCopilotPathLen = 1024

// ValidateCopilotOTelPath accepts "" (default) or an absolute path without control characters.
func ValidateCopilotOTelPath(p string) error {
	if p == "" {
		return nil
	}
	if len(p) > maxCopilotPathLen || !filepath.IsAbs(p) {
		return errors.New("service: invalid copilot telemetry path")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return errors.New("service: invalid copilot telemetry path")
		}
	}
	return nil
}

// CopilotOTelFile is the effective Copilot telemetry file.
func (p Paths) CopilotOTelFile(s Settings) string {
	if s.CopilotOTelPath != "" {
		return s.CopilotOTelPath
	}
	return filepath.Join(p.Dir, "copilot-otel.jsonl")
}

// Paths are the agent's files, all inside one private directory.
type Paths struct {
	Dir          string
	Settings     string
	Token        string
	DB           string
	RemoteConfig string
	Lock         string
	Status       string
}

// Home returns the agent directory: $HARNEXT_HOME or <user config dir>/harnext.
func Home() (string, error) {
	if d := os.Getenv(envHome); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "harnext"), nil
}

func PathsIn(dir string) Paths {
	return Paths{
		Dir:          dir,
		Settings:     filepath.Join(dir, "settings.json"),
		Token:        filepath.Join(dir, "token"),
		DB:           filepath.Join(dir, "agent.db"),
		RemoteConfig: filepath.Join(dir, "agent-config.json"),
		Lock:         filepath.Join(dir, "agent.lock"),
		Status:       filepath.Join(dir, "status.json"),
	}
}

// ErrNotInitialized means settings or token are missing; the user must run init.
var ErrNotInitialized = errors.New("service: not initialized, run init first")

// ValidateToken accepts the agent token shape (hxt_ prefix, URL-safe characters).
func ValidateToken(tok string) error {
	if len(tok) <= 4 || len(tok) > maxTokenLen || !strings.HasPrefix(tok, "hxt_") {
		return errors.New("service: invalid token format")
	}
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return errors.New("service: invalid token format")
		}
	}
	return nil
}

// ValidateServerURL requires https, or http for loopback hosts when allowInsecure is set.
func ValidateServerURL(raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return errors.New("service: invalid server URL")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if allowInsecure && isLoopback(u.Hostname()) {
			return nil
		}
	}
	return errors.New("service: server URL must be https")
}

func isLoopback(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// SaveSettings validates and writes settings (0600, directory 0700).
func (p Paths) SaveSettings(s Settings) error {
	if err := ValidateServerURL(s.ServerURL, s.AllowInsecureLocalhost); err != nil {
		return err
	}
	if err := ValidateCopilotOTelPath(s.CopilotOTelPath); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return errors.New("service: encode settings")
	}
	return writeAtomic(p.Dir, p.Settings, data)
}

// LoadSettings reads and re-validates settings.
func (p Paths) LoadSettings() (Settings, error) {
	data, err := readLimited(p.Settings, maxSettingsSize)
	if errors.Is(err, os.ErrNotExist) {
		return Settings{}, ErrNotInitialized
	}
	if err != nil {
		return Settings{}, errors.New("service: read settings")
	}
	var s Settings
	if json.Unmarshal(data, &s) != nil || ValidateServerURL(s.ServerURL, s.AllowInsecureLocalhost) != nil ||
		ValidateCopilotOTelPath(s.CopilotOTelPath) != nil {
		return Settings{}, errors.New("service: settings file is invalid")
	}
	return s, nil
}

// SaveToken validates and writes the token (0600).
func (p Paths) SaveToken(tok string) error {
	if err := ValidateToken(tok); err != nil {
		return err
	}
	return writeAtomic(p.Dir, p.Token, []byte(tok))
}

// LoadToken reads the token; the file may end with a newline.
func (p Paths) LoadToken() (string, error) {
	data, err := readLimited(p.Token, maxTokenLen+2)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotInitialized
	}
	if err != nil {
		return "", errors.New("service: read token")
	}
	tok := strings.TrimSpace(string(data))
	if ValidateToken(tok) != nil {
		return "", errors.New("service: token file is invalid")
	}
	return tok, nil
}

func readLimited(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("too large")
	}
	return data, nil
}

// writeAtomic writes data to path via a 0600 temporary file in dir, fsync and rename.
// Errors never include paths.
func writeAtomic(dir, path string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.New("service: create directory")
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return errors.New("service: write file")
	}
	name := tmp.Name()
	fail := func() error {
		tmp.Close()
		_ = os.Remove(name)
		return errors.New("service: write file")
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail()
	}
	if _, err := tmp.Write(data); err != nil {
		return fail()
	}
	if err := tmp.Sync(); err != nil {
		return fail()
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return errors.New("service: write file")
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return errors.New("service: write file")
	}
	return nil
}
