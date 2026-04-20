package server

import (
	gosync "sync"
	"time"
)

// broadcasterBufferCap is the per-subscriber buffer size. A slow
// client can fall this many events behind before the broadcaster
// starts dropping events on its channel.
const broadcasterBufferCap = 8

// broadcasterEmitInterval is the minimum wall-clock time between
// broadcasts to subscribers. Emits that arrive within this window
// after a prior broadcast are coalesced into a single trailing
// broadcast that fires once the window elapses. Bounds dashboard
// refetch work when many agents are actively writing files.
const broadcasterEmitInterval = 10 * time.Second

// Event is a refresh signal sent by the sync engine after a pass
// that wrote data. Scope is advisory — subscribers may filter on
// it but are free to treat it as "refetch now".
type Event struct {
	Scope string
}

// Broadcaster fans out Event values from the sync engine to all
// connected SSE clients. It implements sync.Emitter.
//
// Broadcasts are rate-limited with leading+trailing edge semantics:
// the first emit in a quiet period fires immediately, further emits
// within minInterval are coalesced into a single trailing broadcast
// carrying the most recent scope. This keeps first-write latency
// low while capping refetch work during sustained sync bursts.
type Broadcaster struct {
	mu          gosync.Mutex
	subs        map[chan Event]struct{}
	minInterval time.Duration
	lastEmit    time.Time
	pending     *Event
	timer       *time.Timer
}

// NewBroadcaster creates an empty broadcaster with the production
// rate-limit interval.
func NewBroadcaster() *Broadcaster {
	return newBroadcasterWithInterval(broadcasterEmitInterval)
}

// newBroadcasterWithInterval lets tests use a short window (or
// disable rate limiting with 0) without exposing the tuning knob
// to production callers.
func newBroadcasterWithInterval(d time.Duration) *Broadcaster {
	return &Broadcaster{
		subs:        make(map[chan Event]struct{}),
		minInterval: d,
	}
}

// Emit delivers scope to every subscriber, subject to rate limiting.
// The first emit after a quiet gap of at least minInterval fans out
// immediately; emits within the window update the pending scope and
// schedule one trailing broadcast when the window ends.
//
// Delivery is non-blocking: if a subscriber's buffer is full, the
// event is dropped for that subscriber. The engine never blocks on
// slow clients.
func (b *Broadcaster) Emit(scope string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	if b.minInterval == 0 || b.lastEmit.IsZero() ||
		now.Sub(b.lastEmit) >= b.minInterval {
		// Leading edge. Cancel any trailing state left over from the
		// prior window: without this, a timer whose goroutine had
		// already fired and was waiting on b.mu could acquire it
		// after this call and deliver a stale coalesced event. Stop
		// prevents unfired timers from running; clearing pending
		// makes flushTrailing a no-op for timers whose goroutine
		// already started.
		b.pending = nil
		if b.timer != nil {
			b.timer.Stop()
			b.timer = nil
		}
		b.lastEmit = now
		b.broadcastLocked(Event{Scope: scope})
		return
	}

	b.pending = &Event{Scope: scope}
	if b.timer == nil {
		wait := b.minInterval - now.Sub(b.lastEmit)
		b.timer = time.AfterFunc(wait, b.flushTrailing)
	}
}

// flushTrailing is invoked by the trailing-edge timer. It delivers
// the most recent coalesced scope (if any) and clears the timer so
// future emits can schedule a new one.
func (b *Broadcaster) flushTrailing() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.timer = nil
	if b.pending == nil {
		return
	}
	ev := *b.pending
	b.pending = nil
	b.lastEmit = time.Now()
	b.broadcastLocked(ev)
}

// broadcastLocked sends ev to every subscriber using a non-blocking
// select so a full buffer drops the event for that subscriber only.
// Must be called with b.mu held; holding the lock is safe because
// sends never block.
func (b *Broadcaster) broadcastLocked(ev Event) {
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribe returns a receive channel for events and an unsubscribe
// function. Calling unsubscribe closes the channel and removes the
// subscription. It is safe to call unsubscribe multiple times.
func (b *Broadcaster) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, broadcasterBufferCap)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once gosync.Once
	unsub := func() {
		once.Do(func() {
			b.mu.Lock()
			if _, ok := b.subs[ch]; ok {
				delete(b.subs, ch)
				close(ch)
			}
			b.mu.Unlock()
		})
	}
	return ch, unsub
}
