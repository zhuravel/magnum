package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/webhooks"
)

// fakeRelay is magnum-relay behind a fake curl (an execx.Runner): it reads
// the request from the config curl gets on stdin, as the relay would read
// it, and answers GET /v1/events from its journal (relay-api.md).
type fakeRelay struct {
	mu     sync.Mutex
	epoch  string
	oldest int64 // the seq before the oldest kept event
	events []webhooks.Event
	// status, when set, is the code the relay answers (401, 503); timeouts,
	// when set, makes curl time out instead, each time after a few more
	// milliseconds, as curl says.
	status   int
	timeouts bool
	// block makes a long poll with nothing to list wait for its context,
	// as the relay waits for an event (Run's tests).
	block bool
	// now is the relay's clock (the harness's): it stamps the server time
	// and the deliveries received without a time.
	now func() time.Time
	// afters are the cursors the pages were asked after; stdins what curl
	// read on stdin.
	afters []string
	stdins []string
	argv   [][]string
}

func newFakeRelay(epoch string) *fakeRelay { return &fakeRelay{epoch: epoch} }

// deliver adds deliveries to the journal, numbered after the last one and
// received now unless they say when.
func (r *fakeRelay) deliver(evs ...webhooks.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range evs {
		ev.Seq = r.oldest + int64(len(r.events)) + 1
		if ev.ReceivedAt.IsZero() && r.now != nil {
			ev.ReceivedAt = r.now().UTC().Truncate(time.Second)
		}
		r.events = append(r.events, ev)
	}
}

// reset is the relay's journal created again under another epoch.
func (r *fakeRelay) reset(epoch string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.epoch, r.oldest, r.events = epoch, 0, nil
}

func (r *fakeRelay) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.afters)
}

func (r *fakeRelay) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	if c.Name != "curl" {
		return execx.Result{}, fmt.Errorf("fakeRelay: not curl: %s", c.String())
	}
	var raw string
	for line := range strings.SplitSeq(string(c.Stdin), "\n") {
		if v, ok := strings.CutPrefix(line, `url = "`); ok {
			raw = strings.TrimSuffix(v, `"`)
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Path != "/v1/events" {
		return execx.Result{}, fmt.Errorf("fakeRelay: bad url %q", raw)
	}
	q := u.Query()
	after := q.Get("after")
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit == 0 {
		limit = 200
	}
	wait, _ := strconv.Atoi(q.Get("wait"))
	r.mu.Lock()
	r.afters = append(r.afters, after)
	r.stdins = append(r.stdins, string(c.Stdin))
	r.argv = append(r.argv, append([]string{c.Name}, c.Args...))
	answer := func(code int, v any) (execx.Result, error) {
		b, _ := json.Marshal(v)
		return execx.Result{Stdout: append(b, []byte("\n"+strconv.Itoa(code))...)}, nil
	}
	if r.timeouts {
		n := len(r.afters)
		r.mu.Unlock()
		return execx.Result{Code: 28}, &execx.ExitError{Cmd: c, Code: 28,
			Stderr: fmt.Sprintf("curl: (28) Operation timed out after %d milliseconds with 0 bytes received", 35000+n)}
	}
	if r.status != 0 {
		code := r.status
		r.mu.Unlock()
		return answer(code, map[string]string{"error": "refused"})
	}
	start := r.oldest
	if after != "" {
		epoch, seq, _ := strings.Cut(after, ":")
		n, _ := strconv.ParseInt(seq, 10, 64)
		if epoch != r.epoch || n < r.oldest {
			defer r.mu.Unlock()
			return answer(410, map[string]string{"error": "gap", "epoch": r.epoch, "oldest": fmt.Sprintf("%s:%d", r.epoch, r.oldest)})
		}
		start = n
	}
	var page []webhooks.Event
	for _, ev := range r.events {
		if ev.Seq > start && len(page) < limit {
			page = append(page, ev)
		}
	}
	high := r.oldest + int64(len(r.events))
	next := start
	if len(page) > 0 {
		next = page[len(page)-1].Seq
	}
	cursor := func(n int64) string { return fmt.Sprintf("%s:%d", r.epoch, n) }
	body := webhooks.Page{Epoch: r.epoch, Events: page, Next: cursor(next), HighWater: cursor(high), Oldest: cursor(r.oldest)}
	if r.now != nil {
		body.ServerTime = r.now().UTC().Truncate(time.Second)
	}
	block := r.block && len(page) == 0 && wait > 0
	r.mu.Unlock()
	if block {
		<-ctx.Done()
		return execx.Result{}, ctx.Err()
	}
	if body.Events == nil {
		body.Events = []webhooks.Event{}
	}
	return answer(200, body)
}

// prEvent is a pull_request delivery of talkable/talkable#n.
func prEvent(id string, n int, action string) webhooks.Event {
	return webhooks.Event{DeliveryID: id, Event: "pull_request", Action: action, InstallationID: 1, RepositoryID: 2,
		Repository: "talkable/talkable", PRNumbers: []int{n}, HeadSHA: "ffff"}
}

// shadowMod puts the harness in shadow mode over relay ([webhooks] mode =
// "shadow", owners = ["talkable"]), reading it through the real client and
// a fake curl, with a token file of mode 0600; the relay keeps the
// harness's clock.
func shadowMod(relay *fakeRelay) func(*harness) {
	return func(h *harness) {
		relay.now = h.clock.Now
		token := filepath.Join(h.t.TempDir(), "relay.token")
		if err := os.WriteFile(token, []byte("relay-token-0123456789abcdef012345\n"), 0o600); err != nil {
			h.t.Fatal(err)
		}
		h.cfg.Webhooks = config.Webhooks{Mode: config.WebhooksShadow, URL: "https://relay.example", TokenFile: token,
			Owners: []string{"talkable"}, Wait: config.Duration{Duration: 25 * time.Second}, StaleAfter: config.Duration{Duration: 90 * time.Second}}
		h.d.Webhooks = &webhooks.Client{Runner: relay, URL: h.cfg.Webhooks.URL, TokenFile: token}
	}
}

// shadowOn starts what goWebhooks starts but its goroutines: the test reads
// the pages itself (h.e.webhooksStep) and writes what they and the polls
// recorded when it reads the registry (written).
func (h *harness) shadowOn() {
	h.e.loadWebhooks(h.ctx)
	h.e.hooks.on.Store(true)
}

// written writes what the consumer and the comparison recorded, as the
// writer goroutine does once told.
func (h *harness) written() { h.e.flushWebhooks(h.ctx) }

// step reads one page, as the consumer's loop does.
func (h *harness) step() {
	h.t.Helper()
	if ok, _ := h.e.webhooksStep(h.ctx); !ok {
		h.t.Fatalf("the relay's page failed: %+v", h.e.hooks.health)
	}
}

// webhooksTotals sums the hourly totals the consumer kept in kv.
func (h *harness) webhooksTotals() WebhooksHour {
	h.t.Helper()
	h.written()
	_, hours := ReadWebhooks(h.ctx, h.st)
	var sum WebhooksHour
	for _, x := range hours {
		sum.Events += x.Events
		sum.Changes += x.Changes
		sum.Announced += x.Announced
		sum.Missed += x.Missed
		sum.Early += x.Early
		sum.Gaps += x.Gaps
	}
	return sum
}

// webhooksEvents are the registry's events of kind.
func (h *harness) webhooksEvents(kind string) []store.Event {
	h.t.Helper()
	h.written()
	evs, err := h.st.EventsOfKindsSince(h.ctx, time.Time{}, kind)
	if err != nil {
		h.t.Fatal(err)
	}
	return evs
}

// The consumer keeps its cursor in kv with the relay's epoch: a first start
// begins at the relay's newest event (the history is what the polls already
// saw), each page moves it past its events, a restart reads on from it, and
// a gap (the relay's journal created again) starts it at the newest event
// of the new journal.
func TestTheWebhooksCursorIsKeptAcrossRestartsAndAfterAGap(t *testing.T) {
	relay := newFakeRelay("ep1")
	relay.deliver(prEvent("d1", 1, "opened")) // history before the first start
	h := newHarness(t, shadowMod(relay))
	h.shadowOn()
	h.step()
	h.step()
	relay.deliver(prEvent("d2", 1, "synchronize"), prEvent("d3", 1, "labeled"))
	h.step()
	if got, want := relay.calls(), []string{"", "ep1:1", "ep1:1"}; !slices.Equal(got, want) {
		t.Fatalf("pages asked after %q, want %q", got, want)
	}
	h.written()
	if v, _ := h.e.getKV(h.ctx, KVWebhooksCursor); v != `{"epoch":"ep1","cursor":"ep1:3"}` {
		t.Fatalf("kept cursor = %s", v)
	}
	if got := h.webhooksTotals().Events; got != 2 {
		t.Errorf("events = %d, want 2: the history before the first start is not read", got)
	}

	// A restart reads on from the kept cursor.
	h.e = New(h.d)
	h.shadowOn()
	h.step()
	if got := relay.calls(); got[len(got)-1] != "ep1:3" {
		t.Fatalf("after a restart the page was asked after %q, want ep1:3", got[len(got)-1])
	}

	// The relay's journal is created again: 410, then the newest event of
	// the new journal, then on from there.
	relay.reset("ep2")
	relay.deliver(prEvent("d4", 1, "edited"))
	h.step()
	h.step()
	h.step()
	got := relay.calls()
	if want := []string{"ep1:3", "", "ep2:1"}; !slices.Equal(got[len(got)-3:], want) {
		t.Fatalf("pages after the gap asked after %q, want %q", got[len(got)-3:], want)
	}
	if tot := h.webhooksTotals(); tot.Gaps != 1 || tot.Events != 2 {
		t.Errorf("totals = %+v, want 1 gap and still 2 events", tot)
	}
	if len(h.webhooksEvents("webhooks.gap")) != 1 {
		t.Error("the gap wrote no webhooks.gap event")
	}
}

// A delivery the relay lists twice (a repeat after a restart's replay, a
// redelivery the relay kept) counts once and hints once.
func TestTheWebhooksConsumerReadsEachDeliveryOnce(t *testing.T) {
	relay := newFakeRelay("ep1")
	h := newHarness(t, shadowMod(relay))
	h.shadowOn()
	h.step()
	relay.deliver(prEvent("d1", 1, "synchronize"), prEvent("d1", 1, "synchronize"))
	h.step()
	relay.deliver(prEvent("d1", 1, "synchronize"))
	h.step()
	if got := h.webhooksTotals().Events; got != 1 {
		t.Errorf("events = %d, want 1", got)
	}
	if got := len(h.e.hooks.hints); got != 1 {
		t.Errorf("hints = %d, want 1", got)
	}
}

// Each event of a covered owner's watched repository hints at what it
// names: a pull_request*, review or issue_comment event its PR, a check or
// a status its head (and the PRs a check names); an issue's comment, a
// repository no watch has and an owner outside owners hint at nothing.
func TestEachWebhookEventHintsAtWhatItNames(t *testing.T) {
	h := newHarness(t, shadowMod(newFakeRelay("ep1")))
	sha := strings.Repeat("ab", 20)
	repo := "talkable/talkable"
	for _, tc := range []struct {
		name string
		ev   webhooks.Event
		want []hookKey
	}{
		{"push", webhooks.Event{Event: "pull_request", Action: "synchronize", Repository: "talkable/Talkable", PRNumbers: []int{4}, HeadSHA: sha},
			[]hookKey{{repo: repo, pr: 4}}},
		{"review", webhooks.Event{Event: "pull_request_review", Action: "submitted", Repository: repo, PRNumbers: []int{4}},
			[]hookKey{{repo: repo, pr: 4}}},
		{"thread", webhooks.Event{Event: "pull_request_review_thread", Action: "resolved", Repository: repo, PRNumbers: []int{4}},
			[]hookKey{{repo: repo, pr: 4}}},
		{"comment on a PR", webhooks.Event{Event: "issue_comment", Action: "created", Repository: repo, PRNumbers: []int{4}},
			[]hookKey{{repo: repo, pr: 4}}},
		{"check run", webhooks.Event{Event: "check_run", Action: "completed", Repository: repo, PRNumbers: []int{4, 5}, HeadSHA: strings.ToUpper(sha)},
			[]hookKey{{repo: repo, pr: 4}, {repo: repo, pr: 5}, {repo: repo, sha: sha}}},
		{"a fork's check suite", webhooks.Event{Event: "check_suite", Action: "completed", Repository: repo, HeadSHA: sha},
			[]hookKey{{repo: repo, sha: sha}}},
		{"status", webhooks.Event{Event: "status", Repository: repo, HeadSHA: sha}, []hookKey{{repo: repo, sha: sha}}},
		{"comment on an issue", webhooks.Event{Event: "issue_comment", Action: "created", Repository: repo}, nil},
		{"installation", webhooks.Event{Event: "installation_repositories", Action: "added"}, nil},
		{"unwatched repository", webhooks.Event{Event: "pull_request", Action: "opened", Repository: "talkable/other", PRNumbers: []int{1}}, nil},
		{"owner outside owners", webhooks.Event{Event: "pull_request", Action: "opened", Repository: "zhuravel/tools", PRNumbers: []int{1}}, nil},
	} {
		if got := h.e.hintKeys(tc.ev); !slices.Equal(got, tc.want) {
			t.Errorf("%s: hints %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The comparison: a poll change some event hinted at within the 5 minutes
// before it is announced, and so is one whose event comes within a minute
// after the poll saw it; a change with none is missed, with one
// webhooks.missed event at debug level that names the PR, what changed and
// the last event read for it; an event no change followed within 5 minutes
// is early or noise. Every change counts once, and a repository's first
// sync counts nothing.
func TestTheShadowComparisonCountsAnnouncedMissedAndEarly(t *testing.T) {
	relay := newFakeRelay("ep1")
	h := newHarness(t, shadowMod(relay))
	h.open(prSpec{n: 1, head: "a1"}, prSpec{n: 2, head: "b1"}, prSpec{n: 3, head: "c1"})
	h.startup()
	h.shadowOn()
	h.step()
	h.tick() // the first sync: a baseline, nothing compared
	if tot := h.webhooksTotals(); tot.Changes != 0 {
		t.Fatalf("the first sync counted %+v", tot)
	}

	// #1 is pushed with its event, #2 without one; #3 gets an event and no
	// change.
	relay.deliver(prEvent("d1", 1, "synchronize"), prEvent("d3", 3, "labeled"))
	h.step()
	h.advance(30 * time.Second)
	h.gh.setHead("talkable/talkable", 1, "a2")
	h.gh.setHead("talkable/talkable", 2, "b2")
	h.tick()
	if tot := h.webhooksTotals(); tot.Changes != 1 || tot.Announced != 1 || tot.Missed != 0 {
		t.Fatalf("after the push: %+v, want #1 announced and #2 waiting for a late event", tot)
	}
	h.advance(30 * time.Second)
	h.step()
	h.tick() // nothing changed: #1 and #2 count once
	h.advance(31 * time.Second)
	h.step() // #2's minute is over
	if tot := h.webhooksTotals(); tot.Changes != 2 || tot.Announced != 1 || tot.Missed != 1 {
		t.Fatalf("a minute after the push: %+v, want #2 missed", tot)
	}
	missed := h.webhooksEvents("webhooks.missed")
	if len(missed) != 1 || missed[0].Level != "debug" || deref(missed[0].Subject) != "watch:talkable" ||
		!strings.Contains(missed[0].Message, "talkable/talkable#2 change (head)") || !strings.Contains(missed[0].Message, "no event for it") {
		t.Fatalf("missed events = %+v", missed)
	}
	if evs, err := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#2", 0); err != nil || slices.ContainsFunc(evs, func(e store.Event) bool {
		return strings.HasPrefix(e.Kind, "webhooks.")
	}) {
		t.Errorf("the PR's own events hold the consumer's (%v): the CLI reads them by window", err)
	}

	// #1 is pushed again; its event comes 30 s after the poll saw it.
	h.gh.setHead("talkable/talkable", 1, "a3")
	h.tick()
	h.advance(30 * time.Second)
	relay.deliver(prEvent("d4", 1, "synchronize"))
	h.step()
	if tot := h.webhooksTotals(); tot.Changes != 3 || tot.Announced != 2 {
		t.Fatalf("a late event: %+v, want #1 announced", tot)
	}

	// #2's event comes 6 minutes before its change: the change is missed,
	// naming that last event, and the event is early or noise, as #3's is.
	received := h.clock.Now().UTC().Format(time.RFC3339)
	relay.deliver(prEvent("d5", 2, "labeled"))
	h.step()
	for range 12 {
		h.advance(30 * time.Second)
		h.step()
		h.tick()
	}
	h.gh.setHead("talkable/talkable", 2, "b3")
	h.tick()
	h.advance(61 * time.Second)
	h.step()
	tot := h.webhooksTotals()
	if tot.Changes != 4 || tot.Announced != 2 || tot.Missed != 2 || tot.Early != 2 || tot.Events != 4 {
		t.Fatalf("totals = %+v, want 4 changes (2 announced, 2 missed), 2 early or noise, 4 events", tot)
	}
	missed = h.webhooksEvents("webhooks.missed")
	if len(missed) != 2 || !strings.Contains(missed[1].Message, "the last event for it: pull_request.labeled") {
		t.Fatalf("missed events = %+v", missed)
	}
	var data map[string]any
	if err := json.Unmarshal(missed[1].Data, &data); err != nil || data["last_event"] != "pull_request.labeled" ||
		data["last_event_received_at"] != received || data["pr"] != "talkable/talkable#2" {
		t.Errorf("missed data = %s (%v)", missed[1].Data, err)
	}
}

// Only a newer updatedAt is a change: a radar answer from a replica behind
// the last one counts nothing, and the next newer one counts once.
func TestTheShadowComparisonCountsOnlyANewerUpdatedAt(t *testing.T) {
	var hk webhookState
	t0 := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	radar := func(updated time.Time) []github.PRRadar {
		return []github.PRRadar{{NodeID: "N1", Number: 1, HeadRefOid: "a1", UpdatedAt: updated}}
	}
	stored := []store.PR{{NodeID: "N1", Number: 1, HeadSHA: "a1", GHState: store.GHOpen, GHUpdatedAt: new(t0)}}
	for i, tc := range []struct {
		updated time.Time
		want    int
	}{{t0, 0}, {t0.Add(-time.Minute), 0}, {t0, 0}, {t0.Add(time.Minute), 1}, {t0.Add(time.Minute), 0}} {
		if got := hk.diff("talkable/talkable", radar(tc.updated), stored, false, true); len(got) != tc.want {
			t.Errorf("radar %d (updated %s): %d changes %+v, want %d", i, tc.updated.Format("15:04"), len(got), got, tc.want)
		}
	}
}

// An event the relay received longer than 5 minutes before the page that
// lists it (a backlog read after a sleep or an outage) is counted but hints
// at nothing: it could not have announced what a poll sees now, and it is no
// early event either.
func TestABacklogOfOldEventsHintsAtNothing(t *testing.T) {
	relay := newFakeRelay("ep1")
	h := newHarness(t, shadowMod(relay))
	h.shadowOn()
	h.step()
	old := prEvent("d1", 1, "synchronize")
	old.ReceivedAt = h.clock.Now().Add(-6 * time.Minute)
	relay.deliver(old, prEvent("d2", 2, "synchronize"))
	h.step()
	if got := h.webhooksTotals().Events; got != 2 {
		t.Errorf("events = %d, want 2", got)
	}
	if len(h.e.hooks.hints) != 1 || !slices.Contains(h.e.hooks.hints[0].keys, hookKey{repo: "talkable/talkable", pr: 2}) {
		t.Errorf("hints = %+v, want #2's alone", h.e.hooks.hints)
	}
}

// A PR that leaves the open list counts once GitHub confirms it closed, a
// reopened one counts again, and a PR the radar's list skipped once (GitHub
// says it is still open) counts nothing.
func TestTheShadowComparisonCountsAConfirmedCloseOnce(t *testing.T) {
	relay := newFakeRelay("ep1")
	h := newHarness(t, shadowMod(relay))
	t0 := h.clock.Now()
	one, two := prSpec{n: 1, head: "a1", updated: t0}, prSpec{n: 2, head: "b1", updated: t0}
	h.open(one, two)
	h.startup()
	h.shadowOn()
	h.step()
	h.tick()

	// #1 skipped once by the radar: GitHub says it is open.
	h.open(two)
	h.advance(30 * time.Second)
	h.step()
	h.tick()
	h.open(one, two)
	for range 3 {
		h.advance(30 * time.Second)
		h.step()
		h.tick()
	}
	if tot := h.webhooksTotals(); tot.Changes != 0 {
		t.Fatalf("a radar skip counted %+v", tot)
	}

	// #2 closed with its event.
	relay.deliver(prEvent("d1", 2, "closed"))
	h.gh.closed[2] = "CLOSED"
	h.open(one)
	for range 4 {
		h.advance(30 * time.Second)
		h.step()
		h.tick()
	}
	if tot := h.webhooksTotals(); tot.Changes != 1 || tot.Announced != 1 {
		t.Fatalf("a close: %+v, want one announced change", tot)
	}
	h.wantState(2, store.PRClosed)
}

// One event that names several PRs announces the change of each, before
// the poll sees them and after alike: a check run of two PRs, and a status
// on a head two PRs share.
func TestOneEventNamingTwoPRsAnnouncesEachOfThem(t *testing.T) {
	relay := newFakeRelay("ep1")
	h := newHarness(t, shadowMod(relay))
	t0 := h.clock.Now()
	spec := func(n int, head string) prSpec { return prSpec{n: n, head: head, author: "dependabot", updated: t0} }
	h.open(spec(4, "a4"), spec(5, "a5"), spec(6, "s1"), spec(7, "s1"))
	h.startup()
	h.shadowOn()
	h.step()
	h.tick()
	checkRun := func(id string) webhooks.Event {
		return webhooks.Event{DeliveryID: id, Event: "check_run", Action: "completed", Repository: "talkable/talkable", PRNumbers: []int{4, 5}}
	}

	// The event comes before the poll sees both pushes.
	relay.deliver(checkRun("d1"))
	h.step()
	h.advance(30 * time.Second)
	h.open(spec(4, "b4"), spec(5, "b5"), spec(6, "s1"), spec(7, "s1"))
	h.tick()
	if tot := h.webhooksTotals(); tot.Changes != 2 || tot.Announced != 2 {
		t.Fatalf("an event before both pushes: %+v, want both announced", tot)
	}

	// The event comes 20 s after the poll saw both pushes.
	h.advance(30 * time.Second)
	h.open(spec(4, "c4"), spec(5, "c5"), spec(6, "s1"), spec(7, "s1"))
	h.tick()
	h.advance(20 * time.Second)
	relay.deliver(checkRun("d2"))
	h.step()
	if tot := h.webhooksTotals(); tot.Changes != 4 || tot.Announced != 4 {
		t.Fatalf("an event after both pushes: %+v, want both announced", tot)
	}

	// A status on the head both #6 and #7 were pushed to.
	relay.deliver(webhooks.Event{DeliveryID: "d3", Event: "status", Repository: "talkable/talkable", HeadSHA: "S2"})
	h.step()
	h.advance(30 * time.Second)
	h.open(spec(4, "c4"), spec(5, "c5"), spec(6, "s2"), spec(7, "s2"))
	h.tick()
	h.advance(61 * time.Second)
	h.step()
	if tot := h.webhooksTotals(); tot.Changes != 6 || tot.Announced != 6 || tot.Missed != 0 || tot.Early != 0 {
		t.Fatalf("a status on a shared head: %+v, want both announced and no event early", tot)
	}
}

// A PR closed while the relay lost its events (a gap) is no miss: the
// first poll after the gap sees it leave the open list and compares
// nothing, and the close GitHub confirms at the next poll stays uncounted.
func TestACloseDuringAGapIsNoMiss(t *testing.T) {
	relay := newFakeRelay("ep1")
	h := newHarness(t, shadowMod(relay))
	t0 := h.clock.Now()
	one, two := prSpec{n: 1, head: "a1", updated: t0}, prSpec{n: 2, head: "b1", updated: t0}
	h.open(one, two)
	h.startup()
	h.shadowOn()
	h.step()
	h.tick()

	// The relay's journal is lost with #2's close in it.
	relay.reset("ep2")
	h.gh.closed[2] = "CLOSED"
	h.open(one)
	h.step() // 410
	h.step() // the new journal's newest event
	for range 4 {
		h.advance(30 * time.Second)
		h.step()
		h.tick()
	}
	h.advance(61 * time.Second)
	h.step()
	h.wantState(2, store.PRClosed)
	if tot := h.webhooksTotals(); tot.Changes != 0 || tot.Missed != 0 {
		t.Fatalf("totals = %+v, want the close in the gap uncounted", tot)
	}
}

// The poll never waits for the consumer's registry writes: they are the
// writer goroutine's, so a write stuck on a busy registry holds neither the
// comparison nor the tick.
func TestASlowRegistryWriteNeverHoldsThePoll(t *testing.T) {
	relay := newFakeRelay("ep1")
	h := newHarness(t, shadowMod(relay))
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.shadowOn()
	h.step()
	h.tick()

	release, stuck := make(chan struct{}), make(chan struct{}, 1)
	h.e.hooks.setKV = func(ctx context.Context, key, value string) error {
		select {
		case stuck <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return h.st.SetKV(ctx, key, value)
	}
	ctx, cancel := context.WithCancel(h.ctx)
	writer := make(chan struct{})
	go func() {
		defer close(writer)
		h.e.webhooksWriter(ctx)
	}()
	relay.deliver(prEvent("d1", 1, "synchronize"))
	h.step() // tells the writer, whose write sticks
	select {
	case <-stuck:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never wrote")
	}
	h.advance(30 * time.Second)
	h.gh.setHead("talkable/talkable", 1, "a2")
	ticked := make(chan error, 1)
	go func() { ticked <- h.e.Tick(h.ctx) }()
	select {
	case err := <-ticked:
		if err != nil {
			t.Errorf("tick: %v", err)
		}
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("the poll waited for the consumer's registry write")
	}
	h.settle()
	close(release)
	cancel()
	<-writer
	h.e.hooks.setKV = nil
	if tot := h.webhooksTotals(); tot.Changes != 1 || tot.Announced != 1 {
		t.Errorf("totals = %+v, want the push announced once the writes went through", tot)
	}
}

// Owners outside [webhooks] owners are never compared, whatever their
// events and their polls.
func TestWebhooksNeverCompareAnOwnerOutsideOwners(t *testing.T) {
	relay := newFakeRelay("ep1")
	h := newHarness(t, shadowMod(relay))
	h.gh.set("zhuravel/tools", prSpec{n: 5, head: "t1", updated: h.clock.Now()})
	h.startup()
	h.shadowOn()
	h.step()
	h.tick()
	relay.deliver(webhooks.Event{DeliveryID: "d1", Event: "pull_request", Action: "synchronize", Repository: "zhuravel/tools", PRNumbers: []int{5}})
	h.step()
	h.gh.set("zhuravel/tools", prSpec{n: 5, head: "t2", updated: h.clock.Now()})
	h.advance(30 * time.Second)
	h.tick()
	h.advance(2 * time.Minute)
	h.step()
	if tot := h.webhooksTotals(); tot.Changes != 0 || tot.Events != 1 {
		t.Fatalf("totals = %+v, want the event counted and nothing compared", tot)
	}
	if len(h.e.hooks.hints) != 0 {
		t.Errorf("hints = %d, want none for an owner outside owners", len(h.e.hooks.hints))
	}
}

// registryDump is every row of the registry, but the consumer's own: its kv
// keys and webhooks.* events (events without their ids, which the
// consumer's own rows move).
func registryDump(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	for _, table := range dumpQuery(t, db, "", `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`) {
		query := "SELECT * FROM " + table
		switch table {
		case "kv":
			query += ` WHERE key NOT LIKE 'webhooks.%'`
		case "events":
			query = `SELECT at, level, subject, kind, step, phase, message, data_json FROM events WHERE kind NOT LIKE 'webhooks.%' ORDER BY id`
		}
		for _, row := range dumpQuery(t, db, table, query) {
			b.WriteString(row + "\n")
		}
	}
	return b.String()
}

// dumpQuery is each row of query as "<table>: col=value ...", or the one
// column's value when table is "".
func dumpQuery(t *testing.T, db *sql.DB, table, query string) []string {
	t.Helper()
	r, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer r.Close()
	cols, _ := r.Columns()
	var out []string
	for r.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := r.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := table + ":"
		for i, v := range vals {
			if bs, ok := v.([]byte); ok {
				v = string(bs)
			}
			if table == "" {
				row = fmt.Sprint(v)
				break
			}
			row += fmt.Sprintf(" %s=%v", cols[i], v)
		}
		out = append(out, row)
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Shadow mode changes nothing the daemon does: the same PRs, pushed, opened
// and reviewed with the relay's events coming in and compared, leave the
// registry row for row as they leave it with webhooks off (but the
// consumer's own kv keys and events), with the same GitHub reads, the same
// rounds and the same agents' calls.
func TestShadowModeChangesNothingTheDaemonDoes(t *testing.T) {
	type outcome struct {
		dump           string
		gh, rounds, ag []string
		totals         WebhooksHour
	}
	run := func(shadow bool) outcome {
		relay := newFakeRelay("ep1")
		var mods []func(*harness)
		if shadow {
			mods = append(mods, shadowMod(relay))
		}
		h := newHarness(t, mods...)
		feed := func(evs ...webhooks.Event) {
			if shadow {
				relay.deliver(evs...)
				h.step()
			}
		}
		h.open(prSpec{n: 1, head: "base1"})
		h.startup()
		if shadow {
			h.shadowOn()
			h.step()
		}
		h.tick() // the first sync
		feed(prEvent("d1", 2, "opened"))
		h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1"})
		h.tick() // #2 queued
		h.advance(5 * time.Minute)
		feed(prEvent("d2", 1, "labeled"))
		h.tick() // #2 reviewed
		h.wantState(2, store.PRReviewed)
		feed(prEvent("d3", 2, "synchronize"))
		// #3 is skipped (skip_authors): a second PR waiting for the one
		// slot would take it or not as the eviction meets #2's round
		// goroutine ending, which differs between runs, webhooks or not.
		h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"}, prSpec{n: 3, head: "c1", author: "dependabot"})
		h.advance(time.Minute)
		h.tick() // #2 pushed with its event, #3 opened without one
		for range 3 {
			h.advance(20 * time.Minute)
			feed()
			h.tick()
		}
		var rounds []string
		for _, in := range h.rd.all() {
			rounds = append(rounds, fmt.Sprintf("#%d %s %s", in.PR.Number, in.Kind, in.TargetSHA))
		}
		h.gh.mu.Lock()
		gh := slices.Clone(h.gh.calls)
		h.gh.mu.Unlock()
		home := func(s string) string { return strings.ReplaceAll(s, h.layout.Home, "HOME") } // each harness has its own
		var ag []string
		for _, c := range h.ag.all() {
			ag = append(ag, home(c))
		}
		out := outcome{dump: home(registryDump(t, h.st.DB())), gh: gh, rounds: rounds, ag: ag}
		if shadow {
			out.totals = h.webhooksTotals()
		}
		return out
	}
	off, shadow := run(false), run(true)
	if shadow.totals.Changes == 0 || shadow.totals.Announced == 0 || shadow.totals.Missed == 0 {
		t.Fatalf("the shadow run compared nothing (%+v): the test proves nothing", shadow.totals)
	}
	if len(off.rounds) < 2 {
		t.Fatalf("rounds = %q, want the review and the re-review", off.rounds)
	}
	if shadow.dump != off.dump {
		t.Errorf("shadow mode changed the registry:\n--- off\n%s\n--- shadow\n%s", off.dump, shadow.dump)
	}
	if !slices.Equal(shadow.gh, off.gh) {
		t.Errorf("shadow mode changed the GitHub reads:\noff    %q\nshadow %q", off.gh, shadow.gh)
	}
	if !slices.Equal(shadow.rounds, off.rounds) {
		t.Errorf("shadow mode changed the rounds:\noff    %q\nshadow %q", off.rounds, shadow.rounds)
	}
	if !slices.Equal(shadow.ag, off.ag) {
		t.Errorf("shadow mode changed the agents' calls:\noff    %q\nshadow %q", off.ag, shadow.ag)
	}
}

// The stream's health: a refused token makes it unhealthy with the cause
// (said once while it lasts, as a curl time-out whose milliseconds change
// on every try is) and compares nothing; a page makes it healthy again; no
// page within stale_after is unhealthy; a gap waits before the next page
// and holds each repository until its own first poll after it, so a change
// whose event the gap lost is never a miss.
func TestTheWebhooksStreamsHealth(t *testing.T) {
	relay := newFakeRelay("ep1")
	h := newHarness(t, shadowMod(relay), func(h *harness) { h.cfg.Watches[0].Include = []string{"talkable", "other"} })
	t0 := h.clock.Now()
	h.gh.set("talkable/other", prSpec{n: 7, head: "o1", author: "dependabot", updated: t0})
	h.open(prSpec{n: 1, head: "a1", updated: t0})
	h.startup()
	h.shadowOn()
	state := func() (bool, string) {
		h.written()
		health, _ := ReadWebhooks(h.ctx, h.st)
		return health.State(h.clock.Now(), 90*time.Second)
	}
	if ok, cause := state(); ok || cause != "no page from the relay yet" {
		t.Fatalf("before any page: %v %q", ok, cause)
	}
	relay.status = 401
	for range 2 {
		if ok, _ := h.e.webhooksStep(h.ctx); ok {
			t.Fatal("a 401 answered a page")
		}
	}
	if ok, cause := state(); ok || !strings.Contains(cause, "HTTP 401") {
		t.Fatalf("after a 401: %v %q", ok, cause)
	}
	if n := len(h.webhooksEvents("webhooks.unhealthy")); n != 1 {
		t.Errorf("webhooks.unhealthy events = %d, want 1 while the cause lasts", n)
	}
	h.tick() // first sync
	h.gh.setHead("talkable/talkable", 1, "a2")
	h.advance(30 * time.Second)
	h.tick()
	h.advance(2 * time.Minute)
	if ok, _ := h.e.webhooksStep(h.ctx); ok {
		t.Fatal("a 401 answered a page")
	}
	if tot := h.webhooksTotals(); tot.Changes != 0 {
		t.Errorf("an unhealthy stream compared %+v", tot)
	}

	relay.status = 0
	h.step()
	h.step()
	if ok, cause := state(); !ok {
		t.Fatalf("after a page: unhealthy %q", cause)
	}
	if n := len(h.webhooksEvents("webhooks.healthy")); n != 1 {
		t.Errorf("webhooks.healthy events = %d, want 1", n)
	}
	// A busy relay (429: too many long polls wait) is tried again a second
	// later, then longer, and leaves the stream healthy.
	relay.status = 429
	for i, want := range []time.Duration{time.Second, 2 * time.Second} {
		if ok, pause := h.e.webhooksStep(h.ctx); !ok || pause != want {
			t.Errorf("429 #%d: ok %v, waits %s, want %s", i+1, ok, pause, want)
		}
	}
	if ok, cause := state(); !ok || len(h.webhooksEvents("webhooks.unhealthy")) != 1 {
		t.Errorf("a busy relay made the stream unhealthy: %q", cause)
	}
	relay.status = 0
	relay.timeouts = true
	for range 3 {
		if ok, _ := h.e.webhooksStep(h.ctx); ok {
			t.Fatal("a time-out answered a page")
		}
	}
	if ok, cause := state(); ok || !strings.Contains(cause, "timed out") {
		t.Fatalf("after time-outs: %v %q", ok, cause)
	}
	if n := len(h.webhooksEvents("webhooks.unhealthy")); n != 2 {
		t.Errorf("webhooks.unhealthy events = %d, want 2: the 401 and the time-outs once", n)
	}
	relay.timeouts = false
	h.step()
	h.advance(91 * time.Second)
	if ok, cause := state(); ok || cause != "no page from the relay for 1m31s" {
		t.Fatalf("stale: %v %q", ok, cause)
	}

	relay.reset("ep2")
	if ok, pause := h.e.webhooksStep(h.ctx); !ok || pause < time.Second { // 410
		t.Fatalf("a gap: ok %v, waits %s before the next page, want a backoff", ok, pause)
	}
	if ok, cause := state(); ok || !strings.Contains(cause, "gap at") {
		t.Fatalf("after a gap: %v %q", ok, cause)
	}
	h.step() // the cursor at the new journal's newest event
	if ok, _ := state(); ok {
		t.Fatal("a page after a gap made the stream healthy before a poll resynced")
	}
	// Both repositories changed with their events lost in the gap: each
	// one's first poll after it compares nothing, the second one's too.
	h.advance(time.Second)
	h.gh.set("talkable/other", prSpec{n: 7, head: "o2", author: "dependabot", updated: t0})
	h.gh.setHead("talkable/talkable", 1, "a3")
	h.tick()
	h.advance(61 * time.Second)
	h.step()
	if tot := h.webhooksTotals(); tot.Changes != 0 {
		t.Errorf("the polls after a gap compared %+v", tot)
	}
	if ok, cause := state(); !ok {
		t.Fatalf("after every repository's poll resynced: unhealthy %q", cause)
	}
	relay.deliver(prEvent("d1", 1, "synchronize"))
	h.step()
	h.gh.setHead("talkable/talkable", 1, "a4")
	h.tick()
	if tot := h.webhooksTotals(); tot.Changes != 1 || tot.Announced != 1 {
		t.Errorf("after the resync: %+v, want the push announced", tot)
	}
}

// The consumer runs beside the daemon's loop and ends with it, and never
// with --once or a dry run, nor with webhooks off.
func TestTheWebhooksConsumerRunsWithRunAndEndsWithIt(t *testing.T) {
	relay := newFakeRelay("ep1")
	relay.block = true
	h := newHarness(t, shadowMod(relay))
	ctx, cancel := context.WithCancel(h.ctx)
	done := h.goRun(ctx, Options{NoSignals: true})
	deadline := time.Now().Add(5 * time.Second)
	for len(relay.calls()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := relay.calls(); len(got) < 2 || got[0] != "" || got[1] != "ep1:0" {
		t.Fatalf("pages asked after %q, want the newest event, then a long poll after it", got)
	}
	relay.mu.Lock()
	argv, stdin := relay.argv[1], relay.stdins[1]
	relay.mu.Unlock()
	if !slices.Equal(argv, []string{"curl", "--disable", "--config", "-"}) || strings.Contains(strings.Join(argv, " "), "relay-token") ||
		!strings.Contains(stdin, "Authorization: Bearer relay-token-0123456789abcdef012345") || !strings.Contains(stdin, "wait=25") {
		t.Errorf("curl ran as %q with %q", argv, stdin)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if h.e.hooks.on.Load() {
		t.Error("the consumer still runs after Run ended")
	}

	for name, mod := range map[string]func(*harness){
		"--once": func(*harness) {},
		"dry run": func(h *harness) {
			h.d.DryRun = true
		},
		"off": func(h *harness) {
			h.cfg.Webhooks.Mode = config.WebhooksOff
		},
	} {
		relay := newFakeRelay("ep1")
		h := newHarness(t, shadowMod(relay), mod)
		if name == "--once" {
			h.open(prSpec{n: 1, head: "a1"})
			if err := <-h.goRun(h.ctx, Options{NoSignals: true, Once: true}); err != nil {
				t.Fatal(err)
			}
		} else {
			<-h.e.goWebhooks(h.ctx)
		}
		if got := relay.calls(); len(got) != 0 || h.e.hooks.on.Load() {
			t.Errorf("%s: the relay was read (%q)", name, got)
		}
	}
}
