package clock_test

import (
	"testing"
	"time"

	"github.com/mrf/agentwatch/internal/clock"
)

// epoch is a fixed reference time used across fake clock tests.
var epoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// TestRealClock_Now verifies that RealClock.Now returns a time within the
// current wall-clock window.
func TestRealClock_Now(t *testing.T) {
	c := clock.RealClock{}
	before := time.Now()
	got := c.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Errorf("Now() = %v, want between %v and %v", got, before, after)
	}
}

// TestRealClock_NewTicker verifies that a real ticker fires within a reasonable
// timeout.
func TestRealClock_NewTicker(t *testing.T) {
	c := clock.RealClock{}
	tick := c.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	select {
	case <-tick.C():
		// tick received
	case <-time.After(500 * time.Millisecond):
		t.Fatal("real ticker did not fire within 500ms")
	}
}

// TestNew verifies the constructor returns a usable Clock.
func TestNew(t *testing.T) {
	c := clock.New()
	if c.Now().IsZero() {
		t.Error("New().Now() returned zero time")
	}
}

// TestFakeClock_Now verifies the initial time is preserved.
func TestFakeClock_Now(t *testing.T) {
	fc := clock.NewFake(epoch)
	if got := fc.Now(); !got.Equal(epoch) {
		t.Errorf("Now() = %v, want %v", got, epoch)
	}
}

// TestFakeClock_Advance verifies that Advance moves the clock forward by the
// requested duration.
func TestFakeClock_Advance(t *testing.T) {
	fc := clock.NewFake(epoch)
	fc.Advance(5 * time.Minute)
	want := epoch.Add(5 * time.Minute)
	if got := fc.Now(); !got.Equal(want) {
		t.Errorf("Now() after Advance = %v, want %v", got, want)
	}
}

// TestFakeClock_AdvanceMultiple verifies successive Advance calls are
// cumulative.
func TestFakeClock_AdvanceMultiple(t *testing.T) {
	fc := clock.NewFake(epoch)
	fc.Advance(time.Minute)
	fc.Advance(time.Minute)
	want := epoch.Add(2 * time.Minute)
	if got := fc.Now(); !got.Equal(want) {
		t.Errorf("Now() = %v, want %v", got, want)
	}
}

// TestFakeClock_Ticker_DoesNotFireBeforeInterval verifies a ticker does not
// fire when less than one full period has elapsed.
func TestFakeClock_Ticker_DoesNotFireBeforeInterval(t *testing.T) {
	fc := clock.NewFake(epoch)
	tick := fc.NewTicker(time.Minute)
	defer tick.Stop()

	fc.Advance(30 * time.Second)
	select {
	case <-tick.C():
		t.Fatal("ticker fired before interval elapsed")
	default:
	}
}

// TestFakeClock_Ticker_FiresAtInterval verifies a ticker fires exactly once
// after one full period.
func TestFakeClock_Ticker_FiresAtInterval(t *testing.T) {
	fc := clock.NewFake(epoch)
	tick := fc.NewTicker(time.Minute)
	defer tick.Stop()

	fc.Advance(time.Minute)
	select {
	case got := <-tick.C():
		want := epoch.Add(time.Minute)
		if !got.Equal(want) {
			t.Errorf("tick time = %v, want %v", got, want)
		}
	default:
		t.Fatal("ticker did not fire after one period")
	}
}

// TestFakeClock_Ticker_DropExcessWhenFull verifies that when a ticker's
// channel is full, extra ticks are dropped rather than blocking.
func TestFakeClock_Ticker_DropExcessWhenFull(t *testing.T) {
	fc := clock.NewFake(epoch)
	tick := fc.NewTicker(time.Second)
	defer tick.Stop()

	// Advance 3 periods. The channel holds 1; the other 2 are dropped.
	fc.Advance(3 * time.Second)
	count := 0
	for {
		select {
		case <-tick.C():
			count++
		default:
			goto done
		}
	}
done:
	if count != 1 {
		t.Errorf("received %d ticks, want 1 (channel capacity 1)", count)
	}
}

// TestFakeClock_Ticker_StopPreventsFiring verifies that Stop prevents future
// ticks.
func TestFakeClock_Ticker_StopPreventsFiring(t *testing.T) {
	fc := clock.NewFake(epoch)
	tick := fc.NewTicker(time.Minute)
	tick.Stop()

	fc.Advance(time.Hour)
	select {
	case <-tick.C():
		t.Fatal("stopped ticker fired")
	default:
	}
}

// TestFakeClock_MultipleTickers verifies independent tickers fire at their own
// periods.
func TestFakeClock_MultipleTickers(t *testing.T) {
	fc := clock.NewFake(epoch)
	t1 := fc.NewTicker(time.Minute)
	t2 := fc.NewTicker(2 * time.Minute)
	defer t1.Stop()
	defer t2.Stop()

	fc.Advance(time.Minute)

	// t1 should fire; t2 should not.
	select {
	case <-t1.C():
	default:
		t.Fatal("t1 did not fire after 1 minute")
	}
	select {
	case <-t2.C():
		t.Fatal("t2 fired before 2 minutes")
	default:
	}

	fc.Advance(time.Minute)

	// t2 should now fire; t1 should fire again too.
	select {
	case <-t1.C():
	default:
		t.Fatal("t1 did not fire after 2nd minute")
	}
	select {
	case <-t2.C():
	default:
		t.Fatal("t2 did not fire after 2 minutes")
	}
}

// TestFakeClock_RaceAdvanceAndNow exercises concurrent access under the race
// detector.
func TestFakeClock_RaceAdvanceAndNow(t *testing.T) {
	fc := clock.NewFake(epoch)
	done := make(chan struct{})

	go func() {
		for i := 0; i < 100; i++ {
			fc.Advance(time.Millisecond)
		}
		close(done)
	}()

	for {
		select {
		case <-done:
			return
		default:
			_ = fc.Now()
		}
	}
}

// TestFakeClock_RaceTickerAndAdvance exercises concurrent ticker creation and
// advancement under the race detector.
func TestFakeClock_RaceTickerAndAdvance(t *testing.T) {
	fc := clock.NewFake(epoch)
	done := make(chan struct{})

	go func() {
		for i := 0; i < 50; i++ {
			fc.Advance(10 * time.Millisecond)
		}
		close(done)
	}()

	for i := 0; i < 10; i++ {
		tk := fc.NewTicker(time.Millisecond)
		go func(tk clock.Ticker) {
			defer tk.Stop()
			select {
			case <-tk.C():
			case <-done:
			}
		}(tk)
	}

	<-done
}
