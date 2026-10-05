package gateway

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phin-tech/ignatius/go/store"
	"go.opentelemetry.io/otel/metric"
)

var storeDropped, _ = gt.meter.Int64Counter("ignatius.store.dropped",
	metric.WithDescription("Requests not recorded because the store's write queue was full."))

// storeWriter writes requests to the store off the request path (SPEC 13.1). The queue is
// bounded and in memory: when it is full the request is not recorded (and counted), and a
// crash loses what is queued. Close drains it.
//
// A request is "pending" from Enqueue until it is written. Feedback that arrives before
// then waits for it (Wait) instead of finding no row.
type storeWriter struct {
	st  store.Store
	log *slog.Logger
	q   chan store.Request
	wg  sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	pending map[string]chan struct{}

	dropped atomic.Int64
}

func newStoreWriter(st store.Store, size int, log *slog.Logger) *storeWriter {
	w := &storeWriter{st: st, log: log, q: make(chan store.Request, size), pending: map[string]chan struct{}{}}
	w.wg.Add(1)
	go w.run()
	return w
}

// Enqueue never blocks. It reports whether the request was accepted.
func (w *storeWriter) Enqueue(r store.Request) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed { // a request that finished after shutdown began
		w.dropped.Add(1)
		storeDropped.Add(context.Background(), 1)
		return false
	}
	w.pending[r.ID] = make(chan struct{})
	select {
	case w.q <- r:
		return true
	default:
		delete(w.pending, r.ID)
		n := w.dropped.Add(1)
		storeDropped.Add(context.Background(), 1)
		if n == 1 || n%1000 == 0 {
			w.log.Warn("store: write queue full, request not recorded", "dropped_total", n)
		}
		return false
	}
}

func (w *storeWriter) run() {
	defer w.wg.Done()
	for r := range w.q {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := w.st.RecordRequest(ctx, r); err != nil {
			// err can echo a value from the database, so log only that the write failed.
			w.log.Warn("store: record failed", "request_id", r.ID, "error_kind", "store")
		}
		cancel()
		w.mu.Lock()
		if ch := w.pending[r.ID]; ch != nil {
			close(ch)
			delete(w.pending, r.ID)
		}
		w.mu.Unlock()
	}
}

// Wait blocks until the request is written (or was never queued), or ctx is done.
func (w *storeWriter) Wait(ctx context.Context, id string) {
	w.mu.Lock()
	ch := w.pending[id]
	w.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case <-ch:
	case <-ctx.Done():
	}
}

// Flush blocks until everything queued before the call is written, or ctx is done.
func (w *storeWriter) Flush(ctx context.Context) {
	w.mu.Lock()
	chs := make([]chan struct{}, 0, len(w.pending))
	for _, ch := range w.pending {
		chs = append(chs, ch)
	}
	w.mu.Unlock()
	for _, ch := range chs {
		select {
		case <-ch:
		case <-ctx.Done():
			return
		}
	}
}

// Close stops accepting requests and writes what is queued, until ctx is done.
func (w *storeWriter) Close(ctx context.Context) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	close(w.q)
	w.mu.Unlock()
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		w.log.Warn("store: shut down with requests still queued")
	}
}
