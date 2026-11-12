package host

import (
	"context"
	"sync"
	"time"
)

// healthTTL is how long a logical server's health result is reused. /healthz is served without a
// key (httphost never puts the auth guard in front of it), so without this an unauthenticated
// caller could drive a ListTools call into every child on every request. A failure is cached
// too, so a sick child is not hammered either.
const healthTTL = 5 * time.Second

// cachedCheck runs check at most once per ttl; concurrent callers share the run in progress.
func cachedCheck(check func(context.Context) error, ttl time.Duration, now func() time.Time) func(context.Context) error {
	var (
		mu   sync.Mutex
		at   time.Time
		last error
		have bool
	)
	return func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if have && now().Sub(at) < ttl {
			return last
		}
		last = check(ctx)
		at, have = now(), true
		return last
	}
}
