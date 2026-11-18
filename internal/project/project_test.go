package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoName(t *testing.T) {
	cases := map[string]string{
		"git@github.com:acme/acme-bank-api.git":                     "acme/acme-bank-api",
		"https://github.com/acme/acme-bank-api.git":                 "acme/acme-bank-api",
		"https://user:ghp_SECRETTOKEN@github.com/acme/api.git":      "acme/api",
		"https://oauth2:glpat-xyz@gitlab.example.test/a/b/c/repo":   "c/repo",
		"ssh://git@git.example.test:2222/team/svc.git":              "team/svc",
		"https://dev.azure.com/org/proj/_git/repo?version=GBmain#x": "proj/repo",
		"https://github.com/acme/api/":                              "acme/api",
		"/srv/git/acme/api.git":                                     "acme/api",
		"single":                                                    "single",
		"":                                                          "",
	}
	for in, want := range cases {
		if got := RepoName(in); got != want {
			t.Errorf("RepoName(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(RepoName(in), "SECRET") || strings.Contains(RepoName(in), "glpat") {
			t.Errorf("RepoName(%q) leaked a credential", in)
		}
	}
}

func writeRepo(t *testing.T, root, config string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolve(t *testing.T) {
	home := t.TempDir()
	r := NewResolver(home)

	repo := filepath.Join(home, "clients", "acme-bank", "api")
	writeRepo(t, repo, "[core]\n\tbare = false\n[remote \"upstream\"]\n\turl = git@github.com:other/fork.git\n[remote \"origin\"]\n\turl = https://u:tok@github.com/acme/acme-bank-api.git\n")
	sub := filepath.Join(repo, "services", "billing")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := r.Resolve(repo); got != (Identity{Project: "acme/acme-bank-api"}) {
		t.Errorf("root: %+v", got)
	}
	if got := r.Resolve(sub); got != (Identity{Project: "acme/acme-bank-api", Path: "services/billing"}) {
		t.Errorf("sub: %+v", got)
	}

	noRemote := filepath.Join(home, "work", "scratch")
	writeRepo(t, noRemote, "[core]\n\tbare = false\n")
	if got := r.Resolve(noRemote); got != (Identity{Project: "~/work/scratch"}) {
		t.Errorf("no remote: %+v", got)
	}

	plain := filepath.Join(home, "notes")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := r.Resolve(plain); got != (Identity{Project: "~/notes"}) {
		t.Errorf("outside git: %+v", got)
	}
	if got := r.Resolve(home); got != (Identity{Project: "~"}) {
		t.Errorf("home: %+v", got)
	}
	if got := r.Resolve(""); got != (Identity{Project: Unknown}) {
		t.Errorf("empty: %+v", got)
	}
	for _, d := range []string{repo, sub, noRemote, plain} {
		if strings.Contains(r.Resolve(d).Project, home) || strings.Contains(r.Resolve(d).Path, home) {
			t.Errorf("%s: home directory leaked", d)
		}
	}
}

func TestResolveWorktree(t *testing.T) {
	home := t.TempDir()
	r := NewResolver(home)
	main := filepath.Join(home, "main")
	writeRepo(t, main, "[remote \"origin\"]\n\turl = git@github.com:acme/main.git\n")
	wtGit := filepath.Join(main, ".git", "worktrees", "feature")
	if err := os.MkdirAll(wtGit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtGit, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(home, "feature-wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := r.Resolve(wt); got != (Identity{Project: "acme/main"}) {
		t.Errorf("worktree: %+v", got)
	}
}

func TestResolveMarkerRootOutsideGit(t *testing.T) {
	home := t.TempDir()
	r := NewResolver(home)
	mk := func(parts ...string) string {
		d := filepath.Join(append([]string{home}, parts...)...)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	touch := func(dir, name string) {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A folder with AI tool settings is a project; its sub folders group under it.
	site := mk("Projects", "site")
	if err := os.Mkdir(filepath.Join(site, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	ref := mk("Projects", "site", "design", "reference")
	if got := r.Resolve(site); got != (Identity{Project: "~/Projects/site"}) {
		t.Errorf("marker root: %+v", got)
	}
	if got := r.Resolve(ref); got != (Identity{Project: "~/Projects/site", Path: "design/reference"}) {
		t.Errorf("marker sub folder: %+v", got)
	}

	// A manifest marks the root too (an app without git and without AI settings).
	app := mk("Downloads", "app")
	touch(app, "package.json")
	tabs := mk("Downloads", "app", "app", "(tabs)")
	if got := r.Resolve(tabs); got != (Identity{Project: "~/Downloads/app", Path: "app/(tabs)"}) {
		t.Errorf("manifest sub folder: %+v", got)
	}

	// The nearest marker wins: a nested package is its own project outside git.
	pkg := mk("Projects", "site", "packages", "ui")
	touch(pkg, "package.json")
	mk("Projects", "site", "packages", "ui", "src")
	if got := r.Resolve(filepath.Join(pkg, "src")); got != (Identity{Project: "~/Projects/site/packages/ui", Path: "src"}) {
		t.Errorf("nested package: %+v", got)
	}

	// No marker anywhere: the folder itself, as before. A marker in the home directory never captures projects below it.
	touch(home, "package.json")
	loose := mk("scratch", "a")
	if got := r.Resolve(loose); got != (Identity{Project: "~/scratch/a"}) {
		t.Errorf("no marker: %+v", got)
	}
	if got := r.Resolve(home); got != (Identity{Project: "~"}) {
		t.Errorf("home with marker: %+v", got)
	}

	// Git still wins over markers.
	repo := mk("Projects", "repo")
	writeRepo(t, repo, "[remote \"origin\"]\n\turl = git@github.com:acme/repo.git\n")
	touch(repo, "package.json")
	if got := r.Resolve(mk("Projects", "repo", "web")); got != (Identity{Project: "acme/repo", Path: "web"}) {
		t.Errorf("git over marker: %+v", got)
	}
	for _, d := range []string{site, ref, app, tabs, loose} {
		id := r.Resolve(d)
		if strings.Contains(id.Project, home) || strings.Contains(id.Path, home) {
			t.Errorf("%s: home directory leaked: %+v", d, id)
		}
	}
}
