package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

func newBatcher(h *fakeHerdr, c *storetest.Clock) (*Batcher, *Notifier) {
	n := &Notifier{Herdr: h, Enabled: true}
	return &Batcher{Notifier: n, Key: "review-posted", Now: c.Now}, n
}

func item(i int) Item {
	return Item{
		Key:   fmt.Sprintf("talkable/talkable#%d@sha%d", i, i),
		Title: fmt.Sprintf("Review posted: talkable#%d", i),
		Body:  fmt.Sprintf("round 1, approve (%d findings)", i),
		Line:  fmt.Sprintf("#%d approve", i),
	}
}

func TestBatcherSingleItemKeepsItsOwnToast(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	b, _ := newBatcher(h, c)
	b.Add(item(7))

	sent, err := b.Flush(context.Background())
	if err != nil || !sent {
		t.Fatalf("Flush = %v, %v", sent, err)
	}
	want := shown{"Review posted: talkable#7", "round 1, approve (7 findings)"}
	if len(h.shows) != 1 || h.shows[0] != want {
		t.Errorf("shows = %v, want [%v]", h.shows, want)
	}
	if b.Pending() != 0 {
		t.Errorf("pending = %d after flush", b.Pending())
	}
}

func TestBatcherCoalescesIntoOneSummary(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	b, _ := newBatcher(h, c)
	for i := 1; i <= 3; i++ {
		b.Add(item(i))
		c.Add(10 * time.Second)
	}

	sent, err := b.Flush(context.Background())
	if err != nil || !sent {
		t.Fatalf("Flush = %v, %v", sent, err)
	}
	if len(h.shows) != 1 {
		t.Fatalf("toasts = %d, want exactly one summary: %v", len(h.shows), h.shows)
	}
	got := h.shows[0]
	if got.Title != "magnum: 3 reviews posted" {
		t.Errorf("summary title = %q", got.Title)
	}
	if want := "#1 approve\n#2 approve\n#3 approve"; got.Body != want {
		t.Errorf("summary body = %q, want %q", got.Body, want)
	}
}

func TestBatcherSummaryCapsLines(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	b, _ := newBatcher(h, c)
	b.MaxLines = 2
	b.SummaryTitle = "%d things happened"
	for i := 1; i <= 5; i++ {
		b.Add(item(i))
	}
	if _, err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := h.shows[0]
	if got.Title != "5 things happened" {
		t.Errorf("title = %q", got.Title)
	}
	if want := "#1 approve\n#2 approve\n+3 more"; got.Body != want {
		t.Errorf("body = %q, want %q", got.Body, want)
	}
}

func TestBatcherLineDefaultsToTitle(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	b, _ := newBatcher(h, c)
	b.Add(Item{Key: "a", Title: "Alpha", Body: "x"})
	b.Add(Item{Key: "b", Title: "Beta", Body: "y"})
	if _, err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.shows[0].Body; got != "Alpha\nBeta" {
		t.Errorf("body = %q", got)
	}
}

func TestBatcherDueGatesOnWindow(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	b, _ := newBatcher(h, c)
	ctx := context.Background()

	if sent, err := b.FlushDue(ctx); sent || err != nil {
		t.Fatalf("empty FlushDue = %v, %v", sent, err)
	}
	b.Add(item(1))
	if b.Due() {
		t.Errorf("Due immediately after the first Add")
	}
	c.Add(30 * time.Second)
	b.Add(item(2)) // later items do not restart the window
	if sent, _ := b.FlushDue(ctx); sent {
		t.Fatalf("FlushDue sent before the 60s window elapsed")
	}
	c.Add(30 * time.Second) // 60s after the first item
	if !b.Due() {
		t.Fatalf("not Due 60s after the first item")
	}
	sent, err := b.FlushDue(ctx)
	if err != nil || !sent {
		t.Fatalf("FlushDue = %v, %v", sent, err)
	}
	if len(h.shows) != 1 || !strings.Contains(h.shows[0].Title, "2 reviews") {
		t.Errorf("shows = %v; want one 2-review summary", h.shows)
	}
}

func TestBatcherWindowIsConfigurable(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	b, _ := newBatcher(h, c)
	b.Window = 5 * time.Second
	b.Add(item(1))
	c.Add(5 * time.Second)
	if !b.Due() {
		t.Errorf("custom 5s window not honoured")
	}
}

func TestBatcherIgnoresDuplicatePendingKeys(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	b, _ := newBatcher(h, c)
	b.Add(item(1))
	b.Add(item(1))
	if b.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", b.Pending())
	}
	if _, err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.shows[0].Title != "Review posted: talkable#1" {
		t.Errorf("a deduped single item should render as a plain toast, got %q", h.shows[0].Title)
	}
}

func TestBatcherFlushEmptyIsNoop(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	b, _ := newBatcher(h, c)
	if sent, err := b.Flush(context.Background()); sent || err != nil {
		t.Fatalf("Flush = %v, %v", sent, err)
	}
	if len(h.shows) != 0 {
		t.Errorf("toast shown for an empty batch")
	}
}

func TestBatcherKeepsItemsWhenDeliveryFails(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	h.showErr = &herdr.Error{Method: "notification.show", Code: herdr.CodeInvalidRequest, Message: "boom"}
	b, _ := newBatcher(h, c)
	b.Add(item(1))
	b.Add(item(2))

	sent, err := b.Flush(context.Background())
	if sent || err == nil {
		t.Fatalf("Flush = %v, %v; want the delivery error", sent, err)
	}
	if b.Pending() != 2 {
		t.Fatalf("pending = %d; a failed flush must keep its items", b.Pending())
	}
	b.Add(item(3))

	h.showErr = nil
	h.shows = nil
	if sent, err := b.Flush(context.Background()); !sent || err != nil {
		t.Fatalf("retry Flush = %v, %v", sent, err)
	}
	if got := h.shows[0]; got.Title != "magnum: 3 reviews posted" || got.Body != "#1 approve\n#2 approve\n#3 approve" {
		t.Errorf("retry toast = %+v; want all three in original order", got)
	}
}

func TestBatcherRetainedItemsStayDue(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	h.showErr = errors.New("nope")
	b, _ := newBatcher(h, c)
	b.Add(item(1))
	c.Add(time.Minute)
	if _, err := b.FlushDue(context.Background()); err == nil {
		t.Fatalf("want an error")
	}
	if !b.Due() {
		t.Errorf("items kept after a failed flush must stay due so the next tick retries")
	}
}

func TestBatcherDropsItemsWhenNotificationsDisabled(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	b, n := newBatcher(h, c)
	n.Enabled = false
	b.Add(item(1))
	sent, err := b.Flush(context.Background())
	if sent || err != nil {
		t.Fatalf("Flush = %v, %v", sent, err)
	}
	if b.Pending() != 0 || len(h.shows) != 0 {
		t.Errorf("disabled notifier must drop the batch silently (pending %d, shows %v)", b.Pending(), h.shows)
	}
}

func TestBatcherDedupesRepeatedBatchesAcrossFlushes(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	b, n := newBatcher(h, c)
	n.Store = realStore(t, c)
	ctx := context.Background()

	b.Add(item(1))
	if sent, _ := b.Flush(ctx); !sent {
		t.Fatalf("first flush not sent")
	}
	// The same review is reported again (for example a crash-resume
	// re-adds it): the store gate suppresses the duplicate toast.
	c.Add(time.Minute)
	b.Add(item(1))
	sent, err := b.Flush(ctx)
	if sent || err != nil {
		t.Fatalf("repeat Flush = %v, %v; want suppressed", sent, err)
	}
	if b.Pending() != 0 {
		t.Errorf("suppressed batch should still be consumed")
	}
	// A different review is not suppressed.
	b.Add(item(2))
	if sent, _ := b.Flush(ctx); !sent {
		t.Errorf("different item was suppressed")
	}
	if len(h.shows) != 2 {
		t.Errorf("toasts = %d, want 2", len(h.shows))
	}
	// After the dedupe window the original may toast again.
	c.Add(DefaultBatchDedupe + time.Second)
	b.Add(item(1))
	if sent, _ := b.Flush(ctx); !sent {
		t.Errorf("item suppressed after the dedupe window")
	}
}

func TestBatcherKeyIsOrderIndependentAndNamespaced(t *testing.T) {
	c := newClock()
	st := &fakeStore{send: true}
	h := newFakeHerdr()
	b, n := newBatcher(h, c)
	n.Store = st
	ctx := context.Background()

	b.Add(item(1))
	b.Add(item(2))
	_, _ = b.Flush(ctx)
	b.Add(item(2))
	b.Add(item(1))
	_, _ = b.Flush(ctx)
	if len(st.calls) != 2 || st.calls[0] != st.calls[1] {
		t.Fatalf("keys = %v; same item set must give the same key", st.calls)
	}
	if !strings.HasPrefix(st.calls[0], "review-posted:") {
		t.Errorf("key %q lacks the batcher namespace", st.calls[0])
	}
	b.Add(item(3))
	_, _ = b.Flush(ctx)
	if st.calls[2] == st.calls[0] {
		t.Errorf("different item set must give a different key")
	}
}

func TestBatcherUsesFallbackWhenHerdrDown(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	h.showErr = unavailable()
	r := osascriptFake()
	b, n := newBatcher(h, c)
	n.Runner = r
	b.Add(item(1))
	b.Add(item(2))
	if sent, err := b.Flush(context.Background()); !sent || err != nil {
		t.Fatalf("Flush = %v, %v", sent, err)
	}
	assertOsascript(t, r, `display notification "#1 approve\n#2 approve" with title "magnum: 2 reviews posted"`)
}

func TestBatcherConcurrentAddAndFlush(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	b, _ := newBatcher(h, c)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			b.Add(Item{Key: fmt.Sprint(i), Title: "t"})
		}
	}()
	for i := 0; i < 50; i++ {
		_, _ = b.Flush(context.Background())
	}
	<-done
	_, _ = b.Flush(context.Background())
	if b.Pending() != 0 {
		t.Errorf("pending = %d", b.Pending())
	}
}

// A failed delivery must not consume the dedupe window: the retry of the
// unchanged batch has to reach the user.
func TestBatcherRetriesFailedDeliveryWithRealStore(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	h.showErr = &herdr.Error{Method: "notification.show", Code: herdr.CodeInvalidRequest, Message: "boom"}
	b, n := newBatcher(h, c)
	n.Store = realStore(t, c)
	ctx := context.Background()

	b.Add(item(1))
	b.Add(item(2))
	if sent, err := b.Flush(ctx); sent || err == nil {
		t.Fatalf("first Flush = %v, %v; want the delivery error", sent, err)
	}
	if b.Pending() != 2 {
		t.Fatalf("pending = %d; a failed flush must keep its items", b.Pending())
	}

	h.showErr = nil
	h.shows = nil
	c.Add(time.Second)
	sent, err := b.Flush(ctx)
	if err != nil || !sent {
		t.Fatalf("retry Flush = %v, %v; want the unchanged batch delivered, not suppressed", sent, err)
	}
	if len(h.shows) != 1 || h.shows[0].Title != "magnum: 2 reviews posted" {
		t.Errorf("retry toasts = %v; want one 2-review summary", h.shows)
	}
	if b.Pending() != 0 {
		t.Errorf("pending = %d after a delivered retry", b.Pending())
	}

	// Once delivered the gate is armed again: a re-reported identical batch
	// is suppressed.
	b.Add(item(1))
	b.Add(item(2))
	if sent, err := b.Flush(ctx); sent || err != nil {
		t.Fatalf("repeat Flush = %v, %v; want suppressed after the successful delivery", sent, err)
	}
}

func TestBatcherSummaryCountsPerKind(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	b, _ := newBatcher(h, c)
	for i := 1; i <= 3; i++ {
		it := item(i)
		it.Kind = KindReviewPosted
		b.Add(it)
	}
	for i := 1; i <= 14; i++ {
		b.Add(Item{Key: fmt.Sprintf("repo.new:talkable/repo-%d", i), Title: "new repo", Line: fmt.Sprintf("talkable/repo-%d", i), Kind: KindNewRepo})
	}
	if _, err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.shows) != 1 {
		t.Fatalf("toasts = %d, want one summary", len(h.shows))
	}
	got := h.shows[0]
	if want := "magnum: 3 reviews posted, 14 new repositories"; got.Title != want {
		t.Errorf("title = %q, want %q", got.Title, want)
	}
	if want := "#1 approve\n#2 approve\n#3 approve\ntalkable/repo-1\ntalkable/repo-2\n+12 more"; got.Body != want {
		t.Errorf("body = %q, want %q", got.Body, want)
	}
}

func TestBatcherSummaryKindsSingularAndOther(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	b, _ := newBatcher(h, c)
	b.Add(Item{Key: "r", Title: "repo", Kind: KindNewRepo})
	b.Add(Item{Key: "x", Title: "something"})
	b.Add(Item{Key: "p", Title: "posted", Kind: KindReviewPosted})
	if _, err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := "magnum: 1 new repository, 1 update, 1 review posted"; h.shows[0].Title != want {
		t.Errorf("title = %q, want %q", h.shows[0].Title, want)
	}
}

func TestBatcherExplicitSummaryTitleWinsOverKinds(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	b, _ := newBatcher(h, c)
	b.SummaryTitle = "%d events"
	b.Add(Item{Key: "a", Title: "a", Kind: KindNewRepo})
	b.Add(Item{Key: "b", Title: "b", Kind: KindReviewPosted})
	if _, err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.shows[0].Title != "2 events" {
		t.Errorf("title = %q", h.shows[0].Title)
	}
}

func newRepoItem(name string) Item {
	return Item{Key: "repo.new:" + name, Title: "magnum: new repo " + name, Body: "baseline", Line: name, Kind: KindNewRepo, Window: 24 * time.Hour}
}

// An item with a Window keeps the per-key dedupe a direct Toast had: the same
// new repository reported again within the window is dropped from the batch,
// and its key is the one the direct toast used.
func TestBatcherItemWindowDedupesAcrossBatches(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	b, n := newBatcher(h, c)
	n.Store = realStore(t, c)
	ctx := context.Background()

	// A direct toast under the old key already announced repo-a.
	if sent, _ := n.Toast(ctx, "repo.new:talkable/repo-a", "t", "b", 24*time.Hour); !sent {
		t.Fatal("direct toast not sent")
	}
	b.Add(newRepoItem("talkable/repo-a"))
	b.Add(newRepoItem("talkable/repo-b"))
	if sent, err := b.Flush(ctx); !sent || err != nil {
		t.Fatalf("Flush = %v, %v", sent, err)
	}
	if got := h.shows[len(h.shows)-1]; got.Title != "magnum: new repo talkable/repo-b" {
		t.Errorf("toast = %v; want only repo-b, alone", got)
	}

	c.Add(time.Hour)
	b.Add(newRepoItem("talkable/repo-b"))
	if sent, err := b.Flush(ctx); sent || err != nil {
		t.Fatalf("repeat Flush = %v, %v; want suppressed", sent, err)
	}
	if b.Pending() != 0 {
		t.Errorf("suppressed items must be consumed")
	}
	c.Add(24 * time.Hour)
	b.Add(newRepoItem("talkable/repo-b"))
	if sent, _ := b.Flush(ctx); !sent {
		t.Errorf("item suppressed after its window")
	}
}

func TestBatcherReleasesItemKeysWhenDeliveryFails(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	h.showErr = unavailable()
	st := &fakeStore{send: true}
	b, n := newBatcher(h, c)
	n.Store = st // no Runner: delivery fails
	b.Add(newRepoItem("talkable/repo-a"))
	b.Add(item(1)) // no Window: only the batch key

	if sent, err := b.Flush(context.Background()); sent || err == nil {
		t.Fatalf("Flush = %v, %v; want the delivery error", sent, err)
	}
	if b.Pending() != 2 {
		t.Errorf("pending = %d, want both items kept", b.Pending())
	}
	if len(st.forgot) != 2 || !strings.HasPrefix(st.forgot[0], "review-posted:") || st.forgot[1] != "repo.new:talkable/repo-a" {
		t.Errorf("forgot = %v; want the item key and the batch key released", st.forgot)
	}
}

func TestBatcherItemStoreErrorKeepsBatch(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	st := &fakeStore{send: true, err: errors.New("db locked")}
	b, n := newBatcher(h, c)
	n.Store = st
	b.Add(newRepoItem("talkable/repo-a"))
	if sent, err := b.Flush(context.Background()); sent || err == nil {
		t.Fatalf("Flush = %v, %v; want the store error", sent, err)
	}
	if b.Pending() != 1 || len(h.shows) != 0 {
		t.Errorf("pending %d, shows %v; the batch must wait for the next tick", b.Pending(), h.shows)
	}
}

func TestBatcherDisabledDoesNotReserveItemKeys(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	st := &fakeStore{send: true}
	b, n := newBatcher(h, c)
	n.Store = st
	n.Enabled = false
	b.Add(newRepoItem("talkable/repo-a"))
	if sent, err := b.Flush(context.Background()); sent || err != nil {
		t.Fatalf("Flush = %v, %v", sent, err)
	}
	if len(st.calls) != 0 {
		t.Errorf("disabled notifier reserved %v", st.calls)
	}
}
