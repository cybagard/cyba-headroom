package vmproc

import (
	"context"
	"sync"
	"time"
)

// Lister lists VMs; Finder implements it.
type Lister interface {
	List(ctx context.Context) ([]VM, error)
}

// Shared lets several sources share one VM listing per tick: the first caller
// lists, callers arriving meanwhile wait for that result, and it is reused
// until ttl passes. Each listing runs lsof and footprint per VM.
type Shared struct {
	inner Lister
	ttl   time.Duration
	now   func() time.Time

	mu  sync.Mutex
	at  time.Time
	vms []VM
}

// NewShared wraps inner; ttl should be well under the collection interval.
func NewShared(inner Lister, ttl time.Duration, now func() time.Time) *Shared {
	return &Shared{inner: inner, ttl: ttl, now: now}
}

// List returns the cached listing if it is younger than ttl, else lists anew.
// Holding the lock while listing makes concurrent callers wait and share.
// Errors are not cached: one caller's timeout must not become everyone's.
func (s *Shared) List(ctx context.Context) ([]VM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.at.IsZero() && s.now().Sub(s.at) < s.ttl {
		return s.vms, nil
	}
	vms, err := s.inner.List(ctx)
	if err != nil {
		return nil, err
	}
	s.vms, s.at = vms, s.now()
	return vms, nil
}
