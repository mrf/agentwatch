package monitor

import (
	"log/slog"
	"time"

	"github.com/mrf/agentwatch/internal/clock"
	"github.com/mrf/agentwatch/source"
)

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
}

func defaultConfig() config {
	return config{
		pollInterval:        2 * time.Second,
		clock:               clock.Wall(),
		logger:              slog.Default(),
		staleThreshold:      5 * time.Minute,
		completionRetention: 30 * time.Second,
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
