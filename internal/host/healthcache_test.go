package host

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCachedCheckReusesResultWithinTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	calls := 0
	fail := false
	c := cachedCheck(context.Background(), time.Second, func(context.Context) error {
		calls++
		if fail {
			return errors.New("down")
		}
		return nil
	}, 5*time.Second, func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	for range 20 {
		if err := c(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("20 calls inside the ttl ran the check %d times, want 1", calls)
	}
	fail = true
	advance(4 * time.Second)
	if err := c(context.Background()); err != nil || calls != 1 {
		t.Fatalf("inside the ttl the old result stands: err=%v calls=%d", err, calls)
	}
	advance(2 * time.Second)
	if err := c(context.Background()); err == nil || calls != 2 {
		t.Fatalf("after the ttl the check runs again: err=%v calls=%d", err, calls)
	}
	// A failure is cached as well.
	fail = false
	for range 10 {
		_ = c(context.Background())
	}
	if calls != 2 {
		t.Fatalf("failure result was not reused: calls=%d", calls)
	}
}

func TestCachedCheckSharesOneRunAmongConcurrentCallers(t *testing.T) {
	var runs int
	var mu sync.Mutex
	c := cachedCheck(context.Background(), time.Second, func(context.Context) error {
		mu.Lock()
		runs++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		return nil
	}, time.Minute, time.Now)
	var wg sync.WaitGroup
	for range 30 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = c(context.Background()) }()
	}
	wg.Wait()
	if runs != 1 {
		t.Fatalf("30 concurrent callers ran the check %d times, want 1", runs)
	}
}

// A caller that disconnects while the check runs must not turn the check into a failure that the
// next callers read from the cache: the run uses the server's context, not the request's.
func TestCachedCheckIgnoresTheCallersContext(t *testing.T) {
	c := cachedCheck(context.Background(), time.Second, func(ctx context.Context) error {
		return ctx.Err() // a check that notices cancellation, as ListTools does
	}, time.Minute, time.Now)
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c(gone); err != nil {
		t.Fatalf("the cancelled caller's context reached the check: %v", err)
	}
	if err := c(context.Background()); err != nil {
		t.Fatalf("the next caller was served the cancelled caller's failure: %v", err)
	}
}

func TestCachedCheckBoundsEachRun(t *testing.T) {
	c := cachedCheck(context.Background(), 20*time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}, time.Minute, time.Now)
	start := time.Now()
	if err := c(context.Background()); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("a stuck check was not cut off: err=%v after %v", err, time.Since(start))
	}
}
