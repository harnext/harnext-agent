package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Fake secrets sit in every place a real one could: prompts, replies, tool input and output, project files, the git remote, the
// branch, the user's directory name and a file name. Nothing of them may leave the machine, in any upload or in the files the
// agent writes about itself. The values are invented; this test never uses a real secret.
var fakeSecrets = []string{
	"sk-FAKE-PROMPT-SECRET", "FAKE-ANSWER-SECRET", "FAKE-THINKING-SECRET", "FAKE-BASH-SECRET", "FAKE-TOOLRESULT-SECRET",
	"FAKE-ENV-SECRET", "FAKE-APPSETTINGS-SECRET", "FAKE-SOURCE-SECRET", "ghp_FAKEGITTOKEN", "fakegituser", "FAKEUSER-DIR",
	"FAKE-BRANCH-SECRET", "SECRET-FILE-NAME", "FAKE-COPILOT-PROMPT-SECRET", "FAKE-NEGATIVE-SECRET",
}

func TestNoFakeSecretLeavesTheMachine(t *testing.T) {
	h := newChatHarness(t, true)

	repo := filepath.Join(t.TempDir(), "FAKEUSER-DIR", "work", "app")
	for _, d := range []string{".git", "src"} {
		if err := os.MkdirAll(filepath.Join(repo, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(".git", "config"):             "[remote \"origin\"]\n\turl = https://fakegituser:ghp_FAKEGITTOKEN@github.com/acme/app.git\n",
		".env":                                      "API_KEY=FAKE-ENV-SECRET\n",
		"appsettings.json":                          `{"ConnectionString":"Server=FAKE-APPSETTINGS-SECRET"}`,
		filepath.Join("src", "main.ts"):             "const k = 'FAKE-SOURCE-SECRET'\n",
		filepath.Join("src", "SECRET-FILE-NAME.ts"): "// FAKE-SOURCE-SECRET\n",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(repo, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	const sid = "claude-privacy-1"
	user := func(i int, content string) string {
		return fmt.Sprintf(`{"type":"user","sessionId":"%s","uuid":"u%d","cwd":%q,"gitBranch":"feature/FAKE-BRANCH-SECRET","timestamp":"2026-01-01T10:%02d:00.000Z","message":{"role":"user","content":%s}}`,
			sid, i, repo, i, content)
	}
	assistant := func(i int, content, usage string) string {
		return fmt.Sprintf(`{"type":"assistant","sessionId":"%s","requestId":"r%d","uuid":"a%d","cwd":%q,"gitBranch":"feature/FAKE-BRANCH-SECRET","timestamp":"2026-01-01T10:%02d:30.000Z","message":{"id":"m%d","model":"claude-sonnet-5","content":%s,"usage":%s}}`,
			sid, i, i, repo, i, i, content, usage)
	}
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	toolOut := `[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"FAKE-TOOLRESULT-SECRET command failed"}]`
	reply := fmt.Sprintf(`[{"type":"thinking","thinking":"FAKE-THINKING-SECRET"},{"type":"text","text":"FAKE-ANSWER-SECRET"},`+
		`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"export TOKEN=FAKE-BASH-SECRET"}},`+
		`{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":%q,"old_string":"FAKE-SOURCE-SECRET","new_string":"x"}}]`,
		filepath.Join(repo, "src", "SECRET-FILE-NAME.ts"))
	lines := []string{
		user(1, q("please use sk-FAKE-PROMPT-SECRET to deploy")),
		assistant(2, reply, `{"input_tokens":10,"output_tokens":100}`),
		user(3, toolOut),
		user(4, q("that is wrong, FAKE-NEGATIVE-SECRET is still failing")),
		assistant(5, `[{"type":"text","text":"FAKE-ANSWER-SECRET again"}]`, `{"input_tokens":10,"output_tokens":50}`),
	}
	root := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(filepath.Join(root, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	appendFile(t, filepath.Join(root, "proj", sid+".jsonl"), strings.Join(lines, "\n")+"\n")

	// Copilot: a chat session file with a fake prompt, and the telemetry file with its token counts.
	ws := filepath.Join(h.chatRoot, "hash0002")
	if err := os.MkdirAll(filepath.Join(ws, "chatSessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	folderURI := "file://" + filepath.ToSlash(repo)
	if !strings.HasPrefix(folderURI, "file:///") {
		folderURI = "file:///" + strings.TrimPrefix(folderURI, "file://")
	}
	if err := os.WriteFile(filepath.Join(ws, "workspace.json"), []byte(fmt.Sprintf(`{"folder":%q}`, folderURI)), 0o644); err != nil {
		t.Fatal(err)
	}
	const base = int64(1794258000000)
	chat := fmt.Sprintf(`{"kind":2,"k":["requests"],"i":0,"v":[{"requestId":"req-1","timestamp":%d,"modelId":"copilot/claude-test-1","message":{"text":"FAKE-COPILOT-PROMPT-SECRET"}}]}`, base) + "\n" +
		fmt.Sprintf(`{"kind":1,"k":["requests",0,"modelState"],"v":{"value":1,"completedAt":%d}}`, base+60000) + "\n"
	appendFile(t, filepath.Join(ws, "chatSessions", chatSessionID+".jsonl"), chat)
	appendFile(t, h.otel, fmt.Sprintf(`{"traceId":"cccc0000000000000000000000000001","spanId":"dddd000000000001","name":"chat claude-test-1","kind":2,`+
		`"startTime":[1794258000,0],"endTime":[1794258001,500000000],"attributes":{"gen_ai.operation.name":"chat","gen_ai.request.model":"claude-test-1",`+
		`"gen_ai.response.model":"claude-test-1","gen_ai.conversation.id":"%s","gen_ai.agent.name":"panel/editAgent",`+
		`"gen_ai.usage.input_tokens":100,"gen_ai.usage.output_tokens":10},"resource":{"attributes":{"service.version":"0.68.0"}}}`+"\n", chatSessionID))

	var logMu sync.Mutex
	var logs strings.Builder
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{Gate: testGate,
			Paths: h.paths, Version: "0.1.0", Root: root, ChatRoots: []string{h.chatRoot},
			FlushInterval: 30 * time.Millisecond, UploadPeriod: 50 * time.Millisecond, Keys: h.keys, Clock: frozenClock{h.at},
			Logf: func(f string, a ...any) { logMu.Lock(); fmt.Fprintf(&logs, f+"\n", a...); logMu.Unlock() },
		})
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not stop")
		}
	}()

	// Both tools must have uploaded, otherwise the check below would pass on an empty upload.
	eventually(t, "claude and copilot uploads", func() bool {
		var claude, copilot bool
		for _, b := range h.rec.all() {
			claude = claude || strings.Contains(b, `"session_external_id":"`+sid+`"`)
			copilot = copilot || strings.Contains(b, `"session_external_id":"`+chatSessionID+`"`)
		}
		return claude && copilot
	})
	eventually(t, "status file", func() bool {
		s, err := h.paths.ReadStatus()
		return err == nil && s.Upload.Accepted >= 2
	})

	leaks := append([]string{testToken, repo, h.otel, h.chatRoot, root}, fakeSecrets...)
	check := func(what, text string) {
		for _, bad := range leaks {
			if strings.Contains(text, bad) {
				t.Errorf("%s contains %q", what, bad)
			}
		}
	}
	identified := false
	for i, b := range h.rec.all() {
		check(fmt.Sprintf("upload %d", i), b)
		identified = identified || strings.Contains(b, `"project":"acme/app"`)
	}
	if !identified {
		t.Error("no upload carried the project identity acme/app")
	}
	if raw, err := os.ReadFile(h.paths.Status); err == nil {
		check("status file", string(raw))
	}
	logMu.Lock()
	check("log", logs.String())
	logMu.Unlock()
}
