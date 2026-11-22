package antigravity

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"time"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

// Step types and statuses in the steps table, as observed in agy 1.5.14.
// Other step types (e.g. 101) carry no conversation turn and are ignored.
const (
	stepUserInput       = 14
	stepPlannerResponse = 15
	stepToolCall        = 132

	statusDone = 3
)

const latestStepMetadata = "SELECT metadata FROM steps WHERE step_type = ? ORDER BY idx DESC LIMIT 1"

// Protobuf field paths inside the metadata blobs, as observed in agy 1.5.14.
var (
	stepCreatedAt    = []uint64{1}
	stepToolName     = []uint64{4, 2}
	usageInput       = []uint64{9, 2}
	usageOutput      = []uint64{9, 3}
	usageCachedInput = []uint64{9, 5}
	usagePresent     = []uint64{9}
	execModel        = []uint64{10, 1, 28}
	metaWorkspaceURI = []uint64{1, 1}
	metaCreatedAt    = []uint64{2}
)

// parseResult holds monitoring state read from one conversation database.
// No context window is reported: no metadata field reliably holds it.
type parseResult struct {
	model string

	totalUserMsgs  int
	totalModelMsgs int
	totalToolCalls int

	currentTool string

	hasTokens     bool
	contextTokens int
	outputTokens  int

	activity       session.Activity
	workingDir     string
	startedAt      time.Time
	lastActivityAt time.Time
}

// readConversation reads monitoring state from an agy conversation database.
// Only metadata blobs are decoded; step payloads (prompt, response and tool
// text) are never read.
func readConversation(ctx context.Context, db *sql.DB) (parseResult, error) {
	r := parseResult{activity: session.ActivityIdle}

	if err := readCounts(ctx, db, &r); err != nil {
		return parseResult{}, err
	}

	// The latest conversational step decides activity. A finished planner
	// response ends the turn even if an earlier backgrounded tool is still
	// marked running.
	var lastType, lastStatus int
	var lastMeta []byte
	err := db.QueryRowContext(ctx,
		"SELECT step_type, status, metadata FROM steps WHERE step_type IN (?, ?, ?) ORDER BY idx DESC LIMIT 1",
		stepUserInput, stepPlannerResponse, stepToolCall).Scan(&lastType, &lastStatus, &lastMeta)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return parseResult{}, err
	default:
		if lastType == stepPlannerResponse && lastStatus == statusDone {
			r.activity = session.ActivityWaiting
		} else {
			r.activity = session.ActivityWorking
		}
		r.lastActivityAt, _ = pbTimestamp(lastMeta, stepCreatedAt...)
	}

	toolMeta, err := latestBlob(ctx, db, latestStepMetadata, stepToolCall)
	if err != nil {
		return parseResult{}, err
	}
	if name, ok := pbBytes(toolMeta, stepToolName...); ok {
		r.currentTool = string(name)
	}

	plannerMeta, err := latestBlob(ctx, db, latestStepMetadata, stepPlannerResponse)
	if err != nil {
		return parseResult{}, err
	}
	if _, ok := pbBytes(plannerMeta, usagePresent...); ok {
		in, _ := pbVarint(plannerMeta, usageInput...)
		cached, _ := pbVarint(plannerMeta, usageCachedInput...)
		out, _ := pbVarint(plannerMeta, usageOutput...)
		r.hasTokens = true
		r.contextTokens = int(in + cached) //nolint:gosec // token counts fit in int
		r.outputTokens = int(out)          //nolint:gosec // token counts fit in int
	}

	execMeta, err := latestBlob(ctx, db, "SELECT data FROM executor_metadata ORDER BY idx DESC LIMIT 1")
	if err != nil {
		return parseResult{}, err
	}
	if model, ok := pbBytes(execMeta, execModel...); ok {
		r.model = string(model)
	}

	trajMeta, err := latestBlob(ctx, db, "SELECT data FROM trajectory_metadata_blob LIMIT 1")
	if err != nil {
		return parseResult{}, err
	}
	if uri, ok := pbBytes(trajMeta, metaWorkspaceURI...); ok {
		r.workingDir = fileURIPath(string(uri))
	}
	r.startedAt, _ = pbTimestamp(trajMeta, metaCreatedAt...)

	return r, nil
}

func readCounts(ctx context.Context, db *sql.DB, r *parseResult) error {
	rows, err := db.QueryContext(ctx,
		"SELECT step_type, COUNT(*) FROM steps WHERE step_type IN (?, ?, ?) GROUP BY step_type",
		stepUserInput, stepPlannerResponse, stepToolCall)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var typ, n int
		if err := rows.Scan(&typ, &n); err != nil {
			return err
		}
		switch typ {
		case stepUserInput:
			r.totalUserMsgs = n
		case stepPlannerResponse:
			r.totalModelMsgs = n
		case stepToolCall:
			r.totalToolCalls = n
		}
	}
	return rows.Err()
}

// latestBlob returns the single blob selected by query, or nil when no row
// matches.
func latestBlob(ctx context.Context, db *sql.DB, query string, args ...any) ([]byte, error) {
	var b []byte
	err := db.QueryRowContext(ctx, query, args...).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// fileURIPath returns the local path of a file:// URI, or "" for anything
// else.
func fileURIPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return u.Path
}

// buildUpdate converts a parseResult plus cursor deltas into a SourceUpdate.
func buildUpdate(r parseResult, id, workingDir string, msgDelta, toolDelta int) source.SourceUpdate {
	if r.workingDir != "" {
		workingDir = r.workingDir
	}
	return source.SourceUpdate{
		SessionID:          id,
		Activity:           r.activity,
		Model:              r.model,
		HasTokens:          r.hasTokens,
		ContextTokens:      r.contextTokens,
		OutputTokens:       r.outputTokens,
		MessageCountDelta:  msgDelta,
		ToolCallCountDelta: toolDelta,
		CurrentTool:        r.currentTool,
		WorkingDir:         workingDir,
		StartedAt:          r.startedAt,
		LastActivityAt:     r.lastActivityAt,
	}
}
