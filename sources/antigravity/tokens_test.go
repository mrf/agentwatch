package antigravity

import "testing"

func TestParseSession_HasTokensOnlyWithUsageMetadata(t *testing.T) {
	r, err := parseSession([]byte(`{"messages":[{"role":"user","parts":[{"text":"hi"}]}],"model":"gemini-2.5-pro"}`))
	if err != nil {
		t.Fatalf("parseSession: %v", err)
	}
	if buildUpdate(r, "id", "/w", 0, 0).HasTokens {
		t.Error("no usageMetadata: HasTokens = true, want false")
	}
}
