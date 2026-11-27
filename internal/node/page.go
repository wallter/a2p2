package node

import (
	"errors"
	"time"
)

// A reader's byte budget for one inbox read (D70). The MCP inbox tool
// applies DefaultInboxBytes when its caller gives none; the local API and
// the CLI apply none unless asked. The bounds keep one result under what
// the harnesses keep in context (Claude Code saves a result to a file past
// 25,000 tokens or 50,000 characters).
const (
	DefaultInboxBytes = 24000
	MinInboxBytes     = 512
	MaxInboxBytes     = 40000
)

// InboxOpts is how much one inbox read returns.
type InboxOpts struct {
	// Limit: at most this many entries (0: DefaultInboxLimit; above
	// MaxInboxLimit: MaxInboxLimit).
	Limit int
	// Wait: how long to wait for an entry when there is none (at most
	// MaxWait).
	Wait time.Duration
	// MaxBytes, when not 0 (MinInboxBytes to MaxInboxBytes): entries are
	// taken oldest first while Size of the result stays within it
	// (FitPage). The rest is not returned and keeps no receipt.
	MaxBytes int
	// Size is the bytes of the result that lists page, with waiting and
	// more (the entries held now beyond the page) as the caller renders
	// them. The surface knows no result's shape: the local API gives the
	// MCP tool's (localapi.ToolInboxSize). Required with MaxBytes.
	Size func(page []Entry, waiting []Wait, more int) int
}

// FitPage is how many of count entries, oldest first, fit a result of at
// most max bytes, where size(n) is the bytes of the result listing the
// first n and grows with n. When not even the first fits, the read is
// refused (ErrorTooLarge) with the max it needs, so nothing is returned
// and the reader can ask again with that. An entry larger than
// MaxInboxBytes (text of hidden characters, each written \u{...}) fits no
// budget: it is returned alone, over max, when max is MaxInboxBytes, and
// otherwise the refusal names MaxInboxBytes. So no entry is skipped, and
// none holds the queue for ever.
func FitPage(count, max int, size func(n int) int) (int, error) {
	if count == 0 {
		return 0, nil
	}
	// Double until a prefix no longer fits, then halve the interval: a
	// handful of sizes, none much larger than max.
	lo, hi := 0, 1
	for hi <= count && size(hi) <= max {
		lo, hi = hi, hi*2
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
	if lo > 0 {
		return lo, nil
	}
	need := size(1)
	switch {
	case need <= MaxInboxBytes:
		return 0, codedErr(ErrorTooLarge, "the oldest unread entry needs max_bytes %d to be read (this call gave %d); nothing was marked seen: call inbox again with max_bytes %d", need, max, need)
	case max >= MaxInboxBytes:
		return 1, nil
	}
	return 0, codedErr(ErrorTooLarge, "the oldest unread entry takes %d bytes, more than any max_bytes holds; nothing was marked seen: call inbox again with max_bytes %d to read it alone", need, MaxInboxBytes)
}

// page cuts v, the whole inbox looked at, to what one read returns: at
// most limit entries, then what o's budget holds, and counts the rest in
// More.
func (o InboxOpts) page(v InboxView, limit int) (InboxView, error) {
	total := len(v.Entries)
	if len(v.Entries) > limit {
		v.Entries = v.Entries[:limit]
	}
	if o.MaxBytes > 0 {
		es := v.Entries
		n, err := FitPage(len(es), o.MaxBytes, func(n int) int { return o.Size(es[:n], v.Waiting, total-n) })
		if err != nil {
			return InboxView{}, err
		}
		v.Entries = es[:n]
	}
	v.More = total - len(v.Entries)
	return v, nil
}

// check refuses a budget out of bounds, or one without a size.
func (o InboxOpts) check() error {
	switch {
	case o.MaxBytes == 0:
		return nil
	case o.MaxBytes < MinInboxBytes || o.MaxBytes > MaxInboxBytes:
		return userErr("max_bytes is 512 to 40000")
	case o.Size == nil:
		return errors.New("inbox: max_bytes without a size")
	}
	return nil
}
