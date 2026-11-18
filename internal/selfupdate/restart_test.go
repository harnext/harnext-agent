//go:build !windows

package selfupdate

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The helper pattern: the test binary starts itself, the first life restarts into a second one, which proves that the
// process image is replaced (same process id) and that arguments and environment arrive.
func TestRestartReplacesTheProcessImage(t *testing.T) {
	switch os.Getenv("HARNEXT_RESTART_HELPER") {
	case "first":
		exe, _ := os.Executable()
		_ = os.Setenv("HARNEXT_RESTART_HELPER", "second")
		_ = Restart(exe, []string{exe, "-test.run=TestRestartReplacesTheProcessImage", "-test.v"})
		os.Stdout.WriteString("RESTART-FAILED\n")
		os.Exit(3)
	case "second":
		os.Stdout.WriteString("RESTARTED pid=" + strings.TrimSpace(itoa(os.Getpid())) + "\n")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRestartReplacesTheProcessImage", "-test.v")
	cmd.Env = append(os.Environ(), "HARNEXT_RESTART_HELPER=first")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "RESTARTED pid=") || strings.Contains(string(out), "RESTART-FAILED") {
		t.Fatalf("restart: %v\n%s", err, out)
	}
	if pid := itoa(cmd.Process.Pid); !strings.Contains(string(out), "pid="+pid) {
		t.Fatalf("the process id must survive the restart (want %s):\n%s", pid, out)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
