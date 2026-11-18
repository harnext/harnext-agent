//go:build !unix && !windows

package cli

import (
	"errors"
	"time"
)

func terminate(int, time.Duration, func() bool) error {
	return errors.New("stop is supported on macOS and Linux only")
}
