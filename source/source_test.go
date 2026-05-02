package source_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mrf/agentwatch/source"
)

// mockSource is a minimal Source implementation for testing.
type mockSource struct {
	name string
}

func (m *mockSource) Name() string { return m.name }

func (m *mockSource) Discover(_ context.Context) ([]source.SessionHandle, error) {
	return nil, nil
}

func (m *mockSource) Parse(_ context.Context, _ source.SessionHandle, c source.Cursor) (source.SourceUpdate, source.Cursor, error) {
	return source.SourceUpdate{}, c, nil
}

// ---- Registry tests ----

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := source.NewRegistry()

	err := r.Register("mock", func() (source.Source, error) {
		return &mockSource{name: "mock"}, nil
	})
	if err != nil {
		t.Fatalf("Register: unexpected error: %v", err)
	}

	f, ok := r.Get("mock")
	if !ok {
		t.Fatal("Get: expected factory to be present")
	}

	s, err := f()
	if err != nil {
		t.Fatalf("Factory: unexpected error: %v", err)
	}
	if s.Name() != "mock" {
		t.Errorf("Factory source name = %q, want %q", s.Name(), "mock")
	}
}

func TestRegistry_GetMissing(t *testing.T) {
	r := source.NewRegistry()
	_, ok := r.Get("nonexistent")
	if ok {
		t.Error("Get: expected false for missing name")
	}
}

func TestRegistry_DuplicateRegistration(t *testing.T) {
	r := source.NewRegistry()

	factory := func() (source.Source, error) { return &mockSource{name: "dup"}, nil }

	if err := r.Register("dup", factory); err != nil {
		t.Fatalf("first Register: unexpected error: %v", err)
	}

	err := r.Register("dup", factory)
	if err == nil {
		t.Fatal("second Register: expected error for duplicate name, got nil")
	}
	if !errors.Is(err, source.ErrAlreadyRegistered) {
		t.Errorf("second Register: error = %v, want ErrAlreadyRegistered", err)
	}
}

func TestRegistry_Names(t *testing.T) {
	r := source.NewRegistry()

	names := r.Names()
	if len(names) != 0 {
		t.Errorf("Names on empty registry = %v, want []", names)
	}

	_ = r.Register("b", func() (source.Source, error) { return &mockSource{name: "b"}, nil })
	_ = r.Register("a", func() (source.Source, error) { return &mockSource{name: "a"}, nil })
	_ = r.Register("c", func() (source.Source, error) { return &mockSource{name: "c"}, nil })

	names = r.Names()
	if len(names) != 3 {
		t.Fatalf("Names = %v, want 3 entries", names)
	}
	// Names must be sorted for deterministic output.
	want := []string{"a", "b", "c"}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("Names[%d] = %q, want %q", i, n, want[i])
		}
	}
}

func TestRegistry_FactoryExecution(t *testing.T) {
	r := source.NewRegistry()

	calls := 0
	_ = r.Register("counted", func() (source.Source, error) {
		calls++
		return &mockSource{name: "counted"}, nil
	})

	f, _ := r.Get("counted")

	s1, _ := f()
	s2, _ := f()

	if calls != 2 {
		t.Errorf("factory called %d times, want 2", calls)
	}
	if s1 == s2 {
		t.Error("factory returned the same instance twice; expected independent instances")
	}
}

// ---- SessionHandle and SourceUpdate type shape tests ----
// These tests verify the struct fields exist with the expected types.

func TestSessionHandle_Fields(t *testing.T) {
	h := source.SessionHandle{
		ID:         "sess-1",
		Path:       "/tmp/session.jsonl",
		WorkingDir: "/home/user/project",
		StartedAt:  time.Now(),
		Source:     "claude",
	}
	if h.ID != "sess-1" {
		t.Errorf("ID = %q, want %q", h.ID, "sess-1")
	}
}

func TestSourceUpdate_Fields(t *testing.T) {
	// Verify the struct compiles and all required fields are accessible.
	u := source.SourceUpdate{
		SessionID:          "sess-1",
		Slug:               "my-task",
		Model:              "claude-opus-4-6",
		ContextTokens:      1000,
		OutputTokens:       200,
		MaxContextTokens:   200000,
		TokenEstimated:     false,
		MessageCountDelta:  1,
		ToolCallCountDelta: 2,
		CurrentTool:        "bash",
		WorkingDir:         "/home/user/project",
		Branch:             "main",
		StartedAt:          time.Now(),
		LastActivityAt:     time.Now(),
		Terminal:           false,
		EndReason:          "",
		EndedAt:            time.Time{},
	}
	if u.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", u.SessionID, "sess-1")
	}
}
