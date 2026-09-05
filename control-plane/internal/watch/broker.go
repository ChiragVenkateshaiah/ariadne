package watch

import (
	"sync"
	"time"

	ariadnev1 "github.com/chirag/ariadne/control-plane/gen/ariadne/v1"
)

// Broker fans a single stream of ChangeEvents out to any number of
// subscribers (each brain restart is a new subscriber) and retains a bounded
// in-memory history for GetRecentChanges / Replay -- so "what changed just
// before this test failed" never depends on a subscriber having been
// connected at the time.
type Broker struct {
	mu          sync.RWMutex
	subscribers map[int64]chan *ariadnev1.ChangeEvent
	nextID      int64

	history    []*ariadnev1.ChangeEvent
	maxHistory int
}

func NewBroker(maxHistory int) *Broker {
	return &Broker{
		subscribers: make(map[int64]chan *ariadnev1.ChangeEvent),
		maxHistory:  maxHistory,
	}
}

// Publish is called from the informer event handlers. It never blocks on a
// slow subscriber: a subscriber's channel is buffered, and if it's already
// full we drop for that subscriber rather than stall the whole watch loop --
// a stalled sensor is worse than one subscriber missing an event it can
// recover via GetRecentChanges.
func (b *Broker) Publish(ev *ariadnev1.ChangeEvent) {
	b.mu.Lock()
	b.history = append(b.history, ev)
	if len(b.history) > b.maxHistory {
		b.history = b.history[len(b.history)-b.maxHistory:]
	}
	subs := make([]chan *ariadnev1.ChangeEvent, 0, len(b.subscribers))
	for _, ch := range b.subscribers {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribe returns a channel of future events and a cancel func. Bounded
// buffer size chosen generously (events are small and infrequent relative to
// gRPC throughput) so the drop-on-full path above is a true last resort.
func (b *Broker) Subscribe() (<-chan *ariadnev1.ChangeEvent, func()) {
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	ch := make(chan *ariadnev1.ChangeEvent, 256)
	b.subscribers[id] = ch
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		delete(b.subscribers, id)
		b.mu.Unlock()
		close(ch)
	}
	return ch, cancel
}

// Since returns retained events at or after `t`, oldest first. Used both by
// SubscribeRequest.since (catch-up before switching to live tailing) and by
// GetRecentChanges.
func (b *Broker) Since(t time.Time) []*ariadnev1.ChangeEvent {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]*ariadnev1.ChangeEvent, 0, len(b.history))
	for _, ev := range b.history {
		if ev.ObservedAt.AsTime().Before(t) {
			continue
		}
		out = append(out, ev)
	}
	return out
}
