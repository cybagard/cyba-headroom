package vmproc_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/vmproc"
)

// countingLister counts List calls and is slow, like lsof plus footprint.
type countingLister struct{ calls atomic.Int32 }

func (c *countingLister) List(context.Context) ([]vmproc.VM, error) {
	c.calls.Add(1)
	time.Sleep(20 * time.Millisecond)
	return []vmproc.VM{{PID: 1, Kind: vmproc.Docker}}, nil
}

func TestSharedListsOncePerTick(t *testing.T) {
	inner := &countingLister{}
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	shared := vmproc.NewShared(inner, time.Second, clock)

	// The Docker and Tart sources ask at the same moment within one tick.
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if vms, err := shared.List(context.Background()); err != nil || len(vms) != 1 {
				t.Errorf("List = %v, %v", vms, err)
			}
		})
	}
	wg.Wait()
	if n := inner.calls.Load(); n != 1 {
		t.Fatalf("inner List called %d times in one tick, want 1", n)
	}

	// Next tick, 5 s later: fresh data.
	mu.Lock()
	now = now.Add(5 * time.Second)
	mu.Unlock()
	if _, err := shared.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := inner.calls.Load(); n != 2 {
		t.Fatalf("inner List called %d times after the TTL, want 2", n)
	}
}

// flakyLister fails its first call.
type flakyLister struct{ calls int }

func (f *flakyLister) List(context.Context) ([]vmproc.VM, error) {
	f.calls++
	if f.calls == 1 {
		return nil, context.DeadlineExceeded
	}
	return []vmproc.VM{{PID: 1}}, nil
}

func TestSharedDoesNotCacheErrors(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	shared := vmproc.NewShared(&flakyLister{}, time.Second, func() time.Time { return now })
	if _, err := shared.List(context.Background()); err == nil {
		t.Fatal("want the first caller's error")
	}
	// Same tick, another source: one caller's timeout must not become everyone's.
	if vms, err := shared.List(context.Background()); err != nil || len(vms) != 1 {
		t.Fatalf("second List = %v, %v; want a fresh listing", vms, err)
	}
}
