package monitor_test

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/mrf/agentwatch/internal/clock"
	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
)

func TestNew_InvalidOptions(t *testing.T) {
	valid := monitor.WithSources(mock.New())
	var nilSource source.Source
	var nilClock clock.Clock

	tests := []struct {
		name string
		opts []monitor.Option
	}{
		{"zero poll interval", []monitor.Option{valid, monitor.WithPollInterval(0)}},
		{"negative poll interval", []monitor.Option{valid, monitor.WithPollInterval(-time.Second)}},
		{"zero health threshold", []monitor.Option{valid, monitor.WithHealthThreshold(0)}},
		{"negative health threshold", []monitor.Option{valid, monitor.WithHealthThreshold(-1)}},
		{"nil logger", []monitor.Option{valid, monitor.WithLogger(nil)}},
		{"nil clock", []monitor.Option{valid, monitor.WithClock(nilClock)}},
		{"nil source", []monitor.Option{monitor.WithSources(mock.New(), nilSource)}},
		{"negative stale threshold", []monitor.Option{valid, monitor.WithStaleThreshold(-time.Second)}},
		{"negative completion retention", []monitor.Option{valid, monitor.WithCompletionRetention(-time.Second)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := monitor.New(tt.opts...)
			if err == nil {
				t.Fatalf("New returned nil error, monitor %v", m)
			}
			if !errors.Is(err, monitor.ErrInvalidOption) {
				t.Fatalf("error %v does not match ErrInvalidOption", err)
			}
		})
	}
}

func TestNew_ValidBoundaryOptions(t *testing.T) {
	_, err := monitor.New(
		monitor.WithSources(mock.New()),
		monitor.WithPollInterval(time.Nanosecond),
		monitor.WithHealthThreshold(1),
		monitor.WithLogger(slog.Default()),
		monitor.WithClock(clock.Wall()),
		monitor.WithStaleThreshold(0),
		monitor.WithCompletionRetention(0),
	)
	if err != nil {
		t.Fatalf("valid boundary options rejected: %v", err)
	}
}
