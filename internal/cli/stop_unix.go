//go:build unix

package cli

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// terminate sends SIGTERM and waits until stillRunning reports false.
func terminate(pid int, wait time.Duration, stillRunning func() bool) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return errors.New("could not find the agent process")
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return errors.New("could not signal the agent process (different user?)")
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !stillRunning() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("the agent did not stop in time")
}
