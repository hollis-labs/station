package host

import (
	"context"
	"errors"
	"sync"
	"time"
)

// healthTTL is how long a logical server's health result is reused. /healthz is served without a
// key (httphost never puts the auth guard in front of it), so without this an unauthenticated
// caller could drive a ListTools call into every child on every request. A failure is cached
// too, so a sick child is not hammered either.
const healthTTL = 5 * time.Second

// healthTimeout bounds one health run.
const healthTimeout = 3 * time.Second

// errNotAnswering is all an unauthenticated /healthz caller is told about a sick child.
var errNotAnswering = errors.New("not answering")

// cachedCheck runs check at most once per ttl; concurrent callers share the run in progress.
//
// The check runs on base (the server's own context), never on the context of the request that
// happened to trigger it: a client that disconnects mid-check would otherwise cancel the run, and
// the cancellation would be cached as a failure that every other caller sees for ttl (and a deploy's
// health poll could read as 503). Each run is bounded by timeout.
func cachedCheck(base context.Context, timeout time.Duration, check func(context.Context) error, ttl time.Duration, now func() time.Time) func(context.Context) error {
	var (
		mu   sync.Mutex
		at   time.Time
		last error
		have bool
	)
	return func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if have && now().Sub(at) < ttl {
			return last
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(base), timeout)
		last = check(ctx)
		cancel()
		at, have = now(), true
		return last
	}
}
