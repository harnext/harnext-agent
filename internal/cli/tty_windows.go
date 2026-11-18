//go:build windows

package cli

import (
	"io"
	"os"
)

// openTTY opens the console input; standard input can be a pipe when the agent is started from a script.
func openTTY() (io.ReadCloser, error) {
	return os.Open("CONIN$")
}
