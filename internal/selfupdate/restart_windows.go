//go:build windows

package selfupdate

import (
	"errors"
	"os"
	"os/exec"
)

// Restart starts the (new) binary and ends this process: Windows cannot replace a process image in place.
func Restart(path string, args []string) error {
	if len(args) > 0 {
		args = args[1:]
	}
	cmd := exec.Command(path, args...)
	if err := cmd.Start(); err != nil {
		return errors.New("selfupdate: restart failed")
	}
	os.Exit(0)
	return nil
}
