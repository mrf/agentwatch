package opencode_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/mrf/agentwatch/session"
	"github.com/mrf/agentwatch/source"
)

const (
	userMsg        = `{"role":"user","time":{"created":1768471800000}}`
	assistantOpen  = `{"role":"assistant","time":{"created":1768471801000}}`
	assistantStop  = `{"role":"assistant","time":{"created":1768471801000,"completed":1768471805000},"finish":"stop"}`
	assistantTools = `{"role":"assistant","time":{"created":1768471801000,"completed":1768471803000},"finish":"tool-calls"}`
	assistantBare  = `{"role":"assistant"}`
	toolPart       = `{"type":"tool-invocation","toolName":"Edit"}`
	partTS         = "2026-01-15T10:00:02Z"
	partTS2        = "2026-01-15T10:00:03Z"
)

func TestParseActivityTable(t *testing.T) {
	tests := []struct {
		name     string
		messages []string // data JSON, one message each, in order
		toolPart bool
		want     session.Activity
	}{
		{"user message", []string{userMsg}, false, session.ActivityWorking},
		{"user then finished assistant", []string{userMsg, assistantStop}, false, session.ActivityWaiting},
		{"finished assistant with tool parts", []string{userMsg, assistantStop}, true, session.ActivityWaiting},
		{"in-progress assistant", []string{userMsg, assistantOpen}, false, session.ActivityWorking},
		{"in-progress assistant with tool parts", []string{userMsg, assistantOpen}, true, session.ActivityWorking},
		{"assistant finished with tool-calls", []string{userMsg, assistantTools}, false, session.ActivityWorking},
		{"assistant without markers", []string{userMsg, assistantBare}, false, session.ActivityWorking},
		{"assistant without markers, tool parts", []string{userMsg, assistantBare}, true, session.ActivityWorking},
	}
	for i := 0; i < len(tests); i++ {
		tc := tests[i]
		t.Run(tc.name, func(t *testing.T) {
			dbPath := seedFileDB(t, func(db *sql.DB) {
				insertSession(t, db, "s", "", "/p", `{"id":"m"}`, 1, 1,
					"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
				last := ""
				for j := 0; j < len(tc.messages); j++ {
					id := "msg-" + string(rune('a'+j))
					ts := "2026-01-15T10:00:0" + string(rune('0'+j)) + "Z"
					insertMessage(t, db, id, "s", tc.messages[j], ts, ts)
					last = id
				}
				if tc.toolPart {
					insertPart(t, db, "p1", last, "s", toolPart, partTS, partTS)
				}
			})
			src := newSource(t, dbPath)
			h := source.SessionHandle{ID: "s", Path: dbPath, Source: "opencode"}
			u, _, err := src.Parse(context.Background(), h, "")
			if err != nil {
				t.Fatal(err)
			}
			if u.Activity != tc.want {
				t.Errorf("Activity = %q, want %q", u.Activity, tc.want)
			}
		})
	}
}

// Streaming rewrites parts in place: message and tool counts stay put, yet the
// session must report Working, then Waiting once the message completes, then
// go silent again.
func TestParseStreamingReplyReportsWorking(t *testing.T) {
	dbPath := seedFileDB(t, func(db *sql.DB) {
		insertSession(t, db, "s", "", "/p", `{"id":"m"}`, 1, 1,
			"2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z", "")
		insertMessage(t, db, "m1", "s", userMsg, "2026-01-15T10:00:00Z", "2026-01-15T10:00:00Z")
		insertMessage(t, db, "m2", "s", assistantOpen, "2026-01-15T10:00:01Z", "2026-01-15T10:00:01Z")
		insertPart(t, db, "p1", "m2", "s", `{"type":"text","text":"he"}`, partTS, partTS)
	})
	src := newSource(t, dbPath)
	h := source.SessionHandle{ID: "s", Path: dbPath, Source: "opencode"}
	ctx := context.Background()

	u1, c1, err := src.Parse(ctx, h, "")
	if err != nil {
		t.Fatal(err)
	}
	if u1.Activity != session.ActivityWorking {
		t.Fatalf("first Activity = %q, want working", u1.Activity)
	}

	addDataToDB(t, dbPath, func(db *sql.DB) {
		if _, err := db.Exec(`UPDATE part SET data = '{"type":"text","text":"hello wor"}', updated_at = ? WHERE id = 'p1'`, partTS2); err != nil {
			t.Fatal(err)
		}
	})
	u2, c2, err := src.Parse(ctx, h, c1)
	if err != nil {
		t.Fatal(err)
	}
	if u2.SessionID == "" {
		t.Fatal("streamed part update produced no update")
	}
	if u2.Activity != session.ActivityWorking {
		t.Errorf("streaming Activity = %q, want working", u2.Activity)
	}
	if u2.MessageCountDelta != 0 || u2.ToolCallCountDelta != 0 {
		t.Errorf("deltas = %d/%d, want 0/0", u2.MessageCountDelta, u2.ToolCallCountDelta)
	}

	addDataToDB(t, dbPath, func(db *sql.DB) {
		if _, err := db.Exec(`UPDATE message SET data = ? WHERE id = 'm2'`, assistantStop); err != nil {
			t.Fatal(err)
		}
	})
	u3, c3, err := src.Parse(ctx, h, c2)
	if err != nil {
		t.Fatal(err)
	}
	if u3.SessionID == "" {
		t.Fatal("completion produced no update")
	}
	if u3.Activity != session.ActivityWaiting {
		t.Errorf("completed Activity = %q, want waiting", u3.Activity)
	}

	u4, _, err := src.Parse(ctx, h, c3)
	if err != nil {
		t.Fatal(err)
	}
	if u4.SessionID != "" {
		t.Errorf("unchanged session returned non-zero update: %+v", u4)
	}
}
