package copilotid

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var errInvalid = errors.New("copilotid: invalid data")

const (
	keyActive = "github.copilot-github"
	keyPolicy = "defaultAccount.cachedPolicyData"
	keySKU    = "extensionsAssignmentFilterProvider.copilotSku"
	maxDBSize = 512 << 20
)

// DefaultDirs are the globalStorage directories of VS Code (stable and Insiders) under the user config directory.
func DefaultDirs() []string {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return nil
	}
	var out []string
	for _, edition := range []string{"Code", "Code - Insiders"} {
		out = append(out, filepath.Join(cfg, edition, "User", "globalStorage"))
	}
	return out
}

type stamp struct {
	size, walSize int64
	mod, walMod   time.Time
}

// Reader reads the state database of each directory. An unchanged database is not read again.
type Reader struct {
	Dirs []string

	mu    sync.Mutex
	stamp map[string]stamp
	last  map[string]State
}

// Read merges the editions: the first one with an active account provides it; known accounts are the union. ok is false when
// no state database could be read.
func (r *Reader) Read() (State, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stamp == nil {
		r.stamp, r.last = map[string]stamp{}, map[string]State{}
	}
	var out State
	seen := map[string]bool{}
	any := false
	for _, dir := range r.Dirs {
		path := filepath.Join(dir, "state.vscdb")
		st, ok := r.readOne(path)
		if !ok {
			continue
		}
		any = true
		if out.Active == "" && st.Active != "" {
			out.Active, out.ActiveID, out.Plan = st.Active, st.ActiveID, st.Plan
		}
		for _, l := range st.Known {
			if !seen[l] {
				seen[l] = true
				out.Known = append(out.Known, l)
			}
		}
	}
	return out, any
}

func fileStamp(path string) (size int64, mod time.Time, ok bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return 0, time.Time{}, false
	}
	return fi.Size(), fi.ModTime(), true
}

func (r *Reader) readOne(path string) (State, bool) {
	size, mod, ok := fileStamp(path)
	if !ok || size > maxDBSize {
		return State{}, false
	}
	wsize, wmod, _ := fileStamp(path + "-wal")
	cur := stamp{size, wsize, mod, wmod}
	if r.stamp[path] == cur {
		if st, ok := r.last[path]; ok {
			return st, true
		}
	}
	st, err := readDB(path)
	if err != nil {
		return State{}, false
	}
	r.stamp[path], r.last[path] = cur, st
	return st, true
}

// readDB opens a private copy of the database (and its write-ahead log, which holds the newest values) so the editor's
// files are never locked or written.
func readDB(path string) (State, error) {
	dir, err := os.MkdirTemp("", "harnext-vscdb-")
	if err != nil {
		return State{}, err
	}
	defer os.RemoveAll(dir)
	copyPath := filepath.Join(dir, "state.vscdb")
	if err := copyFile(path, copyPath); err != nil {
		return State{}, err
	}
	if _, err := os.Lstat(path + "-wal"); err == nil {
		_ = copyFile(path+"-wal", copyPath+"-wal")
	}
	db, err := sql.Open("sqlite", sqliteURI(copyPath))
	if err != nil {
		return State{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	return queryState(db)
}

// sqliteURI is a read-only file URI that is also right for Windows drive paths and for characters that need escaping.
func sqliteURI(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro"}
	return u.String()
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(in, maxDBSize)); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func queryState(db *sql.DB) (State, error) {
	rows, err := db.Query(`SELECT key, value FROM ItemTable WHERE key IN (?, ?, ?) OR (key LIKE 'github-%-usages')`, keyActive, keyPolicy, keySKU)
	if err != nil {
		return State{}, err
	}
	defer rows.Close()
	var st State
	var sku string
	for rows.Next() {
		var key string
		var val []byte
		if err := rows.Scan(&key, &val); err != nil {
			return State{}, err
		}
		if len(val) > maxValueSize {
			continue
		}
		switch {
		case key == keyActive:
			if l := strings.TrimSpace(string(val)); ValidLogin(l) {
				st.Active = l
			}
		case key == keyPolicy:
			var p struct {
				Account struct {
					ID string `json:"accountId"`
				} `json:"accountPolicyData"`
			}
			if json.Unmarshal(val, &p) == nil && isDigits(p.Account.ID) {
				st.ActiveID = p.Account.ID
			}
		case key == keySKU:
			sku = strings.TrimSpace(string(val))
		case strings.HasPrefix(key, "github-") && strings.HasSuffix(key, "-usages"):
			l := strings.TrimSuffix(strings.TrimPrefix(key, "github-"), "-usages")
			if ValidLogin(l) && strings.Contains(string(val), "github.copilot-chat") {
				st.Known = append(st.Known, l)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return State{}, err
	}
	st.Plan = PlanFromSKU(sku)
	if st.Active != "" {
		found := false
		for _, l := range st.Known {
			found = found || l == st.Active
		}
		if !found {
			st.Known = append(st.Known, st.Active)
		}
	} else {
		// The id and plan belong to the active account only.
		st.ActiveID, st.Plan = "", ""
	}
	return st, rows.Err()
}

func isDigits(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
