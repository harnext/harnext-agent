package account

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHash(t *testing.T) {
	// Vectors come from the server's public.account_email_hash.
	cases := []struct{ in, want string }{
		{"Test.User@Example.test", "5707c4bce9be81649b2e411cd2cd0c785eae8caa2fca35d36626c0d29c2cec48"},
		{"  Test.User@Example.test  ", "5707c4bce9be81649b2e411cd2cd0c785eae8caa2fca35d36626c0d29c2cec48"},
		{"ALICE@EXAMPLE.TEST", "190b3fd563dc07e1694490003a1a9fdb88d4e6eb1838b08010dfb2aa260e8f45"},
	}
	for _, c := range cases {
		if got := Hash(c.in); got != c.want {
			t.Errorf("Hash(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestMaskLabel(t *testing.T) {
	long := strings.Repeat("d", 100) + ".test"
	cases := []struct{ name, in, want string }{
		{"five letters keep two", "Carol@Gmail.test", "ca***@gmail.test"},
		{"spaces", "  carol@gmail.test ", "ca***@gmail.test"},
		{"long local keeps three", "developer@example.test", "dev***@example.test"},
		{"dotted local", "carol.longname@example.test", "car***@example.test"},
		{"six letters keep three", "abcdef@x.test", "abc***@x.test"},
		{"four letters keep two", "abcd@x.test", "ab***@x.test"},
		{"three letters keep one", "abc@x.test", "a***@x.test"},
		{"two letters keep one", "ab@x.test", "a***@x.test"},
		{"one letter local", "c@gmail.test", "c***@gmail.test"},
		{"multibyte runes", "ëdaxyz@example.test", "ëda***@example.test"},
		{"long domain", "carol@" + long, "ca***@" + strings.Repeat("d", 60)},
		{"empty", "", ""},
		{"no at", "carol", ""},
		{"no local", "@example.test", ""},
		{"no domain", "carol@", ""},
	}
	for _, c := range cases {
		got := MaskLabel(c.in)
		if got != c.want {
			t.Errorf("%s: MaskLabel(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
		if utf8.RuneCountInString(got) > 80 {
			t.Errorf("%s: label too long: %d", c.name, utf8.RuneCountInString(got))
		}
	}
}

// A masked label never carries the whole local part, whatever its length (one character is the unavoidable exception), and at
// most three characters of it.
func TestMaskLabelNeverRevealsTheLocalPart(t *testing.T) {
	for n := 2; n <= 40; n++ {
		local := strings.Repeat("x", n)
		got := MaskLabel(local + "@example.test")
		visible := strings.Index(got, "***")
		if visible < 1 || visible > 3 || visible > n/2 {
			t.Errorf("local part of %d letters: %q shows %d characters", n, got, visible)
		}
		if strings.Contains(got, local) {
			t.Errorf("local part of %d letters leaks: %q", n, got)
		}
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeEmail(t *testing.T) {
	filler := `"filler":[` + strings.Repeat(`{"k":"vvvvvvvvvvvvvvvvvvvv","n":[1,2,3]},`, 150000) + `{}]`
	cases := []struct {
		name    string
		content string
		want    string
		ok      bool
	}{
		{"simple", `{"oauthAccount":{"emailAddress":"User@Example.test"}}`, "user@example.test", true},
		{"trimmed", `{"oauthAccount":{"emailAddress":"  user@example.test "}}`, "user@example.test", true},
		{"other keys first", `{"a":{"oauthAccount":{"emailAddress":"nested@example.test"}},"b":[1,{"x":null}],"oauthAccount":{"accountUuid":"u","emailAddress":"user@example.test","org":{"emailAddress":"org@example.test"}}}`, "user@example.test", true},
		{"email before other fields", `{"oauthAccount":{"emailAddress":"user@example.test","x":[1,2,{"y":1}]},"later":"z"}`, "user@example.test", true},
		{"large file", `{` + filler + `,"oauthAccount":{"emailAddress":"big@example.test"}}`, "big@example.test", true},
		{"large file, key first", `{"oauthAccount":{"emailAddress":"first@example.test"},` + filler + `}`, "first@example.test", true},
		{"duplicate key uses the first", `{"oauthAccount":{"emailAddress":"one@example.test"},"oauthAccount":{"emailAddress":"two@example.test"}}`, "one@example.test", true},
		{"null account then object", `{"oauthAccount":null,"oauthAccount":{"emailAddress":"two@example.test"}}`, "two@example.test", true},
		{"missing key", `{"other":{"emailAddress":"x@example.test"}}`, "", false},
		{"missing email", `{"oauthAccount":{"accountUuid":"u"}}`, "", false},
		{"empty email", `{"oauthAccount":{"emailAddress":"  "}}`, "", false},
		{"email not a string", `{"oauthAccount":{"emailAddress":{"a":1}}}`, "", false},
		{"account not an object", `{"oauthAccount":"user@example.test"}`, "", false},
		{"truncated", `{"oauthAccount":{"emailAddress":"user@exa`, "", false},
		{"truncated before account", `{"a":[1,2,`, "", false},
		{"not json", `hello`, "", false},
		{"array root", `[{"oauthAccount":{"emailAddress":"user@example.test"}}]`, "", false},
		{"empty file", ``, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			write(t, filepath.Join(home, ".claude.json"), c.content)
			got, ok := ClaudeEmail(filepath.Join(home, ".claude"), home)
			if got != c.want || ok != c.ok {
				t.Errorf("got (%q, %v), want (%q, %v)", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestClaudeEmailLargeFixtureIsLarge(t *testing.T) {
	filler := strings.Repeat(`{"k":"vvvvvvvvvvvvvvvvvvvv","n":[1,2,3]},`, 150000)
	if len(filler) < 5<<20 {
		t.Fatalf("fixture is only %d bytes", len(filler))
	}
}

func TestClaudeEmailLocation(t *testing.T) {
	const body = `{"oauthAccount":{"emailAddress":"user@example.test"}}`
	home := t.TempDir()
	custom := filepath.Join(t.TempDir(), "profile-b")
	write(t, filepath.Join(home, ".claude.json"), body)
	write(t, filepath.Join(custom, ".claude.json"), strings.Replace(body, "user@", "other@", 1))

	if got, ok := ClaudeEmail(filepath.Join(home, ".claude"), home); !ok || got != "user@example.test" {
		t.Errorf("default dir: got (%q, %v)", got, ok)
	}
	if got, ok := ClaudeEmail(filepath.Join(home, ".claude")+string(filepath.Separator), home); !ok || got != "user@example.test" {
		t.Errorf("default dir with trailing separator: got (%q, %v)", got, ok)
	}
	if got, ok := ClaudeEmail(custom, home); !ok || got != "other@example.test" {
		t.Errorf("custom dir: got (%q, %v)", got, ok)
	}
	// A custom dir must not fall back to the home file.
	if got, ok := ClaudeEmail(filepath.Join(t.TempDir(), "nothing"), home); ok {
		t.Errorf("missing custom config returned %q", got)
	}
}

func TestClaudeEmailRefusals(t *testing.T) {
	const body = `{"oauthAccount":{"emailAddress":"user@example.test"}}`
	t.Run("missing file", func(t *testing.T) {
		home := t.TempDir()
		if _, ok := ClaudeEmail(filepath.Join(home, ".claude"), home); ok {
			t.Error("ok for a missing file")
		}
	})
	t.Run("directory", func(t *testing.T) {
		home := t.TempDir()
		if err := os.Mkdir(filepath.Join(home, ".claude.json"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, ok := ClaudeEmail(filepath.Join(home, ".claude"), home); ok {
			t.Error("ok for a directory")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks need privileges on Windows")
		}
		home := t.TempDir()
		target := filepath.Join(t.TempDir(), "real.json")
		write(t, target, body)
		if err := os.Symlink(target, filepath.Join(home, ".claude.json")); err != nil {
			t.Fatal(err)
		}
		if _, ok := ClaudeEmail(filepath.Join(home, ".claude"), home); ok {
			t.Error("ok for a symlink")
		}
	})
}
