package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

var timingT0 = time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)

func timingAt(d time.Duration) *time.Time { t := timingT0.Add(d); return &t }

// timingStep is one checkout step event at t0+d.
func timingStep(subject, kind, step, phase string, d time.Duration) store.Event {
	e := store.Event{At: timingT0.Add(d), Subject: &subject, Kind: kind, Message: "m"}
	if step != "" {
		e.Step, e.Phase = &step, &phase
	}
	return e
}

// timingRound is a finished first round: checkout 0:00–0:12, the reviewers
// from 1:00, the judge from 20:00, its review verified 3s after it ended.
func timingRound() ([]store.Run, []store.Event) {
	const sub = "slot:review1:pr:7:abcdef1"
	steps := []store.Event{
		timingStep(sub, store.KindStepReset, "", "", -time.Hour),
		timingStep(sub, store.KindStep, "fetch", store.PhaseFail, -time.Hour), // an earlier attempt
		timingStep(sub, store.KindStepReset, "", "", 0),
		timingStep(sub, store.KindStep, "fetch", store.PhaseBegin, 0),
		timingStep(sub, store.KindStep, "fetch", store.PhaseOK, 8*time.Second),
		timingStep(sub, store.KindStep, "verify", store.PhaseBegin, 10*time.Second),
		timingStep(sub, store.KindStep, "verify", store.PhaseOK, 12*time.Second),
		timingStep("slot:review1:pr:7:0000000", store.KindStep, "fetch", store.PhaseBegin, 50*time.Minute), // a restart, after the round began
	}
	runs := []store.Run{
		{ID: "j", Round: 1, Role: store.RoleJudge, Kind: store.RunInitial, State: store.RunVerified, CreatedAt: timingT0.Add(20 * time.Minute),
			SubmittedAt: timingAt(20 * time.Minute), EndedAt: timingAt(34 * time.Minute), VerifiedAt: timingAt(34*time.Minute + 3*time.Second)},
		{ID: "c", Round: 1, Role: store.RoleClaude, Kind: store.RunInitial, State: store.RunVerified, CreatedAt: timingT0.Add(time.Minute),
			SubmittedAt: timingAt(time.Minute + 2*time.Second), EndedAt: timingAt(19*time.Minute + 6*time.Second), VerifiedAt: timingAt(19*time.Minute + 7*time.Second)},
		{ID: "x", Round: 1, Role: store.RoleCodexReview, Kind: store.RunInitial, State: store.RunFailed, CreatedAt: timingT0.Add(time.Minute),
			SubmittedAt: timingAt(time.Minute), EndedAt: timingAt(10 * time.Minute)},
	}
	return runs, steps
}

func timingJudge(role string) bool { return role == store.RoleJudge }

func TestRoundTimingsStages(t *testing.T) {
	runs, steps := timingRound()
	co := lastCheckout(steps, timingT0.Add(time.Minute))
	if !co.found || co.failed || co.end.Sub(co.start) != 12*time.Second {
		t.Fatalf("checkout = %+v", co)
	}
	got := roundTimings(runs, co, timingJudge, timingT0.Add(5*time.Hour))
	want := "fetch/checkout 12s · claude-review 18m04s · codex-review 9m00s (failed) · codex-judge 14m00s · verify 3s · total 34m03s"
	if got == nil || tui.TimingsText(*got) != want || got.Round != 1 || got.Kind != store.RunInitial || got.Running {
		t.Fatalf("timings = %+v\n%s\nwant %s", got, tui.TimingsText(*got), want)
	}

	// The judge still verifying: the round runs, verify and total count to now.
	runs[0].State, runs[0].VerifiedAt = store.RunEnded, nil
	got = roundTimings(runs, co, timingJudge, timingT0.Add(35*time.Minute))
	want = "fetch/checkout 12s · claude-review 18m04s · codex-review 9m00s (failed) · codex-judge 14m00s · verify 1m00s (running) · total 35m00s (running)"
	if tui.TimingsText(*got) != want {
		t.Fatalf("verifying = %s\nwant %s", tui.TimingsText(*got), want)
	}
	// The judge still working.
	runs[0].State, runs[0].EndedAt = store.RunWorking, nil
	got = roundTimings(runs, co, timingJudge, timingT0.Add(30*time.Minute))
	if s := tui.TimingsText(*got); !strings.Contains(s, "codex-judge 10m00s (running)") || strings.Contains(s, "verify") || !got.Running {
		t.Fatalf("judging = %s", s)
	}
	if roundTimings(nil, co, timingJudge, timingT0) != nil {
		t.Fatal("no runs must mean no timings")
	}
}

func TestLastCheckoutPicksTheRoundsCheckout(t *testing.T) {
	_, steps := timingRound()
	// Before the round's checkout ended there is only the failed attempt an
	// hour earlier: no begin in its generation, so nothing.
	if co := lastCheckout(steps, timingT0.Add(-time.Minute)); co.found {
		t.Fatalf("before the checkout: %+v", co)
	}
	// A checkout that ended long before the round is an earlier round's.
	if co := lastCheckout(steps, timingT0.Add(2*time.Hour)); co.found {
		t.Fatalf("stale checkout used: %+v", co)
	}
	// A failed step ends the span as failed.
	sub := "slot:x#1"
	failed := []store.Event{
		timingStep(sub, store.KindStep, "clone", store.PhaseBegin, 0),
		timingStep(sub, store.KindStep, "clone", store.PhaseOK, 30*time.Second),
		timingStep(sub, store.KindStep, "setup", store.PhaseBegin, 31*time.Second),
		timingStep(sub, store.KindStep, "setup", store.PhaseFail, 2*time.Minute),
	}
	if co := lastCheckout(failed, timingT0.Add(3*time.Minute)); !co.found || !co.failed || co.end.Sub(co.start) != 2*time.Minute {
		t.Fatalf("failed checkout = %+v", co)
	}
}

// The board's rows and `magnum status <ref>` read the timings from the
// registry: the latest round's runs and the PR's checkout steps.
func TestTimingsFromTheRegistry(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 7, store.PRReviewed)
	ctx := h.ctx
	runs, steps := timingRound()
	for _, e := range steps {
		if _, err := h.st.AppendEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// An older round that must not count.
	if _, err := h.st.CreateRun(ctx, store.Run{PRID: pr.ID, Round: 0, Role: store.RoleJudge, Kind: store.RunInitial, TargetSHA: "a",
		Identity: "i", ReviewerLogin: "l", State: store.RunVerified, PromptText: "p"}); err != nil {
		t.Fatal(err)
	}
	slot, err := h.st.CreateSlot(ctx, store.Slot{Name: "review1", RepoFullName: "talkable/talkable", Kind: store.SlotKindPool,
		Path: t.TempDir(), MainClone: t.TempDir(), State: store.SlotFree})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.OpenAssignment(ctx, store.Assignment{PRID: pr.ID, SlotID: slot.ID, Path: slot.Path}); err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		created := r.CreatedAt
		h.st.Clock = func() time.Time { return created }
		r.PRID, r.TargetSHA, r.Identity, r.ReviewerLogin, r.PromptText, r.ID = pr.ID, "abcdef1", "i", "l", "p", ""
		got, err := h.st.CreateRun(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.st.UpdateRun(ctx, got.ID, func(u *store.RunUpdate) {
			u.Set("submitted_at", r.SubmittedAt)
			u.Set("ended_at", r.EndedAt)
			u.Set("verified_at", r.VerifiedAt)
		}); err != nil {
			t.Fatal(err)
		}
	}
	want := "fetch/checkout 12s · claude-review 18m04s · codex-review 9m00s (failed) · codex-judge 14m00s · verify 3s · total 34m03s"

	got, err := roundTimingsFor(ctx, h.st, nil, pr.ID, nil, timingT0.Add(5*time.Hour))
	if err != nil || got == nil || tui.TimingsText(*got) != want {
		t.Fatalf("roundTimingsFor = %+v, %v", got, err)
	}
	src := prsSource(h.st, nil, store.BoardFilter{}, nil, h.c.Layout)
	for range 2 { // the second load takes the checkout from the cache
		rows, err := src(ctx)
		if err != nil || len(rows) != 1 || rows[0].LastRound == nil || tui.TimingsText(*rows[0].LastRound) != want {
			t.Fatalf("board rows = %+v, %v", rows, err)
		}
	}

	var b bytes.Buffer
	statusRenderTimings(&b, got)
	if s := b.String(); s != "  timings:   round 1 initial: "+want+"\n" {
		t.Fatalf("status line = %q", s)
	}
	j, _ := json.Marshal(statusDetail{LastRound: got})
	if !strings.Contains(string(j), `"last_round":{"Round":1`) {
		t.Fatalf("json = %s", j)
	}
}

// The judge's own pass, prompted with the reviewers, is a stage of its own
// ("codex-judge own pass", its model fallback's continuation included) after
// the reviewers and before the judge, whose stage and verification read only
// its main run: the judge's span does not start with the reviewers. The
// round's kind is its first run's that is not the own pass.
func TestRoundTimingsListOwnPassAsItsOwnStage(t *testing.T) {
	runs, steps := timingRound()
	co := lastCheckout(steps, timingT0.Add(time.Minute))
	runs[0].CreatedAt = timingT0.Add(time.Minute) // the judge's main run is created up front
	own := []store.Run{
		{ID: "o1", Round: 1, Role: store.RoleJudge, Kind: store.RunOwnPass, State: store.RunAbandoned, CreatedAt: timingT0.Add(59 * time.Second),
			SubmittedAt: timingAt(time.Minute), EndedAt: timingAt(5 * time.Minute)},
		{ID: "o2", Round: 1, Role: store.RoleJudge, Kind: store.RunOwnPass, State: store.RunVerified, CreatedAt: timingT0.Add(5 * time.Minute),
			SubmittedAt: timingAt(5*time.Minute + 30*time.Second), EndedAt: timingAt(16 * time.Minute), VerifiedAt: timingAt(16*time.Minute + time.Second)},
	}
	all := append(own, runs...)
	got := roundTimings(all, co, timingJudge, timingT0.Add(5*time.Hour))
	want := "fetch/checkout 12s · claude-review 18m04s · codex-review 9m00s (failed) · codex-judge own pass 15m00s · " +
		"codex-judge 14m00s · verify 3s · total 34m03s"
	if got == nil || tui.TimingsText(*got) != want || got.Kind != store.RunInitial || got.Running {
		t.Fatalf("timings = %+v\n%s\nwant %s", got, tui.TimingsText(*got), want)
	}

	// The own pass failed; the judge still works.
	all[1].State = store.RunFailed
	all[2].State, all[2].EndedAt, all[2].VerifiedAt = store.RunWorking, nil, nil
	got = roundTimings(all, co, timingJudge, timingT0.Add(30*time.Minute))
	want = "fetch/checkout 12s · claude-review 18m04s · codex-review 9m00s (failed) · codex-judge own pass 15m00s (failed) · " +
		"codex-judge 10m00s (running) · total 30m00s (running)"
	if tui.TimingsText(*got) != want {
		t.Fatalf("judging after a failed own pass = %s\nwant %s", tui.TimingsText(*got), want)
	}
}
