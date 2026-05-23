package session_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/mrf/agentwatch/session"
)

// fullState returns a SessionState with every field populated.
func fullState() session.SessionState {
	now := time.Now().UTC()
	completed := now.Add(time.Hour)
	return session.SessionState{
		ID:                 "sess-abc",
		Source:             "claude",
		Slug:               "my-feature",
		Activity:           session.ActivityWorking,
		Lifecycle:          session.LifecycleActive,
		ContextTokens:      1000,
		OutputTokens:       200,
		TokenEstimated:     true,
		MaxContextTokens:   8000,
		ContextUtilization: 0.125,
		Model:              "claude-opus-4-6",
		WorkingDir:         "/home/user/projects/secret",
		Branch:             "feat/secret-feature",
		CurrentTool:        "Bash",
		MessageCount:       10,
		ToolCallCount:      5,
		StartedAt:          now,
		LastActivityAt:     now,
		LastDataReceivedAt: now,
		CompletedAt:        &completed,
		Subagents: []session.SubagentState{
			{
				ID:             "sub-1",
				ParentID:       "sess-abc",
				Activity:       session.ActivityIdle,
				CurrentTool:    "Read",
				StartedAt:      now,
				LastActivityAt: now,
				Model:          "claude-sonnet-4-6",
				ContextTokens:  500,
				OutputTokens:   50,
				MessageCount:   3,
				ToolCallCount:  2,
				CompletedAt:    now.Add(30 * time.Minute),
			},
		},
	}
}

// --- Policy.Apply passthrough ---

func TestPolicy_Apply_Passthrough(t *testing.T) {
	t.Parallel()
	s := fullState()
	got := session.Policy{}.Apply(s)

	if got.WorkingDir != s.WorkingDir {
		t.Errorf("WorkingDir: got %q, want %q", got.WorkingDir, s.WorkingDir)
	}
	if got.Branch != s.Branch {
		t.Errorf("Branch: got %q, want %q", got.Branch, s.Branch)
	}
	if got.Model != s.Model {
		t.Errorf("Model: got %q, want %q", got.Model, s.Model)
	}
	if got.ID != s.ID {
		t.Errorf("ID: got %q, want %q", got.ID, s.ID)
	}
	if got.Source != s.Source {
		t.Errorf("Source: got %q, want %q", got.Source, s.Source)
	}
}

// --- WorkingDir redaction ---

func TestPolicy_Apply_RedactWorkingDir(t *testing.T) {
	t.Parallel()
	s := fullState()
	got := session.Policy{RedactWorkingDir: true}.Apply(s)

	if got.WorkingDir != "" {
		t.Errorf("WorkingDir: expected empty, got %q", got.WorkingDir)
	}
	if got.Branch != s.Branch {
		t.Errorf("Branch should not be redacted: got %q", got.Branch)
	}
}

// --- Branch redaction ---

func TestPolicy_Apply_RedactBranch(t *testing.T) {
	t.Parallel()
	s := fullState()
	got := session.Policy{RedactBranch: true}.Apply(s)

	if got.Branch != "" {
		t.Errorf("Branch: expected empty, got %q", got.Branch)
	}
	if got.WorkingDir != s.WorkingDir {
		t.Errorf("WorkingDir should not be redacted: got %q", got.WorkingDir)
	}
}

// --- Model redaction ---

func TestPolicy_Apply_RedactModel(t *testing.T) {
	t.Parallel()
	s := fullState()
	got := session.Policy{RedactModel: true}.Apply(s)

	if got.Model != "" {
		t.Errorf("Model: expected empty, got %q", got.Model)
	}
	if got.WorkingDir != s.WorkingDir {
		t.Errorf("WorkingDir should not be redacted: got %q", got.WorkingDir)
	}
	// Subagent model should also be redacted.
	if len(got.Subagents) > 0 && got.Subagents[0].Model != "" {
		t.Errorf("Subagent.Model: expected empty after RedactModel, got %q", got.Subagents[0].Model)
	}
}

// --- SessionID redaction (ID + Slug + subagent IDs) ---

func TestPolicy_Apply_RedactSessionID(t *testing.T) {
	t.Parallel()
	s := fullState()
	got := session.Policy{RedactSessionID: true}.Apply(s)

	if got.ID != "" {
		t.Errorf("ID: expected empty, got %q", got.ID)
	}
	if got.Slug != "" {
		t.Errorf("Slug: expected empty, got %q", got.Slug)
	}
	// Subagent IDs must also be cleared.
	for i, sub := range got.Subagents {
		if sub.ID != "" {
			t.Errorf("Subagents[%d].ID: expected empty, got %q", i, sub.ID)
		}
		if sub.ParentID != "" {
			t.Errorf("Subagents[%d].ParentID: expected empty, got %q", i, sub.ParentID)
		}
	}
	// Non-ID fields must survive.
	if got.Source != s.Source {
		t.Errorf("Source should not be redacted: got %q", got.Source)
	}
}

// --- Source redaction ---

func TestPolicy_Apply_RedactSource(t *testing.T) {
	t.Parallel()
	s := fullState()
	got := session.Policy{RedactSource: true}.Apply(s)

	if got.Source != "" {
		t.Errorf("Source: expected empty, got %q", got.Source)
	}
	if got.ID != s.ID {
		t.Errorf("ID should not be redacted: got %q", got.ID)
	}
}

// --- Full redaction: sensitive fields cleared, non-sensitive preserved ---

func TestPolicy_Apply_FullRedaction(t *testing.T) {
	t.Parallel()
	s := fullState()
	p := session.Policy{
		RedactWorkingDir: true,
		RedactBranch:     true,
		RedactModel:      true,
		RedactSessionID:  true,
		RedactSource:     true,
	}
	got := p.Apply(s)

	// All sensitive fields must be cleared.
	if got.WorkingDir != "" {
		t.Errorf("WorkingDir not redacted: %q", got.WorkingDir)
	}
	if got.Branch != "" {
		t.Errorf("Branch not redacted: %q", got.Branch)
	}
	if got.Model != "" {
		t.Errorf("Model not redacted: %q", got.Model)
	}
	if got.ID != "" {
		t.Errorf("ID not redacted: %q", got.ID)
	}
	if got.Slug != "" {
		t.Errorf("Slug not redacted: %q", got.Slug)
	}
	if got.Source != "" {
		t.Errorf("Source not redacted: %q", got.Source)
	}
	for i, sub := range got.Subagents {
		if sub.ID != "" {
			t.Errorf("Subagents[%d].ID not redacted: %q", i, sub.ID)
		}
		if sub.ParentID != "" {
			t.Errorf("Subagents[%d].ParentID not redacted: %q", i, sub.ParentID)
		}
		if sub.Model != "" {
			t.Errorf("Subagents[%d].Model not redacted: %q", i, sub.Model)
		}
	}

	// Non-sensitive fields must survive.
	if got.Activity != s.Activity {
		t.Errorf("Activity changed: got %q", got.Activity)
	}
	if got.Lifecycle != s.Lifecycle {
		t.Errorf("Lifecycle changed: got %q", got.Lifecycle)
	}
	if got.ContextTokens != s.ContextTokens {
		t.Errorf("ContextTokens changed: got %d", got.ContextTokens)
	}
	if got.OutputTokens != s.OutputTokens {
		t.Errorf("OutputTokens changed: got %d", got.OutputTokens)
	}
	if got.TokenEstimated != s.TokenEstimated {
		t.Errorf("TokenEstimated changed: got %v", got.TokenEstimated)
	}
	if got.MaxContextTokens != s.MaxContextTokens {
		t.Errorf("MaxContextTokens changed: got %d", got.MaxContextTokens)
	}
	if got.ContextUtilization != s.ContextUtilization {
		t.Errorf("ContextUtilization changed: got %v", got.ContextUtilization)
	}
	if got.CurrentTool != s.CurrentTool {
		t.Errorf("CurrentTool changed: got %q", got.CurrentTool)
	}
	if got.MessageCount != s.MessageCount {
		t.Errorf("MessageCount changed: got %d", got.MessageCount)
	}
	if got.ToolCallCount != s.ToolCallCount {
		t.Errorf("ToolCallCount changed: got %d", got.ToolCallCount)
	}
	if !got.StartedAt.Equal(s.StartedAt) {
		t.Errorf("StartedAt changed: got %v", got.StartedAt)
	}
	if len(got.Subagents) != len(s.Subagents) {
		t.Errorf("Subagents length changed: got %d", len(got.Subagents))
	}
	if len(got.Subagents) > 0 {
		if got.Subagents[0].Activity != s.Subagents[0].Activity {
			t.Errorf("Subagents[0].Activity changed: got %q", got.Subagents[0].Activity)
		}
		if got.Subagents[0].CurrentTool != s.Subagents[0].CurrentTool {
			t.Errorf("Subagents[0].CurrentTool changed: got %q", got.Subagents[0].CurrentTool)
		}
	}
}

// --- Apply does not mutate the original ---

func TestPolicy_Apply_DoesNotMutateOriginal(t *testing.T) {
	t.Parallel()
	s := fullState()
	origWorkingDir := s.WorkingDir
	origBranch := s.Branch
	origModel := s.Model
	origID := s.ID
	origSubagentID := s.Subagents[0].ID

	p := session.Policy{
		RedactWorkingDir: true,
		RedactBranch:     true,
		RedactModel:      true,
		RedactSessionID:  true,
		RedactSource:     true,
	}
	_ = p.Apply(s)

	if s.WorkingDir != origWorkingDir {
		t.Errorf("original.WorkingDir mutated: got %q", s.WorkingDir)
	}
	if s.Branch != origBranch {
		t.Errorf("original.Branch mutated: got %q", s.Branch)
	}
	if s.Model != origModel {
		t.Errorf("original.Model mutated: got %q", s.Model)
	}
	if s.ID != origID {
		t.Errorf("original.ID mutated: got %q", s.ID)
	}
	if s.Subagents[0].ID != origSubagentID {
		t.Errorf("original.Subagents[0].ID mutated: got %q", s.Subagents[0].ID)
	}
}

// --- Nil subagents handled gracefully ---

func TestPolicy_Apply_NilSubagents(t *testing.T) {
	t.Parallel()
	s := session.SessionState{
		ID:         "sess-1",
		WorkingDir: "/secret",
		Branch:     "main",
		Model:      "claude-opus-4-6",
		Subagents:  nil,
	}
	p := session.Policy{RedactSessionID: true, RedactWorkingDir: true}
	got := p.Apply(s)

	if got.Subagents != nil {
		t.Errorf("Subagents should remain nil: got %v", got.Subagents)
	}
	if got.WorkingDir != "" {
		t.Errorf("WorkingDir not redacted: %q", got.WorkingDir)
	}
}

// --- Field coverage sentinel ---
//
// This test enumerates the exported fields of SessionState and SubagentState
// using reflection and verifies that each is explicitly listed in the
// privacyReviewedFields set below. If a new field is added to either struct
// without updating this set, the test fails, forcing a privacy review of the
// new field before it ships.
//
// To add a new field: review it for privacy sensitivity, add its handling to
// Policy.Apply if needed, then add its name to the reviewed set.

func TestPrivacyFilter_FieldCoverage(t *testing.T) {
	t.Parallel()

	// All exported fields of SessionState reviewed for privacy sensitivity.
	// Mark sensitive fields with (*).
	sessionStateReviewed := map[string]bool{
		"ID":                 true, // (*) session identifier — RedactSessionID
		"Source":             true, // (*) source name — RedactSource
		"Slug":               true, // (*) short identifier — RedactSessionID
		"Activity":           true, // not sensitive
		"Lifecycle":          true, // not sensitive
		"ContextTokens":      true, // not sensitive
		"OutputTokens":       true, // not sensitive
		"TokenEstimated":     true, // not sensitive
		"MaxContextTokens":   true, // not sensitive
		"ContextUtilization": true, // not sensitive
		"Model":              true, // (*) model name — RedactModel
		"WorkingDir":         true, // (*) local path — RedactWorkingDir
		"Branch":             true, // (*) branch name — RedactBranch
		"CurrentTool":        true, // not sensitive (tool name, not content)
		"MessageCount":       true, // not sensitive
		"ToolCallCount":      true, // not sensitive
		"CompactionCount":    true, // not sensitive (aggregate metric)
		"StartedAt":          true, // not sensitive
		"LastActivityAt":     true, // not sensitive
		"LastDataReceivedAt": true, // not sensitive
		"CompletedAt":        true, // not sensitive
		"Subagents":          true, // reviewed via SubagentState coverage
	}

	subagentStateReviewed := map[string]bool{
		"ID":             true, // (*) identifier — cleared by RedactSessionID
		"ParentID":       true, // (*) parent identifier — cleared by RedactSessionID
		"Slug":           true, // not sensitive — user-visible session label
		"Activity":       true, // not sensitive
		"CurrentTool":    true, // not sensitive
		"StartedAt":      true, // not sensitive
		"LastActivityAt": true, // not sensitive
		"Model":          true, // (*) model name — RedactModel (same sensitivity as SessionState.Model)
		"ContextTokens":  true, // not sensitive
		"OutputTokens":   true, // not sensitive
		"MessageCount":   true, // not sensitive
		"ToolCallCount":  true, // not sensitive
		"CompletedAt":    true, // not sensitive
	}

	checkStruct := func(t *testing.T, typ reflect.Type, reviewed map[string]bool) {
		t.Helper()
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			if !reviewed[f.Name] {
				t.Errorf("field %s.%s is not in the privacy review set — add it and assess sensitivity",
					typ.Name(), f.Name)
			}
		}
	}

	checkStruct(t, reflect.TypeOf(session.SessionState{}), sessionStateReviewed)
	checkStruct(t, reflect.TypeOf(session.SubagentState{}), subagentStateReviewed)
}
