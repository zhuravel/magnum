package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLatestRoundRunsKeepsEachPRsHighestRound(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	a := mustPR(t, st, repo.ID, 1, PRReviewed)
	b := mustPR(t, st, repo.ID, 2, PRReviewed)
	mustPR(t, st, repo.ID, 3, PRBaseline) // no runs: no entry
	run := func(pr PR, round int, role string) {
		t.Helper()
		clk.Add(time.Minute)
		if _, err := st.CreateRun(ctx, Run{PRID: pr.ID, Round: round, Role: role, Kind: RunInitial, TargetSHA: pr.HeadSHA,
			Identity: "i", ReviewerLogin: "l", State: RunEnded, PromptText: "p"}); err != nil {
			t.Fatal(err)
		}
	}
	run(a, 1, RoleJudge)
	run(a, 2, RoleClaude)
	run(a, 2, RoleJudge)
	run(b, 1, RoleClaude)

	all, err := st.LatestRoundRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || len(all[a.ID]) != 2 || len(all[b.ID]) != 1 {
		t.Fatalf("LatestRoundRuns = %+v", all)
	}
	if all[a.ID][0].Round != 2 || all[a.ID][0].Role != RoleClaude || all[a.ID][1].Role != RoleJudge {
		t.Fatalf("PR a runs = %+v", all[a.ID])
	}
	one, err := st.LatestRoundRuns(ctx, b.ID)
	if err != nil || len(one) != 1 || len(one[b.ID]) != 1 {
		t.Fatalf("LatestRoundRuns(b) = %+v, %v", one, err)
	}
}

func TestCheckoutStepsSelectsThePRsSlotSubjects(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	pr := mustPR(t, st, repo.ID, 12, PRQueued)
	other := mustPR(t, st, repo.ID, 123, PRReviewed)
	slot := mustSlot(t, st, "review1", SlotFree)
	if _, err := st.ClaimSlot(ctx, pr.ID, slot.ID); err != nil {
		t.Fatal(err)
	}
	ev := func(subject, kind, step, phase string) {
		t.Helper()
		e := Event{Subject: &subject, Kind: kind, Message: "m"}
		if step != "" {
			e.Step, e.Phase = &step, &phase
		}
		if _, err := st.AppendEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	ev("slot:review1:pr:12:abcdef1", KindStepReset, "", "")
	ev("slot:review1:pr:12:abcdef1", KindStep, "fetch", PhaseBegin)
	ev("slot:review1:pr:123:abcdef1", KindStep, "fetch", PhaseBegin) // another PR whose number starts alike
	ev("slot:review1:pr:12:abcdef1", "slot.head_moved", "", "")      // not a step
	ev("slot:talkable/talkable#12", KindStep, "clone", PhaseBegin)   // the per-PR worktree
	ev("slot:talkable/talkable#123", KindStep, "clone", PhaseBegin)
	ev("slot:review1:release", KindStep, "guard", PhaseBegin)
	ev("slot:review1:pr:12:abcdef1", KindStep, "verify", PhaseOK)

	got, err := st.CheckoutSteps(ctx, pr.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, e := range got {
		subjects = append(subjects, *e.Subject+" "+e.Kind)
	}
	want := []string{
		"slot:review1:pr:12:abcdef1 step.reset", "slot:review1:pr:12:abcdef1 step",
		"slot:talkable/talkable#12 step", "slot:review1:pr:12:abcdef1 step",
	}
	if len(subjects) != len(want) {
		t.Fatalf("subjects = %q, want %q", subjects, want)
	}
	for i := range want {
		if subjects[i] != want[i] {
			t.Fatalf("subjects = %q, want %q", subjects, want)
		}
	}
	if last, err := st.CheckoutSteps(ctx, pr.ID, 1); err != nil || len(last) != 1 || *last[0].Step != "verify" {
		t.Fatalf("limit 1 = %+v, %v", last, err)
	}
	if none, err := st.CheckoutSteps(ctx, other.ID, 0); err != nil || len(none) != 1 || *none[0].Subject != "slot:talkable/talkable#123" {
		t.Fatalf("other PR = %+v, %v", none, err)
	}
}

// The board asks for its rows' latest rounds every 5 s: the query reads each
// PR's runs once to find its highest round, never once per run as a
// correlated subquery does.
func TestLatestRoundRunsReadsEachPRsRunsOnce(t *testing.T) {
	st, _ := newStore(t)
	for _, n := range []int{0, 1, 3} {
		args := make([]any, n)
		for i := range args {
			args[i] = i + 1
		}
		if plan := queryPlan(t, st, latestRoundRunsQuery(n), args...); strings.Contains(plan, "CORRELATED") {
			t.Errorf("%d ids: plan %s\nwant no correlated subquery", n, plan)
		} else {
			t.Logf("%d ids: %s", n, plan)
		}
	}
}
