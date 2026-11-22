//go:build !unix

package jsonl

import "os"

// hasFileID reports whether fileID can identify files on this platform.
const hasFileID = false

// fileID is unavailable here; Position falls back to size and head hash.
func fileID(os.FileInfo) (dev, ino uint64, ok bool) {
	return 0, 0, false
}
