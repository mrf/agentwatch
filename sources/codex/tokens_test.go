package codex_test

import (
	"context"
	"os"
	"testing"

	"github.com/mrf/agentwatch/sources/codex"
)

func TestParse_HasTokensOnlyWhenTokenCountSeen(t *testing.T) {
	dir, h := writeRollout(t, "01234567-abcd-ef01-2345-67890abcdef0", newEnvelopeFixture)
	src := codex.New(codex.WithRoot(dir))

	u1, c1, err := src.Parse(context.Background(), h, "")
	if err != nil {
		t.Fatalf("Parse 1: %v", err)
	}
	if !u1.HasTokens {
		t.Error("batch with token_count: HasTokens = false, want true")
	}

	f, err := os.OpenFile(h.Path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"event_msg","payload":{"type":"agent_message"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	u2, _, err := src.Parse(context.Background(), h, c1)
	if err != nil {
		t.Fatalf("Parse 2: %v", err)
	}
	if u2.HasTokens || u2.ContextTokens != 0 || u2.OutputTokens != 0 {
		t.Errorf("agent_message-only batch: HasTokens=%v ctx=%d out=%d, want unset", u2.HasTokens, u2.ContextTokens, u2.OutputTokens)
	}
}
