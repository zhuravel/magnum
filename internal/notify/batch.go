package notify

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Batcher defaults.
const (
	DefaultBatchWindow  = 60 * time.Second
	DefaultBatchDedupe  = 10 * time.Minute
	DefaultBatchLines   = 5
	DefaultSummaryTitle = "magnum: %d reviews posted"
)

// Kind names what an item announces, in the singular and the plural, for the
// summary title of a mixed batch ("magnum: 3 reviews posted, 14 new
// repositories").
type Kind struct{ One, Many string }

// Kinds of informational toast.
var (
	KindReviewPosted = Kind{One: "review posted", Many: "reviews posted"}
	KindNewRepo      = Kind{One: "new repository", Many: "new repositories"}

	kindOther = Kind{One: "update", Many: "updates"} // an item without a Kind in a kinded batch
)

func (k Kind) count(n int) string {
	if n == 1 {
		return "1 " + k.One
	}
	return fmt.Sprintf("%d %s", n, k.Many)
}

// Item is one event to announce, for example one posted review.
type Item struct {
	// Key identifies the event ("owner/repo#123@sha"). A second Add with a
	// key that is already pending is ignored, and the keys of a flushed batch
	// form its dedupe key, so re-reporting the same events (crash recovery)
	// does not toast twice. Empty means "identify by Title and Body".
	Key string
	// Title and Body are the toast used when the item is announced alone.
	Title, Body string
	// Line is the one-line description used in a multi-item summary; it
	// defaults to Title.
	Line string
	// Kind counts the item in a summary title. When no pending item has a
	// Kind the title is SummaryTitle with the total count (the review-posted
	// batcher's original form).
	Kind Kind
	// Window, with a Key and a dedupe Store, announces the item at most once
	// per Window: the flush reserves Key itself in the store (so an item keeps
	// the dedupe key it had as a direct Toast), drops items whose key is
	// still reserved, and releases the reservations when delivery fails.
	Window time.Duration
}

type pendingItem struct {
	Item
	at time.Time
}

// Batcher coalesces bursts of informational events into one toast: the engine
// Adds an Item per event and calls FlushDue every tick (and Flush on
// shutdown). One pending item flushes as its own toast; several flush as a
// single summary ("magnum: 3 reviews posted, 14 new repositories" with one
// line per item). Safe for concurrent use.
type Batcher struct {
	// Notifier delivers the toasts; required.
	Notifier *Notifier
	// Key namespaces the store dedupe keys; default "batch".
	Key string
	// Window is how long the oldest pending item waits for company before
	// FlushDue sends; default DefaultBatchWindow.
	Window time.Duration
	// Dedupe is the store window for a flushed batch's key; default
	// DefaultBatchDedupe.
	Dedupe time.Duration
	// SummaryTitle is the summary toast title, a format with one %d for the
	// item count; default DefaultSummaryTitle.
	SummaryTitle string
	// MaxLines caps the item lines in a summary body (the rest become
	// "+N more"); default DefaultBatchLines.
	MaxLines int
	// Now returns the current time; nil means time.Now. Tests replace it.
	Now func() time.Time

	mu    sync.Mutex
	items []pendingItem
}

// Add queues an item. The first pending item starts the coalescing window.
func (b *Batcher) Add(it Item) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if it.Key != "" {
		for _, p := range b.items {
			if p.Key == it.Key {
				return
			}
		}
	}
	b.items = append(b.items, pendingItem{Item: it, at: b.now()})
}

// Pending reports how many items wait for a flush.
func (b *Batcher) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items)
}

// Due reports whether the oldest pending item has waited a full Window.
func (b *Batcher) Due() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items) > 0 && !b.now().Before(b.items[0].at.Add(b.window()))
}

// FlushDue flushes when Due; otherwise it does nothing.
func (b *Batcher) FlushDue(ctx context.Context) (bool, error) {
	if !b.Due() {
		return false, nil
	}
	return b.Flush(ctx)
}

// Flush announces everything pending right now, regardless of age, and
// reports whether a toast was delivered. Items whose own Window is still
// reserved are dropped first. The batch is consumed when the toast is
// delivered, suppressed by the dedupe gate, or dropped because notifications
// are disabled. If delivery fails (or the store fails) the items stay queued
// (still due), their item reservations are released and the error is
// returned, so the next tick retries.
func (b *Batcher) Flush(ctx context.Context) (bool, error) {
	if b.Notifier == nil {
		return false, errors.New("notify: batcher has no notifier")
	}
	b.mu.Lock()
	batch := b.items
	b.items = nil
	b.mu.Unlock()
	if len(batch) == 0 || !b.Notifier.Enabled {
		return false, nil
	}
	fresh, reserved, err := b.reserveItems(ctx, batch)
	if err != nil {
		b.restore(batch)
		return false, err
	}
	if len(fresh) == 0 {
		return false, nil
	}
	title, body := b.compose(fresh)
	sent, err := b.Notifier.Toast(ctx, b.dedupeKey(fresh), title, body, b.dedupe())
	if err != nil {
		b.releaseItems(ctx, reserved)
		b.restore(fresh)
		return false, err
	}
	return sent, nil
}

// reserveItems applies the per-item Window gate: it returns the items to
// announce and the keys it reserved for them. On a store error it releases
// what it reserved and returns the error.
func (b *Batcher) reserveItems(ctx context.Context, batch []pendingItem) (fresh []pendingItem, reserved []string, err error) {
	st := b.Notifier.Store
	for _, p := range batch {
		if p.Key == "" || p.Window <= 0 || st == nil {
			fresh = append(fresh, p)
			continue
		}
		ok, err := st.ShouldSend(ctx, p.Key, p.Window)
		if err != nil {
			b.releaseItems(ctx, reserved)
			return nil, nil, fmt.Errorf("toast %q: %w", p.Key, err)
		}
		if !ok {
			b.Notifier.logf("notify: toast %q suppressed (sent within %s)", p.Key, p.Window)
			continue
		}
		reserved = append(reserved, p.Key)
		fresh = append(fresh, p)
	}
	return fresh, reserved, nil
}

func (b *Batcher) releaseItems(ctx context.Context, keys []string) {
	for _, k := range keys {
		b.Notifier.release(ctx, k)
	}
}

// restore puts a failed batch back in front of anything added meanwhile.
func (b *Batcher) restore(batch []pendingItem) {
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := make(map[string]bool, len(batch))
	for _, p := range batch {
		if p.Key != "" {
			seen[p.Key] = true
		}
	}
	merged := append([]pendingItem(nil), batch...)
	for _, p := range b.items {
		if p.Key == "" || !seen[p.Key] {
			merged = append(merged, p)
		}
	}
	b.items = merged
}

func (b *Batcher) compose(batch []pendingItem) (title, body string) {
	if len(batch) == 1 {
		it := batch[0]
		title = it.Title
		if title == "" {
			title = it.Line
		}
		return title, it.Body
	}
	title = b.summaryTitle(batch)
	max := b.MaxLines
	if max <= 0 {
		max = DefaultBatchLines
	}
	var lines []string
	for i, p := range batch {
		if i == max {
			lines = append(lines, fmt.Sprintf("+%d more", len(batch)-max))
			break
		}
		line := p.Line
		if line == "" {
			line = p.Title
		}
		lines = append(lines, line)
	}
	return title, strings.Join(lines, "\n")
}

// summaryTitle is SummaryTitle with the item count, or, when SummaryTitle is
// unset and some item has a Kind, "magnum: " and the count per kind in the
// order the kinds first arrived.
func (b *Batcher) summaryTitle(batch []pendingItem) string {
	kinded := slices.ContainsFunc(batch, func(p pendingItem) bool { return p.Kind != Kind{} })
	if b.SummaryTitle != "" || !kinded {
		format := cmp.Or(b.SummaryTitle, DefaultSummaryTitle)
		if strings.Contains(format, "%d") {
			return fmt.Sprintf(format, len(batch))
		}
		return format
	}
	var order []Kind
	counts := map[Kind]int{}
	for _, p := range batch {
		k := cmp.Or(p.Kind, kindOther)
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
	}
	parts := make([]string, len(order))
	for i, k := range order {
		parts[i] = k.count(counts[k])
	}
	return defaultTitle + ": " + strings.Join(parts, ", ")
}

// dedupeKey is "<Key>:<hash of the sorted item identities>": the same set of
// events always maps to the same key, whatever order they were added in.
func (b *Batcher) dedupeKey(batch []pendingItem) string {
	ids := make([]string, len(batch))
	for i, p := range batch {
		ids[i] = p.Key
		if ids[i] == "" {
			ids[i] = p.Title + "\x00" + p.Body
		}
	}
	slices.Sort(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	ns := b.Key
	if ns == "" {
		ns = "batch"
	}
	return ns + ":" + hex.EncodeToString(sum[:8])
}

func (b *Batcher) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Batcher) window() time.Duration {
	if b.Window > 0 {
		return b.Window
	}
	return DefaultBatchWindow
}

func (b *Batcher) dedupe() time.Duration {
	if b.Dedupe > 0 {
		return b.Dedupe
	}
	return DefaultBatchDedupe
}
