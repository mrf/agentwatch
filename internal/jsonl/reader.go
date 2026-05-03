package jsonl

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrFileTooLarge is returned when the file size exceeds the configured cap.
var ErrFileTooLarge = errors.New("jsonl: file exceeds size cap")

// ErrLineTooLong is returned when a complete line exceeds the configured cap.
var ErrLineTooLong = errors.New("jsonl: line exceeds size cap")

const (
	// DefaultMaxFileSize is the default file-size cap (100 MiB).
	DefaultMaxFileSize = 100 * 1024 * 1024

	// DefaultMaxLineSize is the default per-line cap (1 MiB).
	DefaultMaxLineSize = 1 * 1024 * 1024
)

// Options configures the JSONL reader.
type Options struct {
	// MaxFileSize is the maximum file size in bytes before ReadLines returns
	// ErrFileTooLarge. Zero uses DefaultMaxFileSize.
	MaxFileSize int64

	// MaxLineSize is the maximum byte length of a single line (excluding the
	// newline terminator) before ReadLines returns ErrLineTooLong. Zero uses
	// DefaultMaxLineSize.
	MaxLineSize int
}

func (o Options) fileSize() int64 {
	if o.MaxFileSize <= 0 {
		return DefaultMaxFileSize
	}
	return o.MaxFileSize
}

func (o Options) lineSize() int {
	if o.MaxLineSize <= 0 {
		return DefaultMaxLineSize
	}
	return o.MaxLineSize
}

// ReadLines reads complete newline-terminated lines from the file at path
// starting at the given byte offset. It returns the lines (without their
// newline terminators) and the byte offset immediately after the last complete
// line consumed.
//
// A line is "complete" only if it ends with a newline character. Any trailing
// bytes after the last newline are not returned and are not reflected in
// nextOffset, making ReadLines safe for cursor-based incremental tailing: the
// caller can re-invoke with the returned nextOffset once more data is written.
//
// ReadLines returns ErrFileTooLarge when os.Stat reports a size greater than
// opts.MaxFileSize. ReadLines returns ErrLineTooLong when any complete line is
// longer than opts.MaxLineSize bytes.
//
// Binary garbage in lines is returned as-is; callers are responsible for JSON
// validation.
func ReadLines(path string, offset int64, opts Options) (lines [][]byte, nextOffset int64, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, offset, err
	}
	if fi.Size() > opts.fileSize() {
		return nil, offset, fmt.Errorf("%w: size %d > cap %d", ErrFileTooLarge, fi.Size(), opts.fileSize())
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer func() { _ = f.Close() }()

	if offset > 0 {
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			return nil, offset, fmt.Errorf("jsonl: seek to offset %d: %w", offset, err)
		}
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, offset, fmt.Errorf("jsonl: read %s: %w", path, err)
	}

	if len(data) == 0 {
		return nil, offset, nil
	}

	maxLine := opts.lineSize()
	currentOffset := offset
	remaining := data

	for len(remaining) > 0 {
		idx := bytes.IndexByte(remaining, '\n')
		if idx == -1 {
			// No newline found: remaining bytes are a partial final line.
			break
		}

		lineBytes := remaining[:idx]
		if len(lineBytes) > maxLine {
			return nil, offset, fmt.Errorf("%w: length %d > cap %d", ErrLineTooLong, len(lineBytes), maxLine)
		}

		lines = append(lines, bytes.Clone(lineBytes))
		currentOffset += int64(idx + 1)
		remaining = remaining[idx+1:]
	}

	return lines, currentOffset, nil
}
