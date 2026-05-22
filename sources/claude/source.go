// Package claude provides the Claude Code source for agentwatch.
//
// A ClaudeSource discovers sessions by walking a directory of Claude Code
// project files (typically ~/.claude/projects) and parsing JSONL session logs.
// Use New with WithRoot to configure the root directory; there are no default
// paths — consumers choose the location.
package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

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
// It persists both the file byte offset and the cross-batch subagent
// parent map so that tool_result completion can be detected across parse calls.
type claudeCursor struct {
	Offset  int64             `json:"offset"`
	Parents map[string]string `json:"parents,omitempty"` // parentToolUseID -> toolUseID
}

// ClaudeSource discovers and parses Claude Code JSONL session files.
// The zero value is usable but will not discover any sessions until
// configured with a non-empty root via WithRoot.
type ClaudeSource struct {
	root             string
	window           time.Duration
	sessionEndDir    string
	maxEndMarkerSize int

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
	if s.root == "" {
		return nil, nil
	}

	now := time.Now()
	var handles []source.SessionHandle
	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}

		fi, ferr := d.Info()
		if ferr != nil {
			return nil // skip unreadable files
		}

		// Age filter: skip files not modified within the discover window.
		if s.window > 0 && now.Sub(fi.ModTime()) > s.window {
			return nil
		}

		id := strings.TrimSuffix(d.Name(), ".jsonl")

		// Decode the project path from the parent directory name.
		// Claude Code encodes the working directory as the project directory
		// name by replacing "/" with "-" (e.g. /home/user/proj → -home-user-proj).
		workingDir := decodeProjectPath(filepath.Base(filepath.Dir(path)))

		handles = append(handles, source.SessionHandle{
			ID:         id,
			Path:       path,
			WorkingDir: workingDir,
			Source:     "claude",
			StartedAt:  fi.ModTime(),
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return handles, nil
}

// Parse reads new JSONL lines from the session file starting at cursor c and
// returns an incremental SourceUpdate. The cursor encodes a byte offset and
// the subagent parent map; an empty cursor starts from the beginning.
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
	if err != nil {
		cc = claudeCursor{} // reset on corrupt cursor
	}

	lines, nextOffset, err := jsonl.ReadLines(h.Path, cc.Offset, jsonl.Options{})
	if err != nil {
		return source.SourceUpdate{}, c, err
	}

	// Drain end markers from the configured directory. This populates
	// s.pendingTerminals for all sessions found, not just this one.
	if s.sessionEndDir != "" {
		s.drainEndMarkers(time.Now())
	}

	if len(lines) == 0 {
		// No new JSONL data. Check if there's a pending terminal for this session.
		if cm, ok := s.consumePendingTerminal(h.ID); ok {
			update := source.SourceUpdate{
				SessionID: h.ID,
				Terminal:  true,
				EndReason: cm.reason,
				EndedAt:   cm.endedAt,
			}
			nextCursor, _ := encodeCursor(claudeCursor{Offset: nextOffset, Parents: cc.Parents})
			return update, nextCursor, nil
		}
		return source.SourceUpdate{}, c, nil
	}

	result := parseLinesWithParents(lines, cc.Parents)
	nextParents := buildParentMap(result.subagents)

	// Merge prior parents with new ones (new parse results take precedence).
	mergedParents := cc.Parents
	if len(nextParents) > 0 {
		mergedParents = make(map[string]string, len(cc.Parents)+len(nextParents))
		for k, v := range cc.Parents {
			mergedParents[k] = v
		}
		for k, v := range nextParents {
			mergedParents[k] = v
		}
	}

	nextCursor, _ := encodeCursor(claudeCursor{Offset: nextOffset, Parents: mergedParents})

	if !result.hasData {
		// Check for a terminal even when there was no meaningful JSONL data.
		if cm, ok := s.consumePendingTerminal(h.ID); ok {
			update := source.SourceUpdate{
				SessionID: h.ID,
				Terminal:  true,
				EndReason: cm.reason,
				EndedAt:   cm.endedAt,
			}
			return update, nextCursor, nil
		}
		return source.SourceUpdate{}, nextCursor, nil
	}

	sessionID := result.sessionID
	if sessionID == "" {
		sessionID = h.ID
	}

	update := source.SourceUpdate{
		SessionID:          sessionID,
		Slug:               result.slug,
		Activity:           result.activity,
		Model:              result.model,
		ContextTokens:      result.contextTokens,
		OutputTokens:       result.outputTokens,
		MessageCountDelta:    result.msgDelta,
		ToolCallCountDelta:   result.toolDelta,
		CompactionCountDelta: result.compactionDelta,
		CurrentTool:          result.currentTool,
		WorkingDir:         result.cwd,
		Branch:             result.branch,
		LastActivityAt:     result.lastActivityAt,
		Subagents:          buildSubagentStates(result.subagents),
	}
	if !result.startedAt.IsZero() {
		update.StartedAt = result.startedAt
	}

	// Attach terminal state if a marker was found for this session.
	if cm, ok := s.consumePendingTerminal(sessionID); ok {
		update.Terminal = true
		update.EndReason = cm.reason
		update.EndedAt = cm.endedAt
	}

	return update, nextCursor, nil
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

	f, err := os.Open(s.sessionEndDir)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	entries, err := f.ReadDir(256)
	if err != nil {
		return
	}

	maxSize := int64(s.maxEndMarkerSize)
	if maxSize <= 0 {
		maxSize = defaultMaxEndMarkerSize
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		path := filepath.Join(s.sessionEndDir, entry.Name())

		if info.Size() > maxSize {
			_ = os.Remove(path)
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
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
// Falls back to a plain decimal offset string if the cursor has no parents
// (for compact, backward-compatible output).
func encodeCursor(cc claudeCursor) (source.Cursor, error) {
	if len(cc.Parents) == 0 {
		return source.Cursor(strconv.FormatInt(cc.Offset, 10)), nil
	}
	data, err := json.Marshal(cc)
	if err != nil {
		return source.Cursor(strconv.FormatInt(cc.Offset, 10)), err
	}
	return source.Cursor(data), nil
}

// decodeCursor parses an opaque cursor into a claudeCursor.
// Accepts both the plain decimal offset format (legacy / no subagents) and the
// JSON format (subagent parent map present).
func decodeCursor(c source.Cursor) (claudeCursor, error) {
	if c == "" {
		return claudeCursor{}, nil
	}
	s := string(c)
	// Fast path: plain integer (decimal byte offset from single-format cursors).
	if offset, err := strconv.ParseInt(s, 10, 64); err == nil {
		return claudeCursor{Offset: offset}, nil
	}
	// JSON path: full cursor with parents map.
	var cc claudeCursor
	if err := json.Unmarshal([]byte(s), &cc); err != nil {
		return claudeCursor{}, fmt.Errorf("decode cursor: %w", err)
	}
	return cc, nil
}

// encodeProjectPath encodes an absolute path to the Claude Code project
// directory naming scheme: replace "/" with "-". The leading "/" becomes
// a leading "-" (e.g. /home/user/proj → -home-user-proj).
func encodeProjectPath(path string) string {
	return strings.ReplaceAll(filepath.Clean(path), "/", "-")
}

// decodeProjectPath reverses the encoding to recover the original working
// directory path. The encoding is lossy: slash-to-dash means /home/my-user/proj
// and /home/my/user-proj both encode to -home-my-user-proj. This function
// tries all candidate reconstructions and returns the first one whose parent
// directories exist on the current filesystem. Falls back to treating all
// dashes as slashes when no verified path is found.
func decodeProjectPath(encoded string) string {
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		decoded = encoded
	}

	if !strings.HasPrefix(decoded, "-") {
		return decoded
	}

	parts := strings.Split(decoded[1:], "-") // skip leading dash

	if result := decodeTryPaths(parts); result != "" {
		return result
	}

	// Fallback: treat all dashes as slashes.
	return "/" + strings.Join(parts, "/")
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
