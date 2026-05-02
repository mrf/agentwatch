package eventbus_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mrf/agentwatch/internal/eventbus"
	"github.com/mrf/agentwatch/monitor"
)

func makeEvent(seq uint64) monitor.Event {
	return monitor.Event{Seq: seq, At: time.Now(), Type: monitor.EventSnapshot}
}

// TestSingleSubscriber verifies a single subscriber receives the published event.
func TestSingleSubscriber(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	ctx := context.Background()

	var got monitor.Event
	unsub := bus.Subscribe(monitor.EventSinkFunc(func(_ context.Context, ev monitor.Event) error {
		got = ev
		return nil
	}))
	defer unsub()

	ev := makeEvent(1)
	if err := bus.Publish(ctx, ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got.Seq != ev.Seq {
		t.Errorf("got seq %d, want %d", got.Seq, ev.Seq)
	}
}

// TestMultipleSubscribers verifies all subscribers receive each published event.
func TestMultipleSubscribers(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	ctx := context.Background()

	const n = 5
	counts := make([]int, n)
	for i := 0; i < n; i++ {
		unsub := bus.Subscribe(monitor.EventSinkFunc(func(_ context.Context, ev monitor.Event) error {
			counts[i]++
			return nil
		}))
		defer unsub()
	}

	const publishes = 3
	for p := 0; p < publishes; p++ {
		if err := bus.Publish(ctx, makeEvent(uint64(p))); err != nil {
			t.Fatalf("Publish %d: %v", p, err)
		}
	}

	for i, c := range counts {
		if c != publishes {
			t.Errorf("subscriber %d got %d events, want %d", i, c, publishes)
		}
	}
}

// TestErrorIsolation verifies that a failing subscriber does not block
// delivery to subsequent subscribers.
func TestErrorIsolation(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	ctx := context.Background()

	sentinel := errors.New("sink failure")
	var secondCalled bool

	bus.Subscribe(monitor.EventSinkFunc(func(_ context.Context, _ monitor.Event) error {
		return sentinel
	}))
	bus.Subscribe(monitor.EventSinkFunc(func(_ context.Context, _ monitor.Event) error {
		secondCalled = true
		return nil
	}))

	err := bus.Publish(ctx, makeEvent(1))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error does not wrap sentinel: %v", err)
	}
	if !secondCalled {
		t.Error("second subscriber was not called despite first error")
	}
}

// TestUnsubscribe verifies that an unsubscribed sink no longer receives events.
func TestUnsubscribe(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	ctx := context.Background()

	var count int
	unsub := bus.Subscribe(monitor.EventSinkFunc(func(_ context.Context, _ monitor.Event) error {
		count++
		return nil
	}))

	if err := bus.Publish(ctx, makeEvent(1)); err != nil {
		t.Fatalf("Publish before unsub: %v", err)
	}
	unsub()
	if err := bus.Publish(ctx, makeEvent(2)); err != nil {
		t.Fatalf("Publish after unsub: %v", err)
	}

	if count != 1 {
		t.Errorf("got %d calls, want 1", count)
	}
}

// TestConcurrentSubscribeUnsubscribe exercises concurrent subscribe/unsubscribe
// races under the -race detector.
func TestConcurrentSubscribeUnsubscribe(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	ctx := context.Background()

	const goroutines = 20
	const opsEach = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < opsEach; i++ {
				unsub := bus.Subscribe(monitor.EventSinkFunc(func(_ context.Context, _ monitor.Event) error {
					return nil
				}))
				// publish while subscribed
				_ = bus.Publish(ctx, makeEvent(uint64(i)))
				unsub()
			}
		}()
	}

	wg.Wait()
}

// TestPublishNoSubscribers verifies Publish with zero subscribers returns nil.
func TestPublishNoSubscribers(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	if err := bus.Publish(context.Background(), makeEvent(1)); err != nil {
		t.Errorf("expected nil error with no subscribers, got %v", err)
	}
}
