// Package claude provides the Claude Code source for agentwatch.
//
// A ClaudeSource discovers sessions by walking a directory of Claude Code
// project files (typically ~/.claude/projects) and parsing JSONL session logs.
// Use New with WithRoot to configure the root directory; there are no default
// paths — consumers choose the location.
package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mrf/agentwatch/internal/filewatch"
	"github.com/mrf/agentwatch/internal/jsonl"
	"github.com/mrf/agentwatch/source"
)

// Option configures a ClaudeSource.
type Option func(*ClaudeSource)

// WithRoot sets the root directory to scan for Claude Code session files.
// Discover walks this directory recursively, collecting all *.jsonl files.
// If root is empty, Discover returns no sessions.
func WithRoot(path string) Option {
	return func(s *ClaudeSource) {
		s.root = path
	}
}

// WithDiscoverWindow limits discovery to JSONL files whose modification time
// is within d of the current time. Zero disables age filtering but still uses
// the efficient directory-mtime-caching walker.
func WithDiscoverWindow(d time.Duration) Option {
	return func(s *ClaudeSource) {
		s.window = d
	}
}

// WithSessionEndDir sets the directory where Claude Code deposits session-end
// hook marker files. When set, Parse checks this directory for a matching
// marker and signals Terminal=true on the SourceUpdate when one is found.
// The marker file is removed from disk once consumed.
func WithSessionEndDir(path string) Option {
	return func(s *ClaudeSource) {
		s.sessionEndDir = path
	}
}

// WithMaxEndMarkerSize sets the maximum byte size of an end-marker file to
// accept. Files larger than this are removed from disk without being parsed.
// Defaults to 4096 bytes.
func WithMaxEndMarkerSize(bytes int) Option {
	return func(s *ClaudeSource) {
		s.maxEndMarkerSize = bytes
	}
}

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
	reason  string
	endedAt time.Time
}

// claudeCursor is the JSON-encoded cursor format for the ClaudeSource.
// It persists the file read position plus cross-batch subagent state, so that
// tool_result completion and subagent counts carry across parse calls.
type claudeCursor struct {
	jsonl.Position
	Parents   map[string]string        `json:"parents,omitempty"`   // parentToolUseID -> toolUseID
	Subagents map[string]subagentEntry `json:"subagents,omitempty"` // subagent ID -> cumulative state
}

// ClaudeSource discovers and parses Claude Code JSONL session files.
// The zero value is usable but will not discover any sessions until
// configured with a non-empty root via WithRoot.
type ClaudeSource struct {
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

// New returns a new ClaudeSource configured by opts.
func New(opts ...Option) (source.Source, error) {
	s := &ClaudeSource{
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

// Register adds a ClaudeSource constructor to r under the name "claude".
func Register(r *source.Registry, opts ...Option) error {
	return r.Register("claude", func() (source.Source, error) {
		return New(opts...)
	})
}

// Name returns "claude".
func (s *ClaudeSource) Name() string { return "claude" }

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
func (s *ClaudeSource) Discover(ctx context.Context) ([]source.SessionHandle, error) {
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
// returns an incremental SourceUpdate. The cursor encodes a jsonl.Position
// plus cumulative subagent state; an empty cursor starts from the beginning.
// A corrupt cursor, or a transcript truncated or replaced since the cursor
// was taken, is re-read from the beginning as a Reset update.
//
// Subagent transcripts under <session-id>/subagents/ are read on every call
// and reported in Subagents (cumulative, only those that changed). A change
// confined to subagent files still yields an update for the parent.
//
// If a session-end marker for h.ID is found in the configured sessionEndDir,
// the returned SourceUpdate has Terminal=true and EndReason populated.
//
// If no new data is available, Parse returns a zero SourceUpdate and the
// unchanged cursor.
func (s *ClaudeSource) Parse(ctx context.Context, h source.SessionHandle, c source.Cursor) (source.SourceUpdate, source.Cursor, error) {
	if ctx.Err() != nil {
		return source.SourceUpdate{}, c, ctx.Err()
	}

	cc, err := decodeCursor(c)
	reset := err != nil
	if reset {
		cc = claudeCursor{}
	}

	res, nextPos, rewritten, err := jsonl.ReadFrom(h.Path, cc.Position, jsonl.Options{})
	if err != nil {
		return source.SourceUpdate{}, c, err
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
	touched, dirty := readSubagentFiles(h.Path, &cc)

	var result parseResult
	if len(lines) > 0 {
		result = parseLinesWithParents(lines, cc.knownParents())
		cc.Parents = mergeParents(cc.Parents, buildParentMap(result.subagents))
		batch := buildSubagentStates(result.subagents)
		for i := 0; i < len(batch); i++ {
			id := batch[i].ID
			entry := cc.Subagents[id]
			entry.State = accumulateSubagent(entry.State, batch[i])
			cc.putSubagent(id, entry)
			touched[id] = struct{}{}
		}
	}
	if reset || nextPos != cc.Position {
		dirty = true
	}
	cc.Position = nextPos

	var update source.SourceUpdate
	if result.hasData {
		sessionID := result.sessionID
		if sessionID == "" {
			sessionID = h.ID
		}
		update = source.SourceUpdate{
			SessionID:            sessionID,
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
			LastActivityAt:       result.lastActivityAt,
		}
		if !result.startedAt.IsZero() {
			update.StartedAt = result.startedAt
		}
	}

	if !result.hasData && result.compactionDelta > 0 {
		update = source.SourceUpdate{
			SessionID:            cmp.Or(result.sessionID, h.ID),
			CompactionCountDelta: result.compactionDelta,
			LastActivityAt:       result.lastActivityAt,
		}
	}

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

	// Keyed by the handle, never by the records' sessionId: subagent
	// transcripts carry their parent's sessionId.
	if cm, ok := s.consumePendingTerminal(h.ID); ok {
		if update.SessionID == "" {
			update.SessionID = h.ID
		}
		update.Terminal = true
		update.EndReason = cm.reason
		update.EndedAt = cm.endedAt
	}

	if !dirty {
		return update, c, nil
	}
	nextCursor, _ := encodeCursor(cc)
	return update, nextCursor, nil
}

// mergeParents returns prior with next's entries added (next takes
// precedence). prior is returned unchanged when next is empty.
func mergeParents(prior, next map[string]string) map[string]string {
	if len(next) == 0 {
		return prior
	}
	merged := make(map[string]string, len(prior)+len(next))
	for k, v := range prior {
		merged[k] = v
	}
	for k, v := range next {
		merged[k] = v
	}
	return merged
}

// drainEndMarkers scans the sessionEndDir for end-marker files, validates
// them, stores valid entries in pendingTerminals, and removes the files from
// disk. It is a no-op if the dir was scanned less than 500ms ago.
func (s *ClaudeSource) drainEndMarkers(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Rate-limit scans to avoid hammering the filesystem on every Parse call.
	const minScanInterval = 500 * time.Millisecond
	if now.Sub(s.lastMarkerScan) < minScanInterval {
		return
	}
	s.lastMarkerScan = now

	names := s.listEndMarkerNames()

	maxSize := int64(s.maxEndMarkerSize)
	if maxSize <= 0 {
		maxSize = defaultMaxEndMarkerSize
	}

	for _, name := range names {
		path := filepath.Join(s.sessionEndDir, name)

		data, ok := readBoundedRegularFile(path, maxSize)
		if !ok {
			// Unreadable, not a regular file, or oversize. The name matched
			// the hook's pattern; remove it only if it is still a plain
			// regular file (never touch a FIFO, symlink, or directory).
			removeIfRegular(path)
			continue
		}

		var marker sessionEndMarker
		if err := json.Unmarshal(data, &marker); err != nil {
			_ = os.Remove(path)
			continue
		}

		if err := validateEndMarker(&marker, now); err != nil {
			_ = os.Remove(path)
			continue
		}

		endedAt := now
		if marker.Timestamp != "" {
			if t, terr := time.Parse(time.RFC3339Nano, marker.Timestamp); terr == nil {
				endedAt = t
			}
		}

		s.pendingTerminals[marker.SessionID] = consumedMarker{
			reason:  marker.Reason,
			endedAt: endedAt,
		}

		// Also index by the filename-based session ID (transcript path stem)
		// in case the marker's session_id differs from the JSONL filename.
		if marker.TranscriptPath != "" {
			fileID := strings.TrimSuffix(filepath.Base(marker.TranscriptPath), ".jsonl")
			if fileID != marker.SessionID {
				if _, exists := s.pendingTerminals[fileID]; !exists {
					s.pendingTerminals[fileID] = consumedMarker{
						reason:  marker.Reason,
						endedAt: endedAt,
					}
				}
			}
		}

		_ = os.Remove(path)
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
func (s *ClaudeSource) listEndMarkerNames() []string {
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
func (s *ClaudeSource) consumePendingTerminal(sessionID string) (consumedMarker, bool) {
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
		return fmt.Errorf("empty session_id")
	}
	if len(marker.SessionID) > maxSessionIDLen {
		return fmt.Errorf("session_id too long (%d > %d)", len(marker.SessionID), maxSessionIDLen)
	}
	if !validSessionIDRe.MatchString(marker.SessionID) {
		return fmt.Errorf("session_id contains invalid characters")
	}

	if marker.TranscriptPath != "" {
		if len(marker.TranscriptPath) > maxTranscriptPathLen {
			return fmt.Errorf("transcript_path too long (%d > %d)", len(marker.TranscriptPath), maxTranscriptPathLen)
		}
		if !strings.HasSuffix(marker.TranscriptPath, ".jsonl") {
			return fmt.Errorf("transcript_path does not end with .jsonl")
		}
		if strings.Contains(marker.TranscriptPath, "..") {
			return fmt.Errorf("transcript_path contains path traversal")
		}
	}

	// Reason: truncate silently if too long (non-critical field).
	if len(marker.Reason) > maxReasonLen {
		marker.Reason = marker.Reason[:maxReasonLen]
	}

	// Timestamp: clear if invalid or outside skew window.
	if marker.Timestamp != "" {
		parsed, err := time.Parse(time.RFC3339Nano, marker.Timestamp)
		if err != nil {
			marker.Timestamp = ""
		} else if absDuration(now.Sub(parsed)) > endMarkerTimestampSkew {
			marker.Timestamp = ""
		}
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

// encodeProjectPath encodes an absolute path to the Claude Code project
// directory naming scheme: replace "/" and "." with "-". The leading "/"
// becomes a leading "-" (e.g. /home/user/proj → -home-user-proj, and
// /p/.claude/wt → -p--claude-wt).
func encodeProjectPath(path string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(filepath.Clean(path))
}

// decodeProjectPath reverses the encoding to recover the original working
// directory path. The encoding is lossy: slash-to-dash means /home/my-user/proj
// and /home/my/user-proj both encode to -home-my-user-proj. An empty segment
// ("--") marks a dot-encoded component start (/.claude → --claude). This
// function tries all candidate reconstructions and returns the first one that
// exists on the current filesystem. Falls back to treating all dashes as
// slashes when no verified path is found; callers must treat the result as a
// guess and prefer the cwd recorded in the transcript.
func decodeProjectPath(encoded string) string {
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		decoded = encoded
	}

	if !strings.HasPrefix(decoded, "-") {
		return decoded
	}

	parts := dotMarkedParts(strings.Split(decoded[1:], "-")) // skip leading dash

	if result := decodeTryPaths(parts); result != "" {
		return result
	}

	// Fallback: treat all dashes as slashes.
	return "/" + strings.Join(parts, "/")
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
// hyphen at each part boundary. Prunes candidates whose parent path does not
// exist on the filesystem. Returns the first fully-existing path, or "" if
// none is found.
func decodeTryPaths(parts []string) string {
	if len(parts) == 0 {
		return ""
	}

	type pathState struct{ components []string }

	withSlash := func(s pathState, part string) pathState {
		next := make([]string, len(s.components)+1)
		copy(next, s.components)
		next[len(s.components)] = part
		return pathState{components: next}
	}
	withHyphen := func(s pathState, part string) pathState {
		next := make([]string, len(s.components))
		copy(next, s.components)
		next[len(next)-1] = next[len(next)-1] + "-" + part
		return pathState{components: next}
	}
	statePath := func(s pathState) string {
		return "/" + strings.Join(s.components, "/")
	}

	parentExistsCache := make(map[string]bool)
	pathExistsCache := make(map[string]bool)

	parentExists := func(s pathState) bool {
		if len(s.components) <= 1 {
			return true
		}
		parent := "/" + strings.Join(s.components[:len(s.components)-1], "/")
		if exists, ok := parentExistsCache[parent]; ok {
			return exists
		}
		_, err := os.Stat(parent)
		exists := err == nil
		parentExistsCache[parent] = exists
		return exists
	}

	candidates := []pathState{{components: []string{parts[0]}}}

	for idx := 1; idx < len(parts); idx++ {
		nextCandidates := make([]pathState, 0, len(candidates)*2)
		seen := make(map[string]struct{}, len(candidates)*2)

		for i := 0; i < len(candidates); i++ {
			for _, candidate := range []pathState{
				withSlash(candidates[i], parts[idx]),
				withHyphen(candidates[i], parts[idx]),
			} {
				if !parentExists(candidate) {
					continue
				}
				p := statePath(candidate)
				if _, ok := seen[p]; !ok {
					nextCandidates = append(nextCandidates, candidate)
					seen[p] = struct{}{}
				}
			}
			if len(nextCandidates) > maxDecodePathCandidates {
				return ""
			}
		}

		if len(nextCandidates) == 0 {
			return ""
		}
		candidates = nextCandidates
	}

	for i := 0; i < len(candidates); i++ {
		p := statePath(candidates[i])
		if exists, ok := pathExistsCache[p]; ok {
			if exists {
				return p
			}
			continue
		}
		_, err := os.Stat(p)
		exists := err == nil
		pathExistsCache[p] = exists
		if exists {
			return p
		}
	}

	return ""
}
