// Package dirpath extracts the FOLDER part of a file path for the request-based folder split
// . It is the only place where a path is split: the file name is dropped here and never
// returned, so no parser event can carry it. The product owner allowed reading the folder part locally (2026-11-15).
package dirpath

import "strings"

// MaxLen bounds a path; longer ones are ignored.
const MaxLen = 1024

// Dir returns the folder part of an absolute file path (no trailing separator; "/" stays "/"), or "" when the path has no
// folder part, is relative, too long or contains control characters.
func Dir(path string) string {
	if path == "" || len(path) > MaxLen {
		return ""
	}
	for i := 0; i < len(path); i++ {
		if path[i] < 0x20 {
			return ""
		}
	}
	i := strings.LastIndexAny(path, `/\`)
	if i < 0 {
		return ""
	}
	dir := path[:i]
	if dir == "" {
		dir = "/"
	}
	if !IsAbs(dir) {
		return ""
	}
	return dir
}

// IsAbs: POSIX "/x", Windows "C:\x" / "C:/x" or UNC "\\host\x".
func IsAbs(p string) bool {
	switch {
	case strings.HasPrefix(p, "/"), strings.HasPrefix(p, `\\`):
		return true
	case len(p) >= 2 && p[1] == ':' && (p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z'):
		return true
	}
	return false
}

// Hash is FNV-1a of the whole path. It only deduplicates repeated mentions of the same file inside one request; the path itself
// cannot be recovered from it.
func Hash(path string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(path); i++ {
		h ^= uint64(path[i])
		h *= 1099511628211
	}
	return h
}
