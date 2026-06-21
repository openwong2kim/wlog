package ingest

import (
	"sync"
	"testing"
	"time"

	"github.com/openwong2kim/wlog/internal/model"
)

// drainWithin reads up to n messages from a sub within timeout, returning what
// it got. It stops early if the channel closes.
func drainWithin(s *Sub, n int, timeout time.Duration) []SSEMsg {
	var out []SSEMsg
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case msg, ok := <-s.C():
			if !ok {
				return out
			}
			out = append(out, msg)
		case <-deadline:
			return out
		}
	}
	return out
}

func TestBusSubscribePublishFanout(t *testing.T) {
	b := NewBus(16)
	s1 := b.Subscribe()
	s2 := b.Subscribe()
	defer s1.Close()
	defer s2.Close()

	b.Publish(LogEvent{TS: 1, Kind: "api_request", Summary: "model-x"})
	b.Publish(LogEvent{TS: 2, Kind: "tool_result", Summary: "Bash"})

	for _, s := range []*Sub{s1, s2} {
		got := drainWithin(s, 2, time.Second)
		if len(got) != 2 {
			t.Fatalf("subscriber got %d messages, want 2", len(got))
		}
		if got[0].Kind != "log" || got[0].Event.Kind != "api_request" {
			t.Fatalf("unexpected first message: %+v", got[0])
		}
		if got[1].Event.Summary != "Bash" {
			t.Fatalf("unexpected second message: %+v", got[1])
		}
	}
}

func TestBusSnapshotCoalesce(t *testing.T) {
	b := NewBus(8)
	if snap := b.Snapshot(); snap != (NowSnapshot{}) {
		t.Fatalf("fresh bus snapshot not zero: %+v", snap)
	}
	for i := 0; i < 100; i++ {
		b.SetSnapshot(NowSnapshot{SessionID: "s", CostUSD: float64(i), TS: int64(i)})
	}
	snap := b.Snapshot()
	if snap.CostUSD != 99 || snap.TS != 99 {
		t.Fatalf("snapshot not coalesced to latest: %+v", snap)
	}
}

func TestBusSlowSubscriberRingDropAndMarker(t *testing.T) {
	const ring = 4
	b := NewBus(ring)
	s := b.Subscribe()

	// A slow consumer reads with a small delay per message while a burst of
	// publishes overflows the ring. The contract: the publisher never blocks,
	// some logs are dropped, and the consumer eventually observes a {_drop, N}
	// marker accounting for the gap. We read concurrently because a marker can
	// only be delivered once a ring slot frees (no peek/replace on a channel).
	const total = 200
	var logs, drops, droppedTotal int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for msg := range s.C() {
			switch msg.Kind {
			case "log":
				logs++
			case "_drop":
				drops++
				droppedTotal += msg.Dropped
				if msg.Dropped <= 0 {
					t.Errorf("_drop marker with non-positive count: %d", msg.Dropped)
				}
			}
			// Slow consumer: lag behind the publisher to force overflow.
			time.Sleep(time.Millisecond)
		}
	}()

	for i := 0; i < total; i++ {
		b.Publish(LogEvent{TS: int64(i), Kind: "api_request"})
	}
	// Let the consumer drain residual buffer + markers, then close.
	time.Sleep(200 * time.Millisecond)
	s.Close()
	<-done

	if b.Dropped() == 0 {
		t.Fatalf("expected bus-level drops > 0, got 0")
	}
	if drops == 0 {
		t.Fatalf("expected at least one _drop marker, got none (logs=%d busDropped=%d)", logs, b.Dropped())
	}
	if logs >= total {
		t.Fatalf("slow subscriber should not receive all %d logs; got %d", total, logs)
	}
	t.Logf("ring=%d total=%d delivered logs=%d drops=%d droppedTotal=%d busDropped=%d",
		ring, total, logs, drops, droppedTotal, b.Dropped())
}

func TestBusSlowSubscriberDoesNotBlockOthers(t *testing.T) {
	// Ring is generous (default 256) so 50 in-flight messages fit. The slow
	// subscriber never reads; the fast one drains promptly. The publisher must
	// never block on the slow subscriber, and the fast subscriber must receive
	// every message.
	b := NewBus(0)
	slow := b.Subscribe()
	fast := b.Subscribe()
	defer slow.Close()
	defer fast.Close()

	const total = 50
	var fastCount int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for msg := range fast.C() {
			if msg.Kind == "log" {
				fastCount++
			}
			if fastCount >= total {
				return
			}
		}
	}()

	start := time.Now()
	for i := 0; i < total; i++ {
		b.Publish(LogEvent{TS: int64(i), Kind: "api_request"})
	}
	// Publishing 50 messages with one stalled subscriber must complete fast
	// (non-blocking sends); if the slow subscriber blocked us this would stall.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("publisher blocked by slow subscriber: took %s", elapsed)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("fast subscriber did not receive all messages (got %d/%d)", fastCount, total)
	}
	if fastCount < total {
		t.Fatalf("fast subscriber missed messages: %d/%d", fastCount, total)
	}
}

// C11 regression: when an already-queued _drop marker is itself EVICTED to make
// room for a newer marker, its Dropped count must be carried forward into the
// next marker (not discarded), and the global bus counter must NOT be
// double-incremented for the carried-over count. Otherwise a slow client
// under-reports the gap.
//
// We drive deliverLocked directly (ring size 1) to make the eviction
// deterministic:
//
//	publish A      → ch=[A]
//	publish B      → drop B; evict A(log); marker{2}; ch=[_drop:2]
//	publish C      → drop C; evict _drop:2 (a MARKER); marker must report 3
//
// b.Dropped() counts A, B, C = 3 distinct dropped logs. The surviving marker
// must therefore also report 3 (no loss), and b.Dropped() must be exactly 3
// (the carried-over 2 was already counted, no double count).
func TestBusDropMarkerEvictionCarriesCount(t *testing.T) {
	b := NewBus(1) // ring size 1 for deterministic eviction
	s := b.Subscribe()
	defer s.Close()

	mkLog := func(ts int64) SSEMsg { return SSEMsg{Kind: "log", Event: LogEvent{TS: ts, Kind: "api_request"}} }

	b.mu.Lock()
	b.deliverLocked(s, mkLog(1)) // A → ch=[A]
	b.deliverLocked(s, mkLog(2)) // B dropped, A evicted, ch=[_drop:2]
	b.deliverLocked(s, mkLog(3)) // C dropped, _drop:2 evicted → must carry to 3
	b.mu.Unlock()

	// Drain: exactly one message should remain — the surviving _drop marker.
	got := drainWithin(s, 4, 200*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("expected exactly one surviving message (the marker), got %d: %+v", len(got), got)
	}
	if got[0].Kind != "_drop" {
		t.Fatalf("surviving message must be a _drop marker, got %+v", got[0])
	}
	if got[0].Dropped != 3 {
		t.Fatalf("marker must carry the full dropped count 3 (no loss on marker eviction), got %d", got[0].Dropped)
	}
	if b.Dropped() != 3 {
		t.Fatalf("global bus_dropped must be exactly 3 (no double count of carried-over marker), got %d", b.Dropped())
	}
}

func TestBusCloseSafe(t *testing.T) {
	b := NewBus(8)
	s := b.Subscribe()
	s.Close()
	s.Close() // double close must not panic

	// Publishing after a subscriber closed must not panic and must not deliver.
	b.Publish(LogEvent{TS: 1, Kind: "x"})

	// The channel is closed: a receive returns the zero value and ok=false.
	if _, ok := <-s.C(); ok {
		t.Fatalf("expected closed channel after Close")
	}
}

func TestBusConcurrentPublishSubscribe(t *testing.T) {
	b := NewBus(64)
	var wg sync.WaitGroup

	// Subscribers churn (subscribe, read a bit, close) while publishers fire.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				s := b.Subscribe()
				go func() {
					for range s.C() {
					}
				}()
				time.Sleep(time.Millisecond)
				s.Close()
			}
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				b.Publish(LogEvent{TS: int64(j), Kind: "api_request"})
				b.SetSnapshot(NowSnapshot{SessionID: "s", Tokens: model.Tokens{Input: int64(j)}})
			}
		}()
	}
	wg.Wait()
	// Reaching here without a panic/deadlock is the assertion.
}
