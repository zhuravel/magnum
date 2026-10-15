package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// fullPassHarness is the harness with [pipeline] full_pass_every = every,
// whose rounds leave their judge's run with its review as the pipeline
// does (the count reads them).
func fullPassHarness(t *testing.T, every int, mods ...func(*harness)) *harness {
	t.Helper()
	set := func(h *harness) { h.cfg.Pipeline.FullPassEvery = every }
	h := newHarness(t, append([]func(*harness){set}, mods...)...)
	h.rd.mu.Lock()
	h.rd.judgeLogin = "talkable[bot]"
	h.rd.mu.Unlock()
	return h
}

// pushAndRereview pushes head to PR #2 and runs its re-review once the
// push's quiet period is over; it returns the round's input.
func pushAndRereview(t *testing.T, h *harness, head string) pipeline.RoundInput {
	t.Helper()
	before := len(h.rd.all())
	h.advance(30 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: head})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	ins := h.rd.all()
	if len(ins) != before+1 {
		t.Fatalf("rounds = %d after the push of %s, want %d", len(ins), head, before+1)
	}
	if pr := h.wantState(2, store.PRReviewed); deref(pr.ReviewedSHA) != head {
		t.Fatalf("reviewed_sha %q after the re-review of %s", deref(pr.ReviewedSHA), head)
	}
	return ins[before]
}

// wantOrdinary fails unless in is an ordinary re-review in the judge's
// conversation.
func wantOrdinary(t *testing.T, label string, in pipeline.RoundInput) {
	t.Helper()
	if in.Kind != pipeline.KindRereview || in.FullPass {
		t.Fatalf("%s: kind %s, full pass %v: want an ordinary re-review", label, in.Kind, in.FullPass)
	}
}

// wantFullPass fails unless in is a full pass: a recovery (the judge's
// recovery prompt carries the clause) of every role, no cold judge.
func wantFullPass(t *testing.T, label string, in pipeline.RoundInput) {
	t.Helper()
	if in.Kind != pipeline.KindRecovery || !in.FullPass || in.ColdJudge || in.DeltaCheck != nil || len(in.Roles) < 3 {
		t.Fatalf("%s: kind %s, full pass %v, cold %v, delta check %v, roles %v: want a full pass of every role",
			label, in.Kind, in.FullPass, in.ColdJudge, in.DeltaCheck, roleNames(in.Roles))
	}
}

// lastFullPassOf is PR n's kvPRFullPass record.
func lastFullPassOf(t *testing.T, h *harness, n int) fullPassRecord {
	t.Helper()
	v, ok := kvValue(h, kvPRFullPass(h.pr(n).ID))
	if !ok {
		t.Fatal("no full pass recorded")
	}
	var rec fullPassRecord
	if err := json.Unmarshal([]byte(v), &rec); err != nil {
		t.Fatalf("full pass record %q: %v", v, err)
	}
	return rec
}

// judgeRunOf is PR n's judge run of round.
func judgeRunOf(t *testing.T, h *harness, n, round int) store.Run {
	t.Helper()
	runs, err := h.st.RunsByPR(h.ctx, h.pr(n).ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.Round == round && r.Role == store.RoleJudge {
			return r
		}
	}
	t.Fatalf("no judge run of round %d", round)
	return store.Run{}
}

// After its first review nothing read a long-lived PR whole again: every
// re-review reads the commits since the last one. The Nth automatic
// re-review (full_pass_every) parks the sessions and starts every role
// fresh at its full effort, the reviewers on their first-review prompts
// and the judge on the recovery prompt with the full-pass clause; the start
// event says so, and the full pass is recorded with its judge's run.
func TestTheNthReReviewIsAFullPass(t *testing.T) {
	h := fullPassHarness(t, 2)
	h.reviewedPR(2, "b1")
	wantOrdinary(t, "the first re-review", pushAndRereview(t, h, "b2"))
	if _, ok := kvValue(h, kvPRFullPass(h.pr(2).ID)); ok {
		t.Fatal("a full pass is recorded after an ordinary re-review")
	}

	pr := h.pr(2)
	parks, starts := h.ag.count(fmt.Sprintf("park:%d", pr.ID)), len(agentStarts(h))
	in := pushAndRereview(t, h, "b3")
	wantFullPass(t, "the second re-review", in)
	if h.ag.count(fmt.Sprintf("park:%d", pr.ID)) == parks {
		t.Fatalf("calls %v: want the sessions parked before the full pass", h.ag.all())
	}
	fresh := agentStarts(h)[starts:]
	for _, want := range []string{"codex-judge:xhigh:", "claude-review:high:"} {
		if !slices.Contains(fresh, want) {
			t.Errorf("starts %v: want %s, a fresh session at the full effort", fresh, want)
		}
	}
	for _, s := range fresh {
		if !strings.HasSuffix(s, ":") {
			t.Errorf("start %s resumed a conversation: a full pass starts every role fresh", s)
		}
	}

	evs := approvalEvents(t, h, 2, "engine.round_start")
	last := evs[len(evs)-1]
	if !strings.HasPrefix(last.Message, "re-review (full pass) in ") || eventData(t, last)["full_pass"] != true {
		t.Fatalf("round start %q %s: want the full pass named", last.Message, last.Data)
	}
	why := approvalEvents(t, h, 2, "round.full_pass")
	if len(why) != 1 || why[0].Message != "full pass: re-review 2 since the first review (full_pass_every 2): every role starts in a fresh session and reviews the whole PR" {
		t.Fatalf("round.full_pass events: %+v", why)
	}
	rec := lastFullPassOf(t, h, 2)
	if run := judgeRunOf(t, h, 2, 3); rec.Round != 3 || rec.Run != run.ID || rec.Target != "b3" {
		t.Fatalf("full pass record %+v, want round 3, run %s, target b3", rec, run.ID)
	}
	if _, ok := kvValue(h, kvPRFullPassRound(pr.ID)); ok {
		t.Error("the in-flight full pass mark outlived its round")
	}
}

// The count starts again after a full pass: the next re-review is an
// ordinary one in the fresh judge's conversation, and the Nth after it is
// the next full pass.
func TestAFullPassResetsTheCount(t *testing.T) {
	h := fullPassHarness(t, 2)
	h.reviewedPR(2, "b1")
	wantOrdinary(t, "re-review 1", pushAndRereview(t, h, "b2"))
	wantFullPass(t, "re-review 2", pushAndRereview(t, h, "b3"))
	wantOrdinary(t, "re-review 3", pushAndRereview(t, h, "b4"))
	in := pushAndRereview(t, h, "b5")
	wantFullPass(t, "re-review 4", in)
	if rec := lastFullPassOf(t, h, 2); rec.Round != 5 || rec.Target != "b5" {
		t.Fatalf("full pass record %+v, want the one of round 5", rec)
	}
	why := approvalEvents(t, h, 2, "round.full_pass")
	if len(why) != 2 || !strings.Contains(why[1].Message, "re-review 2 since the full pass of round 3") {
		t.Fatalf("round.full_pass events: %+v", why)
	}
}

// seedRereviews records rounds 2 to n+1 of PR #2 as re-reviews that
// posted on new commits, as an earlier daemon left them.
func seedRereviews(t *testing.T, h *harness, n int) {
	t.Helper()
	pr := h.pr(2)
	for i := range n {
		round := i + 2
		prev, review := fmt.Sprintf("s%d", round-1), int64(900+round)
		if _, err := h.st.CreateRun(h.ctx, store.Run{PRID: pr.ID, Round: round, Role: store.RoleJudge, Kind: store.RunRereview,
			TargetSHA: fmt.Sprintf("s%d", round), PrevReviewedSHA: &prev, Identity: pr.Identity, ReviewerLogin: "talkable[bot]",
			State: store.RunVerified, ReviewID: &review}); err != nil {
			t.Fatal(err)
		}
	}
}

// full_pass_every = 0 turns full passes off: the twentieth re-review is an
// ordinary one. The count reads the runs, so a PR reviewed before the rule
// counts its past re-reviews: with 20, that same re-review is a full pass.
func TestFullPassOffByZero(t *testing.T) {
	for _, tc := range []struct {
		every int
		full  bool
	}{{0, false}, {20, true}} {
		t.Run(fmt.Sprintf("full_pass_every %d", tc.every), func(t *testing.T) {
			h := fullPassHarness(t, tc.every)
			h.reviewedPR(2, "b1")
			seedRereviews(t, h, 19)
			in := pushAndRereview(t, h, "b2")
			if tc.full {
				wantFullPass(t, "the twentieth re-review", in)
				return
			}
			wantOrdinary(t, "the twentieth re-review", in)
		})
	}
}

// A delta check never becomes a full pass: a small push at the Nth
// re-review keeps its check (the judge alone, in its conversation), and the
// next full re-review is the full pass.
func TestADeltaCheckIsNeverAFullPass(t *testing.T) {
	h, _, _ := deltaCheckHarness(t, func(h *harness) { h.cfg.Pipeline.FullPassEvery = 1 })
	h.rd.mu.Lock()
	h.rd.judgeLogin = "talkable[bot]"
	h.rd.mu.Unlock()
	verdictRounds(h, "APPROVED")
	keptForCheck(t, h, liveDelta)
	quietEnd := h.pr(2).HeadChangedAt.Add(5 * time.Minute)
	pollPR(h, quietEnd.Sub(h.clock.Now()), 2, "b2")
	ins := h.rd.all()
	if len(ins) != 2 || ins[1].DeltaCheck == nil || ins[1].FullPass || ins[1].Kind != pipeline.KindRereview ||
		!slices.Equal(roleNames(ins[1].Roles), []string{"codex-judge"}) {
		t.Fatalf("rounds = %d: want the delta check of the judge alone, no full pass", len(ins))
	}
	if evs := approvalEvents(t, h, 2, "round.full_pass"); len(evs) != 0 {
		t.Fatalf("round.full_pass events: %+v", evs)
	}

	reqSetFiles(h, "b2...b3", codeDelta(30))
	pollPR(h, time.Minute, 2, "b3")
	pollPR(h, 30*time.Minute, 2, "b3")
	ins = h.rd.all()
	if len(ins) != 3 {
		t.Fatalf("rounds = %d, want the full re-review of b3", len(ins))
	}
	wantFullPass(t, "the full re-review after the check", ins[2])
}

// A round someone asked for (`magnum review`, the board's r) never becomes
// a full pass, however many re-reviews came before; the next one left to
// the daemon is one.
func TestARoundSomeoneAskedForIsNeverAFullPass(t *testing.T) {
	h := fullPassHarness(t, 1)
	h.reviewedPR(2, "b1")
	if err := h.st.UpdatePR(h.ctx, h.pr(2).ID, func(u *store.PRUpdate) { u.Set("forced", true) }); err != nil {
		t.Fatal(err)
	}
	wantOrdinary(t, "a forced re-review", pushAndRereview(t, h, "b2"))
	wantFullPass(t, "the automatic re-review after it", pushAndRereview(t, h, "b3"))
}

// `magnum review --full` asks for a full pass by hand: the forced round
// runs as one, whether or not one is due, even on the reviewed head.
func TestAFullPassCanBeAskedFor(t *testing.T) {
	h := fullPassHarness(t, 0)
	h.reviewedPR(2, "b1")
	h.advance(time.Minute)
	id := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}, Full: true})
	h.tick()
	if req := h.request(id); req.State != store.RequestDone {
		t.Fatalf("request %s: %s", req.State, deref(req.Result))
	}
	h.tick()
	ins := h.rd.all()
	if len(ins) != 2 {
		t.Fatalf("rounds = %d, want the full pass", len(ins))
	}
	wantFullPass(t, "the requested full pass", ins[1])
	why := approvalEvents(t, h, 2, "round.full_pass")
	if len(why) != 1 || !strings.Contains(why[0].Message, "full pass: requested by magnum review --full: every role starts in a fresh session") {
		t.Fatalf("round.full_pass events: %+v", why)
	}
}

// The status comment's in-progress line says when the round under way is a
// full pass.
func TestTheStatusCommentNamesAFullPassUnderWay(t *testing.T) {
	h := fullPassHarness(t, 1, statusOn)
	h.statusReviewed()
	h.advance(30 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 7, head: statusPush})
	h.tick()
	h.advance(5 * time.Minute) // the quiet period is over
	h.holdRounds()
	before := len(h.rd.all())
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.awaitRound(before)
	h.advance(2 * time.Minute)
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	pr := h.wantState(7, store.PRReviewing)
	if c := h.statusComment(7); !strings.Contains(c.body, "\n- [x] In progress (full pass) since "+utcClock(*pr.LastRoundStartedAt)+"\n") {
		t.Fatalf("the full pass under way:\n%s", c.body)
	}
	h.releaseRound()
	h.wantState(7, store.PRReviewed)
	if _, ok := kvValue(h, kvPRFullPassRound(pr.ID)); ok {
		t.Error("the in-flight full pass mark outlived its round")
	}
}
