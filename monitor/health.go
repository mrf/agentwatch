package monitor

import "time"

// HealthStatus describes the operational status of a monitored source.
type HealthStatus string

const (
	// HealthHealthy means the source is operating normally.
	HealthHealthy HealthStatus = "healthy"
	// HealthDegraded means the source is experiencing transient failures.
	HealthDegraded HealthStatus = "degraded"
	// HealthFailed means the source has exceeded its failure threshold.
	HealthFailed HealthStatus = "failed"
)

// Health holds the current health snapshot for a single source.
// Error strings must be sanitized before leaving the process — no absolute
// paths or panic details through transports.
type Health struct {
	Source           string       `json:"source"`
	Status           HealthStatus `json:"status"`
	DiscoverFailures int          `json:"discoverFailures"`
	ParseFailures    int          `json:"parseFailures"`
	LastError        string       `json:"lastError,omitempty"`
	UpdatedAt        time.Time    `json:"updatedAt"`
}
