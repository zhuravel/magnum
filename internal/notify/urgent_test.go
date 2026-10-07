package notify

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
)

var (
	rateLimited = herdr.NotificationResult{Shown: false, Reason: "rate_limited"}
	shownOK     = herdr.NotificationResult{Shown: true, Reason: "shown"}
)

// sleeper records the waits ToastUrgent asks for without sleeping.
type sleeper struct {
	waits []time.Duration
	err   error // returned by every wait when set
}

func (s *sleeper) sleep(_ context.Context, d time.Duration) error {
	s.waits = append(s.waits, d)
	return s.err
}

func sum(ds []time.Duration) (t time.Duration) {
	for _, d := range ds {
		t += d
	}
	return t
}

func TestToastUrgentRetriesThroughRateLimit(t *testing.T) {
	h := newFakeHerdr()
	h.queue = []herdr.NotificationResult{rateLimited, {Shown: false, Reason: "busy"}, rateLimited}
	r := osascriptFake()
	st := &fakeStore{send: true}
	sl := &sleeper{}
	n := &Notifier{Herdr: h, Store: st, Runner: r, Enabled: true, Sleep: sl.sleep}

	sent, err := n.ToastUrgent(context.Background(), "leak:7", "magnum: IDENTITY LEAK", "talkable#7", time.Hour)
	if err != nil || !sent {
		t.Fatalf("ToastUrgent = %v, %v; want delivered by herdr on the fourth try", sent, err)
	}
	if len(h.shows) != 4 {
		t.Errorf("herdr attempts = %d, want 4", len(h.shows))
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; !reflect.DeepEqual(sl.waits, want) {
		t.Errorf("waits = %v, want %v", sl.waits, want)
	}
	if len(r.Calls) != 0 {
		t.Errorf("osascript ran although herdr showed the toast: %v", r.Calls)
	}
	if !reflect.DeepEqual(st.calls, []string{"leak:7"}) || len(st.forgot) != 0 {
		t.Errorf("store calls = %v, forgot = %v; want one reservation, kept", st.calls, st.forgot)
	}
}

func TestToastUrgentFallsBackToOsascriptAfterTheBudget(t *testing.T) {
	h := newFakeHerdr()
	h.showRes = rateLimited
	r := osascriptFake()
	st := &fakeStore{send: true}
	sl := &sleeper{}
	n := &Notifier{Herdr: h, Store: st, Runner: r, Enabled: true, Sleep: sl.sleep}

	sent, err := n.ToastUrgent(context.Background(), "pause:codex", "magnum: codex paused", "usage 96%", time.Hour)
	if err != nil || !sent {
		t.Fatalf("ToastUrgent = %v, %v; want delivered through osascript", sent, err)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 29}
	for i := range want {
		want[i] *= time.Second
	}
	if !reflect.DeepEqual(sl.waits, want) {
		t.Errorf("waits = %v, want %v", sl.waits, want)
	}
	if got := sum(sl.waits); got != UrgentRetryBudget {
		t.Errorf("total wait = %s, want %s", got, UrgentRetryBudget)
	}
	if len(h.shows) != len(want)+1 {
		t.Errorf("herdr attempts = %d, want %d", len(h.shows), len(want)+1)
	}
	assertOsascript(t, r, `display notification "usage 96%" with title "magnum: codex paused"`)
	if len(st.forgot) != 0 {
		t.Errorf("delivered toast released its key: %v", st.forgot)
	}
}

// The identity-leak alert was lost exactly like this: herdr rate limited and
// no surface left. The reservation must be released so the next report of
// the same leak toasts again.
func TestToastUrgentReleasesKeyWhenNothingDelivers(t *testing.T) {
	c := newClock()
	h := newFakeHerdr()
	h.showRes = rateLimited
	sl := &sleeper{}
	n := &Notifier{Herdr: h, Store: realStore(t, c), Enabled: true, Sleep: sl.sleep} // no Runner
	ctx := context.Background()

	sent, err := n.ToastUrgent(ctx, "leak:7", "T", "B", time.Hour)
	if sent || err == nil {
		t.Fatalf("ToastUrgent = %v, %v; want an error", sent, err)
	}
	h.showRes = shownOK
	c.Add(time.Minute)
	if sent, err := n.ToastUrgent(ctx, "leak:7", "T", "B", time.Hour); !sent || err != nil {
		t.Fatalf("retry = %v, %v; want delivered (the failed attempt must not consume the window)", sent, err)
	}
	if sent, _ := n.ToastUrgent(ctx, "leak:7", "T", "B", time.Hour); sent {
		t.Errorf("a delivered urgent toast must arm the dedupe window")
	}
}

func TestToastUrgentStopsWaitingWhenContextEnds(t *testing.T) {
	h := newFakeHerdr()
	h.showRes = rateLimited
	r := osascriptFake()
	st := &fakeStore{send: true}
	sl := &sleeper{err: context.Canceled}
	n := &Notifier{Herdr: h, Store: st, Runner: r, Enabled: true, Sleep: sl.sleep}

	sent, err := n.ToastUrgent(context.Background(), "k", "T", "B", time.Hour)
	if sent || !errors.Is(err, context.Canceled) {
		t.Fatalf("ToastUrgent = %v, %v; want context.Canceled", sent, err)
	}
	if len(r.Calls) != 0 {
		t.Errorf("osascript ran after cancellation: %v", r.Calls)
	}
	if !reflect.DeepEqual(st.forgot, []string{"k"}) {
		t.Errorf("forgot = %v; want the key released", st.forgot)
	}
}

func TestToastUrgentFallsBackAtOnceWhenHerdrCannotShow(t *testing.T) {
	for name, set := range map[string]func(*fakeHerdr){
		"unavailable": func(h *fakeHerdr) { h.showErr = unavailable() },
		"request error": func(h *fakeHerdr) {
			h.showErr = &herdr.Error{Method: "notification.show", Code: herdr.CodeInvalidRequest}
		},
		"no client": func(h *fakeHerdr) { h.showRes = herdr.NotificationResult{Reason: "no_foreground_client"} },
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHerdr()
			set(h)
			r := osascriptFake()
			sl := &sleeper{}
			n := &Notifier{Herdr: h, Runner: r, Enabled: true, Sleep: sl.sleep}

			sent, err := n.ToastUrgent(context.Background(), "", "T", "B", 0)
			if err != nil || !sent {
				t.Fatalf("ToastUrgent = %v, %v", sent, err)
			}
			if len(sl.waits) != 0 {
				t.Errorf("waited %v; only a rate limit is worth waiting for", sl.waits)
			}
			assertOsascript(t, r, `display notification "B" with title "T"`)
		})
	}
}

func TestToastUrgentRespectsHerdrDisabled(t *testing.T) {
	h := newFakeHerdr()
	h.showRes = herdr.NotificationResult{Reason: "disabled"}
	r := osascriptFake()
	st := &fakeStore{send: true}
	n := &Notifier{Herdr: h, Store: st, Runner: r, Enabled: true, Sleep: (&sleeper{}).sleep}

	if sent, err := n.ToastUrgent(context.Background(), "k", "T", "B", time.Hour); sent || err != nil {
		t.Fatalf("ToastUrgent = %v, %v; want false, nil", sent, err)
	}
	if len(r.Calls) != 0 || len(st.forgot) != 0 {
		t.Errorf("the user turned herdr toasts off: osascript %v, forgot %v", r.Calls, st.forgot)
	}
}

func TestToastUrgentHonoursEnabledAndDedupe(t *testing.T) {
	h := newFakeHerdr()
	n := &Notifier{Herdr: h, Store: &fakeStore{send: false}, Enabled: true}
	if sent, err := n.ToastUrgent(context.Background(), "k", "T", "B", time.Hour); sent || err != nil {
		t.Fatalf("suppressed ToastUrgent = %v, %v", sent, err)
	}
	n.Enabled = false
	n.Store = nil
	if sent, err := n.ToastUrgent(context.Background(), "k", "T", "B", time.Hour); sent || err != nil {
		t.Fatalf("disabled ToastUrgent = %v, %v", sent, err)
	}
	if len(h.shows) != 0 {
		t.Errorf("shows = %v", h.shows)
	}
}

func TestToastUrgentWithoutAnySurface(t *testing.T) {
	n := &Notifier{Enabled: true}
	if _, err := n.ToastUrgent(context.Background(), "", "T", "B", 0); !errors.Is(err, errNoSurface) {
		t.Fatalf("err = %v, want errNoSurface", err)
	}
}

func TestDefaultSleepStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n := &Notifier{}
	start := time.Now()
	if err := n.sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleep = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("sleep ignored the cancelled context")
	}
	if err := n.sleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("short sleep = %v", err)
	}
}

// Ordinary toasts keep their one-shot semantics: herdr's rate limit is final.
func TestToastStillTreatsRateLimitAsFinal(t *testing.T) {
	h := newFakeHerdr()
	h.showRes = rateLimited
	sl := &sleeper{}
	n := &Notifier{Herdr: h, Runner: osascriptFake(), Enabled: true, Sleep: sl.sleep}
	if sent, err := n.Toast(context.Background(), "", "T", "B", 0); sent || err != nil {
		t.Fatalf("Toast = %v, %v", sent, err)
	}
	if len(h.shows) != 1 || len(sl.waits) != 0 {
		t.Errorf("shows %d, waits %v; want one attempt and no wait", len(h.shows), sl.waits)
	}
}
