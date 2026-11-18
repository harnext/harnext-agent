//go:build windows

package cli

import (
	"errors"
	osexec "os/exec"
	"strconv"
	"time"

	"github.com/harnext/harnext-agent/internal/svcmgr"
)

const gracefulWait = 3 * time.Second

func waitGone(wait time.Duration, stillRunning func() bool) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !stillRunning() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !stillRunning()
}

// terminate stops the scheduled task, then falls back to taskkill: gentle first, /F only on timeout.
func terminate(pid int, wait time.Duration, stillRunning func() bool) error {
	// Fails harmlessly when no task exists or the agent runs in the foreground.
	_ = osexec.Command("schtasks", "/End", "/TN", svcmgr.TaskName).Run()
	if waitGone(gracefulWait, stillRunning) {
		return nil
	}
	p := strconv.Itoa(pid)
	_ = osexec.Command("taskkill", "/PID", p).Run()
	if waitGone(gracefulWait, stillRunning) {
		return nil
	}
	if err := osexec.Command("taskkill", "/F", "/PID", p).Run(); err != nil {
		return errors.New("could not stop the agent process (different user?)")
	}
	if !waitGone(wait, stillRunning) {
		return errors.New("the agent did not stop in time")
	}
	return nil
}
