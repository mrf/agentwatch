package mock_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
)

var t0 = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

func handle(id string) source.SessionHandle {
	return source.SessionHandle{
		ID:        id,
		Path:      "/tmp/" + id,
		Source:    "mock",
		StartedAt: t0,
	}
}

func update(sessionID string, activity session.Activity) source.SourceUpdate {
	return source.SourceUpdate{
		SessionID: sessionID,
		Activity:  activity,
	}
}

// TestName verifies the default and overridden source name.
func TestName(t *testing.T) {
	s := mock.New()
	if s.Name() != "mock" {
		t.Fatalf("default name: got %q, want %q", s.Name(), "mock")
	}

	s2 := mock.New(mock.WithName("custom"))
	if s2.Name() != "custom" {
		t.Fatalf("custom name: got %q, want %q", s2.Name(), "custom")
	}
}

// TestDiscoverEmpty verifies that exhausted queue returns nil, nil.
func TestDiscoverEmpty(t *testing.T) {
	s := mock.New()
	handles, err := s.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handles != nil {
		t.Fatalf("expected nil handles, got %v", handles)
	}
}

// TestDiscoverSingleResult verifies a single queued discover result.
func TestDiscoverSingleResult(t *testing.T) {
	h1 := handle("sess-1")
	h2 := handle("sess-2")
	s := mock.New(mock.WithHandles(h1, h2))

	handles, err := s.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(handles) != 2 {
		t.Fatalf("expected 2 handles, got %d", len(handles))
	}
	if handles[0].ID != "sess-1" || handles[1].ID != "sess-2" {
		t.Fatalf("unexpected handles: %v", handles)
	}

	// Second call: queue exhausted
	handles2, err2 := s.Discover(context.Background())
	if err2 != nil {
		t.Fatalf("unexpected error on second call: %v", err2)
	}
	if handles2 != nil {
		t.Fatalf("expected nil after exhausted queue, got %v", handles2)
	}
}

// TestDiscoverMultipleResults verifies multiple queued discover results are returned in order.
func TestDiscoverMultipleResults(t *testing.T) {
	h1 := handle("sess-1")
	h2 := handle("sess-2")
	s := mock.New(
		mock.WithHandles(h1),
		mock.WithHandles(h2),
	)

	got1, _ := s.Discover(context.Background())
	got2, _ := s.Discover(context.Background())

	if len(got1) != 1 || got1[0].ID != "sess-1" {
		t.Fatalf("first discover: got %v", got1)
	}
	if len(got2) != 1 || got2[0].ID != "sess-2" {
		t.Fatalf("second discover: got %v", got2)
	}
}

// TestDiscoverError verifies error injection on Discover.
func TestDiscoverError(t *testing.T) {
	sentinel := errors.New("discover failed")
	s := mock.New(mock.WithDiscoverError(sentinel))

	_, err := s.Discover(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

// TestDiscoverCallRecording verifies that Discover calls are recorded.
func TestDiscoverCallRecording(t *testing.T) {
	s := mock.New()

	if s.DiscoverCallCount() != 0 {
		t.Fatalf("expected 0 calls before any Discover")
	}

	s.Discover(context.Background()) //nolint:errcheck
	s.Discover(context.Background()) //nolint:errcheck

	if s.DiscoverCallCount() != 2 {
		t.Fatalf("expected 2 discover calls, got %d", s.DiscoverCallCount())
	}
}

// TestParseNoScript verifies that Parse with no script returns zero update and same cursor.
func TestParseNoScript(t *testing.T) {
	s := mock.New()
	h := handle("sess-1")
	cursor := source.Cursor("cur:42")

	upd, nextCursor, err := s.Parse(context.Background(), h, cursor)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nextCursor != cursor {
		t.Fatalf("expected cursor unchanged: got %q, want %q", nextCursor, cursor)
	}
	if upd.SessionID != "" {
		t.Fatalf("expected zero update, got non-empty SessionID %q", upd.SessionID)
	}
}

// TestParseScriptedResult verifies a queued parse result is returned.
func TestParseScriptedResult(t *testing.T) {
	h := handle("sess-1")
	upd := update("sess-1", "working")
	nextCursor := source.Cursor("cur:99")

	s := mock.New(mock.WithParseResult("sess-1", upd, nextCursor, nil))

	got, gotCursor, err := s.Parse(context.Background(), h, source.Cursor("cur:0"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.SessionID != "sess-1" {
		t.Fatalf("expected SessionID sess-1, got %q", got.SessionID)
	}
	if gotCursor != nextCursor {
		t.Fatalf("expected cursor %q, got %q", nextCursor, gotCursor)
	}
}

// TestParseMultipleScriptedResults verifies results are returned in order and exhausted.
func TestParseMultipleScriptedResults(t *testing.T) {
	h := handle("sess-1")
	upd1 := source.SourceUpdate{SessionID: "sess-1", ContextTokens: 100}
	upd2 := source.SourceUpdate{SessionID: "sess-1", ContextTokens: 200}

	s := mock.New(
		mock.WithParseResult("sess-1", upd1, source.Cursor("c1"), nil),
		mock.WithParseResult("sess-1", upd2, source.Cursor("c2"), nil),
	)

	got1, cur1, _ := s.Parse(context.Background(), h, source.Cursor(""))
	got2, cur2, _ := s.Parse(context.Background(), h, cur1)
	// Third call: queue exhausted, should return same cursor
	_, cur3, _ := s.Parse(context.Background(), h, cur2)

	if got1.ContextTokens != 100 {
		t.Fatalf("first parse: expected 100 tokens, got %d", got1.ContextTokens)
	}
	if got2.ContextTokens != 200 {
		t.Fatalf("second parse: expected 200 tokens, got %d", got2.ContextTokens)
	}
	if cur3 != cur2 {
		t.Fatalf("exhausted queue: expected cursor unchanged %q, got %q", cur2, cur3)
	}
}

// TestParseErrorInjection verifies that parse errors are returned correctly.
func TestParseErrorInjection(t *testing.T) {
	sentinel := errors.New("parse failed")
	h := handle("sess-1")
	s := mock.New(mock.WithParseResult("sess-1", source.SourceUpdate{}, "", sentinel))

	_, _, err := s.Parse(context.Background(), h, source.Cursor(""))
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

// TestParseCallRecording verifies that Parse calls are recorded with args.
func TestParseCallRecording(t *testing.T) {
	h := handle("sess-1")
	cursor := source.Cursor("cur:42")
	s := mock.New()

	if s.ParseCallCount() != 0 {
		t.Fatalf("expected 0 parse calls before any Parse")
	}

	s.Parse(context.Background(), h, cursor) //nolint:errcheck

	calls := s.ParseCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 parse call, got %d", len(calls))
	}
	if calls[0].Handle.ID != "sess-1" {
		t.Fatalf("expected handle ID sess-1, got %q", calls[0].Handle.ID)
	}
	if calls[0].Cursor != cursor {
		t.Fatalf("expected cursor %q, got %q", cursor, calls[0].Cursor)
	}
}

// TestParseDefaultFallback verifies that the default parse result is used when queue is empty.
func TestParseDefaultFallback(t *testing.T) {
	h := handle("sess-1")
	defaultUpd := source.SourceUpdate{SessionID: "sess-1", ContextTokens: 999}
	s := mock.New(mock.WithDefaultParse(defaultUpd, source.Cursor("default"), nil))

	// No scripted result for sess-1 — should fall back to default.
	got, gotCursor, err := s.Parse(context.Background(), h, source.Cursor(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ContextTokens != 999 {
		t.Fatalf("expected default ContextTokens 999, got %d", got.ContextTokens)
	}
	if gotCursor != source.Cursor("default") {
		t.Fatalf("expected default cursor, got %q", gotCursor)
	}

	// Second call still uses default (it's not consumed).
	got2, _, _ := s.Parse(context.Background(), h, source.Cursor("default"))
	if got2.ContextTokens != 999 {
		t.Fatalf("default should not be consumed: expected 999, got %d", got2.ContextTokens)
	}
}

// TestDefaultFallbackAfterQueueExhausted verifies the default is used after scripted queue empties.
func TestDefaultFallbackAfterQueueExhausted(t *testing.T) {
	h := handle("sess-1")
	scripted := source.SourceUpdate{SessionID: "sess-1", ContextTokens: 1}
	dflt := source.SourceUpdate{SessionID: "sess-1", ContextTokens: 2}

	s := mock.New(
		mock.WithParseResult("sess-1", scripted, source.Cursor("c1"), nil),
		mock.WithDefaultParse(dflt, source.Cursor("default"), nil),
	)

	got1, _, _ := s.Parse(context.Background(), h, source.Cursor(""))
	got2, _, _ := s.Parse(context.Background(), h, source.Cursor("c1"))

	if got1.ContextTokens != 1 {
		t.Fatalf("expected scripted result first, got %d", got1.ContextTokens)
	}
	if got2.ContextTokens != 2 {
		t.Fatalf("expected default after queue exhausted, got %d", got2.ContextTokens)
	}
}

// TestQueueHandles verifies runtime mutation via QueueHandles.
func TestQueueHandles(t *testing.T) {
	s := mock.New()
	h := handle("sess-dynamic")
	s.QueueHandles(h)

	handles, err := s.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(handles) != 1 || handles[0].ID != "sess-dynamic" {
		t.Fatalf("expected dynamic handle, got %v", handles)
	}
}

// TestQueueParseResult verifies runtime mutation via QueueParseResult.
func TestQueueParseResult(t *testing.T) {
	s := mock.New()
	h := handle("sess-1")
	upd := source.SourceUpdate{SessionID: "sess-1", ContextTokens: 77}
	s.QueueParseResult("sess-1", upd, source.Cursor("c77"), nil)

	got, _, err := s.Parse(context.Background(), h, source.Cursor(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ContextTokens != 77 {
		t.Fatalf("expected 77 tokens, got %d", got.ContextTokens)
	}
}

// TestImplementsSourceInterface is a compile-time check.
func TestImplementsSourceInterface(t *testing.T) {
	var _ source.Source = mock.New()
}

// TestConcurrentAccess verifies there are no data races under concurrent use.
func TestConcurrentAccess(t *testing.T) {
	h := handle("sess-1")
	s := mock.New(
		mock.WithHandles(h),
		mock.WithParseResult("sess-1", source.SourceUpdate{SessionID: "sess-1"}, source.Cursor("c1"), nil),
	)

	ctx := context.Background()
	done := make(chan struct{})

	for i := 0; i < 4; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 10; j++ {
				s.Discover(ctx)               //nolint:errcheck
				s.Parse(ctx, h, source.Cursor("")) //nolint:errcheck
				s.DiscoverCallCount()
				s.ParseCallCount()
				s.ParseCalls()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
}
