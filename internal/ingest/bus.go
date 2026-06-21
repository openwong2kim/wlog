// Package ingest is the normalize-and-persist pipeline plus the live bus that
// feeds the SSE endpoint (PLAN §9, REVIEWS A·C, Eng F7 / Design D-02).
//
// The pipeline owns a single worker goroutine: receiver handlers enqueue raw
// OTLP requests (non-blocking; a full queue is signalled as retryable so the
// client retries and no data is silently dropped), the worker dequeues, parses
// each signal through otel.Parser, writes the resulting model.Batch through the
// single store writer, and updates the live bus (a coalesced "now" snapshot of
// the current session plus a fan-out of per-event log messages).
//
// The bus implements two distinct backpressure strategies (PLAN §9):
//   - tick = coalesce: only the latest NowSnapshot is retained; the SSE handler
//     polls it on its own cadence, so a slow consumer simply observes a newer
//     snapshot — old ticks are harmlessly dropped.
//   - log = per-subscriber bounded ring buffer (default 256). On overflow the
//     offending subscriber's drop counter increments and a single {_drop, N}
//     marker is queued for it, so one slow subscriber never blocks publishers or
//     other subscribers (the publish path is fully non-blocking).
package ingest

import (
	"sync"
	"sync/atomic"

	"github.com/openwong2kim/wlog/internal/model"
)

// SSEMsg is one message delivered to an SSE subscriber over its channel. Kind
// names the SSE event ("log" for a live event, "_drop" for an overflow marker).
// For a normal log message Event carries the payload; for a drop marker Dropped
// holds the number of log messages dropped since the last delivered marker.
//
// Ticks are NOT delivered as SSEMsg: the snapshot is coalesced and read by the
// handler via Bus.Snapshot, so SSEMsg covers only the per-event log stream and
// its overflow markers.
type SSEMsg struct {
	// Kind is "log" for a normal event or "_drop" for an overflow marker.
	Kind string
	// Event is the payload when Kind == "log".
	Event LogEvent
	// Dropped is the number of dropped log messages when Kind == "_drop".
	Dropped int
}

// NowSnapshot is the coalesced live view of the current session (the most recent
// session.id seen by the pipeline). It is what the /api/now SSE "tick" event is
// built from. ActiveTool is "" when no tool is currently in progress. TS is the
// unix-ms time the snapshot was produced.
type NowSnapshot struct {
	SessionID     string
	CostUSD       float64
	Tokens        model.Tokens
	ActiveTimeSec float64
	ActiveTool    string
	TS            int64
}

// LogEvent is one live event published onto the bus for the /api/now SSE "log"
// stream. TS is unix-ms. Kind is the event kind (e.g. "api_request",
// "tool_result"). Summary is a short human-readable line. Fields carries the
// kind-specific structured payload (per PLAN §10 timeline kind fields).
type LogEvent struct {
	TS      int64
	Kind    string
	Summary string
	Fields  map[string]any
}

// defaultRingSize is the per-subscriber log ring buffer capacity.
const defaultRingSize = 256

// Bus is the live event bus shared between the pipeline (producer) and the SSE
// handler (which creates subscribers). It is safe for concurrent use. The
// snapshot is coalesced (latest wins); log events fan out to every subscriber's
// bounded ring buffer.
type Bus struct {
	ringSize int

	mu   sync.Mutex
	snap NowSnapshot
	subs map[*Sub]struct{}

	// dropped counts log messages dropped across all subscribers due to ring
	// overflow (exposed to /api/health as bus_dropped, separate from receiver
	// drops). atomic so Counters reads are lock-free.
	dropped atomic.Int64
}

// NewBus returns a bus with the given per-subscriber ring size. A size <= 0
// uses the default (256).
func NewBus(ringSize int) *Bus {
	if ringSize <= 0 {
		ringSize = defaultRingSize
	}
	return &Bus{
		ringSize: ringSize,
		subs:     make(map[*Sub]struct{}),
	}
}

// Sub is a single SSE subscriber. Read messages from C(); call Close() when the
// SSE connection ends. A Sub must not be shared across goroutines beyond the
// usual single-reader pattern.
type Sub struct {
	bus *Bus
	ch  chan SSEMsg

	// dropped is the count of log messages dropped for THIS subscriber since the
	// last drop marker was successfully queued. Guarded by bus.mu.
	dropped int

	closeOnce sync.Once
}

// Subscribe registers a new subscriber and returns it. The returned channel is
// buffered to the bus ring size; the publisher never blocks on it.
func (b *Bus) Subscribe() *Sub {
	s := &Sub{
		bus: b,
		ch:  make(chan SSEMsg, b.ringSize),
	}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

// C returns the receive channel for delivered SSE messages. It is closed when
// the subscriber is Closed.
func (s *Sub) C() <-chan SSEMsg { return s.ch }

// Close removes the subscriber from the bus and closes its channel. Safe to call
// multiple times. After Close, no further messages are delivered.
func (s *Sub) Close() {
	s.closeOnce.Do(func() {
		s.bus.mu.Lock()
		delete(s.bus.subs, s)
		s.bus.mu.Unlock()
		close(s.ch)
	})
}

// SetSnapshot replaces the coalesced "now" snapshot. Cheap and non-blocking;
// only the latest value is retained (old ticks are dropped harmlessly).
func (b *Bus) SetSnapshot(snap NowSnapshot) {
	b.mu.Lock()
	b.snap = snap
	b.mu.Unlock()
}

// Snapshot returns the current coalesced "now" snapshot (a value copy).
func (b *Bus) Snapshot() NowSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snap
}

// Publish fans a log event out to every subscriber's ring buffer. It never
// blocks: a subscriber whose ring is full has the message dropped, its per-sub
// drop counter incremented, and a {_drop, N} marker queued (best-effort) so the
// client learns it missed N messages. The publish path holds bus.mu only to
// snapshot the subscriber set safely.
func (b *Bus) Publish(ev LogEvent) {
	msg := SSEMsg{Kind: "log", Event: ev}
	b.mu.Lock()
	for s := range b.subs {
		b.deliverLocked(s, msg)
	}
	b.mu.Unlock()
}

// deliverLocked attempts to enqueue msg to s. Caller holds bus.mu. The send is
// non-blocking (select with default) because the channel is sized to the ring
// and a full ring means the consumer is behind — the publisher must never
// block. On overflow the message is dropped, the per-subscriber drop counter
// increments, and a {_drop, N} marker is guaranteed into the ring by evicting
// the oldest buffered message to make room (so the consumer ALWAYS eventually
// observes the gap even if no further publishes occur).
func (b *Bus) deliverLocked(s *Sub, msg SSEMsg) {
	select {
	case s.ch <- msg:
		return
	default:
	}

	// Ring full: this message is dropped.
	s.dropped++
	b.dropped.Add(1)

	// Guarantee a drop marker reaches the consumer. First try the normal
	// non-blocking send (the consumer may have just drained a slot). If still
	// full, evict the oldest buffered message (counting it as another drop) to
	// force the marker in. This bounds delivered logs by the ring size while
	// ensuring the marker is observable purely from consumer-side draining.
	marker := SSEMsg{Kind: "_drop", Dropped: s.dropped}
	select {
	case s.ch <- marker:
		s.dropped = 0
		return
	default:
	}

	// Evict oldest to make room for the marker.
	select {
	case evicted := <-s.ch:
		switch evicted.Kind {
		case "log":
			// A buffered log is now also dropped: count it both per-subscriber and
			// globally.
			s.dropped++
			b.dropped.Add(1)
		case "_drop":
			// The evicted message is itself a drop marker. Its Dropped count was
			// already added to the global counter when those messages were
			// originally dropped (and s.dropped was reset to 0 when this marker was
			// queued). If we discarded it, the consumer would under-report the gap.
			// Carry the count forward into the per-subscriber tally so the next
			// marker re-reports it — WITHOUT touching b.dropped again (no global
			// double-count, C11).
			s.dropped += evicted.Dropped
		}
	default:
		// Concurrent drain emptied it; nothing to evict.
	}
	marker = SSEMsg{Kind: "_drop", Dropped: s.dropped}
	select {
	case s.ch <- marker:
		s.dropped = 0
	default:
		// Unreachable in practice: only Publish sends, and it holds bus.mu, so the
		// slot we just evicted is still free. The count carries forward to the
		// next overflow if it ever happens.
	}
}

// Dropped returns the cumulative number of log messages dropped across all
// subscribers due to ring overflow (exposed to /api/health as bus_dropped).
func (b *Bus) Dropped() int64 { return b.dropped.Load() }
