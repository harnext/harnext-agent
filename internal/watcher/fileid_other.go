//go:build !unix

package watcher

import "os"

// Without an inode, truncation is detected by size only.
func platformFileID(os.FileInfo) FileID { return FileID{} }
