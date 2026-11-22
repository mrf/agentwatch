//go:build unix

package jsonl

import (
	"os"
	"syscall"
)

// hasFileID reports whether fileID can identify files on this platform.
const hasFileID = true

// fileID returns the device and inode of the file info describes.
func fileID(info os.FileInfo) (dev, ino uint64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true //nolint:unconvert // Dev and Ino are not uint64 on every unix
}
