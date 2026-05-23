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
	// clears ID and ParentID on every Subagent. Session identifiers can
	// correlate activity across requests or log streams.
	RedactSessionID bool

	// RedactSource replaces Source with an empty string.
	// Source names identify which agent toolchain is in use, which some
	// callers may consider sensitive.
	RedactSource bool
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
	if p.RedactSessionID {
		out.ID = ""
		out.Slug = ""
		for i := range out.Subagents {
			out.Subagents[i].ID = ""
			out.Subagents[i].ParentID = ""
		}
	}
	if p.RedactSource {
		out.Source = ""
	}

	return out
}
