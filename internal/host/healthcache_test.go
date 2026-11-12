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
	c := cachedCheck(func(context.Context) error {
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
	c := cachedCheck(func(context.Context) error {
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
