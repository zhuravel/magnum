package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/webhooks"
)

// webhooksRows are the consumer's kv values and its webhooks.* events'
// messages and data, as the registry holds them.
func (h *harness) webhooksRows() []string {
	h.t.Helper()
	h.written()
	db := h.st.DB()
	return append(dumpQuery(h.t, db, "", `SELECT value FROM kv WHERE key LIKE 'webhooks.%'`),
		dumpQuery(h.t, db, "", `SELECT message || ' ' || coalesce(data_json, '') FROM events WHERE kind LIKE 'webhooks.%'`)...)
}

// A relay gone wrong, or the proxy in front of it, is not magnum's: a page
// that lists more events than the consumer asked for, or carries an epoch or
// a cursor of 10 KB or with control characters, is refused whole, on a
// first start as after a cursor. The stream's health says what was refused
// (once while it lasts), the cursor kept is the last good one, nothing of
// the page is counted or hinted at, and the registry holds nothing of it;
// once the relay answers well again the consumer reads on from its cursor.
func TestAHostileRelaysPageIsRefusedAndNothingOfItIsKept(t *testing.T) {
	relay := newFakeRelay("ep1")
	h := newHarness(t, shadowMod(relay))
	h.shadowOn()
	long := strings.Repeat("9", 10<<10)
	state := func() (bool, string) {
		h.t.Helper()
		h.written()
		health, _ := ReadWebhooks(h.ctx, h.st)
		return health.State(h.clock.Now(), time.Minute)
	}

	// A first start takes the relay's newest event as its cursor: not one
	// of 10 KB.
	relay.tamper = func(p *webhooks.Page) { p.HighWater = "ep1:" + long }
	if ok, _ := h.e.webhooksStep(h.ctx); ok {
		t.Fatal("a first page with a high water of 10 KB was taken")
	}
	if ok, cause := state(); ok || !strings.Contains(cause, "malformed high_water") {
		t.Errorf("the stream's health %v %q, want the malformed high water", ok, cause)
	}
	if v, ok := h.e.getKV(h.ctx, KVWebhooksCursor); ok && v != `{"epoch":"","cursor":""}` {
		t.Fatalf("a refused first page kept the cursor %.80s", v)
	}
	relay.tamper = nil
	h.step()
	relay.deliver(prEvent("d1", 1, "synchronize"))
	h.step()
	h.written()
	kept, _ := h.e.getKV(h.ctx, KVWebhooksCursor)
	if kept != `{"epoch":"ep1","cursor":"ep1:1"}` {
		t.Fatalf("kept cursor = %s", kept)
	}

	for i, tc := range []struct {
		name   string
		tamper func(*webhooks.Page)
		cause  string
	}{
		{"more events than asked", func(p *webhooks.Page) {
			for len(p.Events) <= webhooksPageLimit {
				p.Events = append(p.Events, prEvent(fmt.Sprintf("x%d", len(p.Events)), 2, "labeled"))
			}
		}, "lists 201 events, more than the 200 asked for"},
		{"a next cursor of 10 KB", func(p *webhooks.Page) { p.Next = "ep1:" + long }, "malformed next"},
		{"an epoch of 10 KB", func(p *webhooks.Page) { p.Epoch = long }, "malformed epoch"},
		{"control characters in the high water", func(p *webhooks.Page) { p.HighWater = "ep1:2\x1b[2J\a" }, "malformed high_water"},
		{"a newline in the oldest", func(p *webhooks.Page) { p.Oldest = "ep1:0\nwebhooks.healthy" }, "malformed oldest"},
	} {
		relay.tamper = tc.tamper
		relay.deliver(prEvent(fmt.Sprintf("t%d", i), 3, "labeled"))
		for range 2 {
			if ok, _ := h.e.webhooksStep(h.ctx); ok {
				t.Fatalf("%s: the page was taken", tc.name)
			}
		}
		if ok, cause := state(); ok || !strings.Contains(cause, tc.cause) {
			t.Errorf("%s: the stream's health %v %q, want %q", tc.name, ok, cause, tc.cause)
		}
		if v, _ := h.e.getKV(h.ctx, KVWebhooksCursor); v != kept {
			t.Errorf("%s: the cursor kept is %.80s, want %s", tc.name, v, kept)
		}
	}
	if n := len(h.webhooksEvents("webhooks.unhealthy")); n != 6 {
		t.Errorf("webhooks.unhealthy events = %d, want 6: each refusal said once while it lasted", n)
	}
	if tot := h.webhooksTotals(); tot.Events != 1 || len(h.e.hooks.hints) != 1 {
		t.Errorf("the refused pages counted %+v, %d hints, want d1's alone", tot, len(h.e.hooks.hints))
	}
	for _, v := range h.webhooksRows() {
		if len(v) > 2000 || strings.Contains(v, "9999999999") || strings.ContainsAny(v, "\x1b\a\n") {
			t.Errorf("the registry holds %.120q (%d bytes)", v, len(v))
		}
	}

	relay.tamper = nil
	h.step()
	h.written()
	if v, _ := h.e.getKV(h.ctx, KVWebhooksCursor); v != `{"epoch":"ep1","cursor":"ep1:6"}` {
		t.Errorf("after the relay answered well: cursor %s, want ep1:6", v)
	}
	if ok, cause := state(); !ok {
		t.Errorf("after the relay answered well: unhealthy %q", cause)
	}
}

// An event that names 1,000 PRs hints at the first 100 of them (and its
// head), as many as the relay keeps of one event.
func TestAnEventNamingAThousandPRsHintsAtTheFirst100(t *testing.T) {
	relay := newFakeRelay("ep1")
	h := newHarness(t, shadowMod(relay))
	h.shadowOn()
	h.step()
	nums := make([]int, 1000)
	for i := range nums {
		nums[i] = i + 1
	}
	relay.deliver(webhooks.Event{DeliveryID: "d1", Event: "check_run", Action: "completed", Repository: "talkable/talkable",
		PRNumbers: nums, HeadSHA: strings.Repeat("ab", 20)})
	h.step()
	hk := &h.e.hooks
	if len(hk.hints) != 1 || len(hk.hints[0].keys) != 101 || len(hk.last) != 101 {
		t.Fatalf("%d hints, the first naming %d keys, %d last events: want one hint of 100 PRs and the head", len(hk.hints), len(hk.hints[0].keys), len(hk.last))
	}
	if !slices.Contains(hk.hints[0].keys, hookKey{repo: "talkable/talkable", pr: 100}) ||
		slices.Contains(hk.hints[0].keys, hookKey{repo: "talkable/talkable", pr: 101}) {
		t.Errorf("hint keys %v..., want PRs 1 to 100", hk.hints[0].keys[:3])
	}
}

// A run of 50,000 events, each naming a PR of its own, keeps at most 10,000
// hints and 10,000 last events: the oldest go first, and the hour's totals
// count what was dropped, so `magnum status --json` shows it.
func TestTheWebhooksConsumersMemoryIsBounded(t *testing.T) {
	relay := newFakeRelay("ep1")
	h := newHarness(t, shadowMod(relay))
	h.shadowOn()
	h.step()
	const n = 50_000
	evs := make([]webhooks.Event, n)
	for i := range evs {
		evs[i] = prEvent(fmt.Sprintf("d%d", i+1), i+1, "synchronize")
	}
	relay.deliver(evs...)
	for range n / webhooksPageLimit {
		h.step()
	}
	hk := &h.e.hooks
	if got := len(hk.hints); got != webhooksHintsMax || hk.hints[0].keys[0].pr != n-webhooksHintsMax+1 || hk.hints[got-1].keys[0].pr != n {
		t.Errorf("%d hints (#%d to #%d), want the newest %d", got, hk.hints[0].keys[0].pr, hk.hints[got-1].keys[0].pr, webhooksHintsMax)
	}
	if got := len(hk.last); got != webhooksLastMax {
		t.Errorf("%d last events, want %d", got, webhooksLastMax)
	}
	repo := "talkable/talkable"
	for pr, want := range map[int]bool{n: true, n - webhooksLastMax + 1: true, n - webhooksLastMax: false, 1: false} {
		if _, ok := hk.last[hookKey{repo: repo, pr: pr}]; ok != want {
			t.Errorf("the last event of #%d kept: %v, want %v", pr, ok, want)
		}
	}
	if len(hk.seen) > webhooksSeenMax {
		t.Errorf("%d delivery ids remembered", len(hk.seen))
	}
	tot := h.webhooksTotals()
	if tot.Events != n || tot.HintsDropped != n-webhooksHintsMax || tot.LastDropped != n-webhooksLastMax {
		t.Errorf("totals %+v, want %d events, %d hints and %d last events dropped", tot, n, n-webhooksHintsMax, n-webhooksLastMax)
	}
}

// A cursor kept in kv that the client would refuse (an earlier build wrote
// it from a page it took) is never read after nor written again: the
// consumer starts again at the relay's newest event.
func TestAKeptCursorTheClientWouldRefuseIsNeitherReadAfterNorWrittenAgain(t *testing.T) {
	for _, bad := range []string{
		`{"epoch":"ep1","cursor":"ep1:` + strings.Repeat("9", 10<<10) + `"}`,
		`{"epoch":"ep1\u001b[2J","cursor":"ep1:1"}`,
	} {
		relay := newFakeRelay("ep1")
		relay.deliver(prEvent("d1", 1, "opened"))
		h := newHarness(t, shadowMod(relay))
		if err := h.st.SetKV(h.ctx, KVWebhooksCursor, bad); err != nil {
			t.Fatal(err)
		}
		h.shadowOn()
		h.step()
		if got := relay.calls(); len(got) != 1 || got[0] != "" {
			t.Errorf("%.40s: pages asked after %.40q, want the relay's newest event", bad, got)
		}
		h.written()
		if v, _ := h.e.getKV(h.ctx, KVWebhooksCursor); v != `{"epoch":"ep1","cursor":"ep1:1"}` {
			t.Errorf("%.40s: cursor kept %.80s", bad, v)
		}
	}
}

// A push made while the Mac sleeps is no webhook miss: the relay stores its
// event during the sleep, and the page that lists it after the wake comes
// with it as a backlog that hints at nothing, so each repository's first
// poll after the sleep compares nothing (its hold, like a gap's), whether
// the page comes before the poll or after it; the backlog event is still
// the last one read for the PR, and the next push is announced again.
func TestAPushDuringASleepIsNoWebhookMiss(t *testing.T) {
	for _, pageFirst := range []bool{true, false} {
		name := map[bool]string{true: "the page before the poll", false: "the poll before the page"}[pageFirst]
		t.Run(name, func(t *testing.T) {
			relay := newFakeRelay("ep1")
			h := newHarness(t, shadowMod(relay), func(h *harness) {
				// A stream that still looks healthy after the sleep, so
				// staleness keeps no poll from counting.
				h.cfg.Webhooks.StaleAfter = config.Duration{Duration: time.Hour}
			})
			h.open(prSpec{n: 1, head: "a1"})
			h.startup()
			h.shadowOn()
			h.step()
			h.tick() // the first sync

			// The Mac sleeps 20 minutes; 2 minutes in, #1 is pushed and the
			// relay stores its event.
			h.advance(2 * time.Minute)
			h.gh.setHead("talkable/talkable", 1, "a2")
			relay.deliver(prEvent("d1", 1, "synchronize"))
			h.advance(18 * time.Minute)
			if pageFirst {
				h.step()
				h.tick()
			} else {
				h.tick()
				h.step()
			}
			h.advance(61 * time.Second)
			h.step()
			h.tick()
			if tot := h.webhooksTotals(); tot.Changes != 0 || tot.Missed != 0 || tot.Events != 1 {
				t.Errorf("after the sleep: %+v, want the push uncounted", tot)
			}
			if n := len(h.webhooksEvents("webhooks.missed")); n != 0 {
				t.Errorf("webhooks.missed events = %d, want none", n)
			}
			if s, ok := h.e.hooks.last[hookKey{repo: "talkable/talkable", pr: 1}]; !ok || s.name != "pull_request.synchronize" {
				t.Errorf("the last event read for #1 = %+v (%v), want the backlog's", s, ok)
			}

			h.gh.setHead("talkable/talkable", 1, "a3")
			relay.deliver(prEvent("d2", 1, "synchronize"))
			h.advance(30 * time.Second)
			h.step()
			h.tick()
			if tot := h.webhooksTotals(); tot.Changes != 1 || tot.Announced != 1 {
				t.Errorf("the next push: %+v, want it announced", tot)
			}
		})
	}
}

// A backlog without a sleep (the relay's listing failing for 8 minutes
// while the polls waited for GitHub's budget, the ticks going on) holds the
// repositories it names: each one's next poll compares nothing, so a push
// whose event came in the backlog is no miss, and the poll after it counts
// again.
func TestABacklogHoldsItsRepositoryUntilItsNextPoll(t *testing.T) {
	relay := newFakeRelay("ep1")
	h := newHarness(t, shadowMod(relay))
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.shadowOn()
	h.step()
	h.tick() // the first sync

	relay.status = 503
	if err := h.st.SetKV(h.ctx, store.KVGHPollPausedUntil, store.FormatTime(h.clock.Now().Add(8*time.Minute))); err != nil {
		t.Fatal(err)
	}
	h.advance(30 * time.Second)
	h.gh.setHead("talkable/talkable", 1, "a2")
	relay.deliver(prEvent("d1", 1, "synchronize"))
	for range 14 {
		h.advance(30 * time.Second)
		_, _ = h.e.webhooksStep(h.ctx)
		h.tick()
	}
	h.advance(30 * time.Second) // the polls' pause is over
	relay.status = 0
	h.step() // d1, received 7.5 minutes before the page
	h.tick()
	h.advance(61 * time.Second)
	h.step()
	h.tick()
	if tot := h.webhooksTotals(); tot.Changes != 0 || tot.Missed != 0 {
		t.Errorf("after the backlog: %+v, want the push uncounted", tot)
	}

	h.gh.setHead("talkable/talkable", 1, "a3")
	relay.deliver(prEvent("d2", 1, "synchronize"))
	h.advance(30 * time.Second)
	h.step()
	h.tick()
	if tot := h.webhooksTotals(); tot.Changes != 1 || tot.Announced != 1 {
		t.Errorf("the next push: %+v, want it announced", tot)
	}
}
