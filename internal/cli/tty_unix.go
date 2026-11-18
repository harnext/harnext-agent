//go:build unix

package cli

import (
	"io"
	"os"
)

// openTTY opens the controlling terminal: with `curl | sh` standard input is a pipe, but the user can still be asked.
func openTTY() (io.ReadCloser, error) {
	return os.Open("/dev/tty")
}
