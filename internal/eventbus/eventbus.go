// Package eventbus provides internal fan-out event delivery to multiple subscribers.
// It is the mechanism behind monitor.MultiSink.
package eventbus

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/mrf/agentwatch/monitor"
)

// Bus delivers monitor.Event values sequentially to all registered subscribers.
// Errors from individual subscribers are joined and returned from Publish.
// All methods are safe for concurrent use.
type Bus struct {
	mu   sync.RWMutex
	next uint64
	subs map[uint64]monitor.EventSink
}

// New returns an empty Bus ready for use.
func New() *Bus {
	return &Bus{subs: make(map[uint64]monitor.EventSink)}
}

// Subscribe registers sink for future events and returns a function that
// removes the registration. Calling the returned function more than once is
// safe and has no effect after the first call.
func (b *Bus) Subscribe(sink monitor.EventSink) (unsubscribe func()) {
	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = sink
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
		})
	}
}

// Publish delivers ev to every currently registered subscriber in registration
// order. Delivery is sequential. A subscriber error does not stop delivery to
// remaining subscribers. All errors are joined and returned.
func (b *Bus) Publish(ctx context.Context, ev monitor.Event) error {
	b.mu.RLock()
	// Snapshot sinks in order of registration (ascending id).
	ids := make([]uint64, 0, len(b.subs))
	for id := range b.subs {
		ids = append(ids, id)
	}
	// Sort ascending so delivery order matches registration order.
	slices.Sort(ids)
	sinks := make([]monitor.EventSink, len(ids))
	for i, id := range ids {
		sinks[i] = b.subs[id]
	}
	b.mu.RUnlock()

	var errs []error
	for _, sink := range sinks {
		if err := sink.HandleEvent(ctx, ev); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
