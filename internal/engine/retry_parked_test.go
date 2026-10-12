package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	revA = "aaaaaaa1111111111111111111111111111111aa"
	revB = "bbbbbbb2222222222222222222222222222222bb"
)

// parkFailing ticks every step until every PR of ns is in needs_attention
// after its failed rounds (the round script fails them all).
func (h *harness) parkFailing(step time.Duration, ns ...int) {
	h.t.Helper()
	for range 80 {
		parked := 0
		for _, n := range ns {
			if h.pr(n).State == store.PRNeedsAttention {
				parked++
			}
		}
		if parked == len(ns) {
			return
		}
		h.advance(step)
		h.tick()
	}
	for _, n := range ns {
		pr := h.pr(n)
		h.t.Logf("#%d: %s, %d attempts", n, pr.State, pr.Attempts)
	}
	h.t.Fatalf("PRs %v never all needed attention", ns)
}

// restartOn starts a new daemon on the same registry running build rev
// (the same config and prompts), through its start-up.
func (h *harness) restartOn(rev string) {
	h.t.Helper()
	h.e = New(h.d)
	h.e.build = Build{Version: "dev", Revision: rev}
	h.startup()
}

// A restart on a new build retries the PRs failed rounds parked under the
// old one: they go back in line (queued, or rereview_pending for a reviewed
// PR) with a fresh retry budget and are not forced, with one event each and
// one toast for them all. A PR a fix loop parked stays, and a restart on the
// build the PRs failed on again retries nothing.
func TestARestartOnANewBuildRetriesPRsAFailedRoundParked(t *testing.T) {
	h := newHarness(t)
	h.e.build = Build{Version: "dev", Revision: revA}
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeError, Round: 1, Error: `pipeline: new claude-judge run: agents: new run: unknown role "claude-judge"`}, nil
	}
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"}, prSpec{n: 3, head: "b3"})
	h.tick()
	reviewed := h.pr(3) // #3 was reviewed at an older head: its failed rounds were re-reviews
	if err := h.st.UpdatePR(h.ctx, reviewed.ID, func(u *store.PRUpdate) {
		u.Set("reviewed_sha", "a3")
		u.Set("reviewed_at", h.clock.Now().Add(-2*time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	h.parkFailing(10*time.Minute, 2, 3)
	for _, n := range []int{2, 3} {
		pr := h.pr(n)
		if why, _ := h.e.getKV(h.ctx, KVPRAttention(pr.ID)); why != attention.KindFailed {
			t.Fatalf("#%d parked as %q, want %q", n, why, attention.KindFailed)
		}
		raw, _ := h.e.getKV(h.ctx, kvPRAttentionRun(pr.ID))
		var run runFingerprint
		if err := json.Unmarshal([]byte(raw), &run); err != nil || run.Build != revA || run.Config == "" {
			t.Fatalf("#%d: the attention's run = %q (%v), want build %s and a config sum", n, raw, err, revA)
		}
	}

	// #4 needs the author's call after its fix loop: no restart retries that.
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"}, prSpec{n: 3, head: "b3"}, prSpec{n: 4, head: "b4"})
	h.tick()
	fixed := h.pr(4)
	repo, err := h.st.RepoByID(h.ctx, fixed.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	h.e.needsAttention(h.ctx, &roundJob{repo: repo, pr: fixed}, fixed, []string{fixed.State}, attention.KindFix,
		"fix: 5 fix cycles, 1 finding left", nil)
	h.wantState(4, store.PRNeedsAttention)
	h.settle()
	before := len(h.nh.shown())

	h.restartOn(revB)
	for n, want := range map[int]string{2: store.PRQueued, 3: store.PRRereviewPending} {
		pr := h.wantState(n, want)
		if pr.Attempts != 0 || pr.Forced || pr.NextAttemptAt != nil {
			t.Errorf("#%d after the restart: attempts %d, forced %v, next attempt %v; want 0, not forced, none",
				n, pr.Attempts, pr.Forced, pr.NextAttemptAt)
		}
	}
	h.wantState(4, store.PRNeedsAttention)
	evs := h.events("pr.retried_after_restart")
	if len(evs) != 2 {
		t.Fatalf("pr.retried_after_restart events = %+v, want one per retried PR", evs)
	}
	for _, ev := range evs {
		if want := "retried after the restart on " + textx.ShortSHA(revB) + ": the failure's build differs"; !strings.HasPrefix(ev.Message, want) {
			t.Errorf("event %q, want it to start %q", ev.Message, want)
		}
	}
	h.flush()
	toasts := toastsWith(h, "retrying 2 PRs parked by failed rounds before this build")
	if len(toasts) != 1 || len(h.nh.shown()) < before+1 {
		t.Fatalf("toasts = %v, want one for both PRs", h.nh.shown()[before:])
	}
	if !strings.Contains(toasts[0], "talkable/talkable#2") || !strings.Contains(toasts[0], "talkable/talkable#3") {
		t.Errorf("toast %q does not list both PRs", toasts[0])
	}

	// They fail again on this build: a restart on it retries nothing.
	h.parkFailing(10*time.Minute, 2, 3)
	h.restartOn(revB)
	h.wantState(2, store.PRNeedsAttention)
	h.wantState(3, store.PRNeedsAttention)
	h.wantState(4, store.PRNeedsAttention)
	if evs := h.events("pr.retried_after_restart"); len(evs) != 2 {
		t.Fatalf("pr.retried_after_restart events = %+v, want none more on the same build", evs)
	}
}

// A PR parked again on the build a restart retried it on is toasted again:
// the attention toast's dedupe key names the run, so the second failure is
// news while the first one's toast is still in its window.
func TestAPRTheRestartRetriedThatFailsAgainIsToasted(t *testing.T) {
	h := newHarness(t)
	h.e.build = Build{Version: "dev", Revision: revA}
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeError, Round: 1, Error: "boom"}, nil
	}
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2"})
	h.tick()
	h.parkFailing(2*time.Minute, 2)
	if got := toastsWith(h, "talkable#2 needs attention"); len(got) != 1 {
		t.Fatalf("attention toasts = %v, want one", h.nh.shown())
	}
	h.restartOn(revB)
	h.wantState(2, store.PRQueued)
	first := h.clock.Now()
	h.parkFailing(2*time.Minute, 2)
	if h.clock.Now().Sub(first) >= attentionWindow {
		t.Fatalf("the second park came %s after the restart, past the toast's window", h.clock.Now().Sub(first))
	}
	if got := toastsWith(h, "talkable#2 needs attention"); len(got) != 2 {
		t.Fatalf("attention toasts = %v, want a second one for the failure on the new build", h.nh.shown())
	}
}

// What a restart compares: the build (its revision, and the binary's mtime,
// so a rebuild of one commit is another build), the config and the
// prompts; the event names each part that differs.
func TestARunFingerprintNamesWhatDiffers(t *testing.T) {
	mt := time.Date(2026, 10, 12, 13, 39, 0, 0, time.UTC)
	a := runFingerprint{Build: buildID(Build{Revision: revA, ModTime: mt}), Config: "c1", Prompts: "p1"}
	cases := []struct {
		name string
		b    runFingerprint
		want string
	}{
		{"the same run", a, ""},
		{"another revision", runFingerprint{Build: buildID(Build{Revision: revB, ModTime: mt}), Config: "c1", Prompts: "p1"}, "build differs"},
		{"a rebuild of the commit", runFingerprint{Build: buildID(Build{Revision: revA, ModTime: mt.Add(time.Minute)}), Config: "c1", Prompts: "p1"}, "build differs"},
		{"another config", runFingerprint{Build: a.Build, Config: "c2", Prompts: "p1"}, "config differs"},
		{"other prompts", runFingerprint{Build: a.Build, Config: "c1", Prompts: "p2"}, "prompts differ"},
		{"config and prompts", runFingerprint{Build: a.Build, Config: "c2", Prompts: "p2"}, "config and prompts differ"},
		{"everything", runFingerprint{Build: "x", Config: "c2", Prompts: "p2"}, "build, config and prompts differ"},
	}
	for _, c := range cases {
		if got := differs(a.changes(c.b)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	cfg := config.Defaults()
	sum := configSum(cfg)
	if len(sum) != 12 || configSum(cfg) != sum {
		t.Errorf("configSum = %q then %q, want the same 12 hex digits", sum, configSum(cfg))
	}
	cfg.Daemon.MaxConcurrentReviews++
	if configSum(cfg) == sum {
		t.Errorf("a changed key kept the config sum %q", sum)
	}
	if buildID(Build{Version: "v1.2.0"}) != "v1.2.0" {
		t.Errorf("a build without a revision or a binary is its version: %q", buildID(Build{Version: "v1.2.0"}))
	}
}
