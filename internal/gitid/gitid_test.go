package gitid

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// setup writes fixtures under a temp base; keys are slash paths relative to it, and @HOME@ / @BASE@ in a value become
// the slash form of the home and base directories. The home directory is <base>/home.
func setup(t *testing.T, files map[string]string) (base, home string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", "")
	base = t.TempDir()
	home = filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	repl := strings.NewReplacer("@HOME@", filepath.ToSlash(home), "@BASE@", filepath.ToSlash(base))
	for rel, content := range files {
		write(t, filepath.Join(base, filepath.FromSlash(rel)), repl.Replace(content))
	}
	return base, home
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func emailCfg(addr string) string { return "[user]\n\temail = " + addr + "\n" }

func TestEmail(t *testing.T) {
	huge := "[user]\n\temail = huge@example.test\n" + strings.Repeat("# padding\n", maxFileSize/8)
	tests := []struct {
		name  string
		files map[string]string
		dir   string // relative to base
		want  string // "" means ok=false
	}{
		{
			name: "local overrides global",
			files: map[string]string{
				"home/.gitconfig":            emailCfg("global@example.test"),
				"home/work/repo/.git/config": emailCfg("Local@Example.test"),
			},
			dir:  "home/work/repo/sub",
			want: "local@example.test",
		},
		{
			name:  "global when local has no email",
			files: map[string]string{"home/.gitconfig": emailCfg("global@example.test"), "home/work/repo/.git/config": "[core]\n\tbare = false\n"},
			dir:   "home/work/repo",
			want:  "global@example.test",
		},
		{
			name:  "default xdg path alone",
			files: map[string]string{"home/.config/git/config": emailCfg("xdg@example.test")},
			dir:   "home",
			want:  "xdg@example.test",
		},
		{
			name: "gitconfig overrides default xdg",
			files: map[string]string{
				"home/.config/git/config": emailCfg("xdg@example.test"),
				"home/.gitconfig":         emailCfg("dot@example.test"),
			},
			dir:  "home",
			want: "dot@example.test",
		},
		{
			name: "includeIf gitdir with trailing slash and tilde",
			files: map[string]string{
				"home/.gitconfig":          emailCfg("global@example.test") + "[includeIf \"gitdir:~/work/\"]\n\tpath = ~/.gitconfig-work\n",
				"home/.gitconfig-work":     emailCfg("work@example.test"),
				"home/work/repo/.git/HEAD": "ref: refs/heads/main\n",
			},
			dir:  "home/work/repo/sub",
			want: "work@example.test",
		},
		{
			name: "includeIf gitdir not matching",
			files: map[string]string{
				"home/.gitconfig":              emailCfg("global@example.test") + "[includeIf \"gitdir:~/work/\"]\n\tpath = ~/.gitconfig-work\n",
				"home/.gitconfig-work":         emailCfg("work@example.test"),
				"home/personal/repo/.git/HEAD": "x",
			},
			dir:  "home/personal/repo",
			want: "global@example.test",
		},
		{
			name: "includeIf absolute pattern with double star",
			files: map[string]string{
				"home/.gitconfig":           "[includeIf \"gitdir:@HOME@/work/**\"]\n\tpath = work.inc\n",
				"home/work.inc":             emailCfg("abs@example.test"),
				"home/work/a/b/repo/.git/x": "x",
			},
			dir:  "home/work/a/b/repo",
			want: "abs@example.test",
		},
		{
			name: "includeIf gitdir/i is case-insensitive",
			files: map[string]string{
				"home/.gitconfig":       "[includeIf \"gitdir/i:@HOME@/WORK/\"]\n\tpath = work.inc\n",
				"home/work.inc":         emailCfg("fold@example.test"),
				"home/work/repo/.git/x": "x",
			},
			dir:  "home/work/repo",
			want: "fold@example.test",
		},
		{
			name: "includeIf gitdir is case-sensitive",
			files: map[string]string{
				"home/.gitconfig":       "[includeIf \"gitdir:@HOME@/WORK/\"]\n\tpath = work.inc\n",
				"home/work.inc":         emailCfg("fold@example.test"),
				"home/work/repo/.git/x": "x",
			},
			dir:  "home/work/repo",
			want: "",
		},
		{
			name: "includeIf relative pattern matches anywhere",
			files: map[string]string{
				"home/.gitconfig":          "[includeIf \"gitdir:client/\"]\n\tpath = c.inc\n",
				"home/c.inc":               emailCfg("client@example.test"),
				"home/src/client/r/.git/x": "x",
			},
			dir:  "home/src/client/r",
			want: "client@example.test",
		},
		{
			name: "includeIf in local config overrides",
			files: map[string]string{
				"home/.gitconfig":            emailCfg("global@example.test"),
				"home/work/repo/.git/config": "[includeIf \"gitdir:@HOME@/work/**\"]\n\tpath = ../../../extra.inc\n",
				"home/extra.inc":             emailCfg("extra@example.test"),
			},
			dir:  "home/work/repo",
			want: "extra@example.test",
		},
		{
			name: "onbranch and hasconfig are not evaluated",
			files: map[string]string{
				"home/.gitconfig": emailCfg("global@example.test") +
					"[includeIf \"onbranch:main\"]\n\tpath = a.inc\n[includeIf \"hasconfig:remote.*.url:*\"]\n\tpath = a.inc\n",
				"home/a.inc":         emailCfg("never@example.test"),
				"home/r/.git/config": "[core]\n",
			},
			dir:  "home/r",
			want: "global@example.test",
		},
		{
			name: "include of a relative file",
			files: map[string]string{
				"home/.gitconfig":    "[include]\n\tpath = sub/extra.cfg\n",
				"home/sub/extra.cfg": "[include]\n\tpath = more.cfg\n",
				"home/sub/more.cfg":  emailCfg("rel@example.test"),
			},
			dir:  "home",
			want: "rel@example.test",
		},
		{
			name: "include after email overrides it, before email is overridden",
			files: map[string]string{
				"home/.gitconfig": "[include]\n\tpath = a.cfg\n[user]\n\temail = later@example.test\n",
				"home/a.cfg":      emailCfg("early@example.test"),
			},
			dir:  "home",
			want: "later@example.test",
		},
		{
			name: "include cycle terminates",
			files: map[string]string{
				"home/.gitconfig": "[include]\n\tpath = a.cfg\n",
				"home/a.cfg":      "[include]\n\tpath = b.cfg\n",
				"home/b.cfg":      "[include]\n\tpath = a.cfg\n" + emailCfg("cycle@example.test"),
			},
			dir:  "home",
			want: "cycle@example.test",
		},
		{
			name:  "quoted value and inline comment",
			files: map[string]string{"home/.gitconfig": "[user]\n\temail = \"Quoted@Example.test\" # work\n"},
			dir:   "home",
			want:  "quoted@example.test",
		},
		{
			name:  "semicolon comment and trailing space",
			files: map[string]string{"home/.gitconfig": "# c\n; c\n[user]\n\tname = X\n\temail = semi@example.test   ; c\n"},
			dir:   "home",
			want:  "semi@example.test",
		},
		{
			name:  "case-insensitive section and key, crlf and bom",
			files: map[string]string{"home/.gitconfig": "\xef\xbb\xbf[USER]\r\n\tEMail = Upper@Example.test\r\n"},
			dir:   "home",
			want:  "upper@example.test",
		},
		{
			name:  "key on the section line",
			files: map[string]string{"home/.gitconfig": "[user] email = inline@example.test\n"},
			dir:   "home",
			want:  "inline@example.test",
		},
		{
			name:  "user subsection is not user",
			files: map[string]string{"home/.gitconfig": "[user \"x\"]\n\temail = sub@example.test\n"},
			dir:   "home",
			want:  "",
		},
		{
			name:  "line continuation is skipped",
			files: map[string]string{"home/.gitconfig": emailCfg("keep@example.test") + "[user]\n\temail = cont@example.test \\\n"},
			dir:   "home",
			want:  "keep@example.test",
		},
		{
			name:  "unterminated quote is skipped",
			files: map[string]string{"home/.gitconfig": emailCfg("keep@example.test") + "[user]\n\temail = \"bad@example.test\n"},
			dir:   "home",
			want:  "keep@example.test",
		},
		{
			name: "empty local value wins and gives no address",
			files: map[string]string{
				"home/.gitconfig":    emailCfg("global@example.test"),
				"home/r/.git/config": "[user]\n\temail =\n",
			},
			dir:  "home/r",
			want: "",
		},
		{
			name: "no repo uses global",
			files: map[string]string{
				"home/.gitconfig": emailCfg("global@example.test"),
				"home/loose/f":    "x",
			},
			dir:  "home/loose",
			want: "global@example.test",
		},
		{
			name:  "no repo and no config",
			files: map[string]string{"home/loose/f": "x"},
			dir:   "home/loose",
			want:  "",
		},
		{
			name:  "no email anywhere",
			files: map[string]string{"home/.gitconfig": "[core]\n\teditor = vi\n", "home/r/.git/config": "[core]\n"},
			dir:   "home/r",
			want:  "",
		},
		{
			name:  "huge file is ignored",
			files: map[string]string{"home/.gitconfig": huge, "home/r/.git/config": "[core]\n"},
			dir:   "home/r",
			want:  "",
		},
		{
			name:  "empty directory argument",
			files: map[string]string{"home/.gitconfig": emailCfg("global@example.test")},
			dir:   "",
			want:  "global@example.test",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, home := setup(t, tt.files)
			dir := ""
			if tt.dir != "" {
				dir = filepath.Join(base, filepath.FromSlash(tt.dir))
			}
			got, ok := NewResolver(home).Email(dir)
			if got != tt.want || ok != (tt.want != "") {
				t.Fatalf("Email() = (%q, %v), want %q", got, ok, tt.want)
			}
		})
	}
}

func TestXDGConfigHome(t *testing.T) {
	base, home := setup(t, map[string]string{"xdgdir/git/config": emailCfg("xdg@example.test")})
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "xdgdir"))
	if got, ok := NewResolver(home).Email(home); !ok || got != "xdg@example.test" {
		t.Fatalf("XDG_CONFIG_HOME config not used: (%q, %v)", got, ok)
	}

	// The default ~/.config is ignored once XDG_CONFIG_HOME is set, and ~/.gitconfig still wins over it.
	write(t, filepath.Join(home, ".config", "git", "config"), emailCfg("default@example.test"))
	if got, _ := NewResolver(home).Email(home); got != "xdg@example.test" {
		t.Fatalf("default xdg dir used despite XDG_CONFIG_HOME: %q", got)
	}
	write(t, filepath.Join(home, ".gitconfig"), emailCfg("dot@example.test"))
	if got, _ := NewResolver(home).Email(home); got != "dot@example.test" {
		t.Fatalf("~/.gitconfig must override XDG: %q", got)
	}

	t.Setenv("XDG_CONFIG_HOME", "relative/dir")
	if got, _ := NewResolver(home).Email(home); got != "dot@example.test" {
		t.Fatalf("relative XDG_CONFIG_HOME must be ignored: %q", got)
	}
}

func TestIncludeDepthLimit(t *testing.T) {
	// c0 is ~/.gitconfig; c1..c10 are allowed includes, c11 is too deep.
	chain := func(last int) map[string]string {
		files := map[string]string{}
		for i := 0; i <= last; i++ {
			content := ""
			if i < last {
				content = fmt.Sprintf("[include]\n\tpath = c%d.cfg\n", i+1)
			} else {
				content = emailCfg("deep@example.test")
			}
			name := fmt.Sprintf("home/c%d.cfg", i)
			if i == 0 {
				name = "home/.gitconfig"
			}
			files[name] = content
		}
		return files
	}
	for _, tt := range []struct {
		last int
		want bool
	}{{10, true}, {11, false}} {
		_, home := setup(t, chain(tt.last))
		if _, ok := NewResolver(home).Email(home); ok != tt.want {
			t.Errorf("chain of %d includes: ok = %v, want %v", tt.last, ok, tt.want)
		}
	}
}

func TestIncludeFanOutIsBounded(t *testing.T) {
	// 20 levels of 20 includes of the next level would be 20^10 reads without the file budget.
	files := map[string]string{}
	var fan strings.Builder
	for i := 0; i < 20; i++ {
		fan.WriteString("[include]\n\tpath = n1.cfg\n")
	}
	files["home/.gitconfig"] = fan.String()
	for l := 1; l < 10; l++ {
		var b strings.Builder
		for i := 0; i < 20; i++ {
			fmt.Fprintf(&b, "[include]\n\tpath = n%d.cfg\n", l+1)
		}
		files[fmt.Sprintf("home/n%d.cfg", l)] = b.String()
	}
	files["home/n10.cfg"] = emailCfg("x@example.test")
	_, home := setup(t, files)
	done := make(chan struct{})
	go func() { NewResolver(home).Email(home); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("include fan-out was not bounded")
	}
}

func TestWorktreeAndCommonDir(t *testing.T) {
	base, home := setup(t, map[string]string{
		"home/.gitconfig":                            emailCfg("global@example.test"),
		"home/work/main/.git/config":                 emailCfg("main@example.test"),
		"home/work/main/.git/worktrees/wt/commondir": "../..\n",
		"home/work/wt/sub/f":                         "x",
	})
	gitDir := filepath.Join(base, "home", "work", "main", ".git", "worktrees", "wt")
	write(t, filepath.Join(base, "home", "work", "wt", ".git"), "gitdir: "+gitDir+"\n")

	if got, ok := NewResolver(home).Email(filepath.Join(base, "home", "work", "wt", "sub")); !ok || got != "main@example.test" {
		t.Fatalf("worktree with commondir: (%q, %v)", got, ok)
	}

	// A relative gitdir pointer, as written for submodules.
	write(t, filepath.Join(base, "home", "work", "sm", ".git"), "gitdir: ../main/.git/worktrees/wt\n")
	if got, ok := NewResolver(home).Email(filepath.Join(base, "home", "work", "sm")); !ok || got != "main@example.test" {
		t.Fatalf("relative gitdir pointer: (%q, %v)", got, ok)
	}
}

func TestWorktreeGitdirCondition(t *testing.T) {
	base, home := setup(t, map[string]string{
		"home/.gitconfig":                            "[includeIf \"gitdir:~/work/\"]\n\tpath = w.inc\n",
		"home/w.inc":                                 emailCfg("work@example.test"),
		"home/work/main/.git/config":                 "[core]\n",
		"home/work/main/.git/worktrees/wt/commondir": "../..\n",
		"elsewhere/wt/f":                             "x",
	})
	gitDir := filepath.Join(base, "home", "work", "main", ".git", "worktrees", "wt")
	write(t, filepath.Join(base, "elsewhere", "wt", ".git"), "gitdir: "+gitDir+"\n")
	// The condition is matched against the git directory, not the checkout location.
	if got, ok := NewResolver(home).Email(filepath.Join(base, "elsewhere", "wt")); !ok || got != "work@example.test" {
		t.Fatalf("(%q, %v)", got, ok)
	}
}

func TestSymlinks(t *testing.T) {
	t.Run("target inside home is followed", func(t *testing.T) {
		_, home := setup(t, map[string]string{"home/dotfiles/gitconfig": emailCfg("dot@example.test")})
		link(t, filepath.Join(home, "dotfiles", "gitconfig"), filepath.Join(home, ".gitconfig"))
		if got, ok := NewResolver(home).Email(home); !ok || got != "dot@example.test" {
			t.Fatalf("(%q, %v)", got, ok)
		}
	})
	t.Run("target outside home is refused", func(t *testing.T) {
		base, home := setup(t, map[string]string{"outside/gitconfig": emailCfg("out@example.test")})
		link(t, filepath.Join(base, "outside", "gitconfig"), filepath.Join(home, ".gitconfig"))
		if got, ok := NewResolver(home).Email(home); ok {
			t.Fatalf("symlink out of home was read: %q", got)
		}
	})
	t.Run("repository config symlink out of home is refused", func(t *testing.T) {
		base, home := setup(t, map[string]string{"outside/gitconfig": emailCfg("out@example.test"), "home/r/.git/HEAD": "x"})
		link(t, filepath.Join(base, "outside", "gitconfig"), filepath.Join(home, "r", ".git", "config"))
		if got, ok := NewResolver(home).Email(filepath.Join(home, "r")); ok {
			t.Fatalf("symlink out of home was read: %q", got)
		}
	})
	t.Run("include symlink out of home is refused", func(t *testing.T) {
		base, home := setup(t, map[string]string{
			"outside/inc":     emailCfg("out@example.test"),
			"home/.gitconfig": "[include]\n\tpath = inc\n",
		})
		link(t, filepath.Join(base, "outside", "inc"), filepath.Join(home, "inc"))
		if got, ok := NewResolver(home).Email(home); ok {
			t.Fatalf("symlink out of home was read: %q", got)
		}
	})
	t.Run("dangling and looping symlinks", func(t *testing.T) {
		_, home := setup(t, nil)
		link(t, filepath.Join(home, "missing"), filepath.Join(home, ".gitconfig"))
		if _, ok := NewResolver(home).Email(home); ok {
			t.Fatal("dangling symlink produced an address")
		}
		loop := filepath.Join(home, ".config", "git", "config")
		if err := os.MkdirAll(filepath.Dir(loop), 0o755); err != nil {
			t.Fatal(err)
		}
		link(t, loop, loop)
		if _, ok := NewResolver(home).Email(home); ok {
			t.Fatal("symlink loop produced an address")
		}
	})
	t.Run("directory in place of a file", func(t *testing.T) {
		_, home := setup(t, nil)
		if err := os.MkdirAll(filepath.Join(home, ".gitconfig"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, ok := NewResolver(home).Email(home); ok {
			t.Fatal("directory produced an address")
		}
	})
}

func link(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
}

func TestCache(t *testing.T) {
	base, home := setup(t, map[string]string{"home/.gitconfig": emailCfg("one@example.test"), "home/r/.git/config": "[core]\n"})
	dir := filepath.Join(base, "home", "r")
	now := time.Now()
	r := NewResolver(home)
	r.now = func() time.Time { return now }

	if got, _ := r.Email(dir); got != "one@example.test" {
		t.Fatalf("first = %q", got)
	}

	// Same size and a restored mtime would hide the change; a different size is what the stamp notices.
	cfg := filepath.Join(home, ".gitconfig")
	write(t, cfg, emailCfg("two-longer@example.test"))
	if got, _ := r.Email(dir); got != "two-longer@example.test" {
		t.Fatalf("changed file not noticed: %q", got)
	}

	// A file that appears later (the local config gets an e-mail) also invalidates.
	write(t, filepath.Join(dir, ".git", "config"), emailCfg("local@example.test"))
	if got, _ := r.Email(dir); got != "local@example.test" {
		t.Fatalf("changed local config not noticed: %q", got)
	}

	// Within the TTL an unchanged answer is served from the cache; after it the files are read again.
	r.mu.Lock()
	n := len(r.cache)
	r.mu.Unlock()
	if n != 1 {
		t.Fatalf("cache entries = %d, want 1", n)
	}
	now = now.Add(cacheTTL - time.Second)
	if got, _ := r.Email(dir); got != "local@example.test" {
		t.Fatalf("within ttl = %q", got)
	}
	r.mu.Lock()
	at := r.cache[dir+"\x00"+filepath.Join(dir, ".git")].at
	r.mu.Unlock()
	if at.Equal(now) {
		t.Fatal("entry was refreshed within the TTL")
	}
	now = now.Add(2 * time.Second)
	if got, _ := r.Email(dir); got != "local@example.test" {
		t.Fatalf("after ttl = %q", got)
	}
	r.mu.Lock()
	at = r.cache[dir+"\x00"+filepath.Join(dir, ".git")].at
	r.mu.Unlock()
	if !at.Equal(now) {
		t.Fatal("entry was not refreshed after the TTL")
	}
}

func TestCacheIsBounded(t *testing.T) {
	_, home := setup(t, nil)
	r := NewResolver(home)
	for i := 0; i < maxCacheEntries+5; i++ {
		d := filepath.Join(home, fmt.Sprintf("r%d", i))
		write(t, filepath.Join(d, ".git", "config"), "[core]\n")
		r.Email(d)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) > maxCacheEntries {
		t.Fatalf("cache entries = %d", len(r.cache))
	}
}

func TestEmailNeverPanics(t *testing.T) {
	r := &Resolver{} // zero value: nil clock and cache
	if _, ok := r.Email("/nonexistent/dir"); ok {
		t.Fatal("unexpected ok")
	}
	if _, ok := NewResolver("").Email(""); ok {
		t.Fatal("unexpected ok with empty home")
	}
}

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern, path string
		want          bool
	}{
		{"/a/b/**", "/a/b/c/.git", true},
		{"/a/b/**", "/a/b/c/d/e/.git", true},
		{"/a/b/**", "/a/bc/.git", false},
		{"/a/*/.git", "/a/x/.git", true},
		{"/a/*/.git", "/a/x/y/.git", false},
		{"/a/**/.git", "/a/.git", true},
		{"/a/**/.git", "/a/x/y/.git", true},
		{"**/repo/**", "/home/u/repo/.git", true},
		{"**/repo/**", "/home/u/myrepo/.git", false},
		{"/a/re?o/.git", "/a/repo/.git", true},
		{"/a/re?o/.git", "/a/re/o/.git", false},
		{"/a/x**y/.git", "/a/x/y/.git", false},
		{"/a/x**y/.git", "/a/xzzy/.git", true},
		{"C:/Users/u/work/**", "C:/Users/u/work/r/.git", true},
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/bb", false},
		{"/a/**", "/a", false},
		{"", "", true},
		{strings.Repeat("*", maxPatternLen+1), "x", false},
	}
	for _, tt := range tests {
		if got := globMatch(tt.pattern, tt.path); got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func TestParseValue(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{" plain ", "plain", true},
		{`"  spaced  "`, "  spaced  ", true},
		{`a "b c" d`, "a b c d", true},
		{`x # c`, "x", true},
		{`x ; c`, "x", true},
		{`"x # not a comment"`, "x # not a comment", true},
		{`a\"b`, `a"b`, true},
		{`a\\b`, `a\b`, true},
		{`a\tb`, "a\tb", true},
		{`a\`, "", false},
		{`a\q`, "", false},
		{`"open`, "", false},
		{``, "", true},
	}
	for _, tt := range tests {
		got, ok := parseValue(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseValue(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestParseHeader(t *testing.T) {
	tests := []struct {
		in           string
		section, sub string
		rest         string
		ok           bool
	}{
		{"[user]", "user", "", "", true},
		{"[User]  # c", "user", "", "# c", true},
		{`[includeIf "gitdir:~/w/"]`, "includeif", "gitdir:~/w/", "", true},
		{`[remote "or\"igin"]`, "remote", `or"igin`, "", true},
		{"[user] email = a", "user", "", "email = a", true},
		{"[user", "", "", "", false},
		{`[a "b]`, "", "", "", false},
		{"[]", "", "", "", false},
	}
	for _, tt := range tests {
		section, sub, rest, ok := parseHeader(tt.in)
		if section != tt.section || sub != tt.sub || rest != tt.rest || ok != tt.ok {
			t.Errorf("parseHeader(%q) = (%q, %q, %q, %v)", tt.in, section, sub, rest, ok)
		}
	}
}
