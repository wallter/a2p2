package portmap

import (
	"context"
	"sync"
	"time"
)

// Timing of a Keeper. Variables so tests can shorten them.
var (
	// Lifetime is what is asked for; a mapping is renewed at half of what
	// the router granted.
	Lifetime = 2 * time.Hour
	// retryAfter is the wait after a failure; minRenew the shortest wait
	// between renewals, whatever the router granted.
	retryAfter = 5 * time.Minute
	minRenew   = 30 * time.Second
	// unmapTimeout bounds removing the mapping at the end.
	unmapTimeout = 2 * time.Second
)

// Mapper is what a Keeper needs of a Client (fakes in tests).
type Mapper interface {
	Map(ctx context.Context, internal uint16, lifetime time.Duration, prev *Mapping) (Mapping, error)
	Unmap(ctx context.Context, m Mapping) error
}

// State is a Keeper's latest outcome.
type State struct {
	Mapping *Mapping // nil while there is none
	Err     string   // why the last attempt failed
	Tried   time.Time
}

// Keeper holds one mapping while its context lasts: it maps, renews at
// half the lifetime, maps again after a failure, and removes the mapping
// when the context ends.
type Keeper struct {
	m        Mapper
	internal uint16
	changed  func(State)

	mu sync.Mutex
	st State
}

// NewKeeper keeps internal mapped through m; changed, if set, is called
// after every attempt.
func NewKeeper(m Mapper, internal uint16, changed func(State)) *Keeper {
	return &Keeper{m: m, internal: internal, changed: changed}
}

// State is the latest outcome.
func (k *Keeper) State() State {
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k.st
	if st.Mapping != nil {
		c := *st.Mapping
		st.Mapping = &c
	}
	return st
}

// Run keeps the mapping until ctx ends, then removes it.
func (k *Keeper) Run(ctx context.Context) {
	var cur *Mapping
	defer func() {
		if cur != nil {
			uctx, cancel := context.WithTimeout(context.Background(), unmapTimeout)
			defer cancel()
			_ = k.m.Unmap(uctx, *cur)
		}
	}()
	for {
		m, err := k.m.Map(ctx, k.internal, Lifetime, cur)
		if ctx.Err() != nil {
			if err == nil {
				cur = &m // granted as we stopped: still removed
			}
			return
		}
		wait := retryAfter
		st := State{Tried: time.Now()}
		if err != nil {
			st.Err = err.Error()
			// A renewal that failed leaves the old mapping in place until it
			// expires; the next attempt starts afresh.
			cur = nil
		} else {
			cur = &m
			c := m
			st.Mapping = &c
			if m.Lifetime > 0 {
				wait = max(m.Lifetime/2, minRenew)
			} else {
				wait = time.Hour // permanent (UPnP): checked hourly
			}
		}
		k.mu.Lock()
		k.st = st
		k.mu.Unlock()
		if k.changed != nil {
			k.changed(st)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
