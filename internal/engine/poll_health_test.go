package engine

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// TestAWatchWhosePollsFailIsRecordedAndToastedOncePerStreak: a watch whose
// radar keeps failing keeps its last good poll and the start of the failure
// with a short cause (the daemon's last poll moves on regardless); at 12
// minutes nothing is toasted, at 15 one toast says so, and a further tick
// sends none. A good poll ends the streak, and a failure after it is a new
// streak with a toast of its own. Another watch's polls are unaffected.
func TestAWatchWhosePollsFailIsRecordedAndToastedOncePerStreak(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick()
	lastOK := h.clock.Now()
	poll := func(err error) {
		t.Helper()
		h.gh.mu.Lock()
		h.gh.radarErrs = map[string]error{"talkable": err} // nil: the radar answers
		h.gh.mu.Unlock()
		h.advance(time.Minute)
		_ = h.e.Tick(h.ctx) // a failed radar fails the tick
		h.settle()
	}
	watch := func(owner string) WatchPoll {
		t.Helper()
		v, ok, err := h.st.GetKV(h.ctx, store.KVWatchPoll(owner))
		if err != nil || !ok {
			t.Fatalf("watch %s poll kv: %q %v %v", owner, v, ok, err)
		}
		p, ok := ParseWatchPoll(v)
		if !ok {
			t.Fatalf("watch %s poll kv unreadable: %q", owner, v)
		}
		return p
	}
	failing := func() []string { return h.nh.titles("polls failing") }

	if p := watch("talkable"); !p.LastOK.Equal(lastOK) || !p.FailingSince.IsZero() || p.Error != "" {
		t.Fatalf("after a good poll: %+v", p)
	}
	e502 := &github.APIError{Op: "radar talkable page 1", Status: 502}
	poll(e502)
	since := h.clock.Now()
	for range 12 {
		poll(e502)
	}
	now := h.clock.Now()
	p := watch("talkable")
	if !p.FailingSince.Equal(since) || p.Error != "HTTP 502" || !p.LastOK.Equal(lastOK) || p.Failing(now) != 12*time.Minute {
		t.Fatalf("12 minutes failing: %+v (failing %s)", p, p.Failing(now))
	}
	if last, _ := h.e.kvTime(h.ctx, kvLastPoll); !last.Equal(now) {
		t.Errorf("the daemon's last poll = %s, want %s", last, now)
	}
	if z := watch("zhuravel"); !z.LastOK.Equal(now) || z.Failing(now) != 0 || z.Error != "" {
		t.Errorf("the other watch: %+v", z)
	}
	if got := failing(); len(got) != 0 {
		t.Fatalf("toasted at 12 minutes: %q", got)
	}

	for range 3 {
		poll(e502) // 15 minutes: the toast is queued
	}
	poll(e502) // the batch comes due
	got := failing()
	if len(got) != 1 || got[0] != "magnum: talkable polls failing 15m (HTTP 502)" {
		t.Fatalf("toasts at 15 minutes = %q", got)
	}
	body := strings.Join(toastsWith(h, "polls failing"), "\n")
	clock := func(t time.Time) string { return t.Local().Format("15:04") }
	for _, want := range []string{"since " + clock(since), "last good poll " + clock(lastOK), "watch talkable"} {
		if !strings.Contains(body, want) {
			t.Errorf("toast lacks %q: %q", want, body)
		}
	}
	for range 5 {
		poll(e502)
	}
	if got := failing(); len(got) != 1 {
		t.Fatalf("toasted again in the same streak: %q", got)
	}

	poll(nil)
	ok := h.clock.Now()
	if p := watch("talkable"); !p.LastOK.Equal(ok) || !p.FailingSince.IsZero() || p.Error != "" {
		t.Fatalf("after the radar answered: %+v", p)
	}

	e504 := &github.APIError{Op: "radar talkable page 1", Status: 504, Message: "We couldn't respond to your request in time."}
	for range 17 {
		poll(e504)
	}
	if p := watch("talkable"); !p.FailingSince.Equal(ok.Add(time.Minute)) || p.Error != "HTTP 504" || !p.LastOK.Equal(ok) {
		t.Fatalf("the new streak: %+v", p)
	}
	if got := failing(); len(got) != 2 || got[1] != "magnum: talkable polls failing 15m (HTTP 504)" {
		t.Fatalf("toasts after the second streak = %q", got)
	}
}

// TestPollFailureCauseIsShortAndRedacted: the cause a watch's failure keeps
// is "HTTP <status>" when GitHub answered one, else the error's first 60
// runes on one line, tokens masked.
func TestPollFailureCauseIsShortAndRedacted(t *testing.T) {
	if got := pollCause(&github.APIError{Op: "radar talkable page 1", Status: 502}); got != "HTTP 502" {
		t.Errorf("HTTP error: %q", got)
	}
	token := "ghp_" + strings.Repeat("a", 36)
	err := errors.New("radar talkable:\n" + token + " " + strings.Repeat("connection reset by peer ", 5))
	got := pollCause(err)
	if strings.Contains(got, "ghp_") || strings.Contains(got, "\n") || utf8.RuneCountInString(got) > 60 {
		t.Errorf("cause %q", got)
	}
	if !strings.HasPrefix(got, "radar talkable: <redacted> connection reset") {
		t.Errorf("cause %q", got)
	}
	if got := pollCause(&github.APIError{Op: "radar talkable page 1", Errors: []github.GraphQLError{{Message: "Something went wrong"}}}); !strings.Contains(got, "Something went wrong") {
		t.Errorf("GraphQL error: %q", got)
	}
}

// TestWatchPollRoundTrips: the kv value reads back as written, and a watch
// that does not fail has no failing time.
func TestWatchPollRoundTrips(t *testing.T) {
	now := time.Date(2026, 10, 5, 10, 47, 0, 0, time.UTC)
	p := WatchPoll{LastOK: now.Add(-50 * time.Minute), FailingSince: now.Add(-47 * time.Minute), Error: "HTTP 502"}
	back, ok := ParseWatchPoll(p.Value())
	if !ok || back != p {
		t.Fatalf("round trip: %+v %v from %q", back, ok, p.Value())
	}
	if d := back.Failing(now); d != 47*time.Minute {
		t.Errorf("failing %s", d)
	}
	if d := (WatchPoll{LastOK: now}).Failing(now); d != 0 {
		t.Errorf("a good watch fails %s", d)
	}
	for _, bad := range []string{"", "{", "x"} {
		if _, ok := ParseWatchPoll(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}
