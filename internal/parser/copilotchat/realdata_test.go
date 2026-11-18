//go:build realdata

package copilotchat

import (
	"bufio"
	"os"
	"testing"

	"github.com/harnext/harnext-agent/internal/analyzer"
)

type countingExtractor struct{ calls int }

func (c *countingExtractor) UserFeatures(text string) analyzer.UserFeatures {
	c.calls++
	return analyzer.UserFeatures{Chars: len([]rune(text))}
}
func (c *countingExtractor) Settings() analyzer.Settings { return analyzer.Settings{} }

// Reads a real chatSessions file (HARNEXT_REAL_COPILOT_CHAT) and logs only counts.
func TestRealChatSession(t *testing.T) {
	path := os.Getenv("HARNEXT_REAL_COPILOT_CHAT")
	if path == "" {
		t.Skip("HARNEXT_REAL_COPILOT_CHAT not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Skip("file not readable")
	}
	defer f.Close()

	ex := &countingExtractor{}
	var starts, states, tools, ignored, skipped int
	states2 := map[int]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), MaxLineBytes)
	lines := 0
	for sc.Scan() {
		lines++
		Parse(sc.Bytes(), ex, func(ev Event) {
			switch e := ev.(type) {
			case RequestStart:
				starts++
				if e.HasState {
					states2[e.State]++
				}
			case RequestState:
				states++
				states2[e.State]++
			case RequestTools:
				tools += e.ToolCount
			case Ignored:
				ignored++
			case Skipped:
				skipped++
			}
		})
	}
	t.Logf("lines=%d starts=%d state_events=%d tool_names=%d ignored=%d skipped=%d analyzed_calls=%d states=%v", lines, starts, states, tools, ignored, skipped, ex.calls, states2)
	if starts == 0 {
		t.Error("no requests parsed")
	}
}
