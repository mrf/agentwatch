package monitor

import "time"

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
