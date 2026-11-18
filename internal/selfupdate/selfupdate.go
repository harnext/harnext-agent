// Package selfupdate keeps the agent on the latest release without any action from the user.
//
// The server publishes a signed release notice (GET /api/v1/agent-release): the same Ed25519 envelope and trusted keys as
// the remote configuration, so a compromised website or download host alone cannot make agents install a different
// binary. The agent only ever moves forward (a notice for an older or equal version is ignored), downloads the package
// for its own platform from the notice, checks size and sha256 against the signed values, extracts the binary, checks that
// it starts, and replaces itself. The caller restarts into the new binary.
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

const (
	noticePath       = "/api/v1/agent-release"
	maxNoticeBytes   = 128 << 10
	maxPackageBytes  = 64 << 20
	maxBinaryBytes   = 160 << 20
	sanityTimeout    = 15 * time.Second
	noticeType       = "agent-release"
	binaryBaseName   = "harnext-agent"
	updateTempSuffix = ".update"
	oldSuffix        = ".old"
)

var (
	versionRE = regexp.MustCompile(`^\d{1,4}\.\d{1,4}\.\d{1,4}$`)
	fileRE    = regexp.MustCompile(`^harnext-agent_\d{1,4}\.\d{1,4}\.\d{1,4}_(darwin|linux|windows)_(amd64|arm64)\.(tar\.gz|zip)$`)
	sha256RE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Options configure a check. Zero GOOS/GOARCH mean the running platform.
type Options struct {
	ServerURL              string
	AllowInsecureLocalhost bool
	Current                string
	Keys                   remoteconfig.TrustedKeys
	HTTPClient             *http.Client
	BinaryPath             string
	GOOS, GOARCH           string
}

// File is one package of a release.
type File struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Release is the verified content of a release notice.
type Release struct {
	Version      string          `json:"version"`
	DownloadBase string          `json:"download_base"`
	Files        map[string]File `json:"files"`
}

// ErrNoUpdate means the agent is already on the latest version (or updates are not possible for this build).
var ErrNoUpdate = errors.New("selfupdate: already up to date")

func (o Options) client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (o Options) platform() (string, string) {
	goos, goarch := o.GOOS, o.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return goos, goarch
}

// Eligible reports whether this build can update itself: a release version, trusted keys and a server address.
func Eligible(current string, keys remoteconfig.TrustedKeys, serverURL string) bool {
	return versionRE.MatchString(current) && len(keys) > 0 && serverURL != ""
}

// Newer reports whether latest is a higher release version than current; anything that is not a plain x.y.z is never newer.
func Newer(current, latest string) bool {
	c, ok1 := parseVersion(current)
	l, ok2 := parseVersion(latest)
	if !ok1 || !ok2 {
		return false
	}
	for i := range c {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	if !versionRE.MatchString(v) {
		return out, false
	}
	for i, part := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(part)
		out[i] = n
	}
	return out, true
}

func (o Options) allowedURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return nil, false
	}
	if u.Scheme == "https" {
		return u, true
	}
	host := u.Hostname()
	return u, o.AllowInsecureLocalhost && u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")
}

// FetchRelease downloads and verifies the signed release notice.
func FetchRelease(ctx context.Context, o Options) (*Release, error) {
	base, ok := o.allowedURL(strings.TrimRight(o.ServerURL, "/"))
	if !ok {
		return nil, errors.New("selfupdate: server address not allowed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String()+noticePath, nil)
	if err != nil {
		return nil, errors.New("selfupdate: bad request")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, errors.New("selfupdate: notice unavailable") // *url.Error would embed the URL
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("selfupdate: notice status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxNoticeBytes+1))
	if err != nil || len(body) > maxNoticeBytes {
		return nil, errors.New("selfupdate: notice unreadable")
	}
	env, err := remoteconfig.ParseEnvelope(body)
	if err != nil {
		return nil, errors.New("selfupdate: notice malformed")
	}
	payload, err := remoteconfig.VerifySigned(env, o.Keys)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: notice not trusted (%s)", remoteconfig.CodeOf(err))
	}
	return parseNotice(payload, o)
}

func parseNotice(payload []byte, o Options) (*Release, error) {
	var n struct {
		Type string `json:"type"`
		Release
	}
	if err := json.Unmarshal(payload, &n); err != nil || n.Type != noticeType || !versionRE.MatchString(n.Version) {
		return nil, errors.New("selfupdate: notice invalid")
	}
	if _, ok := o.allowedURL(n.DownloadBase); !ok || len(n.Files) == 0 || len(n.Files) > 16 {
		return nil, errors.New("selfupdate: notice invalid")
	}
	for name, f := range n.Files {
		if !fileRE.MatchString(name) || !sha256RE.MatchString(f.SHA256) || f.Size <= 0 || f.Size > maxPackageBytes || !strings.Contains(name, "_"+n.Version+"_") {
			return nil, errors.New("selfupdate: notice invalid")
		}
	}
	rel := n.Release
	return &rel, nil
}

// Asset picks the package for the platform.
func (r *Release) Asset(goos, goarch string) (name string, f File, ok bool) {
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	name = fmt.Sprintf("harnext-agent_%s_%s_%s.%s", r.Version, goos, goarch, ext)
	f, ok = r.Files[name]
	return name, f, ok
}

// Download fetches the package and checks size and sha256 against the signed values.
func Download(ctx context.Context, o Options, rel *Release, name string, f File) ([]byte, error) {
	base, ok := o.allowedURL(rel.DownloadBase)
	if !ok {
		return nil, errors.New("selfupdate: download address not allowed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base.String(), "/")+"/"+name, nil)
	if err != nil {
		return nil, errors.New("selfupdate: bad request")
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, errors.New("selfupdate: download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("selfupdate: download status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, f.Size+1))
	if err != nil || int64(len(data)) != f.Size {
		return nil, errors.New("selfupdate: download size mismatch")
	}
	sum := sha256.Sum256(data)
	want, _ := hex.DecodeString(f.SHA256)
	if subtle.ConstantTimeCompare(sum[:], want) != 1 {
		return nil, errors.New("selfupdate: download checksum mismatch")
	}
	return data, nil
}

// ExtractBinary returns the agent binary inside a package (a tar.gz or zip holding exactly the binary).
func ExtractBinary(pkg []byte, goos string) ([]byte, error) {
	want := binaryBaseName
	if goos == "windows" {
		want += ".exe"
		zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
		if err != nil {
			return nil, errors.New("selfupdate: package unreadable")
		}
		for _, zf := range zr.File {
			if path.Base(zf.Name) != want || zf.FileInfo().IsDir() {
				continue
			}
			rc, err := zf.Open()
			if err != nil {
				return nil, errors.New("selfupdate: package unreadable")
			}
			defer rc.Close()
			return readBounded(rc)
		}
		return nil, errors.New("selfupdate: binary not in package")
	}
	gz, err := gzip.NewReader(bytes.NewReader(pkg))
	if err != nil {
		return nil, errors.New("selfupdate: package unreadable")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("selfupdate: binary not in package")
		}
		if err != nil {
			return nil, errors.New("selfupdate: package unreadable")
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == want {
			return readBounded(tr)
		}
	}
}

func readBounded(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxBinaryBytes+1))
	if err != nil || len(b) == 0 || len(b) > maxBinaryBytes {
		return nil, errors.New("selfupdate: binary unreadable")
	}
	return b, nil
}

// Install writes the new binary next to the running one, checks that it starts on this machine and replaces the old one.
// On Unix the running file can be renamed over; on Windows it is moved aside first (CleanupOld removes it later).
func Install(binPath string, bin []byte, goos string) error {
	dir := filepath.Dir(binPath)
	tmp := filepath.Join(dir, "."+filepath.Base(binPath)+updateTempSuffix)
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return errors.New("selfupdate: cannot write next to the agent binary")
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return errors.New("selfupdate: cannot prepare the new binary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), sanityTimeout)
	defer cancel()
	if err := exec.CommandContext(ctx, tmp, "--help").Run(); err != nil {
		_ = os.Remove(tmp)
		return errors.New("selfupdate: the new binary does not start on this machine")
	}
	if goos == "windows" {
		old := binPath + oldSuffix
		_ = os.Remove(old)
		if err := os.Rename(binPath, old); err != nil {
			_ = os.Remove(tmp)
			return errors.New("selfupdate: cannot move the running binary aside")
		}
		if err := os.Rename(tmp, binPath); err != nil {
			_ = os.Rename(old, binPath)
			_ = os.Remove(tmp)
			return errors.New("selfupdate: cannot place the new binary")
		}
		return nil
	}
	if err := os.Rename(tmp, binPath); err != nil {
		_ = os.Remove(tmp)
		return errors.New("selfupdate: cannot replace the agent binary")
	}
	return nil
}

// CleanupOld removes the binary Install moved aside on Windows and any leftover temporary file; best effort.
func CleanupOld(binPath string) {
	_ = os.Remove(binPath + oldSuffix)
	_ = os.Remove(filepath.Join(filepath.Dir(binPath), "."+filepath.Base(binPath)+updateTempSuffix))
}

// Update checks for a newer release and installs it. It returns the installed version, ErrNoUpdate when there is none, or
// an error that never contains URLs or tokens. The caller restarts the agent after a nil error.
func Update(ctx context.Context, o Options) (string, error) {
	rel, err := FetchRelease(ctx, o)
	if err != nil {
		return "", err
	}
	if !Newer(o.Current, rel.Version) {
		return "", ErrNoUpdate
	}
	goos, goarch := o.platform()
	name, f, ok := rel.Asset(goos, goarch)
	if !ok {
		return "", errors.New("selfupdate: no package for this platform")
	}
	pkg, err := Download(ctx, o, rel, name, f)
	if err != nil {
		return "", err
	}
	bin, err := ExtractBinary(pkg, goos)
	if err != nil {
		return "", err
	}
	if err := Install(o.BinaryPath, bin, goos); err != nil {
		return "", err
	}
	return rel.Version, nil
}
