package session_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrf/agentwatch/session"
)

func testKey(b byte) []byte {
	return bytes.Repeat([]byte{b}, 32)
}

func mustPseudonymizer(t *testing.T, opts ...session.PseudonymizerOption) *session.Pseudonymizer {
	t.Helper()
	z, err := session.NewPseudonymizer(opts...)
	if err != nil {
		t.Fatalf("NewPseudonymizer: %v", err)
	}
	return z
}

func TestPseudonymizer_StableAndDistinct(t *testing.T) {
	t.Parallel()
	z := mustPseudonymizer(t, session.WithPseudonymKey(testKey(1)))

	a1 := z.Pseudonym("sess-abc")
	a2 := z.Pseudonym("sess-abc")
	b := z.Pseudonym("sess-def")

	if a1 != a2 {
		t.Errorf("same ID gave different pseudonyms: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Errorf("different IDs collided: %q", a1)
	}
	if a1 == "" || strings.Contains(a1, "sess-abc") {
		t.Errorf("pseudonym %q is empty or leaks the ID", a1)
	}
	if len(a1) != len(b) {
		t.Errorf("pseudonym length not stable: %d vs %d", len(a1), len(b))
	}
}

func TestPseudonymizer_EmptyStaysEmpty(t *testing.T) {
	t.Parallel()
	z := mustPseudonymizer(t, session.WithPseudonymKey(testKey(1)))
	if got := z.Pseudonym(""); got != "" {
		t.Errorf("empty ID should stay empty, got %q", got)
	}
}

func TestPseudonymizer_DifferentKeysDiffer(t *testing.T) {
	t.Parallel()
	z1 := mustPseudonymizer(t, session.WithPseudonymKey(testKey(1)))
	z2 := mustPseudonymizer(t, session.WithPseudonymKey(testKey(2)))
	if z1.Pseudonym("sess-abc") == z2.Pseudonym("sess-abc") {
		t.Error("different keys produced the same pseudonym")
	}
}

func TestPseudonymizer_SameKeySharesCorrelation(t *testing.T) {
	t.Parallel()
	z1 := mustPseudonymizer(t, session.WithPseudonymKey(testKey(7)))
	z2 := mustPseudonymizer(t, session.WithPseudonymKey(testKey(7)))
	if z1.Pseudonym("sess-abc") != z2.Pseudonym("sess-abc") {
		t.Error("same key should produce the same pseudonym across instances")
	}
}

func TestPseudonymizer_DefaultKeyIsRandom(t *testing.T) {
	t.Parallel()
	z1 := mustPseudonymizer(t)
	z2 := mustPseudonymizer(t)
	if z1.Pseudonym("sess-abc") == z2.Pseudonym("sess-abc") {
		t.Error("two default pseudonymizers share a key")
	}
}

func TestPseudonymizer_RejectsShortKey(t *testing.T) {
	t.Parallel()
	if _, err := session.NewPseudonymizer(session.WithPseudonymKey(make([]byte, 31))); err == nil {
		t.Error("expected error for key shorter than 32 bytes")
	}
}

func TestPseudonymizer_KeyIsCopied(t *testing.T) {
	t.Parallel()
	key := testKey(3)
	z := mustPseudonymizer(t, session.WithPseudonymKey(key))
	before := z.Pseudonym("sess-abc")
	key[0] ^= 0xff
	if got := z.Pseudonym("sess-abc"); got != before {
		t.Error("mutating the caller's key slice changed pseudonyms")
	}
}

func TestPolicy_Apply_Pseudonymize(t *testing.T) {
	t.Parallel()
	z := mustPseudonymizer(t, session.WithPseudonymKey(testKey(1)))
	s := fullState()
	got := session.Policy{Pseudonymizer: z}.Apply(s)

	if got.ID != z.Pseudonym(s.ID) {
		t.Errorf("ID: got %q, want pseudonym %q", got.ID, z.Pseudonym(s.ID))
	}
	if got.Slug != z.Pseudonym(s.Slug) {
		t.Errorf("Slug: got %q, want pseudonym", got.Slug)
	}
	sub := got.Subagents[0]
	if sub.ID != z.Pseudonym(s.Subagents[0].ID) {
		t.Errorf("Subagent ID: got %q, want pseudonym", sub.ID)
	}
	if sub.Slug != z.Pseudonym(s.Subagents[0].Slug) {
		t.Errorf("Subagent Slug: got %q, want pseudonym", sub.Slug)
	}
	if sub.ParentID != got.ID {
		t.Errorf("Subagent ParentID %q must equal parent's pseudonymous ID %q", sub.ParentID, got.ID)
	}

	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, secret := range []string{s.ID, s.Slug, s.Subagents[0].ID, s.Subagents[0].Slug} {
		if strings.Contains(string(data), secret) {
			t.Errorf("serialized output leaks %q: %s", secret, data)
		}
	}
	if got.WorkingDir != s.WorkingDir {
		t.Error("Pseudonymizer alone must not touch other fields")
	}
	if s.ID != "sess-abc" {
		t.Error("Apply mutated the original")
	}
}

// Pseudonymization wins over blanking so enabling both never drops correlation.
func TestPolicy_Apply_PseudonymizeOverridesRedactSessionID(t *testing.T) {
	t.Parallel()
	z := mustPseudonymizer(t, session.WithPseudonymKey(testKey(1)))
	got := session.Policy{RedactSessionID: true, Pseudonymizer: z}.Apply(fullState())
	if got.ID != z.Pseudonym("sess-abc") {
		t.Errorf("ID: got %q, want pseudonym", got.ID)
	}
}

func TestPolicy_SessionID(t *testing.T) {
	t.Parallel()
	z := mustPseudonymizer(t, session.WithPseudonymKey(testKey(1)))
	cases := []struct {
		name   string
		policy session.Policy
		want   string
	}{
		{"passthrough", session.Policy{}, "sess-abc"},
		{"redact", session.Policy{RedactSessionID: true}, ""},
		{"pseudonymize", session.Policy{Pseudonymizer: z}, z.Pseudonym("sess-abc")},
	}
	for i := 0; i < len(cases); i++ {
		tc := cases[i]
		if got := tc.policy.SessionID("sess-abc"); got != tc.want {
			t.Errorf("%s: SessionID = %q, want %q", tc.name, got, tc.want)
		}
	}
}
