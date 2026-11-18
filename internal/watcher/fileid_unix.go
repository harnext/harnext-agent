//go:build unix

package watcher

import (
	"os"
	"syscall"
)

func platformFileID(fi os.FileInfo) FileID {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return FileID{Dev: uint64(st.Dev), Ino: uint64(st.Ino)}
	}
	return FileID{}
}
