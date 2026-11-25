package node

// Who may have bytes, decided under one view, and the cancellation a
// transfer holds while it moves them [B2, B3].
//
// A decision reads the room's membership (memory, Rooms) and the rows (one
// store transaction) as of the same commit: the store's change count is
// read before the membership and again inside the transaction, and the two
// are consistent only if no commit fell between (with one connection, a
// transaction sees every commit counted before it; Rooms publishes a
// membership change under its lock before any reader can see the count
// move past it). Every answer is affirmative or a refusal: a view that
// cannot be read consistently, a missing permission or an undecided grant
// is a refusal.
//
// A transfer holds a lease on its decision. Before each read of at most
// 64 KiB the lease compares the change count and the clock with those it
// was decided under, and decides again when either moved; a watcher does
// the same every leaseTick, so a transfer blocked on the network is cut as
// well (its context ends: the transport closes the body or moves the write
// deadline). A publication (a part made live, an agent's copy) asks the
// same rule again inside the transaction that makes the bytes live.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/store"
)

// maxViewTries bounds the attempts at a consistent view; past it the
// answer is a refusal.
const maxViewTries = 16

// leaseTick is how often a lease's watcher looks for a change.
var leaseTick = 25 * time.Millisecond

// errNoView: the membership and the rows could not be read as of one
// commit (writes kept landing between them).
var errNoView = errors.New("files: no consistent view of the room could be read")

// A rule decides, from one consistent view, whether bytes may move now
// and until when by the clock (zero: no clock bound).
type rule func(v *roomView, st members.State) (ok bool, until time.Time, err error)

// within runs fn over room at now with the membership and the rows read
// as of the same commit; seen is the change count they are consistent
// with. ErrNoRoom if the room is not held.
func (f *Files) within(ctx context.Context, room string, now time.Time, fn func(v *roomView, st members.State, seen int64) error) error {
	for range maxViewTries {
		seen := f.m.st.Changes()
		st, err := f.r.State(room)
		if err != nil {
			return err
		}
		_ = hookAt("files:view") // tests: a commit between the membership and the rows
		stale := false
		err = f.m.st.View(ctx, func(rd *store.Reader) error {
			if f.m.st.Changes() != seen {
				stale = true
				return nil
			}
			v := f.m.newView(rd, room, now)
			defer func() {
				if v.expiredByClock {
					f.m.pruneDue.Store(true)
				}
			}()
			if err := v.load(); err != nil {
				return err
			}
			return fn(v, st, seen)
		})
		if !stale {
			return err
		}
	}
	return errNoView
}

// ask runs r over one consistent view of room at now.
func (f *Files) ask(ctx context.Context, room string, now time.Time, r rule) (ok bool, until time.Time, seen int64, err error) {
	err = f.within(ctx, room, now, func(v *roomView, st members.State, s int64) error {
		seen = s
		ok, until, err = r(v, st)
		return err
	})
	if err != nil {
		ok = false
	}
	return ok, until, seen, err
}

// gate is r asked inside the transaction that makes bytes live: the
// membership and the change count are read just before it begins
// (store.Gate's prepare), and the rule is asked over the transaction's own
// rows; a commit between the two is store.ErrStale, and the store asks
// again.
func (f *Files) gate(room string, r rule) func() store.Gate {
	return func() store.Gate {
		seen := f.m.st.Changes()
		st, serr := f.r.State(room)
		now := f.m.now()
		return func(rd *store.Reader) (bool, error) {
			if serr != nil {
				return false, nil
			}
			if f.m.st.Changes() != seen {
				return false, store.ErrStale
			}
			v := f.m.newView(rd, room, now)
			if err := v.load(); err != nil {
				return false, err
			}
			ok, until, err := r(v, st)
			if err != nil || !ok {
				return false, err
			}
			return until.IsZero() || now.Before(until), nil
		}
	}
}

// A lease is a decision that bytes may move, held for one transfer. Its
// context ends when the decision no longer holds.
type lease struct {
	f    *Files
	room string
	r    rule
	ctx  context.Context
	stop context.CancelCauseFunc
	once sync.Once
	done chan struct{}
	// t0 and r0 are this machine's clock and the real clock at the grant:
	// the watcher's clock is t0 plus the real time since (it never reads
	// the machine's clock, which the transfer's own goroutine owns).
	t0, r0 time.Time

	mu    sync.Mutex
	seen  int64
	until time.Time
}

// errRevoked: the decision a transfer was started under no longer holds.
var errRevoked = errors.New("files: the transfer was stopped: the file may no longer be moved")

// grant asks r now and, if yes, returns a lease on it, watched until end.
func (f *Files) grant(ctx context.Context, room string, r rule) (*lease, error) {
	now := f.m.now()
	ok, until, seen, err := f.ask(ctx, room, now, r)
	if err != nil {
		return nil, err
	}
	if !ok || !until.IsZero() && !now.Before(until) {
		return nil, errRevoked
	}
	l := &lease{f: f, room: room, r: r, done: make(chan struct{}), t0: now, r0: time.Now(), seen: seen, until: until}
	l.ctx, l.stop = context.WithCancelCause(ctx)
	go l.watch()
	return l, nil
}

// valid reports whether the decision still holds, deciding again if the
// store or the clock moved since. Called by the transfer before each read.
func (l *lease) valid() bool { return l.validAt(l.f.m.now()) }

func (l *lease) validAt(now time.Time) bool {
	if l.ctx.Err() != nil {
		return false
	}
	l.mu.Lock()
	seen, until := l.seen, l.until
	l.mu.Unlock()
	if l.f.m.st.Changes() == seen && (until.IsZero() || now.Before(until)) {
		return true
	}
	ok, until, seen, err := l.f.ask(context.WithoutCancel(l.ctx), l.room, now, l.r)
	if err != nil || !ok || !until.IsZero() && !now.Before(until) {
		l.stop(errRevoked)
		return false
	}
	l.mu.Lock()
	l.seen, l.until = seen, until
	l.mu.Unlock()
	return true
}

func (l *lease) watch() {
	t := time.NewTicker(leaseTick)
	defer t.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-l.done:
			return
		case <-t.C:
			if !l.validAt(l.t0.Add(time.Since(l.r0))) {
				return
			}
		}
	}
}

// end releases the lease when the transfer is over.
func (l *lease) end() {
	l.once.Do(func() {
		close(l.done)
		l.stop(context.Canceled)
	})
}

// leasedReader asks the lease before every read of at most 64 KiB.
type leasedReader struct {
	r interface{ Read([]byte) (int, error) }
	l *lease
}

func (c *leasedReader) Read(p []byte) (int, error) {
	if !c.l.valid() {
		return 0, errRevoked
	}
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	return c.r.Read(p)
}

// ---- the decision ----

// fileCheck is everything the rules ask of one file of one message, read
// from one consistent view (decide).
type fileCheck struct {
	known      bool // the room is held
	member     bool // the peer is a member now in this machine's view, with a permission
	selfMember bool // this machine is a member now, with a permission
	mf         *store.MessageFiles
	ref        core.FileRef
	isList     bool
	// listed is F1 decided affirmatively: the winning version commits to
	// sha or lists it in its own verified manifest, and this machine may
	// keep that version's text now (a member, the text readable under its
	// own permission, not withdrawn, not expired by its clock). An
	// undecided grant is not listed.
	listed    bool
	textUntil time.Time // when the winning version's text expires here
	mayRead   bool      // the peer may read the winning version's text (F2)
	how       string
	size      int64 // of the bytes held in the room, if any
	held      bool  // held in the room, for any message
	mode      string
	listBad   bool // the winning version's manifest is invalid
	lapsed    bool
	wanting   bool      // a want lives: not lapsed and not past WantLife by the clock
	wantUntil time.Time // when it ends by the clock
	want      store.FileWant
	part      store.FilePart
	hasPart   bool
	fits      bool
	selfAuth  bool
}

// check reads, from one consistent view, what serving, carrying or
// receiving sha of msg needs, for peer (empty: no peer).
func (f *Files) check(ctx context.Context, room, msg, sha, peer string) (fileCheck, error) {
	var c fileCheck
	if !core.ValidRoom(room) || !validRecordID(msg) || !core.ValidSHA256(sha) {
		return c, nil
	}
	err := f.within(ctx, room, f.m.now(), func(v *roomView, st members.State, _ int64) (err error) {
		c, err = f.decide(v, st, msg, sha, peer)
		return err
	})
	if errors.Is(err, ErrNoRoom) {
		return fileCheck{}, nil
	}
	return c, err
}

// decide is the decision over one consistent view. For a peer, it first
// decides that the peer is a member with a permission and may read the
// message, from the membership and the message's own record, before any
// attachment row is read (B8).
func (f *Files) decide(v *roomView, st members.State, msg, sha, peer string) (fileCheck, error) {
	c := fileCheck{known: true}
	room, self, now := v.room, f.m.ID, v.now
	selfPerm, ok := st.Permissions[self]
	c.selfMember = ok && slices.Contains(st.Members, self)
	var perm members.Permission
	if peer != "" {
		var ok bool
		perm, ok = st.Permissions[peer]
		c.member = ok && slices.Contains(st.Members, peer)
		if !c.member {
			return c, nil
		}
		m0, err := v.get(msg)
		if err != nil || m0 == nil || m0.f.Kind != core.KindMessage {
			return c, err
		}
		if m0.author != peer {
			ok, err := readableBy(v, perm)(m0)
			if err != nil || !ok {
				return c, err
			}
		}
	}
	mf, err := v.rd.MessageFiles(room, msg, self, now)
	if err != nil {
		return c, err
	}
	c.mf = mf
	c.ref, c.isList, c.listed = mf.Listed(sha)
	c.selfAuth = mf.Author == self
	if mf.Winner != nil && !mf.Gone {
		if l, ok, err := v.rd.FileList(room, mf.Winner.ID); err != nil {
			return c, err
		} else if ok && l.State == store.ListInvalid {
			c.listBad = true
		}
		w, err := v.get(mf.Winner.ID)
		if err != nil {
			return c, err
		}
		if c.listed {
			keep := c.selfMember && w != nil && (w.reason == store.ReasonLive || w.reason == store.ReasonMissing) &&
				!store.TextExpired(w.f.Created, now)
			if keep && w.author != self {
				a, err := textAccess(v, selfPerm, w.author, w.f, w.id)
				if err != nil {
					return c, err
				}
				keep = a == store.TextReadable
			}
			c.listed = keep
			if keep {
				c.textUntil = w.f.Created.Add(store.Retention)
			}
		}
		if peer != "" && c.listed {
			c.mayRead = w.author == peer
			if !c.mayRead {
				if c.mayRead, err = readableBy(v, perm)(w); err != nil {
					return c, err
				}
			}
		}
	}
	if c.how, err = v.rd.HeldFor(room, sha, msg); err != nil {
		return c, err
	}
	if c.size, c.held, err = v.rd.Held(room, sha); err != nil {
		return c, err
	}
	if c.mode, err = v.rd.FileMode(room); err != nil {
		return c, err
	}
	if c.want, c.wanting, err = v.rd.FileWant(room, msg, sha); err != nil {
		return c, err
	}
	if c.wanting {
		c.wantUntil = c.want.At.Add(core.WantLife)
		switch {
		case !c.want.Lapsed.IsZero():
			c.wanting, c.lapsed = false, true
		case !now.Before(c.wantUntil):
			c.wanting = false // past its life by this clock, lapsed or not yet marked
		}
	}
	if c.part, c.hasPart, err = v.rd.FilePart(room, msg, sha); err != nil {
		return c, err
	}
	ru, mu, err := v.rd.FileUsage(room)
	if err != nil {
		return c, err
	}
	br, bm := f.m.st.FileBudgets()
	c.fits = c.listed && ru+c.ref.Size <= br && mu+c.ref.Size <= bm
	return c, nil
}

func validRecordID(s string) bool { return len(s) == 32 && isLowerHex(s) }

// servable is F2: the peer is a member now, the winning version lists sha
// and may be kept, the peer may read it, and the bytes are held here for
// this message (written here or received for it).
func (c fileCheck) servable() bool {
	return c.known && c.member && c.listed && c.mayRead && c.held && (c.how == store.HowSent || c.how == store.HowReceived)
}

// heldForMsg: the bytes are this message's (written or received for it).
func (c fileCheck) heldForMsg() bool { return c.how == store.HowSent || c.how == store.HowReceived }

// ---- the rules ----

// ruleOf makes a rule of a predicate over decide's answer, bound by the
// winning version's expiry.
func (f *Files) ruleOf(msg, sha, peer string, ok func(fileCheck) bool, until func(fileCheck) time.Time) rule {
	return func(v *roomView, st members.State) (bool, time.Time, error) {
		c, err := f.decide(v, st, msg, sha, peer)
		if err != nil || !ok(c) {
			return false, time.Time{}, err
		}
		if until != nil {
			return true, until(c), nil
		}
		return true, c.textUntil, nil
	}
}

// serveRule: bytes of sha for msg may go to peer (GET) [F2].
func (f *Files) serveRule(msg, sha, peer string) rule {
	return f.ruleOf(msg, sha, peer, fileCheck.servable, nil)
}

// uploadRule: this machine, the author, may upload sha of msg's version to
// peer, which wanted it [F2, F9].
func (f *Files) uploadRule(msg, sha, peer, version string) rule {
	return f.ruleOf(msg, sha, peer, func(c fileCheck) bool {
		return c.servable() && c.selfAuth && c.mf.Winner.ID == version
	}, nil)
}

// putRule: an upload of sha for msg from peer may be taken: the peer is a
// member and the author, this machine wants it of the peer for the winning
// version, by this clock, and does not hold it for the message [F9].
func (f *Files) putRule(msg, sha, peer string) rule {
	return f.ruleOf(msg, sha, peer, putAllowed(peer), func(c fileCheck) time.Time {
		if c.wantUntil.Before(c.textUntil) {
			return c.wantUntil
		}
		return c.textUntil
	})
}

func putAllowed(peer string) func(fileCheck) bool {
	return func(c fileCheck) bool {
		return c.member && c.mf != nil && !c.mf.Gone && c.mf.Author == peer && c.listed && c.wanting &&
			c.want.Author == peer && c.want.Version == c.mf.Winner.ID && !c.heldForMsg()
	}
}

// keepRule: this machine may take and keep sha for msg (a download) [F1].
func (f *Files) keepRule(msg, sha string) rule {
	return f.ruleOf(msg, sha, "", func(c fileCheck) bool { return c.listed && !c.heldForMsg() }, nil)
}

// copyRule: an agent may be given a copy of sha of msg: the winning version
// is still version, lists the file, may be kept here, the bytes are usable
// here, and the agent was shown that version (or this machine wrote it)
// [F3, F4].
func (f *Files) copyRule(msg, sha, agent, version string) rule {
	return func(v *roomView, st members.State) (bool, time.Time, error) {
		c, err := f.decide(v, st, msg, sha, "")
		if err != nil || !c.listed || c.isList || c.mf.Winner.ID != version || c.how == "" || !c.held {
			return false, time.Time{}, err
		}
		if ok, err := shownTo(v.rd, agent, v.room, c); err != nil || !ok {
			return false, time.Time{}, err
		}
		return true, c.textUntil, nil
	}
}
