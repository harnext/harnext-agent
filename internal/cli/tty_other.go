//go:build !unix && !windows

package cli

import (
	"errors"
	"io"
)

func openTTY() (io.ReadCloser, error) {
	return nil, errors.New("no terminal")
}
