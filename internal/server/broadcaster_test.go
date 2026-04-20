package server

import (
	"sync"
	"testing"
	"time"
)

func TestBroadcaster_EmitFansOutToAllSubscribers(t *testing.T) {
	b := NewBroadcaster(10 * time.Second)
	sub1, unsub1 := b.Subscribe()
	defer unsub1()
	sub2, unsub2 := b.Subscribe()
	defer unsub2()

	b.Emit("messages")

	for i, sub := range []<-chan Event{sub1, sub2} {
		select {
		case ev := <-sub:
			if ev.Scope != "messages" {
				t.Errorf("sub %d: got scope %q, want %q", i, ev.Scope, "messages")
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d: timed out waiting for event", i)
		}
	}
}

func TestBroadcaster_EmitIsNonBlockingOnSlowSubscriber(t *testing.T) {
	// Disable rate limiting so every Emit attempts a broadcast, which
	// is what exercises the non-blocking select-default path against
	// a slow subscriber.
	b := NewBroadcaster(0)
	slow, unsub := b.Subscribe()
	defer unsub()

	// Don't read from slow. Fill its buffer + one extra; Emit must not block.
	const extra = 5
	done := make(chan struct{})
	go func() {
		for range broadcasterBufferCap + extra {
			b.Emit("messages")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked on slow subscriber")
	}

	// Drain what we can — drop count >= extra, exact count not guaranteed.
	drained := 0
	for {
		select {
		case <-slow:
			drained++
		case <-time.After(50 * time.Millisecond):
			if drained == 0 {
				t.Fatalf("slow subscriber received nothing")
			}
			return
		}
	}
}

func TestBroadcaster_UnsubscribeStopsDelivery(t *testing.T) {
	b := NewBroadcaster(10 * time.Second)
	sub, unsub := b.Subscribe()
	unsub()

	b.Emit("messages")

	select {
	case ev, ok := <-sub:
		if ok {
			t.Fatalf("got event after unsubscribe: %v", ev)
		}
		// channel closed by unsubscribe — acceptable
	case <-time.After(100 * time.Millisecond):
		// no delivery — also acceptable
	}
}

func TestBroadcaster_ConcurrentSubscribeAndEmit(t *testing.T) {
	// Disable rate limiting so each subscriber's Emit reliably
	// produces a broadcast during the race.
	b := NewBroadcaster(0)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			sub, unsub := b.Subscribe()
			defer unsub()
			b.Emit("sessions")
			select {
			case <-sub:
			case <-time.After(time.Second):
				t.Errorf("concurrent subscriber did not receive event")
			}
		})
	}
	wg.Wait()
}

func TestBroadcaster_LeadingEdgeEmitsImmediately(t *testing.T) {
	b := NewBroadcaster(time.Second)
	sub, unsub := b.Subscribe()
	defer unsub()

	b.Emit("messages")

	select {
	case ev := <-sub:
		if ev.Scope != "messages" {
			t.Errorf("got scope %q, want %q", ev.Scope, "messages")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("first emit did not broadcast immediately")
	}
}

func TestBroadcaster_CoalescesWithinWindow(t *testing.T) {
	const interval = 100 * time.Millisecond
	b := NewBroadcaster(interval)
	sub, unsub := b.Subscribe()
	defer unsub()

	// Leading-edge broadcast drains the first emit.
	b.Emit("sessions")
	select {
	case <-sub:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("leading-edge emit did not broadcast immediately")
	}

	// Bursts within the window are coalesced; no broadcast yet.
	b.Emit("messages")
	b.Emit("sync")
	b.Emit("sessions")

	select {
	case ev := <-sub:
		t.Fatalf("got early broadcast during rate-limit window: %v", ev)
	case <-time.After(interval / 2):
	}

	// After the window elapses a single trailing broadcast arrives
	// carrying the most recent scope.
	select {
	case ev := <-sub:
		if ev.Scope != "sessions" {
			t.Errorf("trailing scope %q, want %q", ev.Scope, "sessions")
		}
	case <-time.After(interval * 3):
		t.Fatal("trailing broadcast never arrived")
	}

	// The three coalesced emits produce exactly one trailing broadcast.
	select {
	case ev := <-sub:
		t.Fatalf("got duplicate broadcast after trailing fire: %v", ev)
	case <-time.After(interval):
	}
}

func TestBroadcaster_LeadingEdgeCancelsPendingTrailing(t *testing.T) {
	const interval = 50 * time.Millisecond
	b := NewBroadcaster(interval)
	sub, unsub := b.Subscribe()
	defer unsub()

	// Leading broadcast fills the window.
	b.Emit("a")
	select {
	case <-sub:
	case <-time.After(interval):
		t.Fatal("leading emit did not broadcast")
	}

	// Rate-limited emit schedules a trailing broadcast of "b".
	b.Emit("b")

	// Simulate the race: another Emit arrives just after the window
	// boundary but before the in-flight trailing timer can acquire
	// the lock. Backdating lastEmit forces the next Emit to take the
	// leading branch while pending is still set and the timer is
	// still armed.
	b.mu.Lock()
	b.lastEmit = time.Now().Add(-2 * interval)
	b.mu.Unlock()

	b.Emit("c")
	select {
	case ev := <-sub:
		if ev.Scope != "c" {
			t.Errorf("leading broadcast scope %q, want %q", ev.Scope, "c")
		}
	case <-time.After(interval):
		t.Fatal("second leading emit did not broadcast")
	}

	// The pre-existing trailing timer for "b" may still fire. If the
	// leading branch did not cancel pending/timer, flushTrailing
	// would now deliver a stale "b" broadcast. Wait past the
	// original deadline and assert no extra event arrives.
	select {
	case ev := <-sub:
		t.Fatalf("stale trailing broadcast after leading edge: %v", ev)
	case <-time.After(2 * interval):
	}
}

func TestBroadcaster_StaleTrailingCallbackDoesNotConsumeNewerPending(t *testing.T) {
	// Narrow race: a trailing callback whose timer already fired is
	// waiting for b.mu; a leading-edge Emit runs first and a follow-up
	// rate-limited Emit installs a new pending+timer. Without a
	// generation token, the stale callback clobbers b.timer and
	// broadcasts the newer pending event immediately, violating the
	// rate limit.
	const interval = 50 * time.Millisecond
	b := NewBroadcaster(interval)
	sub, unsub := b.Subscribe()
	defer unsub()

	b.Emit("a")
	<-sub

	// Rate-limited emit schedules a timer; capture the generation
	// the scheduled callback will check against when it runs.
	b.Emit("b")
	b.mu.Lock()
	staleGen := b.timerGen
	b.mu.Unlock()

	// Force the next Emit into the leading branch, invalidating the
	// prior timer's generation.
	b.mu.Lock()
	b.lastEmit = time.Now().Add(-2 * interval)
	b.mu.Unlock()
	b.Emit("c")
	<-sub

	// Rate-limited emit after the leading edge installs a fresh
	// pending+timer under the new generation.
	b.Emit("d")

	// Simulate the stale callback from the "b" timer finally acquiring
	// the lock after being blocked. With the generation check it must
	// return without touching state; without it, the stale callback
	// would clear b.timer and broadcast "d" prematurely.
	b.flushTrailing(staleGen)

	// No premature broadcast in the window right after the stale
	// callback supposedly ran.
	select {
	case ev := <-sub:
		t.Fatalf("stale callback consumed newer pending: %v", ev)
	case <-time.After(interval / 2):
	}

	// The new timer scheduled for "d" must still be live and deliver
	// "d" on its original schedule. A bug that lets the stale callback
	// null out b.timer would orphan the new timer here — in which case
	// the callback still fires, finds pending == nil, and no event
	// arrives.
	select {
	case ev := <-sub:
		if ev.Scope != "d" {
			t.Errorf("got scope %q, want d", ev.Scope)
		}
	case <-time.After(interval * 3):
		t.Fatal("new trailing timer did not fire with pending scope")
	}
}

func TestBroadcaster_EmitAfterIntervalBroadcastsImmediately(t *testing.T) {
	const interval = 50 * time.Millisecond
	b := NewBroadcaster(interval)
	sub, unsub := b.Subscribe()
	defer unsub()

	b.Emit("first")
	<-sub

	time.Sleep(interval * 2)

	b.Emit("second")
	select {
	case ev := <-sub:
		if ev.Scope != "second" {
			t.Errorf("got scope %q, want %q", ev.Scope, "second")
		}
	case <-time.After(interval):
		t.Fatal("emit after quiet interval did not broadcast immediately")
	}
}
