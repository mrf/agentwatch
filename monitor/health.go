package monitor

import (
	"regexp"
	"strings"
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
	Source           string       `json:"source"`
	Status           HealthStatus `json:"status"`
	DiscoverFailures int          `json:"discoverFailures"`
	ParseFailures    int          `json:"parseFailures"`
	LastError        string       `json:"lastError,omitempty"`
	UpdatedAt        time.Time    `json:"updatedAt"`
}

// sourceHealth is the mutable per-source health tracking state.
type sourceHealth struct {
	discoverFailures int
	parseFailures    int
	lastError        string
	status           HealthStatus
	updatedAt        time.Time
}

// totalFailures returns the combined discover + parse failure count.
func (sh *sourceHealth) totalFailures() int {
	return sh.discoverFailures + sh.parseFailures
}

// computeStatus determines the HealthStatus from total failures and the threshold.
// threshold = degraded transition; 2*threshold = failed transition.
func computeStatus(totalFailures, threshold int) HealthStatus {
	if totalFailures >= 2*threshold {
		return HealthFailed
	}
	if totalFailures >= threshold {
		return HealthDegraded
	}
	return HealthHealthy
}

// sanitizeError removes absolute paths and panic details from error strings
// before they leave the process (plan §5 rule 3).
func sanitizeError(msg string) string {
	// Replace panic stacktraces with "internal error".
	if strings.Contains(msg, "panic:") || strings.Contains(msg, "goroutine") {
		return "internal error"
	}

	// Replace absolute Unix paths.
	msg = unixPathRe.ReplaceAllString(msg, "<path>")
	// Replace absolute Windows paths.
	msg = windowsPathRe.ReplaceAllString(msg, "<path>")

	return msg
}

var (
	unixPathRe    = regexp.MustCompile(`/[a-zA-Z0-9_./-]+`)
	windowsPathRe = regexp.MustCompile(`[A-Z]:\\[a-zA-Z0-9_.\\-]+`)
)
