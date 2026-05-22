// Package cursor provides the Cursor IDE agent source for agentwatch.
//
// # Root resolution
//
// This source does NOT read any environment variable. Callers must supply the
// base directory explicitly via WithRoot. If no WithRoot is provided the source
// returns no sessions. A caller that wants environment fallback can implement
// it itself:
//
//	home, _ := os.UserHomeDir()
//	src := cursor.New(cursor.WithRoot(filepath.Join(home, ".cursor")))
package cursor

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

// Source implements source.Source for Cursor IDE agent sessions.
//
// Cursor stores agent transcripts at:
//
//	<root>/projects/<encoded-name>/agent-transcripts/<id>.jsonl       (flat)
//	<root>/projects/<encoded-name>/agent-transcripts/<id>/<id>.jsonl  (nested)
//
// The project directory name is a dash-encoded representation of the project
// folder path (path separators replaced by dashes).
type Source struct {
	root   string
	window time.Duration
	walker *filewatch.Walker
}

// Option configures a Source.
type Option func(*Source)

// WithRoot sets the Cursor data directory (the directory containing
// "projects/"). Without this option the source discovers no sessions.
func WithRoot(path string) Option {
	return func(s *Source) {
		s.root = path
	}
}

// WithDiscoverWindow limits discovery to transcript files whose modification
// time is within d of the current time. Zero disables age filtering.
func WithDiscoverWindow(d time.Duration) Option {
	return func(s *Source) {
		s.window = d
	}
}

// New creates a new Cursor source with the given options.
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
	projectsDir := filepath.Join(s.root, "projects")
	wopts := []filewatch.Option{
		filewatch.WithPattern("*.jsonl"),
	}
	if s.window > 0 {
		wopts = append(wopts, filewatch.WithMaxAge(s.window))
	}
	s.walker = filewatch.New([]string{projectsDir}, wopts...)
}

// Register registers this source with a source.Registry using a factory that
// calls New(opts...).
func Register(r *source.Registry, opts ...Option) error {
	return r.Register("cursor", func() (source.Source, error) {
		return New(opts...), nil
	})
}

// Name implements source.Source.
func (s *Source) Name() string { return "cursor" }

// Discover implements source.Source. It walks <root>/projects for agent
// transcript JSONL files in both flat and nested layouts, and returns a handle
// for each.
func (s *Source) Discover(ctx context.Context) ([]source.SessionHandle, error) {
	if s.walker == nil {
		return nil, nil
	}

	entries, err := s.walker.Discover(ctx)
	if err != nil {
		return nil, err
	}

	projectsDir := filepath.Join(s.root, "projects")
	handles := make([]source.SessionHandle, 0, len(entries))
	for _, e := range entries {
		sessionID, workingDir, ok := parseTranscriptPath(e.Path, projectsDir)
		if !ok {
			continue
		}
		handles = append(handles, source.SessionHandle{
			ID:         sessionID,
			Path:       e.Path,
			WorkingDir: workingDir,
			StartedAt:  e.ModTime,
			Source:     "cursor",
		})
	}
	return handles, nil
}

// Parse implements source.Source. The cursor is the byte offset into the
// transcript file as a decimal string. An empty cursor means start from zero.
func (s *Source) Parse(ctx context.Context, h source.SessionHandle, c source.Cursor) (source.SourceUpdate, source.Cursor, error) {
	var offset int64
	if c != "" {
		var err error
		offset, err = strconv.ParseInt(string(c), 10, 64)
		if err != nil {
			return source.SourceUpdate{}, c, fmt.Errorf("cursor: invalid cursor %q: %w", c, err)
		}
	}

	lines, nextOffset, err := jsonl.ReadLines(h.Path, offset, jsonl.Options{})
	if err != nil {
		return source.SourceUpdate{}, c, fmt.Errorf("cursor: read %s: %w", h.Path, err)
	}

	var acc accumulator
	for i := 0; i < len(lines); i++ {
		parseLine(lines[i], &acc)
	}

	update := acc.toUpdate()
	update.WorkingDir = h.WorkingDir
	update.SessionID = h.ID
	next := source.Cursor(strconv.FormatInt(nextOffset, 10))
	return update, next, nil
}

// parseTranscriptPath extracts the session ID and working directory from a
// transcript file path. It returns false if the path is not a valid transcript.
//
// Valid layouts:
//
//	<projectsDir>/<encoded-name>/agent-transcripts/<id>.jsonl       (flat)
//	<projectsDir>/<encoded-name>/agent-transcripts/<id>/<id>.jsonl  (nested)
func parseTranscriptPath(path, projectsDir string) (sessionID, workingDir string, ok bool) {
	rel, err := filepath.Rel(projectsDir, path)
	if err != nil {
		return "", "", false
	}

	// rel is one of:
	//   <encoded-name>/agent-transcripts/<id>.jsonl         (flat, 3 parts)
	//   <encoded-name>/agent-transcripts/<id>/<id>.jsonl    (nested, 4 parts)
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 3 {
		return "", "", false
	}

	encodedName := parts[0]
	if parts[1] != "agent-transcripts" {
		return "", "", false
	}

	name := filepath.Base(path)
	if !strings.HasSuffix(name, ".jsonl") {
		return "", "", false
	}
	sessionID = strings.TrimSuffix(name, ".jsonl")
	workingDir = decodeProjectName(encodedName)
	return sessionID, workingDir, true
}

// decodeProjectName converts a Cursor dash-encoded project directory name back
// to a filesystem path. Cursor replaces path separators with dashes.
//
// Example: "Users-john-Code-myapp" → "/Users/john/Code/myapp"
//
// This is a best-effort decode; project names containing dashes are
// indistinguishable from path separators in this encoding.
func decodeProjectName(encoded string) string {
	if encoded == "" {
		return ""
	}
	return "/" + strings.ReplaceAll(encoded, "-", "/")
}
