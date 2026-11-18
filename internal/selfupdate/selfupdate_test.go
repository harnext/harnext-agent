package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		{"0.2.1", "0.2.2", true}, {"0.2.9", "0.3.0", true}, {"0.9.0", "1.0.0", true},
		{"0.2.2", "0.2.2", false}, {"0.2.3", "0.2.2", false}, {"1.0.0", "0.99.0", false},
		{"dev", "0.2.2", false}, {"0.2.1", "dev", false}, {"0.2.1", "0.2.2-rc1", false}, {"", "0.2.2", false},
	}
	for _, c := range cases {
		if got := Newer(c.cur, c.latest); got != c.want {
			t.Errorf("Newer(%q,%q) = %v, want %v", c.cur, c.latest, got, c.want)
		}
	}
}

func TestEligible(t *testing.T) {
	keys := remoteconfig.TrustedKeys{"k1": make([]byte, 32)}
	if !Eligible("0.2.1", keys, "https://app.example.test") {
		t.Error("a release build with keys must be eligible")
	}
	for _, c := range []struct {
		v    string
		k    remoteconfig.TrustedKeys
		s    string
		what string
	}{{"dev", keys, "https://x", "development build"}, {"0.2.1", remoteconfig.TrustedKeys{}, "https://x", "no keys"}, {"0.2.1", keys, "", "no server"}} {
		if Eligible(c.v, c.k, c.s) {
			t.Errorf("%s must not be eligible", c.what)
		}
	}
}

type fixture struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	srv  *httptest.Server
	keys remoteconfig.TrustedKeys
	bin  []byte
	pkg  []byte
	name string
	ver  string
	sum  string
	size int64
	// overrides
	typ, sig, kid string
	badSum        bool
}

func tarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(content)
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func newFixture(t *testing.T, version, script string) *fixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{pub: pub, priv: priv, ver: version, kid: "k1", typ: noticeType, keys: remoteconfig.TrustedKeys{"k1": pub}}
	f.bin = []byte(script)
	goos, goarch := runtime.GOOS, runtime.GOARCH
	if goos == "windows" {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create("harnext-agent.exe")
		_, _ = w.Write(f.bin)
		_ = zw.Close()
		f.pkg = buf.Bytes()
		f.name = "harnext-agent_" + version + "_" + goos + "_" + goarch + ".zip"
	} else {
		f.pkg = tarGz(t, "harnext-agent", f.bin)
		f.name = "harnext-agent_" + version + "_" + goos + "_" + goarch + ".tar.gz"
	}
	sum := sha256.Sum256(f.pkg)
	f.sum, f.size = hex.EncodeToString(sum[:]), int64(len(f.pkg))
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent-release", func(w http.ResponseWriter, _ *http.Request) {
		s := f.sum
		if f.badSum {
			s = strings.Repeat("0", 64)
		}
		payload, _ := json.Marshal(map[string]any{
			"type": f.typ, "version": f.ver, "download_base": f.srv.URL + "/pkg",
			"files": map[string]any{f.name: map[string]any{"sha256": s, "size": f.size}},
		})
		sig := ed25519.Sign(f.priv, payload)
		sigText := base64.RawURLEncoding.EncodeToString(sig)
		if f.sig != "" {
			sigText = f.sig
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"payload": base64.RawURLEncoding.EncodeToString(payload), "sig": sigText, "kid": f.kid})
	})
	mux.HandleFunc("/pkg/", func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.URL.Path, "/pkg/") != f.name {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(f.pkg)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) options(t *testing.T, current string) Options {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "harnext-agent")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if err := os.WriteFile(bin, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Options{ServerURL: f.srv.URL, AllowInsecureLocalhost: true, Current: current, Keys: f.keys, BinaryPath: bin}
}

const okScript = "#!/bin/sh\nexit 0\n"

func TestUpdateInstallsANewerSignedRelease(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture binary is a shell script")
	}
	f := newFixture(t, "0.2.2", okScript)
	o := f.options(t, "0.2.1")
	v, err := Update(t.Context(), o)
	if err != nil || v != "0.2.2" {
		t.Fatalf("Update = %q, %v", v, err)
	}
	got, _ := os.ReadFile(o.BinaryPath)
	if string(got) != okScript {
		t.Fatalf("binary was not replaced: %q", got)
	}
	if st, _ := os.Stat(o.BinaryPath); st.Mode().Perm()&0o100 == 0 {
		t.Error("the new binary must be executable")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(o.BinaryPath), ".harnext-agent.update")); err == nil {
		t.Error("temporary file left behind")
	}
}

func TestUpdateRefusesWhatIsNotNewerOrTrusted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture binary is a shell script")
	}
	unchanged := func(t *testing.T, o Options) {
		t.Helper()
		if got, _ := os.ReadFile(o.BinaryPath); string(got) != "old binary" {
			t.Fatalf("the binary must stay untouched, now %q", got)
		}
	}
	t.Run("same version", func(t *testing.T) {
		f := newFixture(t, "0.2.1", okScript)
		o := f.options(t, "0.2.1")
		if _, err := Update(t.Context(), o); err != ErrNoUpdate {
			t.Fatalf("err = %v", err)
		}
		unchanged(t, o)
	})
	t.Run("older version is never installed", func(t *testing.T) {
		f := newFixture(t, "0.1.0", okScript)
		o := f.options(t, "0.2.1")
		if _, err := Update(t.Context(), o); err != ErrNoUpdate {
			t.Fatalf("err = %v", err)
		}
		unchanged(t, o)
	})
	t.Run("bad signature", func(t *testing.T) {
		f := newFixture(t, "0.2.2", okScript)
		f.sig = base64.RawURLEncoding.EncodeToString(make([]byte, 64))
		o := f.options(t, "0.2.1")
		if _, err := Update(t.Context(), o); err == nil || err == ErrNoUpdate {
			t.Fatalf("err = %v", err)
		}
		unchanged(t, o)
	})
	t.Run("unknown key id", func(t *testing.T) {
		f := newFixture(t, "0.2.2", okScript)
		f.kid = "other"
		o := f.options(t, "0.2.1")
		if _, err := Update(t.Context(), o); err == nil || err == ErrNoUpdate {
			t.Fatalf("err = %v", err)
		}
		unchanged(t, o)
	})
	t.Run("a signed document of another type", func(t *testing.T) {
		f := newFixture(t, "0.2.2", okScript)
		f.typ = "agent-config"
		o := f.options(t, "0.2.1")
		if _, err := Update(t.Context(), o); err == nil || err == ErrNoUpdate {
			t.Fatalf("err = %v", err)
		}
		unchanged(t, o)
	})
	t.Run("checksum of the package differs from the signed one", func(t *testing.T) {
		f := newFixture(t, "0.2.2", okScript)
		f.badSum = true
		o := f.options(t, "0.2.1")
		if _, err := Update(t.Context(), o); err == nil || !strings.Contains(err.Error(), "checksum") {
			t.Fatalf("err = %v", err)
		}
		unchanged(t, o)
	})
	t.Run("new binary that does not start is not installed", func(t *testing.T) {
		f := newFixture(t, "0.2.2", "#!/bin/sh\nexit 3\n")
		o := f.options(t, "0.2.1")
		if _, err := Update(t.Context(), o); err == nil || !strings.Contains(err.Error(), "does not start") {
			t.Fatalf("err = %v", err)
		}
		unchanged(t, o)
	})
	t.Run("plain http only for localhost development", func(t *testing.T) {
		f := newFixture(t, "0.2.2", okScript)
		o := f.options(t, "0.2.1")
		o.AllowInsecureLocalhost = false
		if _, err := Update(t.Context(), o); err == nil || err == ErrNoUpdate {
			t.Fatalf("err = %v", err)
		}
		unchanged(t, o)
	})
}

func TestExtractBinaryFromZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("harnext-agent.exe")
	_, _ = w.Write([]byte("MZ-binary"))
	_ = zw.Close()
	got, err := ExtractBinary(buf.Bytes(), "windows")
	if err != nil || string(got) != "MZ-binary" {
		t.Fatalf("zip: %q %v", got, err)
	}
	if _, err := ExtractBinary(tarGz(t, "something-else", []byte("x")), "linux"); err == nil {
		t.Error("a package without the agent binary must be refused")
	}
}

func TestErrorsNeverContainTheServerAddress(t *testing.T) {
	o := Options{ServerURL: "http://127.0.0.1:1/secret-path", AllowInsecureLocalhost: true, Current: "0.2.1", Keys: remoteconfig.TrustedKeys{"k": make([]byte, 32)}}
	_, err := FetchRelease(t.Context(), o)
	if err == nil || strings.Contains(err.Error(), "secret-path") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("err = %v", err)
	}
}

// The server (TypeScript) signs the notice; the agent (Go) must verify exactly what it produces.
func TestVerifiesTheNoticeTheServerSigns(t *testing.T) {
	data, err := os.ReadFile(filepath.FromSlash("../../contracts/testdata/agent-release.v1.vector.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vec struct {
		TrustedKeys map[string]string `json:"trusted_keys"`
		Manifest    Release           `json:"manifest"`
		Envelope    json.RawMessage   `json:"envelope"`
	}
	if err := json.Unmarshal(data, &vec); err != nil {
		t.Fatal(err)
	}
	var pairs []string
	for kid, k := range vec.TrustedKeys {
		pairs = append(pairs, kid+":"+k)
	}
	keys, err := remoteconfig.ParseTrustedKeys(strings.Join(pairs, ","))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(vec.Envelope) }))
	defer srv.Close()
	rel, err := FetchRelease(t.Context(), Options{ServerURL: srv.URL, AllowInsecureLocalhost: true, Current: "0.2.1", Keys: keys})
	if err != nil {
		t.Fatalf("the notice the server signs must verify: %v", err)
	}
	if rel.Version != vec.Manifest.Version || len(rel.Files) != len(vec.Manifest.Files) || rel.Files["harnext-agent_0.9.9_linux_amd64.tar.gz"].Size != 2222 {
		t.Fatalf("release = %+v", rel)
	}
	if _, _, ok := rel.Asset("windows", "amd64"); !ok {
		t.Error("the windows asset must be found")
	}
	// Another key does not verify it.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := FetchRelease(t.Context(), Options{ServerURL: srv.URL, AllowInsecureLocalhost: true, Current: "0.2.1",
		Keys: remoteconfig.TrustedKeys{"test-release": other.Public().(ed25519.PublicKey)}}); err == nil {
		t.Fatal("a notice signed by another key must be refused")
	}
}
