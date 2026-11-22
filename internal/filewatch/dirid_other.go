//go:build !unix

package filewatch

import (
	"io/fs"
	"path/filepath"
)

// dirID uniquely identifies a directory by its fully resolved path on
// platforms without stable (device, inode) pairs exposed through os.FileInfo.
type dirID struct {
	resolved string
}

func identify(path string, _ fs.FileInfo) (dirID, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return dirID{}, false
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return dirID{}, false
	}
	return dirID{resolved: abs}, true
}
