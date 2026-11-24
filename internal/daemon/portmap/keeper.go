package portmap

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

// Timing of a Keeper. Variables so tests can shorten them.
var (
	// Lifetime is what is asked for; a mapping is renewed at half of what
	// the router granted, however short.
	Lifetime = 2 * time.Hour
	// retryAfter is the longest wait after a failure: shorter while a
	// granted mapping would expire sooner.
	retryAfter = 5 * time.Minute
	// minWait is the shortest wait between attempts, whatever the router
	// granted.
	minWait = time.Second
	// permanentCheck is how often a permanent (UPnP) mapping is renewed.
	permanentCheck = time.Hour
	// unmapTimeout bounds removing each mapping.
	unmapTimeout = 2 * time.Second
)

// Mapper is what a Keeper needs of a Client (fakes in tests).
type Mapper interface {
	Map(ctx context.Context, internal uint16, lifetime time.Duration, prev *Mapping, note Note) (Mapping, error)
	Unmap(ctx context.Context, m Mapping) error
}

// Record keeps, durably, every mapping that may exist on the router, so
// that a later run removes what an earlier one left (a file in the home).
type Record interface {
	Load() ([]Mapping, error)
	Save([]Mapping) error
}

// State is a Keeper's latest outcome.
type State struct {
	Mapping *Mapping // the mapping held now; nil while there is none
	Err     string   // why the last attempt failed
	// Left is the mappings that may still exist on the router and could
	// not be removed yet, each with why: retried at the end of the run
	// and at the start of the next.
	Left  []string
	Tried time.Time
}

// Keeper holds one mapping while its context lasts: it removes what
// earlier runs left, maps, renews at half the lifetime, maps again after
// a failure, and removes every mapping it may have made when the context
// ends. With internal 0 it only removes what earlier runs left.
type Keeper struct {
	m        Mapper
	internal uint16
	rec      Record
	changed  func(State)

	mu   sync.Mutex
	st   State
	held []Mapping         // may exist on the router; written to rec before each request
	why  map[string]string // key: why its removal failed
	live string            // key of the mapping granted: kept whatever a renewal of it answers
}

// NewKeeper keeps internal mapped through m, recording in rec; changed,
// if set, is called after every attempt.
func NewKeeper(m Mapper, internal uint16, rec Record, changed func(State)) *Keeper {
	return &Keeper{m: m, internal: internal, rec: rec, changed: changed, why: map[string]string{}}
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
	st.Left = k.leftLocked()
	return st
}

func (k *Keeper) leftLocked() []string {
	var out []string
	for _, h := range k.held {
		if w, ok := k.why[h.Key()]; ok {
			out = append(out, h.String()+": "+w)
		}
	}
	return out
}

// note records h before a request (asking) or forgets it after a
// definite answer that nothing was made. The record is written before the
// request goes out; a failure to write stops the request. A renewal of the
// mapping granted changes nothing: it exists until removed or expired,
// whatever the renewal's answer.
func (k *Keeper) note(h Mapping, asking bool) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if h.Key() == k.live {
		return nil
	}
	i := slices.IndexFunc(k.held, func(x Mapping) bool { return x.Key() == h.Key() })
	switch {
	case asking && i < 0:
		k.held = append(k.held, h)
	case asking:
		k.held[i] = h
	case i >= 0:
		k.held = slices.Delete(k.held, i, i+1)
		delete(k.why, h.Key())
	}
	return k.saveLocked()
}

func (k *Keeper) saveLocked() error {
	if k.rec == nil {
		return nil
	}
	return k.rec.Save(slices.Clone(k.held))
}

// granted replaces the held entry of m with m itself (its external
// address and lifetime now known).
func (k *Keeper) granted(m Mapping) {
	k.mu.Lock()
	defer k.mu.Unlock()
	i := slices.IndexFunc(k.held, func(x Mapping) bool { return x.Key() == m.Key() })
	if i < 0 {
		k.held = append(k.held, m)
	} else {
		k.held[i] = m
	}
	k.live = m.Key()
	_ = k.saveLocked()
}

// remove tries to remove every held mapping but keep (by key), and
// forgets those removed.
func (k *Keeper) remove(keep string) {
	k.mu.Lock()
	todo := slices.Clone(k.held)
	k.mu.Unlock()
	for _, h := range todo {
		if h.Key() == keep {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), unmapTimeout)
		err := k.m.Unmap(ctx, h)
		cancel()
		k.mu.Lock()
		// A UPnP forward that is not this machine's is left alone: nothing
		// of this machine's is there to remove.
		if err == nil || errors.Is(err, ErrNotOurs) {
			if h.Key() == k.live {
				k.live = ""
			}
			k.held = slices.DeleteFunc(k.held, func(x Mapping) bool { return x.Key() == h.Key() })
			delete(k.why, h.Key())
		} else {
			k.why[h.Key()] = "could not remove it: " + err.Error()
		}
		_ = k.saveLocked()
		k.mu.Unlock()
	}
}

func (k *Keeper) publish(st State) {
	k.mu.Lock()
	k.st = st
	st.Left = k.leftLocked()
	k.mu.Unlock()
	if k.changed != nil {
		k.changed(st)
	}
}

// Run keeps the mapping until ctx ends, then removes it.
func (k *Keeper) Run(ctx context.Context) {
	if k.rec != nil {
		left, err := k.rec.Load()
		if err != nil {
			// Nothing is sent to the router and the record is kept: the
			// person removes the mapping by hand (UnusableText).
			k.publish(State{Err: UnusableText(err), Tried: time.Now()})
			<-ctx.Done()
			return
		}
		k.mu.Lock()
		k.held = left
		k.mu.Unlock()
	}
	k.remove("") // what earlier runs left: removed before anything is asked
	defer k.remove("")
	if k.internal == 0 {
		// Only cleaning up: what could not be removed is tried again every
		// retryAfter, and at the end (the deferred remove).
		for {
			k.publish(State{Tried: time.Now()})
			if len(k.State().Left) == 0 {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryAfter):
			}
			k.remove("")
		}
	}
	var cur *Mapping
	for {
		m, err := k.m.Map(ctx, k.internal, Lifetime, cur, k.note)
		if ctx.Err() != nil {
			if err == nil {
				k.granted(m) // granted as we stopped: still removed
			}
			return
		}
		st := State{Tried: time.Now()}
		var wait time.Duration
		if err != nil {
			st.Err = err.Error()
			// A failed renewal leaves the mapping in place until it
			// expires: it stays held (to be removed) and is renewed again
			// before then.
			wait = retryAfter
			if cur != nil {
				st.Mapping = cur
				if cur.Lifetime > 0 {
					wait = min(wait, max(time.Until(cur.Expires)/2, minWait))
				}
			}
		} else {
			cur = &m
			k.granted(m)
			k.remove(m.Key()) // anything else held is not the mapping now
			c := m
			st.Mapping = &c
			if m.Lifetime > 0 {
				wait = max(m.Lifetime/2, minWait)
			} else {
				wait = permanentCheck
			}
		}
		k.publish(st)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// UnusableText says, for status, that a record could not be used and what
// a person does about it.
func UnusableText(err error) string {
	return err.Error() + "; nothing was sent to the router. If the router forwards a TCP port described \"" +
		description + "\" to this machine, remove that forward in the router's settings, then delete the file"
}

// LeftText joins Left for one line.
func LeftText(left []string) string { return strings.Join(left, "; ") }
