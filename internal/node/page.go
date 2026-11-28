package node

import (
	"errors"
	"time"
	"unicode/utf8"
)

// A reader's byte budget for one inbox read (D70, D72). The MCP inbox
// tool applies DefaultInboxBytes when its caller gives none, and reads a
// larger max_bytes as MaxInboxBytes; the local API and the CLI apply none
// unless asked. The bounds keep one result under what the harnesses keep
// in context: Claude Code warns past 10,000 tokens and saves a result to
// a file past 25,000, and its inbox results were measured at 1.54 to 3.44
// bytes a token (2.05 for short entries). The default still holds one
// plain 16 KiB text, and the ceiling every field of an entry but its text,
// at their largest, with room for text_cut (TestAnEntryAtItsLargestBesidesItsTextFitsTheCeiling).
const (
	DefaultInboxBytes = 18000
	MinInboxBytes     = 512
	MaxInboxBytes     = 32000
)

// NeedSlack is what a refusal adds to the size it measured when it names
// the max_bytes to ask again with: an entry that arrives between the two
// calls can lengthen more's digits, or start a stream fetching.
const NeedSlack = 32

// InboxOpts is how much one inbox read returns.
type InboxOpts struct {
	// Limit: at most this many entries (0: DefaultInboxLimit; above
	// MaxInboxLimit: MaxInboxLimit).
	Limit int
	// Wait: how long to wait for an entry when there is none (at most
	// MaxWait).
	Wait time.Duration
	// MaxBytes, when not 0 (MinInboxBytes to MaxInboxBytes): the result,
	// as Size measures it, stays within it. Entries are taken oldest first
	// while it fits (FitPage); the rest is not returned and keeps no
	// receipt. An oldest entry that fits no budget at all is returned with
	// its text cut to fit (Entry.TextCut).
	MaxBytes int
	// Level, when not "": the read first sets agent's level in its room,
	// or every room, as Levels does (U2), and lists them in LevelsSet. The
	// budget is checked against the inbox as it will be at that level
	// before the level is set, so a read refused for its budget changes
	// nothing.
	Level string
	// Size is the bytes of the result showing v as the caller renders it.
	// The surface knows no result's shape: the local API gives the MCP
	// tool's (localapi.ToolInboxSize). Required with MaxBytes.
	Size func(v InboxView) int
	// Ceiling, when not 0 (MinInboxBytes to MaxInboxBytes, at least
	// MaxBytes): the most any read of this caller holds, in place of
	// MaxInboxBytes, set by a reader whose harness shows less inline
	// (D74). An oldest entry that fits no budget up to it is cut at it,
	// as at MaxInboxBytes, and a refusal names it. When even that entry
	// without its text does not fit it, it is returned alone with no text
	// as long as that fits MaxInboxBytes: no entry holds the queue, and
	// only such a result exceeds the ceiling.
	Ceiling int
}

// ceiling is the most a read of o holds: Ceiling, or MaxInboxBytes.
func (o InboxOpts) ceiling() int {
	if o.Ceiling > 0 {
		return o.Ceiling
	}
	return MaxInboxBytes
}

// FitPage is how many of count entries, oldest first, fit a result of at
// most max bytes, where size(n) is the bytes of the result listing the
// first n and grows with n. When not even the first fits it returns 0,
// and need, the size of the result listing the first alone; the caller
// refuses the read or cuts that entry (InboxOpts.page). It measures a
// handful of sizes, none much larger than max.
func FitPage(count, max int, size func(n int) int) (n, need int) {
	if count == 0 {
		return 0, 0
	}
	// Double until a prefix no longer fits, then halve the interval.
	lo, hi := 0, 1
	first := -1
	for hi <= count {
		s := size(hi)
		if hi == 1 {
			first = s
		}
		if s > max {
			break
		}
		lo, hi = hi, hi*2
	}
	if lo == 0 {
		return 0, first
	}
	hi = min(hi, count+1)
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if size(mid) <= max {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo, 0
}

// page cuts v, the whole inbox looked at, to what one read returns: at
// most limit entries, then what o's budget holds, and counts in More the
// entries held that it leaves out. levelSet: this read has already set a
// level, which a refusal then says.
func (o InboxOpts) page(v InboxView, limit int, levelSet bool) (InboxView, error) {
	held := len(v.Entries)
	es := v.Entries
	if len(es) > limit {
		es = es[:limit]
	}
	if o.MaxBytes > 0 {
		ceil := o.ceiling()
		set := ""
		if levelSet {
			set = "; the level was set: leave level out when you call again"
		}
		// More is measured as every entry held, the most it can be: the
		// shim adds to it what it returned but could not mark seen.
		measure := func(page []Entry) int {
			w := v
			w.Entries, w.More = page, held
			return o.Size(w)
		}
		if levelSet && len(v.LevelsSet) > 0 && (measure(nil) > o.MaxBytes || measure(es[:min(1, len(es))]) > o.MaxBytes) {
			// The level was set before a wait, and the result with the
			// rooms set listed leaves no room: list none (the reader asked
			// for the level and it is set) rather than refuse, or cut an
			// entry that fits alone (D74).
			w := v
			w.LevelsSet = nil
			return o.page(w, limit, levelSet)
		}
		if empty := measure(nil); empty > o.MaxBytes {
			if len(v.LevelsSet) > 0 && !levelSet {
				return InboxView{}, codedErr(ErrorTooLarge, "the list of the %d rooms whose level this call sets takes the result to %d bytes before any message, over max_bytes %d; nothing was changed: set the level in a call of its own (inbox with level and peek), then read", len(v.LevelsSet), empty, o.MaxBytes)
			}
			return InboxView{}, codedErr(ErrorTooLarge, "this result takes %d bytes before any message, over max_bytes %d; nothing was marked seen: call inbox again with max_bytes %d%s", empty, o.MaxBytes, min(empty+NeedSlack, ceil), set)
		}
		n, need := FitPage(len(es), o.MaxBytes, func(n int) int { return measure(es[:n]) })
		if n == 0 && len(es) > 0 {
			switch {
			case need <= ceil:
				return InboxView{}, codedErr(ErrorTooLarge, "the oldest unread entry needs max_bytes of at least %d to be read (this call gave %d); nothing was marked seen: call inbox again with max_bytes %d%s", need, o.MaxBytes, min(need+NeedSlack, ceil), set)
			case o.MaxBytes < ceil:
				return InboxView{}, codedErr(ErrorTooLarge, "the oldest unread entry takes %d bytes, more than any max_bytes holds; nothing was marked seen: call inbox again with max_bytes %d to read it with its text cut to fit%s", need, ceil, set)
			}
			e, ok := cutToFit(es[0], func(e Entry) int { return measure([]Entry{e}) }, o.MaxBytes)
			if !ok {
				bare := measure([]Entry{entryWithText(es[0], "")})
				if len(v.LevelsSet) > 0 && !levelSet {
					// Without the list of rooms set, the entry fits
					// (TestAnEntryAtItsLargestBesidesItsTextFitsTheCeiling).
					return InboxView{}, codedErr(ErrorTooLarge, "the list of the %d rooms whose level this call sets and the oldest unread entry take %d bytes before its text, over max_bytes %d; nothing was changed: set the level in a call of its own (inbox with level and peek), then read", len(v.LevelsSet), bare, o.MaxBytes)
				}
				if c, ok := withoutText(es[0], func(e Entry) int { return measure([]Entry{e}) }); ok && ceil < MaxInboxBytes {
					// Its fields alone pass the ceiling: it comes alone,
					// with no text, over the ceiling but within
					// MaxInboxBytes, rather than block every later entry.
					v.Entries, v.More = []Entry{c}, held-1
					return v, nil
				}
				return InboxView{}, codedErr(ErrorTooLarge, "the oldest unread entry takes %d bytes before its text, more than any max_bytes holds; nothing was marked seen%s", bare, set)
			}
			es, n = []Entry{e}, 1
		}
		es = es[:n]
	}
	v.Entries = es
	v.More = held - len(es)
	return v, nil
}

// withoutText is e with no text, TextCut saying what was left out, when
// that fits MaxInboxBytes.
func withoutText(e Entry, size func(Entry) int) (Entry, bool) {
	full := size(e)
	c := entryWithText(e, "")
	c.TextCut = max(full-size(c), 1)
	return c, size(c) <= MaxInboxBytes
}

func entryWithText(e Entry, text string) Entry {
	e.Text = text
	return e
}

// cutToFit is e with the longest start of its text, cut between
// characters, for which size stays within budget, and TextCut the bytes of
// the rendered text left out (size measures them: Text is escaped and
// quoted by the renderer, so no cut falls inside an escape). ok is false
// when not even an empty text fits.
func cutToFit(e Entry, size func(Entry) int, budget int) (Entry, bool) {
	full := size(e)
	// The ends of the text's characters: a cut is at one of them.
	ends := []int{0}
	for i := 0; i < len(e.Text); {
		_, w := utf8.DecodeRuneInString(e.Text[i:])
		i += w
		ends = append(ends, i)
	}
	// TextCut is measured as full, more than it will be, so that its
	// digits never grow the result once the real count replaces it.
	fits := func(k int) bool {
		c := e
		c.Text, c.TextCut = e.Text[:ends[k]], full
		return size(c) <= budget
	}
	if !fits(0) {
		return e, false
	}
	lo, hi := 0, len(ends)-1 // fits(lo); the whole text does not fit
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	c := e
	c.Text = e.Text[:ends[lo]]
	c.TextCut = max(full-size(c), 1)
	return c, true
}

// check refuses a budget out of bounds, or one without a size.
func (o InboxOpts) check() error {
	switch {
	case o.Ceiling != 0 && (o.Ceiling < MinInboxBytes || o.Ceiling > MaxInboxBytes):
		return userErr("ceiling is %d to %d, or 0", MinInboxBytes, MaxInboxBytes)
	case o.MaxBytes == 0:
		return nil
	case o.MaxBytes < MinInboxBytes || o.MaxBytes > MaxInboxBytes:
		return userErr("max_bytes is %d to %d", MinInboxBytes, MaxInboxBytes)
	case o.Size == nil:
		return errors.New("inbox: max_bytes without a size")
	case o.Ceiling != 0 && o.Ceiling < o.MaxBytes:
		return userErr("ceiling is at least max_bytes")
	}
	return nil
}
