package session

// Policy controls which SessionState fields are redacted before external
// exposure. A zero-value Policy is a passthrough — nothing is redacted.
//
// All fields default to false (no redaction). Callers opt in to each
// redaction independently so that consumers can choose the sensitivity
// level appropriate for their deployment.
//
// Design note: every exported field of SessionState and SubagentState must be
// reviewed and listed in the TestPrivacyFilter_FieldCoverage sentinel test. If
// you add a field to either struct, add it to that set and assess whether it
// belongs under an existing or new Policy flag.
type Policy struct {
	// RedactWorkingDir replaces WorkingDir with an empty string.
	// WorkingDir is a local filesystem path and may reveal project names or
	// directory layouts that the caller does not want to expose.
	RedactWorkingDir bool

	// RedactBranch replaces Branch with an empty string.
	// Branch names often encode feature names, ticket IDs, or user handles.
	RedactBranch bool

	// RedactModel replaces Model with an empty string.
	// Model names may reveal subscription tier or internal tooling choices.
	RedactModel bool

	// RedactSessionID replaces ID and Slug with empty strings, and also
	// clears ID, ParentID, and Slug on every Subagent. Session identifiers can
	// correlate activity across requests or log streams. Set Pseudonymizer
	// instead to hide them while keeping them correlatable.
	RedactSessionID bool

	// RedactSource replaces Source with an empty string.
	// Source names identify which agent toolchain is in use, which some
	// callers may consider sensitive.
	RedactSource bool

	// Pseudonymizer, when non-nil, replaces every identifier RedactSessionID
	// would blank with its keyed pseudonym instead, so clients can still
	// correlate a session across snapshots, deltas, and removals. It takes
	// precedence over RedactSessionID. Use one Pseudonymizer for both
	// transports so their IDs agree.
	Pseudonymizer *Pseudonymizer
}

// SessionID returns id as it should appear externally under p: unchanged,
// pseudonymized, or blank. Transports use it for identifiers outside a
// SessionState, such as removal lists and lifecycle events.
func (p Policy) SessionID(id string) string {
	switch {
	case p.Pseudonymizer != nil:
		return p.Pseudonymizer.Pseudonym(id)
	case p.RedactSessionID:
		return ""
	default:
		return id
	}
}

// Apply returns a deep copy of s with sensitive fields redacted according to
// the policy. The original state is never mutated.
func (p Policy) Apply(s SessionState) SessionState {
	out := s.Clone()

	if p.RedactWorkingDir {
		out.WorkingDir = ""
	}
	if p.RedactBranch {
		out.Branch = ""
	}
	if p.RedactModel {
		out.Model = ""
		for i := range out.Subagents {
			out.Subagents[i].Model = ""
		}
	}
	out.ID = p.SessionID(out.ID)
	out.Slug = p.SessionID(out.Slug)
	for i := range out.Subagents {
		out.Subagents[i].ID = p.SessionID(out.Subagents[i].ID)
		out.Subagents[i].ParentID = p.SessionID(out.Subagents[i].ParentID)
		out.Subagents[i].Slug = p.SessionID(out.Subagents[i].Slug)
	}
	if p.RedactSource {
		out.Source = ""
	}

	return out
}
