package monitor

import (
	"context"
	"errors"
	"io/fs"
	"time"
)

// HealthStatus indicates the operational state of a source.
type HealthStatus string

const (
	// HealthHealthy means the source is operating normally.
	HealthHealthy HealthStatus = "healthy"
	// HealthDegraded means the source is experiencing intermittent failures.
	HealthDegraded HealthStatus = "degraded"
	// HealthFailed means the source has stopped providing data.
	HealthFailed HealthStatus = "failed"
)

// Health reports the operational state of a single source.
type Health struct {
	Source string       `json:"source"`
	Status HealthStatus `json:"status"`
	// DiscoverFailures is the current run of consecutive Discover errors.
	DiscoverFailures int `json:"discoverFailures"`
	// ParseFailures is the number of sessions whose parse errors have not
	// yet recovered. These degrade a source but never fail it.
	ParseFailures int `json:"parseFailures"`
	// LastError is the category of the most recent failure: one of the
	// LastError* constants, or empty once the source has fully recovered.
	// It is never free text from the underlying error, so it cannot carry
	// paths or panic details; the detailed error goes to the monitor's
	// logger only.
	LastError string    `json:"lastError,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Fixed values of Health.LastError.
const (
	LastErrorDiscover           = "discover failed"
	LastErrorDiscoverNotFound   = "discover failed: not found"
	LastErrorDiscoverPermission = "discover failed: permission denied"
	LastErrorDiscoverTimeout    = "discover failed: timeout"
	LastErrorParse              = "parse failed"
)

// discoverErrorCategory maps a Discover error to its LastError category.
func discoverErrorCategory(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return LastErrorDiscoverNotFound
	case errors.Is(err, fs.ErrPermission):
		return LastErrorDiscoverPermission
	case errors.Is(err, context.DeadlineExceeded):
		return LastErrorDiscoverTimeout
	default:
		return LastErrorDiscover
	}
}

// sessionParseHealth tracks one session's parse errors. A session has an
// entry only while it is unrecovered.
type sessionParseHealth struct {
	failures  int  // consecutive parse failures
	successes int  // consecutive parse successes since degraded
	degraded  bool // sticky once failures reached the threshold
}

// sourceHealth is the mutable per-source health tracking state. All methods
// require the caller to hold Monitor.mu.
//
// Hysteresis (lift: agent-racer@5c193229 monitor/health.go): entering a bad
// state takes threshold consecutive failures, leaving it takes threshold
// consecutive successes, so a flapping source stays bad.
//
// Only Discover failures can fail a source. Parse errors are tracked per
// session and at most degrade it, so one broken file cannot take down a
// source that is otherwise reading fine.
type sourceHealth struct {
	discoverFailures  int          // consecutive; resets on a successful Discover
	discoverSuccesses int          // consecutive since entering a bad state
	discoverLevel     HealthStatus // sticky until discoverSuccesses reaches threshold
	parse             map[string]*sessionParseHealth
	lastError         string
	status            HealthStatus
	updatedAt         time.Time
}

func newSourceHealth() *sourceHealth {
	return &sourceHealth{
		discoverLevel: HealthHealthy,
		parse:         make(map[string]*sessionParseHealth),
		status:        HealthHealthy,
	}
}

// computeStatus maps consecutive Discover failures to a status.
// threshold = degraded transition; 2*threshold = failed transition.
func computeStatus(failures, threshold int) HealthStatus {
	if failures >= 2*threshold {
		return HealthFailed
	}
	if failures >= threshold {
		return HealthDegraded
	}
	return HealthHealthy
}

func (sh *sourceHealth) recordDiscoverFailure(category string, threshold int) {
	sh.discoverSuccesses = 0
	sh.discoverFailures++
	sh.lastError = category
	if level := computeStatus(sh.discoverFailures, threshold); level == HealthFailed ||
		(level == HealthDegraded && sh.discoverLevel == HealthHealthy) {
		sh.discoverLevel = level
	}
}

func (sh *sourceHealth) recordDiscoverSuccess(threshold int) {
	sh.discoverFailures = 0
	if sh.discoverLevel == HealthHealthy {
		return
	}
	sh.discoverSuccesses++
	if sh.discoverSuccesses >= threshold {
		sh.discoverLevel = HealthHealthy
		sh.discoverSuccesses = 0
	}
}

// recordParses folds one poll's parse outcomes into per-session state.
// handleIDs are the sessions Discover returned; failed holds those whose
// Parse errored. Sessions no longer discovered are forgotten, so a deleted
// bad file stops counting against the source.
func (sh *sourceHealth) recordParses(handleIDs []string, failed map[string]struct{}, threshold int) {
	live := make(map[string]struct{}, len(handleIDs))
	for i := 0; i < len(handleIDs); i++ {
		id := handleIDs[i]
		live[id] = struct{}{}
		if _, bad := failed[id]; bad {
			sh.recordParseFailure(id, threshold)
		} else {
			sh.recordParseSuccess(id, threshold)
		}
	}
	for id := range sh.parse {
		if _, ok := live[id]; !ok {
			delete(sh.parse, id)
		}
	}
}

func (sh *sourceHealth) recordParseFailure(id string, threshold int) {
	p, ok := sh.parse[id]
	if !ok {
		p = &sessionParseHealth{}
		sh.parse[id] = p
	}
	p.successes = 0
	p.failures++
	if p.failures >= threshold {
		p.degraded = true
	}
	sh.lastError = LastErrorParse
}

func (sh *sourceHealth) recordParseSuccess(id string, threshold int) {
	p, ok := sh.parse[id]
	if !ok {
		return
	}
	p.failures = 0
	if !p.degraded {
		delete(sh.parse, id)
		return
	}
	p.successes++
	if p.successes >= threshold {
		delete(sh.parse, id)
	}
}

// refresh recomputes status and clears lastError once nothing is outstanding.
func (sh *sourceHealth) refresh() {
	sh.status = sh.discoverLevel
	if sh.status == HealthHealthy {
		for _, p := range sh.parse {
			if p.degraded {
				sh.status = HealthDegraded
				break
			}
		}
	}
	if sh.discoverLevel == HealthHealthy && sh.discoverFailures == 0 && len(sh.parse) == 0 {
		sh.lastError = ""
	}
}
