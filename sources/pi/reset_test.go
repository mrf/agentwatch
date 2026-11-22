package pi_test

import (
	"testing"

	"github.com/mrf/agentwatch/internal/testutil"
	"github.com/mrf/agentwatch/sources/pi"
)

func TestParse_TruncationAndRotationReset(t *testing.T) {
	root, h := writeSession(t, "full001", "")
	testutil.CheckRewriteResets(t, pi.New(pi.WithRoot(root)), h,
		fullSessionFixture,
		`{"type":"message","id":"m9","parentId":"m2","timestamp":"2024-01-15T11:00:00Z","message":{"role":"user","content":"again"}}`+"\n",
	)
}
