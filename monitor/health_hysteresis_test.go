package monitor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mrf/agentwatch/internal/clock"
	"github.com/mrf/agentwatch/monitor"
	"github.com/mrf/agentwatch/source"
	"github.com/mrf/agentwatch/sources/mock"
)

// healthPoller drives a mock source through a monitor so each hysteresis
// scenario reads as a script of polls.
type healthPoller struct {
	t   *testing.T
	clk *clock.Mock
	src *mock.Source
	m   *monitor.Monitor
}

func newHealthPoller(t *testing.T, threshold int, ids ...string) *healthPoller {
	t.Helper()
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	handles := make([]source.SessionHandle, 0, len(ids))
	for i := 0; i < len(ids); i++ {
		handles = append(handles, source.SessionHandle{ID: ids[i], Source: "test", StartedAt: clk.Now()})
	}
	src.SetHandles(handles)
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithClock(clk),
		monitor.WithHealthThreshold(threshold),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &healthPoller{t: t, clk: clk, src: src, m: m}
}

func (p *healthPoller) poll() monitor.Health {
	p.t.Helper()
	p.clk.Advance(time.Second)
	if err := p.m.PollOnce(context.Background()); err != nil {
		p.t.Fatalf("PollOnce: %v", err)
	}
	return p.m.Health()["test"]
}

func (p *healthPoller) pollDiscoverError() monitor.Health {
	p.t.Helper()
	p.src.QueueDiscoverError(errors.New("discover down"))
	return p.poll()
}

func (p *healthPoller) pollParseError(ids ...string) monitor.Health {
	p.t.Helper()
	for i := 0; i < len(ids); i++ {
		p.src.QueueParseResult(ids[i], source.Update{}, "", errors.New("bad line"))
	}
	return p.poll()
}

func TestHealth_OneBadFileAmongGoodDoesNotFailSource(t *testing.T) {
	t.Parallel()
	const threshold = 2
	p := newHealthPoller(t, threshold, "good1", "bad", "good2")

	// Far more polls than the 2*threshold that used to fail the source.
	var h monitor.Health
	for i := 0; i < 10*threshold; i++ {
		h = p.pollParseError("bad")
		if h.Status == monitor.HealthFailed {
			t.Fatalf("poll %d: one bad file failed the source", i)
		}
	}
	if h.Status != monitor.HealthDegraded {
		t.Errorf("Status = %q, want %q", h.Status, monitor.HealthDegraded)
	}
	if h.ParseFailures != 1 {
		t.Errorf("ParseFailures = %d, want 1 (one bad session)", h.ParseFailures)
	}
	if h.DiscoverFailures != 0 {
		t.Errorf("DiscoverFailures = %d, want 0", h.DiscoverFailures)
	}
}

func TestHealth_ManyBadFilesNeverFailSource(t *testing.T) {
	t.Parallel()
	p := newHealthPoller(t, 1, "a", "b", "c")
	var h monitor.Health
	for i := 0; i < 6; i++ {
		h = p.pollParseError("a", "b", "c")
	}
	if h.Status != monitor.HealthDegraded {
		t.Errorf("Status = %q, want %q (parse errors never fail a source)", h.Status, monitor.HealthDegraded)
	}
	if h.ParseFailures != 3 {
		t.Errorf("ParseFailures = %d, want 3", h.ParseFailures)
	}
}

func TestHealth_ParseRecoveryRequiresThresholdSuccesses(t *testing.T) {
	t.Parallel()
	const threshold = 3
	p := newHealthPoller(t, threshold, "good", "bad")
	for i := 0; i < threshold; i++ {
		p.pollParseError("bad")
	}
	assertHealthStatus(t, p.m, "test", monitor.HealthDegraded)

	for i := 0; i < threshold-1; i++ {
		h := p.poll()
		if h.Status != monitor.HealthDegraded {
			t.Fatalf("clean poll %d: Status = %q, want degraded (hysteresis)", i+1, h.Status)
		}
	}
	h := p.poll()
	if h.Status != monitor.HealthHealthy {
		t.Errorf("after %d clean polls Status = %q, want healthy", threshold, h.Status)
	}
	if h.ParseFailures != 0 {
		t.Errorf("ParseFailures = %d, want 0 after recovery", h.ParseFailures)
	}
	if h.LastError != "" {
		t.Errorf("LastError = %q, want empty after recovery", h.LastError)
	}
}

func TestHealth_ParseFlappingStaysDegraded(t *testing.T) {
	t.Parallel()
	const threshold = 2
	p := newHealthPoller(t, threshold, "flaky")
	for i := 0; i < threshold; i++ {
		p.pollParseError("flaky")
	}
	assertHealthStatus(t, p.m, "test", monitor.HealthDegraded)

	// success, failure, ...: never threshold consecutive successes.
	for i := 0; i < 4; i++ {
		p.poll()
		assertHealthStatus(t, p.m, "test", monitor.HealthDegraded)
		p.pollParseError("flaky")
		assertHealthStatus(t, p.m, "test", monitor.HealthDegraded)
	}
}

func TestHealth_VanishedBadFileStopsCountingAgainstSource(t *testing.T) {
	t.Parallel()
	p := newHealthPoller(t, 1, "good", "bad")
	p.pollParseError("bad")
	assertHealthStatus(t, p.m, "test", monitor.HealthDegraded)

	// The bad file is deleted: Discover no longer returns it.
	p.src.SetHandles([]source.SessionHandle{{ID: "good", Source: "test", StartedAt: p.clk.Now()}})
	h := p.poll()
	if h.ParseFailures != 0 {
		t.Errorf("ParseFailures = %d, want 0 once the file is gone", h.ParseFailures)
	}
	if h.Status != monitor.HealthHealthy {
		t.Errorf("Status = %q, want healthy once the file is gone", h.Status)
	}
}

func TestHealth_DiscoverRecoveryRequiresThresholdSuccesses(t *testing.T) {
	t.Parallel()
	const threshold = 3
	p := newHealthPoller(t, threshold, "s1")
	for i := 0; i < 2*threshold; i++ {
		p.pollDiscoverError()
	}
	assertHealthStatus(t, p.m, "test", monitor.HealthFailed)

	for i := 0; i < threshold-1; i++ {
		h := p.poll()
		if h.Status != monitor.HealthFailed {
			t.Fatalf("clean poll %d: Status = %q, want failed until %d clean polls", i+1, h.Status, threshold)
		}
	}
	h := p.poll()
	if h.Status != monitor.HealthHealthy {
		t.Errorf("after %d clean polls Status = %q, want healthy", threshold, h.Status)
	}
	if h.DiscoverFailures != 0 {
		t.Errorf("DiscoverFailures = %d, want 0", h.DiscoverFailures)
	}
}

func TestHealth_DiscoverFlappingDoesNotEscalateOrRecover(t *testing.T) {
	t.Parallel()
	const threshold = 2
	p := newHealthPoller(t, threshold, "s1")
	for i := 0; i < threshold; i++ {
		p.pollDiscoverError()
	}
	assertHealthStatus(t, p.m, "test", monitor.HealthDegraded)

	// Failures are never consecutive enough to reach failed, and successes
	// are never consecutive enough to recover.
	for i := 0; i < 6; i++ {
		p.poll()
		assertHealthStatus(t, p.m, "test", monitor.HealthDegraded)
		p.pollDiscoverError()
		assertHealthStatus(t, p.m, "test", monitor.HealthDegraded)
	}
}

func TestHealth_RecoveryEmitsHealthEventOnlyWhenHealthy(t *testing.T) {
	t.Parallel()
	const threshold = 2
	clk := testClock()
	src := mock.New(mock.WithName("test"))
	src.SetHandles([]source.SessionHandle{{ID: "s1", Source: "test", StartedAt: clk.Now()}})
	sink := &recordingSink{}
	m, err := monitor.New(
		monitor.WithSources(src),
		monitor.WithSink(sink),
		monitor.WithClock(clk),
		monitor.WithHealthThreshold(threshold),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < threshold; i++ {
		src.QueueDiscoverError(errors.New("down"))
		if err := m.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
	}
	sink.events = nil

	for i := 0; i < threshold; i++ {
		if err := m.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
	}
	var got []monitor.HealthStatus
	for i := 0; i < len(sink.events); i++ {
		if sink.events[i].Type == monitor.EventHealth {
			got = append(got, sink.events[i].Health.Status)
		}
	}
	if len(got) != 1 || got[0] != monitor.HealthHealthy {
		t.Errorf("health events = %v, want exactly [healthy]", got)
	}
}
