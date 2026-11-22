package jsonl

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"strconv"
	"strings"
)

// maxHeadLen caps how many leading bytes of a file a Position hashes.
const maxHeadLen = 4096

// Position is a resumable read position in a file: a byte offset plus enough
// identity to tell whether the file at a path is still the one the offset was
// taken from. The zero value reads from the start of the file.
//
// The identity is the device and inode where the platform exposes them, and
// a hash of the file's first HeadLen bytes. Only bytes before Offset are
// hashed, so appends never change it.
type Position struct {
	Offset  int64  `json:"offset,omitempty"`
	Dev     uint64 `json:"dev,omitempty"`
	Ino     uint64 `json:"ino,omitempty"`
	HeadLen int    `json:"headLen,omitempty"`
	HeadSum uint64 `json:"headSum,omitempty"`
}

// Encode returns p as a string for use in an opaque source cursor.
func (p Position) Encode() string {
	data, err := json.Marshal(p)
	if err != nil {
		// Unreachable for a struct of integers; a bare offset still decodes.
		return strconv.FormatInt(p.Offset, 10)
	}
	return string(data)
}

// ParsePosition decodes a string produced by Encode. It also accepts the
// empty string (the zero Position) and a bare decimal offset, the encoding
// cursors used before Position existed; such a legacy position has no
// identity, so ReadFrom checks it by file size only.
func ParsePosition(s string) (Position, error) {
	var p Position
	switch {
	case s == "":
		return p, nil
	case strings.HasPrefix(s, "{"):
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			return Position{}, fmt.Errorf("jsonl: decode position: %w", err)
		}
	default:
		offset, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return Position{}, fmt.Errorf("jsonl: decode position: %w", err)
		}
		p.Offset = offset
	}
	if err := p.Validate(); err != nil {
		return Position{}, err
	}
	return p, nil
}

// Validate reports an error if p could not have come from ReadFrom.
func (p Position) Validate() error {
	if p.Offset < 0 || p.HeadLen < 0 || p.HeadLen > maxHeadLen || int64(p.HeadLen) > p.Offset {
		return fmt.Errorf("jsonl: invalid position %+v", p)
	}
	return nil
}

// ReadFrom is Read resuming from pos, after checking that the file at path is
// still the one pos was taken from: it must be at least pos.Offset bytes long,
// have the same device and inode where the platform exposes them, and start
// with the bytes pos hashed. When a check fails the file was truncated,
// replaced or rewritten, so ReadFrom reads from the beginning instead and
// reports reset; the caller must then discard whatever it accumulated from
// the old file. A position with an offset but no identity is checked by size
// only.
//
// next is the position to resume from on the following call. On error, next
// is pos and reset is false.
func ReadFrom(path string, pos Position, opts Options) (res Result, next Position, reset bool, err error) {
	f, info, err := openRegular(path)
	if err != nil {
		return Result{NextOffset: pos.Offset}, pos, false, err
	}
	defer func() { _ = f.Close() }()

	dev, ino, hasID := fileID(info)
	start := pos
	ok, err := pos.matches(f, info.Size(), dev, ino, hasID)
	if err != nil {
		return Result{NextOffset: pos.Offset}, pos, false, fmt.Errorf("jsonl: verify %s: %w", path, err)
	}
	if !ok {
		start, reset = Position{}, true
	}

	res, err = readOpen(f, path, start.Offset, opts)
	if err != nil {
		return Result{NextOffset: pos.Offset}, pos, false, err
	}

	next = Position{Offset: res.NextOffset}
	if hasID {
		next.Dev, next.Ino = dev, ino
	}
	switch headLen := int(min(res.NextOffset, maxHeadLen)); {
	case headLen == start.HeadLen:
		next.HeadLen, next.HeadSum = start.HeadLen, start.HeadSum
	case headLen > 0:
		sum, complete, err := headSum(f, headLen)
		if err != nil {
			return Result{NextOffset: pos.Offset}, pos, false, fmt.Errorf("jsonl: hash %s: %w", path, err)
		}
		if complete {
			next.HeadLen, next.HeadSum = headLen, sum
		}
	}
	return res, next, reset, nil
}

// matches reports whether the open file f is the file p was taken from.
func (p Position) matches(f *os.File, size int64, dev, ino uint64, hasID bool) (bool, error) {
	if p.Offset > size {
		return false, nil
	}
	if hasID && p.Ino != 0 && (p.Dev != dev || p.Ino != ino) {
		return false, nil
	}
	if p.HeadLen == 0 {
		return true, nil
	}
	sum, complete, err := headSum(f, p.HeadLen)
	if err != nil {
		return false, err
	}
	return complete && sum == p.HeadSum, nil
}

// headSum hashes the first n bytes of f. complete is false when f holds
// fewer than n bytes.
func headSum(f *os.File, n int) (sum uint64, complete bool, err error) {
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, 0); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, false, nil
		}
		return 0, false, err
	}
	h := fnv.New64a()
	_, _ = h.Write(buf) // hash.Hash writes never fail
	return h.Sum64(), true, nil
}
