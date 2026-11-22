package monitor

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mrf/agentwatch/internal/clock"
	"github.com/mrf/agentwatch/source"
)

// ErrInvalidOption is matched (via errors.Is) by every error New returns for
// an invalid configuration.
var ErrInvalidOption = errors.New("monitor: invalid option")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidOption, fmt.Sprintf(format, args...))
}

// Option configures a Monitor.
type Option func(*config)

type config struct {
	sources             []source.Source
	pollInterval        time.Duration
	sink                EventSink
	logger              *slog.Logger
	clock               clock.Clock
	staleThreshold      time.Duration
	completionRetention time.Duration
	healthThreshold     int
}

func defaultConfig() config {
	return config{
		pollInterval:        2 * time.Second,
		clock:               clock.Wall(),
		logger:              slog.Default(),
		staleThreshold:      5 * time.Minute,
		completionRetention: 30 * time.Second,
		healthThreshold:     3,
	}
}

// WithSources sets the sources the monitor will poll.
func WithSources(sources ...source.Source) Option {
	return func(c *config) {
		c.sources = sources
	}
}

// WithPollInterval sets how often Run calls PollOnce.
func WithPollInterval(d time.Duration) Option {
	return func(c *config) {
		c.pollInterval = d
	}
}

// WithSink sets the event sink for the monitor.
func WithSink(sink EventSink) Option {
	return func(c *config) {
		c.sink = sink
	}
}

// WithLogger sets the structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) {
		c.logger = l
	}
}

// WithClock sets the clock for time operations. Used in tests.
func WithClock(c clock.Clock) Option {
	return func(cfg *config) {
		cfg.clock = c
	}
}

// WithStaleThreshold sets how long an active session can go without
// new data before being transitioned to terminal with a "stale" event.
// A zero value disables stale detection.
func WithStaleThreshold(d time.Duration) Option {
	return func(c *config) {
		c.staleThreshold = d
	}
}

// WithCompletionRetention sets how long terminal sessions remain
// in the store before being removed.
func WithCompletionRetention(d time.Duration) Option {
	return func(c *config) {
		c.completionRetention = d
	}
}

// WithHealthThreshold sets the number of consecutive failures before a source
// transitions from healthy to degraded. At 2*threshold it transitions to failed.
// A successful poll resets the failure count and returns the source to healthy.
func WithHealthThreshold(n int) Option {
	return func(c *config) {
		c.healthThreshold = n
	}
}

func (c *config) validate() error {
	if len(c.sources) == 0 {
		return invalid("at least one source is required")
	}
	for i := 0; i < len(c.sources); i++ {
		if c.sources[i] == nil {
			return invalid("WithSources: source %d is nil", i)
		}
	}
	switch {
	case c.pollInterval <= 0:
		return invalid("WithPollInterval: must be positive, got %v", c.pollInterval)
	case c.healthThreshold < 1:
		return invalid("WithHealthThreshold: must be at least 1, got %d", c.healthThreshold)
	case c.logger == nil:
		return invalid("WithLogger: logger is nil")
	case c.clock == nil:
		return invalid("WithClock: clock is nil")
	case c.staleThreshold < 0:
		return invalid("WithStaleThreshold: must not be negative, got %v", c.staleThreshold)
	case c.completionRetention < 0:
		return invalid("WithCompletionRetention: must not be negative, got %v", c.completionRetention)
	}
	return nil
}
