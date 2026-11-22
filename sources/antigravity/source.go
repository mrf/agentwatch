// Package antigravity provides the Antigravity CLI source for agentwatch.
//
// An AntigravitySource discovers sessions by scanning a directory of
// Antigravity CLI session files (typically ~/.antigravitycli). Each session
// is stored as a <uuid>.json file that Antigravity rewrites in full on every
// update — unlike Claude, which appends to JSONL files.
//
// Because Antigravity rewrites files rather than appending, the source uses
// file mtime as its cursor instead of a byte offset. The cursor also encodes
// previous message and tool-call totals so that deltas can be computed
// correctly across rewrites.
//
// Use New with WithRoot to configure the session root directory. Process
// scanning for working directory detection is source-private in v1.
package antigravity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mrf/agentwatch/internal/filewatch"
	"github.com/mrf/agentwatch/source"
)

// lastConversationFile is the file that tracks the most recent session UUID.
const lastConversationFile = "last-conversation-id"

// procCacheTTL bounds how stale the /proc scan used for working-directory
// detection may be. Discover runs every poll; walking /proc that often is
// wasted work for data that changes only when agy starts or exits.
const procCacheTTL = 5 * time.Second

// sessionIDPattern matches the canonical UUID stem of a session file.
var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Option configures an AntigravitySource.
type Option func(*AntigravitySource)

// WithRoot sets the root directory to scan for Antigravity CLI session files.
// Discover walks this directory for <uuid>.json files, treating each file as
// a session keyed by its filename stem (the Antigravity session UUID). Other
// JSON files under the root are ignored.
// If root is empty, Discover returns no sessions.
func WithRoot(path string) Option {
	return func(s *AntigravitySource) {
		s.root = path
	}
}

// WithDiscoverWindow limits discovery to session files whose modification
// time is within d of the current time. Zero disables age filtering.
func WithDiscoverWindow(d time.Duration) Option {
	return func(s *AntigravitySource) {
		s.window = d
	}
}

// AntigravitySource discovers and parses Antigravity CLI session files.
// The zero value is usable but discovers no sessions until configured with
// a non-empty root via WithRoot.
type AntigravitySource struct {
	root   string
	window time.Duration
	walker *filewatch.Walker

	now       func() time.Time
	scanProcs func() []string

	procMu    sync.Mutex
	procCwds  []string
	procAt    time.Time
	procFresh bool
}

// New returns a new AntigravitySource configured by opts.
func New(opts ...Option) (source.Source, error) {
	s := &AntigravitySource{now: time.Now, scanProcs: scanProcFS}
	for i := 0; i < len(opts); i++ {
		opts[i](s)
	}
	if s.root != "" {
		wopts := []filewatch.Option{
			filewatch.WithPattern("*.json"),
		}
		if s.window > 0 {
			wopts = append(wopts, filewatch.WithMaxAge(s.window))
		}
		s.walker = filewatch.New([]string{s.root}, wopts...)
	}
	return s, nil
}

// Register adds an AntigravitySource constructor to r under the name "antigravity".
func Register(r *source.Registry, opts ...Option) error {
	return r.Register("antigravity", func() (source.Source, error) {
		return New(opts...)
	})
}

// Name returns "antigravity".
func (s *AntigravitySource) Name() string { return "antigravity" }

// Discover scans the configured root for Antigravity CLI session files.
// Each <uuid>.json file is returned as a SessionHandle. The session ID is the
// filename stem (the UUID). WorkingDir is populated via process inspection
// when a matching agy process is found; otherwise it is left empty.
func (s *AntigravitySource) Discover(ctx context.Context) ([]source.SessionHandle, error) {
	if s.walker == nil {
		return nil, nil
	}

	entries, err := s.walker.Discover(ctx)
	if err != nil {
		return nil, err
	}

	handles := make([]source.SessionHandle, 0, len(entries))
	var uuidToDir map[string]string
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		sessionID := strings.TrimSuffix(filepath.Base(e.Path), ".json")
		if !sessionIDPattern.MatchString(sessionID) {
			continue
		}
		if uuidToDir == nil {
			uuidToDir = s.workingDirs()
		}

		handles = append(handles, source.SessionHandle{
			ID:         sessionID,
			Path:       e.Path,
			WorkingDir: uuidToDir[sessionID],
			StartedAt:  e.ModTime,
			Source:     "antigravity",
		})
	}
	if len(handles) == 0 {
		return nil, nil
	}
	return handles, nil
}

// workingDirs maps the active session UUID to its working directory. The
// mapping is only unambiguous when exactly one agy process is running.
func (s *AntigravitySource) workingDirs() map[string]string {
	lastID := readLastConversationID(s.root)
	if lastID == "" {
		return nil
	}
	cwds := s.procWorkingDirs()
	if len(cwds) != 1 {
		return nil
	}
	return map[string]string{lastID: cwds[0]}
}

// procWorkingDirs returns the working directories of running agy processes,
// rescanning /proc at most once per procCacheTTL.
func (s *AntigravitySource) procWorkingDirs() []string {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	now := s.now()
	if !s.procFresh || now.Sub(s.procAt) >= procCacheTTL {
		s.procCwds = s.scanProcs()
		s.procAt = now
		s.procFresh = true
	}
	return s.procCwds
}

// Parse reads the session's JSON file and returns a SourceUpdate.
//
// Because Antigravity rewrites the file on every update, the cursor encodes
// both the file mtime and the message/tool counts observed on the previous
// poll. If the mtime has not changed since the last cursor, no new data is
// returned. When the file is rewritten, Parse re-reads the entire file and
// returns only the delta relative to what the cursor recorded.
func (s *AntigravitySource) Parse(ctx context.Context, h source.SessionHandle, c source.Cursor) (source.SourceUpdate, source.Cursor, error) {
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

	r, err := parseSession(data)
	if err != nil {
		// Advance the cursor past the bad file so we do not retry it
		// forever. Log-worthy but not fatal — the monitor health system
		// will surface repeated parse failures.
		nextCursor := encodeCursor(currentMtime, prevMsgs, prevTools)
		return source.SourceUpdate{}, nextCursor, fmt.Errorf("antigravity: parse %s: %w", h.Path, err)
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
