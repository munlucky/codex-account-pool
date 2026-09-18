package accountpool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func take(t *testing.T, p *Pool, session string) *Lease {
	t.Helper()
	l, err := p.Acquire(context.Background(), "google", session)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func TestAffinityPreferredSelectorAndBoundedFailover(t *testing.T) {
	p := New(10, time.Minute)
	ids := []string{"A", "B"}
	l := take(t, p, "one")
	id, err := l.Select("A", "", ids, false)
	if id != "A" || err != nil {
		t.Fatal(id, err)
	}
	if id = l.Alternate(Unknown, ids, false, func(string) Quota { t.Fatal("must not probe"); return Usable }); id != "" {
		t.Fatal(id)
	}
	if id = l.Alternate(Exhausted, ids, false, func(string) Quota { return Usable }); id != "B" {
		t.Fatal(id)
	}
	l.ObserveSuccess(id)
	if l.Alternate(Exhausted, ids, false, func(string) Quota { return Usable }) != "" {
		t.Fatal("unbounded failover")
	}
	l.Release()
	l = take(t, p, "one")
	id, err = l.Select("A", "", ids, true)
	if id != "B" || err != nil {
		t.Fatal(id, err)
	}
	if _, err = l.Select("A", "A", ids, true); !errors.Is(err, ErrContinuity) {
		t.Fatal(err)
	}
	l.Release()
	l = take(t, p, "new")
	id, err = l.Select("A", "B", ids, false)
	if id != "B" || err != nil {
		t.Fatal(id, err)
	}
	if l.Alternate(Exhausted, ids, false, func(string) Quota { t.Fatal("explicit selector probes"); return Usable }) != "" {
		t.Fatal("selector rotated")
	}
	l.Release()
	l = take(t, p, "deleted")
	if _, err = l.Select("A", "missing", ids, false); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	l.Release()
}
func TestAuthUsePreservesExistingSession(t *testing.T) {
	p := New(10, time.Minute)
	ids := []string{"A", "B"}
	l := take(t, p, "old")
	l.Select("A", "", ids, false)
	l.Release()
	l = take(t, p, "old")
	id, err := l.Select("B", "", ids, false)
	if id != "A" || err != nil {
		t.Fatal(id, err)
	}
	l.Release()
	l = take(t, p, "new")
	id, err = l.Select("B", "", ids, false)
	if id != "B" || err != nil {
		t.Fatal(id, err)
	}
	l.Release()
}
func TestCapacityCancellationEvictionAndExpiry(t *testing.T) {
	p := New(1, time.Minute)
	now := time.Now()
	p.now = func() time.Time { return now }
	l := take(t, p, "one")
	l.Select("A", "", []string{"A"}, false)
	if _, err := p.Acquire(context.Background(), "google", "two"); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Acquire(ctx, "google", "one"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	l.Release()
	now = now.Add(2 * time.Minute)
	l = take(t, p, "one")
	if _, err := l.Select("A", "", []string{"A"}, true); !errors.Is(err, ErrContinuity) {
		t.Fatal(err)
	}
	l.Release()
	l = take(t, p, "two")
	l.Release()
	if len(p.entries) != 1 {
		t.Fatal(len(p.entries))
	}
}
func TestConcurrentSessionLeaseSeesCommittedAccount(t *testing.T) {
	p := New(10, time.Minute)
	l := take(t, p, "one")
	l.Select("A", "", []string{"A", "B"}, false)
	var wg sync.WaitGroup
	wg.Add(1)
	ready := make(chan struct{})
	result := make(chan string, 1)
	go func() {
		defer wg.Done()
		close(ready)
		next, err := p.Acquire(context.Background(), "google", "one")
		if err != nil {
			result <- "error"
			return
		}
		defer next.Release()
		id, _ := next.Select("A", "", []string{"A", "B"}, true)
		result <- id
	}()
	<-ready
	l.ObserveSuccess("B")
	l.Release()
	wg.Wait()
	if id := <-result; id != "B" {
		t.Fatal(id)
	}
}
func TestQuotaCooldownIsModelScopedAndExpires(t *testing.T) {
	p := New(10, time.Minute)
	now := time.Now()
	p.now = func() time.Time { return now }
	ids := []string{"A", "B"}
	l := take(t, p, "one")
	l.SetModel("gemini")
	l.Select("A", "", ids, false)
	l.ObserveQuota("A", Exhausted)
	l.ObserveQuota("B", Usable)
	l.Release()
	l = take(t, p, "two")
	l.SetModel("gemini")
	id, _ := l.Select("A", "", ids, false)
	if id != "B" {
		t.Fatal(id)
	}
	l.Release()
	l = take(t, p, "three")
	l.SetModel("claude")
	id, _ = l.Select("A", "", ids, false)
	if id != "A" {
		t.Fatal(id)
	}
	l.Release()
	now = now.Add(2 * time.Minute)
	l = take(t, p, "four")
	l.SetModel("gemini")
	id, _ = l.Select("A", "", ids, false)
	if id != "A" {
		t.Fatal(id)
	}
	l.Release()
}
