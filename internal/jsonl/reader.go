package jsonl

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	// DefaultMaxReadBytes is the default per-call read budget (16 MiB).
	DefaultMaxReadBytes = 16 * 1024 * 1024

	// DefaultMaxLineSize is the default per-line cap (1 MiB).
	DefaultMaxLineSize = 1 * 1024 * 1024

	readBufferSize = 64 * 1024
)

// ErrNotRegular is returned by Read when the path is not a regular file (a
// directory, FIFO, socket, or device).
var ErrNotRegular = errors.New("jsonl: not a regular file")

// requireRegular passes through a stat error, or returns ErrNotRegular when
// info describes anything other than a regular file.
func requireRegular(info os.FileInfo, err error) error {
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: mode %s", ErrNotRegular, info.Mode().Type())
	}
	return nil
}

// Options configures the JSONL reader.
type Options struct {
	// MaxReadBytes is the per-call read budget in bytes. Read stops starting
	// new lines once it has consumed this many bytes, so a single call returns
	// at most roughly this much data and the caller continues from NextOffset
	// on its next poll. A line that begins inside the budget is always read to
	// its end, so one call may overshoot by up to one line. Zero uses
	// DefaultMaxReadBytes.
	MaxReadBytes int64

	// MaxLineSize is the maximum byte length of a single line (excluding the
	// newline terminator). Longer complete lines are skipped and counted in
	// Result.Skipped. Zero uses DefaultMaxLineSize.
	MaxLineSize int
}

func (o Options) readBudget() int64 {
	if o.MaxReadBytes <= 0 {
		return DefaultMaxReadBytes
	}
	return o.MaxReadBytes
}

func (o Options) lineSize() int {
	if o.MaxLineSize <= 0 {
		return DefaultMaxLineSize
	}
	return o.MaxLineSize
}

// Result is the outcome of a single Read call.
type Result struct {
	// Lines are the complete lines read, without newline terminators.
	Lines [][]byte

	// NextOffset is the byte offset immediately after the last complete line
	// consumed (returned or skipped).
	NextOffset int64

	// Skipped is the number of complete lines that exceeded MaxLineSize and
	// were passed over without being returned.
	Skipped int
}

// Read reads complete newline-terminated lines from the file at path starting
// at the given byte offset.
//
// A line is "complete" only if it ends with a newline character. Any trailing
// bytes after the last newline are not returned and are not reflected in
// NextOffset, making Read safe for cursor-based incremental tailing: the
// caller can re-invoke with NextOffset once more data is written. This holds
// for oversized trailing lines too — an unterminated line is never skipped,
// because it may still be growing.
//
// A complete line longer than opts.MaxLineSize is skipped: NextOffset advances
// past it and Result.Skipped is incremented, so one bad line cannot wedge a
// cursor. Skipped lines are streamed and never held in memory.
//
// Read consumes at most about opts.MaxReadBytes per call (see Options); a file
// of any total size is therefore consumed incrementally across calls. Callers
// must treat NextOffset > offset with zero Lines as progress (all consumed
// lines were skipped).
//
// Read returns ErrNotRegular, without opening the path, if it is not a regular
// file.
//
// Binary garbage in lines is returned as-is; callers are responsible for JSON
// validation.
func Read(path string, offset int64, opts Options) (Result, error) {
	fail := Result{NextOffset: offset}

	// Stat before opening: open(2) on a FIFO blocks until a writer appears,
	// which would stall the caller's whole poll.
	if err := requireRegular(os.Stat(path)); err != nil {
		return fail, err
	}

	f, err := os.Open(path)
	if err != nil {
		return fail, err
	}
	defer func() { _ = f.Close() }()

	// Re-check the opened file in case the path was swapped after the Stat.
	if err := requireRegular(f.Stat()); err != nil {
		return fail, err
	}

	if offset > 0 {
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			return fail, fmt.Errorf("jsonl: seek to offset %d: %w", offset, err)
		}
	}

	var (
		br       = bufio.NewReaderSize(f, readBufferSize)
		maxLine  = opts.lineSize()
		budget   = opts.readBudget()
		res      = Result{NextOffset: offset}
		consumed int64
	)

	for consumed < budget {
		line, n, complete, err := readLine(br, maxLine)
		if err != nil {
			return fail, fmt.Errorf("jsonl: read %s: %w", path, err)
		}
		if !complete {
			break
		}
		consumed += n
		res.NextOffset += n
		if line == nil {
			res.Skipped++
			continue
		}
		res.Lines = append(res.Lines, line)
	}

	return res, nil
}

// ReadLines is Read without the skipped-line count. It returns the lines and
// the byte offset immediately after the last complete line consumed.
func ReadLines(path string, offset int64, opts Options) (lines [][]byte, nextOffset int64, err error) {
	res, err := Read(path, offset, opts)
	if err != nil {
		return nil, offset, err
	}
	return res.Lines, res.NextOffset, nil
}

// readLine reads one line from br. n is the number of bytes consumed
// including the newline. complete is false when the input ended before a
// newline (a partial line). For a complete line longer than maxLine, line is
// nil and the line has been discarded; otherwise line is a fresh copy of the
// content without the newline.
func readLine(br *bufio.Reader, maxLine int) (line []byte, n int64, complete bool, err error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		n += int64(len(chunk))
		// Keep accumulating only while the line can still fit (content plus
		// its newline); past that we only count bytes.
		if n <= int64(maxLine)+1 {
			buf = append(buf, chunk...)
		}
		switch {
		case err == nil:
			if n > int64(maxLine)+1 {
				return nil, n, true, nil
			}
			return buf[:len(buf)-1], n, true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return nil, n, false, nil
		default:
			return nil, n, false, err
		}
	}
}
