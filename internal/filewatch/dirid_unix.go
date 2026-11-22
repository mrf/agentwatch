//go:build unix

package filewatch

import (
	"io/fs"
	"syscall"
)

// dirID uniquely identifies a directory on disk, independent of the path used
// to reach it.
type dirID struct {
	dev, ino uint64
}

// identify returns the (device, inode) identity of the directory described by
// info (which must come from os.Stat, not os.Lstat). The path is unused on
// unix; it exists for the portable fallback.
func identify(_ string, info fs.FileInfo) (dirID, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return dirID{}, false
	}
	// Dev is int32 on darwin and uint32/uint64 elsewhere; widen explicitly.
	return dirID{dev: uint64(st.Dev), ino: uint64(st.Ino)}, true //nolint:unconvert // width differs per GOOS
}
