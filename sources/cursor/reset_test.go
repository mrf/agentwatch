package cursor_test

import (
	"testing"

	"github.com/mrf/agentwatch/internal/testutil"
	"github.com/mrf/agentwatch/sources/cursor"
)

func TestParse_TruncationAndRotationReset(t *testing.T) {
	h := writeTranscript(t, "home-user-proj", "rot001", "")
	testutil.CheckRewriteResets(t, cursor.New(), h,
		flatFixture,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"done"}]}}`+"\n",
	)
}
