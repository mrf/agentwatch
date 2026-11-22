// Package claude provides the Claude Code source for agentwatch.
//
// A Source discovers sessions by walking a directory of Claude Code
// project files (typically ~/.claude/projects) and parsing JSONL session logs.
// Use New with WithRoot to configure the root directory; there are no default
// paths — consumers choose the location.
package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mrf/agentwatch/internal/filewatch"
	"github.com/mrf/agentwatch/internal/jsonl"
	"github.com/mrf/agentwatch/source"
)

// Option configures a Source.
type Option func(*Source)

// WithRoot sets the root directory to scan for Claude Code session files.
// Discover walks this directory recursively, collecting all *.jsonl files.
// If root is empty, Discover returns no sessions.
func WithRoot(path string) Option {
	return func(s *Source) {
		s.root = path
	}
}

// WithDiscoverWindow limits discovery to JSONL files whose modification time
// is within d of the current time. Zero disables age filtering but still uses
// the efficient directory-mtime-caching walker.
func WithDiscoverWindow(d time.Duration) Option {
	return func(s *Source) {
		s.window = d
	}
}

// WithSessionEndDir sets the directory where Claude Code deposits session-end
// hook marker files. When set, Parse checks this directory for a matching
// marker and signals Terminal=true on the Update when one is found.
// The marker file is removed from disk once consumed.
func WithSessionEndDir(path string) Option {
	return func(s *Source) {
		s.sessionEndDir = path
	}
}

// WithMaxEndMarkerSize sets the maximum byte size of an end-marker file to
// accept. Files larger than this are removed from disk without being parsed.
// Defaults to 4096 bytes.
func WithMaxEndMarkerSize(bytes int) Option {
	return func(s *Source) {
		s.maxEndMarkerSize = bytes
	}
}

var errInvalidEndMarker = errors.New("invalid end marker")

// End-marker validation constants. These mirror the corresponding values from
// the agent-racer monitor for behavioural parity.
const (
	// defaultMaxEndMarkerSize caps end-marker files at 4096 bytes.
	// Legitimate markers are a few hundred bytes; larger files are suspect.
	defaultMaxEndMarkerSize = 4096

	// maxSessionIDLen is the upper bound on a session ID length in a marker.
	maxSessionIDLen = 128

	// maxReasonLen caps the reason field to prevent large strings.
	maxReasonLen = 512

	// maxTranscriptPathLen caps the transcript_path field length.
	maxTranscriptPathLen = 1024

	// endMarkerTimestampSkew is the maximum deviation a marker timestamp may
	// have from the current time. Markers outside this window have their
	// timestamp cleared so the caller falls back to the current time.
	endMarkerTimestampSkew = time.Hour

	// pendingTerminalTTL bounds how long a drained marker waits for its
	// session to be parsed. A discovered session consumes it within a poll or
	// two; one never parsed (outside the discover window, or another root's)
	// would otherwise be held forever.
	pendingTerminalTTL = time.Hour

	// maxDecodePathCandidates bounds the filesystem path disambiguation search.
	maxDecodePathCandidates = 4096
)

// validSessionIDRe matches session IDs with safe characters only.
var validSessionIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// sessionEndMarker is the JSON shape of a Claude Code session-end hook file.
type sessionEndMarker struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	Reason         string `json:"reason"`
	Timestamp      string `json:"timestamp"`
}

// consumedMarker holds the validated data extracted from a session-end marker.
type consumedMarker struct {
	reason    string
	endedAt   time.Time
	drainedAt time.Time
}

// claudeCursor is the JSON-encoded cursor format for the Source.
// It persists the file read position plus cross-batch subagent state, so that
// tool_result completion and subagent counts carry across parse calls.
type claudeCursor struct {
	jsonl.Position
	LastMessageID string                   `json:"lastMessageId,omitempty"` // last counted assistant message.id
	Parents       map[string]string        `json:"parents,omitempty"`       // parentToolUseID -> toolUseID
	Subagents     map[string]subagentEntry `json:"subagents,omitempty"`     // subagent ID -> cumulative state
}

// Source discovers and parses Claude Code JSONL session files.
// The zero value is usable but will not discover any sessions until
// configured with a non-empty root via WithRoot.
type Source struct {
	root             string
	window           time.Duration
	sessionEndDir    string
	maxEndMarkerSize int
	contextWindows   map[string]int    // overrides merged over defaultContextWindows
	walker           *filewatch.Walker // nil when root is empty

	mu               sync.Mutex
	pendingTerminals map[string]consumedMarker // session ID -> consumed marker
	lastMarkerScan   time.Time
}

// New returns a new Source configured by opts.
func New(opts ...Option) (source.Source, error) {
	s := &Source{
		pendingTerminals: make(map[string]consumedMarker),
	}
	for i := 0; i < len(opts); i++ {
		opts[i](s)
	}
	if s.root != "" {
		wopts := []filewatch.Option{
			filewatch.WithPattern("*.jsonl"),
			// Subagent transcripts are attached to their parent by Parse.
			filewatch.WithSkipDir(func(path string) bool { return isSubagentsDir(s.root, path) }),
		}
		if s.window > 0 {
			wopts = append(wopts, filewatch.WithMaxAge(s.window))
		}
		s.walker = filewatch.New([]string{s.root}, wopts...)
	}
	return s, nil
}

// Register adds a Source constructor to r under the name "claude".
func Register(r *source.Registry, opts ...Option) error {
	return r.Register("claude", func() (source.Source, error) {
		return New(opts...)
	})
}

// Name returns "claude".
func (s *Source) Name() string { return "claude" }

// Discover walks the configured root and returns a SessionHandle for every
// *.jsonl file found. The session ID is derived from the filename (without the
// .jsonl extension). StartedAt is set from the file modification time.
// WorkingDir is populated by decoding the parent directory name (Claude Code
// encodes project paths by replacing "/" with "-").
//
// When a discover window is set (WithDiscoverWindow), only files whose
// modification time is within that window are returned. The filter uses the
// real file mtime on every call (not a cached value) because Claude JSONL
// files are appended to during a session — their mtime changes without
// updating the parent directory's mtime.
func (s *Source) Discover(ctx context.Context) ([]source.SessionHandle, error) {
	if s.walker == nil {
		return nil, nil
	}
	entries, err := s.walker.Discover(ctx)
	if err != nil {
		return nil, err
	}

	handles := make([]source.SessionHandle, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		path := entries[i].Path
		// Claude Code encodes the working directory as the project directory
		// name by replacing "/" with "-" (e.g. /home/user/proj → -home-user-proj).
		handles = append(handles, source.SessionHandle{
			ID:         strings.TrimSuffix(filepath.Base(path), ".jsonl"),
			Path:       path,
			WorkingDir: decodeProjectPath(filepath.Base(filepath.Dir(path))),
			Source:     "claude",
			StartedAt:  entries[i].ModTime,
		})
	}
	return handles, nil
}

// Parse reads new JSONL lines from the session file starting at cursor c and
// returns an incremental Update. The cursor encodes a jsonl.Position
// plus cumulative subagent state; an empty cursor starts from the beginning.
// A corrupt cursor, or a transcript truncated or replaced since the cursor
// was taken, is re-read from the beginning as a Reset update.
//
// Subagent transcripts under <session-id>/subagents/ are read on every call
// and reported in Subagents (cumulative, only those that changed). A change
// confined to subagent files still yields an update for the parent.
//
// If a session-end marker for h.ID is found in the configured sessionEndDir,
// the returned Update has Terminal=true and EndReason populated.
//
// If no new data is available, Parse returns a zero Update and the
// unchanged cursor.
func (s *Source) Parse(ctx context.Context, h source.SessionHandle, c source.Cursor) (source.Update, source.Cursor, error) {
	if ctx.Err() != nil {
		return source.Update{}, c, ctx.Err()
	}

	cc, err := decodeCursor(c)
	reset := err != nil
	if reset {
		cc = claudeCursor{}
	}

	res, nextPos, rewritten, err := jsonl.ReadFrom(h.Path, cc.Position, jsonl.Options{})
	if err != nil {
		return source.Update{}, c, err
	}
	if rewritten {
		// The subagent state was derived from the old file too.
		cc, reset = claudeCursor{}, true
	}
	lines := res.Lines

	// Drain end markers from the configured directory. This populates
	// s.pendingTerminals for all sessions found, not just this one.
	if s.sessionEndDir != "" {
		s.drainEndMarkers(time.Now())
	}

	// Subagent files are read before the parent's lines so that a tool_result
	// in this batch can complete a subagent first seen in this poll.
	touched, dirty, subSkipped := readSubagentFiles(h.Path, &cc)

	var result parseResult
	if len(lines) > 0 {
		result = parseParentLines(lines, &cc, touched)
	}
	if reset || nextPos != cc.Position {
		dirty = true
	}
	cc.Position = nextPos

	update := s.buildUpdate(h, &result)
	applySubagentsAndReset(h, &update, &cc, touched, reset)
	s.applyPendingTerminal(h, &update)

	update.SkippedLines = res.Skipped + subSkipped

	if !dirty {
		return update, c, nil
	}
	nextCursor, _ := encodeCursor(cc)
	return update, nextCursor, nil
}

// parseParentLines parses the parent transcript's new lines, folds the
// subagents they report into cc, and adds those IDs to touched.
func parseParentLines(lines [][]byte, cc *claudeCursor, touched map[string]struct{}) parseResult {
	result := parseRecords(lines, cc.knownParents(), cc.LastMessageID, false)
	cc.LastMessageID = result.lastMsgID
	cc.Parents = mergeParents(cc.Parents, buildParentMap(result.subagents))
	batch := buildSubagentStates(result.subagents)
	for i := 0; i < len(batch); i++ {
		id := batch[i].ID
		entry := cc.Subagents[id]
		entry.State = accumulateSubagent(entry.State, batch[i])
		cc.putSubagent(id, entry)
		touched[id] = struct{}{}
	}
	cc.pruneParents()
	return result
}

// buildUpdate converts the parent's parse result into the session update. It
// is zero when the batch held neither data nor a compaction.
func (s *Source) buildUpdate(h source.SessionHandle, result *parseResult) source.Update {
	if !result.hasData {
		if result.compactionDelta > 0 {
			return source.Update{
				SessionID:            cmp.Or(result.sessionID, h.ID),
				CompactionCountDelta: result.compactionDelta,
				LastActivityAt:       result.lastActivityAt,
			}
		}
		return source.Update{}
	}
	return source.Update{
		SessionID:            cmp.Or(result.sessionID, h.ID),
		Slug:                 result.slug,
		Activity:             result.activity,
		Model:                result.model,
		HasTokens:            result.hasTokens,
		ContextTokens:        result.contextTokens,
		OutputTokens:         result.outputTokens,
		MaxContextTokens:     s.contextWindow(result.model),
		MessageCountDelta:    result.msgDelta,
		ToolCallCountDelta:   result.toolDelta,
		CompactionCountDelta: result.compactionDelta,
		CurrentTool:          result.currentTool,
		WorkingDir:           result.cwd,
		Branch:               result.branch,
		StartedAt:            result.startedAt,
		LastActivityAt:       result.lastActivityAt,
	}
}

// applySubagentsAndReset attaches the touched subagents' cumulative states to
// update and flags a Reset, giving the update h's ID when either applies.
func applySubagentsAndReset(h source.SessionHandle, update *source.Update, cc *claudeCursor, touched map[string]struct{}, reset bool) {
	if len(touched) > 0 {
		if update.SessionID == "" {
			update.SessionID = h.ID
		}
		update.Subagents = cc.subagentStates(touched)
	}

	if reset {
		update.SessionID = cmp.Or(update.SessionID, h.ID)
		update.Reset = true
	}
}

// applyPendingTerminal marks update terminal when an end marker for h was
// drained. Keyed by the handle, never by the records' sessionId: subagent
// transcripts carry their parent's sessionId.
func (s *Source) applyPendingTerminal(h source.SessionHandle, update *source.Update) {
	cm, ok := s.consumePendingTerminal(h.ID)
	if !ok {
		return
	}
	if update.SessionID == "" {
		update.SessionID = h.ID
	}
	update.Terminal = true
	update.EndReason = cm.reason
	update.EndedAt = cm.endedAt
}

// mergeParents returns prior with next's entries added (next takes
// precedence). prior is returned unchanged when next is empty.
func mergeParents(prior, next map[string]string) map[string]string {
	if len(next) == 0 {
		return prior
	}
	merged := make(map[string]string, len(prior)+len(next))
	maps.Copy(merged, prior)
	maps.Copy(merged, next)
	return merged
}

// drainEndMarkers scans the sessionEndDir for end-marker files, validates
// them, stores valid entries in pendingTerminals, and removes the files from
// disk. It is a no-op if the dir was scanned less than 500ms ago.
func (s *Source) drainEndMarkers(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Rate-limit scans to avoid hammering the filesystem on every Parse call.
	const minScanInterval = 500 * time.Millisecond
	if now.Sub(s.lastMarkerScan) < minScanInterval {
		return
	}
	s.lastMarkerScan = now

	for id, cm := range s.pendingTerminals {
		if now.Sub(cm.drainedAt) >= pendingTerminalTTL {
			delete(s.pendingTerminals, id)
		}
	}

	names := s.listEndMarkerNames()

	maxSize := int64(s.maxEndMarkerSize)
	if maxSize <= 0 {
		maxSize = defaultMaxEndMarkerSize
	}

	for _, name := range names {
		s.drainEndMarker(filepath.Join(s.sessionEndDir, name), maxSize, now)
	}
}

// drainEndMarker reads, validates and removes the marker file at path,
// recording a valid marker in pendingTerminals. Must be called with s.mu held.
func (s *Source) drainEndMarker(path string, maxSize int64, now time.Time) {
	data, ok := readBoundedRegularFile(path, maxSize)
	if !ok {
		// Unreadable, not a regular file, or oversize. The name matched
		// the hook's pattern; remove it only if it is still a plain
		// regular file (never touch a FIFO, symlink, or directory).
		removeIfRegular(path)
		return
	}
	defer func() { _ = os.Remove(path) }()

	var marker sessionEndMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return
	}
	if err := validateEndMarker(&marker, now); err != nil {
		return
	}

	endedAt := now
	if t, ok := parseTimestamp(marker.Timestamp); ok {
		endedAt = t
	}

	cm := consumedMarker{
		reason:    marker.Reason,
		endedAt:   endedAt,
		drainedAt: now,
	}
	s.pendingTerminals[marker.SessionID] = cm

	// Also index by the filename-based session ID (transcript path stem)
	// in case the marker's session_id differs from the JSONL filename.
	if marker.TranscriptPath == "" {
		return
	}
	fileID := strings.TrimSuffix(filepath.Base(marker.TranscriptPath), ".jsonl")
	if fileID == marker.SessionID {
		return
	}
	if _, exists := s.pendingTerminals[fileID]; !exists {
		s.pendingTerminals[fileID] = cm
	}
}

// isEndMarkerName reports whether name is one the SessionEnd hook writes:
// "<session_id>.json" with a session ID that passes the same character and
// length rules as the marker's own session_id field.
func isEndMarkerName(name string) bool {
	id, ok := strings.CutSuffix(name, ".json")
	return ok && id != "" && len(id) <= maxSessionIDLen && validSessionIDRe.MatchString(id)
}

// listEndMarkerNames returns the names of every regular file in sessionEndDir
// that looks like a hook-written marker. It loops ReadDir until the directory
// is exhausted and collects names before any file is removed, so deleting
// markers cannot disturb the directory iteration.
func (s *Source) listEndMarkerNames() []string {
	f, err := os.Open(s.sessionEndDir)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var names []string
	for {
		entries, err := f.ReadDir(256)
		for _, entry := range entries {
			if entry.Type().IsRegular() && isEndMarkerName(entry.Name()) {
				names = append(names, entry.Name())
			}
		}
		if err != nil {
			// io.EOF ends the scan; any other error keeps what was read.
			return names
		}
	}
}

// readBoundedRegularFile reads path only if it is a regular file no larger
// than maxSize. The mode is re-checked on the open handle and the read is
// capped, so a file swapped or grown after listing cannot overrun the bound.
func readBoundedRegularFile(path string, maxSize int64) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSize {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil || int64(len(data)) > maxSize {
		return nil, false
	}
	return data, true
}

// removeIfRegular removes path only if it is currently a regular file.
func removeIfRegular(path string) {
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
		_ = os.Remove(path)
	}
}

// consumePendingTerminal retrieves and removes a pending terminal entry for
// the given session ID. Returns the entry and true if found; zero value and
// false otherwise.
func (s *Source) consumePendingTerminal(sessionID string) (consumedMarker, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cm, ok := s.pendingTerminals[sessionID]; ok {
		delete(s.pendingTerminals, sessionID)
		return cm, true
	}
	return consumedMarker{}, false
}

// validateEndMarker checks that a parsed marker has reasonable field values.
// Returns an error describing the first structural failure. Non-critical fields
// (timestamp, reason) are sanitised in place rather than causing rejection.
func validateEndMarker(marker *sessionEndMarker, now time.Time) error {
	if marker.SessionID == "" {
		return fmt.Errorf("%w: empty session_id", errInvalidEndMarker)
	}
	if len(marker.SessionID) > maxSessionIDLen {
		return fmt.Errorf("%w: session_id too long (%d > %d)", errInvalidEndMarker, len(marker.SessionID), maxSessionIDLen)
	}
	if !validSessionIDRe.MatchString(marker.SessionID) {
		return fmt.Errorf("%w: session_id contains invalid characters", errInvalidEndMarker)
	}

	if err := validateTranscriptPath(marker.TranscriptPath); err != nil {
		return err
	}

	// Reason: truncate silently if too long (non-critical field).
	if len(marker.Reason) > maxReasonLen {
		marker.Reason = marker.Reason[:maxReasonLen]
	}

	// Timestamp: clear if invalid or outside skew window.
	if marker.Timestamp != "" {
		parsed, err := time.Parse(time.RFC3339Nano, marker.Timestamp)
		if err != nil || absDuration(now.Sub(parsed)) > endMarkerTimestampSkew {
			marker.Timestamp = ""
		}
	}

	return nil
}

// validateTranscriptPath checks a marker's optional transcript_path.
func validateTranscriptPath(p string) error {
	if p == "" {
		return nil
	}
	if len(p) > maxTranscriptPathLen {
		return fmt.Errorf("%w: transcript_path too long (%d > %d)", errInvalidEndMarker, len(p), maxTranscriptPathLen)
	}
	if !strings.HasSuffix(p, ".jsonl") {
		return fmt.Errorf("%w: transcript_path does not end with .jsonl", errInvalidEndMarker)
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("%w: transcript_path contains path traversal", errInvalidEndMarker)
	}
	return nil
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// encodeCursor converts a claudeCursor to an opaque source.Cursor string.
func encodeCursor(cc claudeCursor) (source.Cursor, error) {
	data, err := json.Marshal(cc)
	if err != nil {
		return source.Cursor(strconv.FormatInt(cc.Offset, 10)), err
	}
	return source.Cursor(data), nil
}

// decodeCursor parses an opaque cursor into a claudeCursor. It also accepts
// the legacy plain decimal offset, which carries no file identity.
func decodeCursor(c source.Cursor) (claudeCursor, error) {
	if c == "" {
		return claudeCursor{}, nil
	}
	s := string(c)
	var cc claudeCursor
	if offset, err := strconv.ParseInt(s, 10, 64); err == nil {
		cc.Offset = offset
	} else if err := json.Unmarshal([]byte(s), &cc); err != nil {
		return claudeCursor{}, fmt.Errorf("decode cursor: %w", err)
	}
	if err := cc.Validate(); err != nil {
		return claudeCursor{}, fmt.Errorf("decode cursor: %w", err)
	}
	for id, e := range cc.Subagents {
		if err := e.Validate(); err != nil {
			return claudeCursor{}, fmt.Errorf("decode cursor: subagent %s: %w", id, err)
		}
	}
	return cc, nil
}

// decodeProjectPath reverses the encoding to recover the original working
// directory path. The encoding is lossy: slash-to-dash means /home/my-user/proj
// and /home/my/user-proj both encode to -home-my-user-proj. An empty segment
// ("--") marks a dot-encoded component start (/.claude → --claude). This
// function tries all candidate reconstructions and returns the first one that
// exists on the current filesystem. Falls back to treating all dashes as
// separators when no verified path is found; callers must treat the result as a
// guess and prefer the cwd recorded in the transcript.
//
// Windows paths encode every non-alphanumeric character as "-", so
// C:\git\cc-plus is stored as "C--git-cc-plus" (drive letter, then "--" for
// ":\"). Those decode to backslash-separated paths rooted at the drive.
func decodeProjectPath(encoded string) string {
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		decoded = encoded
	}

	var root, sep, rest string
	switch {
	case strings.HasPrefix(decoded, "-"):
		root, sep, rest = "/", "/", decoded[1:]
	case isWindowsDriveEncoding(decoded):
		root, sep, rest = decoded[:1]+`:\`, `\`, decoded[3:]
	default:
		return decoded
	}

	parts := dotMarkedParts(strings.Split(rest, "-"))

	if result := decodeTryPaths(root, sep, parts); result != "" {
		return result
	}

	// Fallback: treat all dashes as separators.
	return root + strings.Join(parts, sep)
}

// isWindowsDriveEncoding reports whether name starts with an encoded Windows
// drive root: an ASCII letter followed by "--" (from ":\").
func isWindowsDriveEncoding(name string) bool {
	if len(name) < 3 || name[1] != '-' || name[2] != '-' {
		return false
	}
	c := name[0]
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// dotMarkedParts folds each empty segment (an encoded "." immediately after a
// separator) into the following segment as a leading ".". Trailing empty
// segments are dropped.
func dotMarkedParts(parts []string) []string {
	out := make([]string, 0, len(parts))
	dot := false
	for i := 0; i < len(parts); i++ {
		if parts[i] == "" {
			dot = true
			continue
		}
		if dot {
			out = append(out, "."+parts[i])
			dot = false
			continue
		}
		out = append(out, parts[i])
	}
	return out
}

// decodeTryPaths iteratively builds candidate paths by choosing slash or
// hyphen at each part boundary, joining components under root with sep. Prunes
// candidates whose parent path does not exist on the filesystem. Returns the
// first fully-existing path, or "" if none is found.
func decodeTryPaths(root, sep string, parts []string) string {
	if len(parts) == 0 {
		return ""
	}

	d := pathDecoder{root: root, sep: sep, parentExists: make(map[string]bool)}
	candidates := [][]string{{parts[0]}}

	for idx := 1; idx < len(parts); idx++ {
		next, ok := d.expand(candidates, parts[idx])
		if !ok || len(next) == 0 {
			return ""
		}
		candidates = next
	}

	for i := 0; i < len(candidates); i++ {
		p := d.path(candidates[i])
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	return ""
}

// pathDecoder holds decodeTryPaths' inputs and its parent-existence cache.
// A candidate is a list of path components joined under root with sep.
type pathDecoder struct {
	root, sep    string
	parentExists map[string]bool
}

func (d *pathDecoder) path(components []string) string {
	return d.root + strings.Join(components, d.sep)
}

// hasParent reports whether the directory holding the candidate's last
// component exists.
func (d *pathDecoder) hasParent(components []string) bool {
	if len(components) <= 1 {
		return true
	}
	parent := d.path(components[:len(components)-1])
	if exists, ok := d.parentExists[parent]; ok {
		return exists
	}
	_, err := os.Stat(parent)
	exists := err == nil
	d.parentExists[parent] = exists
	return exists
}

// expand extends every candidate with part, once as a new component and once
// hyphen-joined to the last, keeping distinct candidates whose parent exists.
// It reports false once more than maxDecodePathCandidates survive.
func (d *pathDecoder) expand(candidates [][]string, part string) ([][]string, bool) {
	next := make([][]string, 0, len(candidates)*2)
	seen := make(map[string]struct{}, len(candidates)*2)

	for i := 0; i < len(candidates); i++ {
		cur := candidates[i]
		withSlash := append(slices.Clip(cur), part)
		withHyphen := slices.Clone(cur)
		withHyphen[len(withHyphen)-1] += "-" + part

		for _, candidate := range [][]string{withSlash, withHyphen} {
			if !d.hasParent(candidate) {
				continue
			}
			p := d.path(candidate)
			if _, ok := seen[p]; !ok {
				next = append(next, candidate)
				seen[p] = struct{}{}
			}
		}
		if len(next) > maxDecodePathCandidates {
			return nil, false
		}
	}
	return next, true
}
