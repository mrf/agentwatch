// Package gemini provides the Gemini CLI source for agentwatch.
//
// A GeminiSource discovers sessions by scanning a directory of Gemini CLI
// session directories (typically ~/.gemini/tmp). Each session directory
// contains a checkpoint.json file that Gemini rewrites in full on every
// update — unlike Claude, which appends to JSONL files.
//
// Because Gemini rewrites files rather than appending, the source uses file
// mtime as its cursor instead of a byte offset. The cursor also encodes
// previous message and tool-call totals so that deltas can be computed
// correctly across rewrites.
//
// Use New with WithRoot to configure the session root directory. Process
// scanning for hash-to-working-dir mapping is source-private in v1.
package gemini

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mrf/agentwatch/internal/filewatch"
	"github.com/mrf/agentwatch/source"
)

// checkpointFile is the name of the session file written by Gemini CLI.
const checkpointFile = "checkpoint.json"

// Option configures a GeminiSource.
type Option func(*GeminiSource)

// WithRoot sets the root directory to scan for Gemini CLI session directories.
// Discover walks this directory one level deep, treating each subdirectory as
// a session keyed by its directory name (the Gemini session hash).
// If root is empty, Discover returns no sessions.
func WithRoot(path string) Option {
	return func(s *GeminiSource) {
		s.root = path
	}
}

// GeminiSource discovers and parses Gemini CLI session checkpoint files.
// The zero value is usable but discovers no sessions until configured with
// a non-empty root via WithRoot.
type GeminiSource struct {
	root   string
	walker *filewatch.Walker
}

// New returns a new GeminiSource configured by opts.
func New(opts ...Option) (source.Source, error) {
	s := &GeminiSource{}
	for i := 0; i < len(opts); i++ {
		opts[i](s)
	}
	if s.root != "" {
		s.walker = filewatch.New([]string{s.root}, filewatch.WithPattern(checkpointFile))
	}
	return s, nil
}

// Register adds a GeminiSource constructor to r under the name "gemini".
func Register(r *source.Registry, opts ...Option) error {
	return r.Register("gemini", func() (source.Source, error) {
		return New(opts...)
	})
}

// Name returns "gemini".
func (s *GeminiSource) Name() string { return "gemini" }

// Discover scans the configured root for Gemini CLI session directories.
// Each subdirectory that contains a checkpoint.json is returned as a
// SessionHandle. The session ID is the subdirectory name (the Gemini hash).
// WorkingDir is populated via process inspection when a matching gemini
// process is found; otherwise it is left empty.
func (s *GeminiSource) Discover(ctx context.Context) ([]source.SessionHandle, error) {
	if s.walker == nil {
		return nil, nil
	}

	entries, err := s.walker.Discover(ctx)
	if err != nil {
		return nil, err
	}

	// Build hash → workingDir map by inspecting running Gemini processes.
	hashToDir := scanGeminiProcesses()

	handles := make([]source.SessionHandle, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		// Each checkpoint.json lives one level below root: root/<hash>/checkpoint.json
		sessionDir := filepath.Dir(e.Path)
		hash := filepath.Base(sessionDir)

		handles = append(handles, source.SessionHandle{
			ID:         hash,
			Path:       e.Path,
			WorkingDir: hashToDir[hash],
			StartedAt:  e.ModTime,
			Source:     "gemini",
		})
	}
	return handles, nil
}

// Parse reads the session's checkpoint.json and returns a SourceUpdate.
//
// Because Gemini rewrites the file on every update, the cursor encodes both
// the file mtime and the message/tool counts observed on the previous poll.
// If the mtime has not changed since the last cursor, no new data is returned.
// When the file is rewritten, Parse re-reads the entire file and returns only
// the delta relative to what the cursor recorded.
func (s *GeminiSource) Parse(ctx context.Context, h source.SessionHandle, c source.Cursor) (source.SourceUpdate, source.Cursor, error) {
	if ctx.Err() != nil {
		return source.SourceUpdate{}, c, ctx.Err()
	}

	fi, err := os.Stat(h.Path)
	if err != nil {
		return source.SourceUpdate{}, c, err
	}

	currentMtime := fi.ModTime().UnixNano()
	prevMtime, prevMsgs, prevTools := decodeCursor(c)

	if prevMtime == currentMtime {
		// File has not changed since last poll.
		return source.SourceUpdate{}, c, nil
	}

	data, err := os.ReadFile(h.Path)
	if err != nil {
		return source.SourceUpdate{}, c, err
	}

	r, err := parseCheckpoint(data)
	if err != nil {
		// Advance the cursor past the bad file so we do not retry it
		// forever. Log-worthy but not fatal — the monitor health system
		// will surface repeated parse failures.
		nextCursor := encodeCursor(currentMtime, prevMsgs, prevTools)
		return source.SourceUpdate{}, nextCursor, fmt.Errorf("gemini: parse %s: %w", h.Path, err)
	}

	totalMsgs := r.totalUserMsgs + r.totalModelMsgs
	msgDelta := totalMsgs - prevMsgs
	toolDelta := r.totalToolCalls - prevTools

	// Clamp deltas to zero in case of a rollback or inconsistency.
	if msgDelta < 0 {
		msgDelta = 0
	}
	if toolDelta < 0 {
		toolDelta = 0
	}

	nextCursor := encodeCursor(currentMtime, totalMsgs, r.totalToolCalls)

	update := buildUpdate(r, h.ID, h.WorkingDir, msgDelta, toolDelta)

	return update, nextCursor, nil
}

// encodeCursor serialises mtime nanoseconds, message count, and tool count
// into an opaque cursor string: "<mtime_ns>,<msg_count>,<tool_count>".
func encodeCursor(mtimeNS int64, msgs, tools int) source.Cursor {
	return source.Cursor(fmt.Sprintf("%d,%d,%d", mtimeNS, msgs, tools))
}

// decodeCursor parses the cursor produced by encodeCursor.
// An empty or malformed cursor returns zeros, causing a full re-parse.
func decodeCursor(c source.Cursor) (mtimeNS int64, msgs, tools int) {
	if c == "" {
		return 0, 0, 0
	}
	parts := strings.SplitN(string(c), ",", 3)
	if len(parts) != 3 {
		return 0, 0, 0
	}
	mtime, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, 0
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0
	}
	t, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0, 0, 0
	}
	return mtime, m, t
}
