package llmgateway

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// asyncStore decouples the relay from the database. A call is answered first
// and recorded after, so a slow or unreachable database must never add latency
// to, or fail, a call that already succeeded; but it also must not lose the
// record quietly. So:
//
//   - entries queue in memory (bounded) and a worker writes them in batches;
//   - a failed write is retried with backoff, holding the batch (no loss, no
//     reordering) until the database returns or the store closes;
//   - a full queue drops the new entry, counts it, and logs it loudly (the
//     entry's metadata goes to the log so it can be recovered by hand);
//   - Close drains what it can within its deadline and reports the rest.
//
// Writes are idempotent on Entry.ID, so a retry after an ambiguous failure
// cannot double-count.
type asyncStore struct {
	inner  Store
	logger *slog.Logger
	ch     chan Entry
	done   chan struct{}
	wg     sync.WaitGroup

	closeOnce sync.Once
	closing   atomic.Bool

	inflight atomic.Int64
	dropped  atomic.Uint64
	failed   atomic.Uint64

	flushMu  sync.Mutex
	flushReq chan chan struct{}
}

const (
	asyncQueue    = 4096
	asyncBatch    = 100
	asyncInterval = time.Second
)

func newAsyncStore(inner Store, logger *slog.Logger) *asyncStore {
	a := &asyncStore{inner: inner, logger: logger, ch: make(chan Entry, asyncQueue),
		done: make(chan struct{}), flushReq: make(chan chan struct{})}
	a.wg.Add(1)
	go a.run()
	return a
}

func (a *asyncStore) Add(_ context.Context, entries ...Entry) error {
	for _, e := range entries {
		if a.closing.Load() {
			a.lost(e, "store closing")
			continue
		}
		select {
		case a.ch <- e:
		default:
			a.lost(e, "queue full")
		}
	}
	return nil
}

func (a *asyncStore) lost(e Entry, why string) {
	a.dropped.Add(1)
	a.logger.Error("ledger.entry_dropped", "why", why, "entry_id", e.ID, "request_id", e.RequestID,
		"principal", e.Row.SessionID, "provider", e.Row.Provider, "model", e.Row.Model, "outcome", e.Outcome,
		"output_tokens", e.Row.Usage.OutputTokens.Tokens, "input_tokens", e.Row.Usage.UncachedInputTokens.Tokens)
}

func (a *asyncStore) run() {
	defer a.wg.Done()
	t := time.NewTicker(asyncInterval)
	defer t.Stop()
	var batch []Entry
	flush := func() {
		if len(batch) == 0 {
			return
		}
		a.inflight.Store(int64(len(batch)))
		a.write(batch)
		a.inflight.Store(0)
		batch = batch[:0]
	}
	for {
		select {
		case e := <-a.ch:
			batch = append(batch, e)
			if len(batch) >= asyncBatch {
				flush()
			}
		case <-t.C:
			flush()
		case reply := <-a.flushReq:
			for drained := false; !drained; {
				select {
				case e := <-a.ch:
					batch = append(batch, e)
				default:
					drained = true
				}
			}
			flush()
			close(reply)
		case <-a.done:
			for drained := false; !drained; {
				select {
				case e := <-a.ch:
					batch = append(batch, e)
				default:
					drained = true
				}
			}
			flush()
			return
		}
	}
}

// write retries a batch until it lands or the store is told to stop.
func (a *asyncStore) write(batch []Entry) {
	backoff := 500 * time.Millisecond
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := a.inner.Add(ctx, batch...)
		cancel()
		if err == nil {
			return
		}
		a.failed.Add(1)
		a.logger.Error("ledger.write_failed", "entries", len(batch), "retry_in", backoff.String(), "err", err)
		select {
		case <-time.After(backoff):
		case <-a.done:
			// Closing: one last attempt happens through the loop above on the
			// next iteration only if the database is back; otherwise report.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := a.inner.Add(ctx, batch...)
			cancel()
			if err != nil {
				for _, e := range batch {
					a.lost(e, "database unreachable at shutdown")
				}
			}
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// Flush writes everything queued so far (or gives up at ctx).
func (a *asyncStore) Flush(ctx context.Context) error {
	reply := make(chan struct{})
	select {
	case a.flushReq <- reply:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-reply:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *asyncStore) Recent(ctx context.Context, q Query) ([]Entry, error) {
	return a.inner.Recent(ctx, q)
}
func (a *asyncStore) Totals(ctx context.Context, q Query) ([]Total, error) {
	return a.inner.Totals(ctx, q)
}

func (a *asyncStore) Stats() SinkStats {
	return SinkStats{Queued: len(a.ch) + int(a.inflight.Load()), Dropped: a.dropped.Load(), Failed: a.failed.Load()}
}

// Close stops accepting entries, drains, and closes the inner store.
func (a *asyncStore) Close() {
	a.closeOnce.Do(func() {
		a.closing.Store(true)
		close(a.done)
		a.wg.Wait()
		a.inner.Close()
	})
}
