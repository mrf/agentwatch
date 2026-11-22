package codex_test

import (
	"strconv"
	"testing"

	"github.com/mrf/agentwatch/internal/jsonl"
	"github.com/mrf/agentwatch/internal/testutil"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/codex"
)

// cursorOffset returns the byte offset inside an opaque codex cursor.
func cursorOffset(t *testing.T, c source.Cursor) string {
	t.Helper()
	pos, err := jsonl.ParsePosition(string(c))
	if err != nil {
		t.Fatalf("cursor %q: %v", c, err)
	}
	return strconv.FormatInt(pos.Offset, 10)
}

func TestParse_TruncationAndRotationReset(t *testing.T) {
	root, h := writeRollout(t, "01234567-abcd-ef01-2345-67890abcdef0", "")
	testutil.CheckRewriteResets(t, codex.New(codex.WithRoot(root)), h,
		newEnvelopeFixture,
		`{"type":"response_item","payload":{"type":"message","role":"user"}}`+"\n",
	)
}
