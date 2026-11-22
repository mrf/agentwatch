package claude

import "maps"

// defaultContextWindows maps exact model IDs, as they appear in transcript
// assistant records, to their context window in tokens. A model absent from
// the table has an unknown window and reports MaxContextTokens 0, so its
// utilization stays 0 rather than guessing; extend it with WithContextWindows.
//
// Sources: Anthropic's models overview and the 1M-context GA announcement
// (Opus 4.6 / Sonnet 4.6 at 1M; Fable 5.1, Opus 5 and Sonnet 5 at 1M;
// Haiku 4.5 at 200K). The 5.5 point releases are mapped to their family's
// window and are not separately confirmed.
var defaultContextWindows = map[string]int{
	"claude-fable-5-1":  1_000_000,
	"claude-opus-5-5":   1_000_000,
	"claude-sonnet-5-5": 1_000_000,
	"claude-opus-4-6":   1_000_000,
	"claude-sonnet-4-6": 1_000_000,
	"claude-haiku-4-5":  200_000,
}

// WithContextWindows adds or overrides entries in the model-to-context-window
// table used to fill Update.MaxContextTokens. Keys are exact model IDs.
// Entries merge over the defaults; the map is copied.
func WithContextWindows(windows map[string]int) Option {
	return func(s *Source) {
		if s.contextWindows == nil {
			s.contextWindows = make(map[string]int, len(windows))
		}
		maps.Copy(s.contextWindows, windows)
	}
}

// contextWindow returns the context window for model, or 0 if unknown.
func (s *Source) contextWindow(model string) int {
	if n, ok := s.contextWindows[model]; ok {
		return n
	}
	return defaultContextWindows[model]
}
