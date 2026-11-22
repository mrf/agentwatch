// Package antigravity provides the Antigravity CLI (agy) source for
// agentwatch.
//
// agy stores each conversation as a SQLite database, <uuid>.db, in its
// conversations directory (typically ~/.gemini/antigravity-cli/conversations
// as of agy 1.5.14). The database runs in WAL mode while agy has it open, so
// recent writes may live only in the <uuid>.db-wal sidecar. The source opens
// databases read-only and decodes only step metadata — never step payloads.
//
// The cursor is a fingerprint of the database and WAL file stats plus the
// message and tool-call totals seen on the previous poll, so unchanged
// conversations are skipped without opening them and deltas survive
// in-place step updates.
//
// Use New with WithRoot to configure the conversations directory.
package antigravity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mrf/agentwatch/internal/filewatch"
	"github.com/mrf/agentwatch/internal/sqlitero"
	"github.com/mrf/agentwatch/source"
)

// sessionIDPattern matches the canonical UUID stem of a conversation file.
var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Option configures an AntigravitySource.
type Option func(*AntigravitySource)

// WithRoot sets the agy conversations directory. Discover walks it for
// <uuid>.db files, treating each as a session keyed by the UUID. Other files
// are ignored. If root is empty, Discover returns no sessions.
func WithRoot(path string) Option {
	return func(s *AntigravitySource) {
		s.root = path
	}
}

// WithDiscoverWindow limits discovery to conversations written within d of
// the current time, counting writes still pending in the WAL. Zero disables
// age filtering.
func WithDiscoverWindow(d time.Duration) Option {
	return func(s *AntigravitySource) {
		s.window = d
	}
}

// AntigravitySource discovers and parses agy conversation databases.
// The zero value is usable but discovers no sessions until configured with
// a non-empty root via WithRoot.
type AntigravitySource struct {
	root   string
	window time.Duration
	walker *filewatch.Walker
	now    func() time.Time
}

// New returns a new AntigravitySource configured by opts.
func New(opts ...Option) (source.Source, error) {
	s := &AntigravitySource{now: time.Now}
	for i := 0; i < len(opts); i++ {
		opts[i](s)
	}
	if s.root != "" {
		s.walker = filewatch.New([]string{s.root}, filewatch.WithPattern("*.db"))
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

// Discover scans the configured root for <uuid>.db conversation files. The
// session ID is the UUID; WorkingDir is reported by Parse from the
// conversation's own metadata.
func (s *AntigravitySource) Discover(ctx context.Context) ([]source.SessionHandle, error) {
	if s.walker == nil {
		return nil, nil
	}

	// Age filtering happens here rather than in the walker: the .db mtime
	// lags while agy's writes sit uncheckpointed in the WAL.
	entries, err := s.walker.Discover(ctx)
	if err != nil {
		return nil, err
	}

	now := s.now()
	handles := make([]source.SessionHandle, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		sessionID := strings.TrimSuffix(filepath.Base(e.Path), ".db")
		if !sessionIDPattern.MatchString(sessionID) {
			continue
		}
		if s.window > 0 {
			last := e.ModTime
			if wal, err := os.Stat(e.Path + "-wal"); err == nil && wal.ModTime().After(last) {
				last = wal.ModTime()
			}
			if now.Sub(last) > s.window {
				continue
			}
		}
		handles = append(handles, source.SessionHandle{
			ID:        sessionID,
			Path:      e.Path,
			StartedAt: e.ModTime,
			Source:    "antigravity",
		})
	}
	if len(handles) == 0 {
		return nil, nil
	}
	return handles, nil
}

// Parse reads the conversation database and returns a SourceUpdate.
//
// If neither the database nor its WAL has changed since the cursor was
// taken, no data is returned. Otherwise the conversation is re-read and the
// message and tool-call deltas are computed against the cursor's totals.
func (s *AntigravitySource) Parse(ctx context.Context, h source.SessionHandle, c source.Cursor) (source.SourceUpdate, source.Cursor, error) {
	if ctx.Err() != nil {
		return source.SourceUpdate{}, c, ctx.Err()
	}

	fp, err := statFingerprint(h.Path)
	if err != nil {
		return source.SourceUpdate{}, c, err
	}
	prev, prevMsgs, prevTools, ok := decodeCursor(c)
	if ok && prev == fp {
		return source.SourceUpdate{}, c, nil
	}

	r, err := readFile(ctx, h.Path)
	if err != nil {
		if ctx.Err() != nil {
			return source.SourceUpdate{}, c, err
		}
		if !ok {
			// Keep the unreadable cursor so the next successful read still
			// resets; advancing would pair zero totals with a valid cursor.
			return source.SourceUpdate{}, c, fmt.Errorf("antigravity: read %s: %w", filepath.Base(h.Path), err)
		}
		// Advance past the unreadable state so it is not retried until the
		// files change; the monitor's health tracking surfaces the failure.
		next := encodeCursor(fp, prevMsgs, prevTools)
		return source.SourceUpdate{}, next, fmt.Errorf("antigravity: read %s: %w", filepath.Base(h.Path), err)
	}

	totalMsgs := r.totalUserMsgs + r.totalModelMsgs
	next := encodeCursor(fp, totalMsgs, r.totalToolCalls)

	// A non-empty cursor we cannot read, or a conversation that now holds
	// fewer messages or tool calls than the cursor recorded, means the
	// database was replaced or truncated: report absolute totals as a Reset.
	if !ok || totalMsgs < prevMsgs || r.totalToolCalls < prevTools {
		u := buildUpdate(r, h.ID, h.WorkingDir, totalMsgs, r.totalToolCalls)
		u.Reset = true
		return u, next, nil
	}
	return buildUpdate(r, h.ID, h.WorkingDir, totalMsgs-prevMsgs, r.totalToolCalls-prevTools), next, nil
}

func readFile(ctx context.Context, path string) (parseResult, error) {
	db, err := sqlitero.Open(path)
	if err != nil {
		return parseResult{}, err
	}
	defer func() { _ = db.Close() }()
	return readConversation(ctx, db)
}

// fingerprint identifies one state of a conversation's database and WAL.
type fingerprint struct {
	dbMtime, dbSize, walMtime, walSize int64
}

func statFingerprint(path string) (fingerprint, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fingerprint{}, err
	}
	fp := fingerprint{dbMtime: fi.ModTime().UnixNano(), dbSize: fi.Size()}
	if wal, err := os.Stat(path + "-wal"); err == nil {
		fp.walMtime, fp.walSize = wal.ModTime().UnixNano(), wal.Size()
	}
	return fp, nil
}

// encodeCursor serialises the fingerprint and totals into an opaque cursor:
// "<db_mtime_ns>,<db_size>,<wal_mtime_ns>,<wal_size>,<msgs>,<tools>".
func encodeCursor(fp fingerprint, msgs, tools int) source.Cursor {
	return source.Cursor(fmt.Sprintf("%d,%d,%d,%d,%d,%d", fp.dbMtime, fp.dbSize, fp.walMtime, fp.walSize, msgs, tools))
}

// decodeCursor parses the cursor produced by encodeCursor. The empty cursor
// is a valid "nothing parsed yet" state (zero totals, ok); a malformed one
// returns ok == false so the caller can report a Reset.
func decodeCursor(c source.Cursor) (fp fingerprint, msgs, tools int, ok bool) {
	if c == "" {
		return fingerprint{}, 0, 0, true
	}
	parts := strings.Split(string(c), ",")
	if len(parts) != 6 {
		return fingerprint{}, 0, 0, false
	}
	var n [6]int64
	for i := 0; i < len(parts); i++ {
		v, err := strconv.ParseInt(parts[i], 10, 64)
		if err != nil {
			return fingerprint{}, 0, 0, false
		}
		n[i] = v
	}
	return fingerprint{dbMtime: n[0], dbSize: n[1], walMtime: n[2], walSize: n[3]}, int(n[4]), int(n[5]), true
}
