//go:build !windows

package selfupdate

import (
	"errors"
	"syscall"
)

// Restart replaces the running process with the (new) binary at path, keeping the process id so a service manager keeps
// supervising it. The caller has already released the instance lock and flushed its state.
func Restart(path string, args []string) error {
	if len(args) == 0 {
		args = []string{path}
	}
	if err := syscall.Exec(path, args, syscallEnviron()); err != nil {
		return errors.New("selfupdate: restart failed")
	}
	return nil
}
