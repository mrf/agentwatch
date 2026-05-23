// Package pi provides the Pi coding agent source for agentwatch.
//
// Pi (github.com/earendil-works/pi) stores sessions as JSONL files at:
//
//	<sessions_dir>/--<path>--/<timestamp>_<uuid>.jsonl
//
// where <path> is the working directory with "/" replaced by "-".
// The sessions directory defaults to ~/.pi/agent/sessions but can be
// overridden by passing it via WithRoot. Consumers that want environment
// fallback can implement it themselves:
//
//	sessDir := os.Getenv("PI_CODING_AGENT_SESSION_DIR")
//	if sessDir == "" {
//	    base := os.Getenv("PI_CODING_AGENT_DIR")
//	    if base == "" {
//	        home, _ := os.UserHomeDir()
//	        base = filepath.Join(home, ".pi", "agent")
//	    }
//	    sessDir = filepath.Join(base, "sessions")
//	}
//	src := pi.New(pi.WithRoot(sessDir))
package pi

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mrf/agentwatch/internal/filewatch"
	"github.com/mrf/agentwatch/internal/jsonl"
	"github.com/mrf/agentwatch/source"
)

// Source implements source.Source for Pi coding agent sessions.
//
// Pi stores sessions under a root sessions directory in per-project
// subdirectories named --<encoded-path>--. Each session is a JSONL file
// named <timestamp>_<uuid>.jsonl.
type Source struct {
	root   string
	window time.Duration
	walker *filewatch.Walker
}

// Option configures a Source.
type Option func(*Source)

// WithRoot sets the Pi sessions directory (the directory containing encoded-path
// subdirectories such as --home-user-project--). Without this option the source
// discovers no sessions.
func WithRoot(path string) Option {
	return func(s *Source) {
		s.root = path
	}
}

// WithDiscoverWindow limits discovery to session files whose modification time
// is within d of the current time. Zero disables age filtering.
func WithDiscoverWindow(d time.Duration) Option {
	return func(s *Source) {
		s.window = d
	}
}

// New creates a new Pi source with the given options.
func New(opts ...Option) *Source {
	s := &Source{}
	for _, o := range opts {
		o(s)
	}
	s.buildWalker()
	return s
}

func (s *Source) buildWalker() {
	if s.root == "" {
		return
	}
	wopts := []filewatch.Option{
		filewatch.WithPattern("*.jsonl"),
	}
	if s.window > 0 {
		wopts = append(wopts, filewatch.WithMaxAge(s.window))
	}
	s.walker = filewatch.New([]string{s.root}, wopts...)
}

// Register registers this source with a source.Registry using a factory that
// calls New(opts...).
func Register(r *source.Registry, opts ...Option) error {
	return r.Register("pi", func() (source.Source, error) {
		return New(opts...), nil
	})
}

// Name implements source.Source.
func (s *Source) Name() string { return "pi" }

// Discover implements source.Source. It walks the configured root for Pi session
// JSONL files and returns a handle for each one found.
func (s *Source) Discover(ctx context.Context) ([]source.SessionHandle, error) {
	if s.walker == nil {
		return nil, nil
	}

	entries, err := s.walker.Discover(ctx)
	if err != nil {
		return nil, err
	}

	handles := make([]source.SessionHandle, 0, len(entries))
	for _, e := range entries {
		name := filepath.Base(e.Path)
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		sessionID := sessionIDFromFilename(name)
		handles = append(handles, source.SessionHandle{
			ID:        sessionID,
			Path:      e.Path,
			StartedAt: e.ModTime, // refined by parser on first Parse call
			Source:    "pi",
		})
	}
	return handles, nil
}

// Parse implements source.Source. The cursor is the byte offset into the
// session file as a decimal string. An empty cursor means start from zero.
func (s *Source) Parse(ctx context.Context, h source.SessionHandle, c source.Cursor) (source.SourceUpdate, source.Cursor, error) {
	var offset int64
	if c != "" {
		var err error
		offset, err = strconv.ParseInt(string(c), 10, 64)
		if err != nil {
			return source.SourceUpdate{}, c, fmt.Errorf("pi: invalid cursor %q: %w", c, err)
		}
	}

	lines, nextOffset, err := jsonl.ReadLines(h.Path, offset, jsonl.Options{})
	if err != nil {
		return source.SourceUpdate{}, c, fmt.Errorf("pi: read %s: %w", h.Path, err)
	}

	isFirst := offset == 0
	var acc accumulator
	for i := 0; i < len(lines); i++ {
		parseLine(lines[i], isFirst && i == 0, &acc)
	}

	update := acc.toUpdate()
	next := source.Cursor(strconv.FormatInt(nextOffset, 10))
	return update, next, nil
}

// sessionIDFromFilename extracts the session UUID from a Pi session filename.
//
// Pi session filenames have the form: <timestamp>_<uuid>.jsonl
// The UUID is the part after the last underscore.
func sessionIDFromFilename(name string) string {
	stem := strings.TrimSuffix(name, ".jsonl")
	if idx := strings.LastIndex(stem, "_"); idx >= 0 {
		return stem[idx+1:]
	}
	return stem
}
