package service

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/harnext/harnext-agent/internal/aggregate"
	"github.com/harnext/harnext-agent/internal/analyzer"
	"github.com/harnext/harnext-agent/internal/parser/copilotchat"
	"github.com/harnext/harnext-agent/internal/watcher"
)

const (
	workspaceJSONMax   = 4 << 10
	folderReadAttempts = 3
	chatCacheCap       = 4096
)

var windowsDrivePath = regexp.MustCompile(`^/[A-Za-z]:/`)

type chatExHolder struct{ ex analyzer.Extractor }

type folderEntry struct {
	folder   string
	attempts int
	final    bool
}

// chatSource feeds Copilot chat session lines into the aggregator. HandleLine calls are
// serialized by the watcher; the extractor is swapped concurrently when the configuration changes.
type chatSource struct {
	agg *aggregate.CopilotAggregator
	ex  atomic.Pointer[chatExHolder]

	// unanalyzed counts lines read while no extractor was set (friction needs the text, which is not kept).
	unanalyzed atomic.Int64

	mu      sync.Mutex
	dirs    map[string]*folderEntry
	applied map[string]struct{}
}

var _ watcher.ChatSink = (*chatSource)(nil)

func newChatSource(agg *aggregate.CopilotAggregator) *chatSource {
	return &chatSource{agg: agg, dirs: map[string]*folderEntry{}, applied: map[string]struct{}{}}
}

// setExtractor replaces the extractor; nil switches content analysis off.
func (c *chatSource) setExtractor(ex analyzer.Extractor) {
	if c == nil {
		return
	}
	if ex == nil {
		c.ex.Store(nil)
		return
	}
	c.ex.Store(&chatExHolder{ex: ex})
}

func (c *chatSource) HandleLine(f watcher.ChatFile, line []byte, tooLong bool) {
	c.applyFolder(f)
	if tooLong {
		c.agg.AddChat(f.SessionID, copilotchat.Skipped{Reason: copilotchat.ReasonLineTooLong})
		return
	}
	var ex analyzer.Extractor
	if h := c.ex.Load(); h != nil {
		ex = h.ex
	} else {
		c.unanalyzed.Add(1)
	}
	copilotchat.Parse(line, ex, func(ev copilotchat.Event) { c.agg.AddChat(f.SessionID, ev) })
}

func (c *chatSource) applyFolder(f watcher.ChatFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.applied[f.SessionID]; ok {
		return
	}
	e := c.dirs[f.WorkspaceDir]
	if e == nil {
		if len(c.dirs) >= chatCacheCap {
			c.dirs = map[string]*folderEntry{}
		}
		e = &folderEntry{}
		c.dirs[f.WorkspaceDir] = e
	}
	if !e.final && e.folder == "" && e.attempts < folderReadAttempts {
		e.attempts++
		folder, definitive := readWorkspaceFolder(filepath.Join(f.WorkspaceDir, "workspace.json"))
		e.folder, e.final = folder, definitive || folder != ""
	}
	if e.folder == "" {
		return
	}
	if len(c.applied) >= chatCacheCap {
		c.applied = map[string]struct{}{}
	}
	c.applied[f.SessionID] = struct{}{}
	c.agg.SetFolder(f.SessionID, e.folder)
}

// readWorkspaceFolder returns the local folder of a workspace.json; definitive is true when
// retrying cannot help (unusable content, remote or multi-root workspace).
func readWorkspaceFolder(path string) (folder string, definitive bool) {
	data, err := readLimited(path, workspaceJSONMax)
	if err != nil {
		return "", err.Error() == "too large"
	}
	var w struct {
		Folder string `json:"folder"`
	}
	if json.Unmarshal(data, &w) != nil {
		return "", true
	}
	return folderFromURI(w.Folder), true
}

// folderFromURI accepts only local file:// URIs and returns an absolute path.
func folderFromURI(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") {
		return ""
	}
	p := u.Path
	switch {
	case windowsDrivePath.MatchString(p):
		p = p[1:]
	case !strings.HasPrefix(p, "/"):
		return ""
	}
	if strings.ContainsRune(p, 0) {
		return ""
	}
	return p
}
