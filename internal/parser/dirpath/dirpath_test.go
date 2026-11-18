package dirpath

import (
	"strings"
	"testing"
)

func TestDir(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/home/dev/proj/pkg/foo_test.go", "/home/dev/proj/pkg"},
		{"/f.go", "/"},
		{`C:\dev\My.Project\Main.GO`, `C:\dev\My.Project`},
		{"c:/dev/x.go", "c:/dev"},
		{`\\srv\share/p\a.go`, `\\srv\share/p`},
		{"src/App.test.TSX", ""}, // relative
		{"README.md", ""},
		{"", ""},
		{"/a\x00b/c.go", ""},
		{"/" + strings.Repeat("d", MaxLen) + "/f.go", ""},
		{"/a/b/", "/a/b"}, // a trailing separator means the "file name" is empty; the folder part is the path itself
	}
	for _, tt := range tests {
		if got := Dir(tt.in); got != tt.want {
			t.Errorf("Dir(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDirNeverReturnsTheFileName(t *testing.T) {
	for _, p := range []string{"/work/proj/SECRET-FILE-NAME.go", `C:\work\SECRET-FILE-NAME.go`, "/SECRET-FILE-NAME"} {
		if strings.Contains(Dir(p), "SECRET-FILE-NAME") {
			t.Errorf("Dir(%q) leaks the file name", p)
		}
	}
}

func TestHashDistinguishesFilesOfOneFolder(t *testing.T) {
	if Hash("/a/b.go") == Hash("/a/c.go") || Hash("/a/b.go") != Hash("/a/b.go") {
		t.Fatal("hash must separate files and be stable")
	}
}
