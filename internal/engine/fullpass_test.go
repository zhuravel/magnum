package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
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

// An overdue full pass overrides a delta check: a small push gets the
// judge's check alone, but when the next one is the Nth re-review, its push
// of 2 lines runs the full pass of every role instead, with no delta check
// in flight, so small pushes no longer defer the full pass without end.
func TestAnOverdueFullPassOverridesADeltaCheck(t *testing.T) {
	h, _, _ := deltaCheckHarness(t, func(h *harness) { h.cfg.Pipeline.FullPassEvery = 2 })
	h.rd.mu.Lock()
	h.rd.judgeLogin = "talkable[bot]"
	h.rd.mu.Unlock()
	inFlight := verdictRounds(h, "APPROVED")
	keptForCheck(t, h, liveDelta)
	quietEnd := h.pr(2).HeadChangedAt.Add(5 * time.Minute)
	pollPR(h, quietEnd.Sub(h.clock.Now()), 2, "b2")
	ins := h.rd.all()
	if len(ins) != 2 || ins[1].DeltaCheck == nil || ins[1].FullPass || !slices.Equal(roleNames(ins[1].Roles), []string{"codex-judge"}) {
		t.Fatalf("rounds = %d: want the delta check of b2, re-review 1", len(ins))
	}

	reqSetFiles(h, "b2...b3", liveDelta[2:])
	pollPR(h, 30*time.Minute, 2, "b3")
	pollPR(h, 30*time.Minute, 2, "b3")
	ins = h.rd.all()
	if len(ins) != 3 {
		t.Fatalf("rounds = %d, want the full pass of b3", len(ins))
	}
	wantFullPass(t, "the small push at the Nth re-review", ins[2])
	if got := (*inFlight)[2]; got != "" {
		t.Fatalf("delta check in flight %q: want none", got)
	}
	for _, kind := range []string{"round.delta_check_dropped", "round.delta_check_fresh"} {
		if evs := approvalEvents(t, h, 2, kind); len(evs) != 0 {
			t.Fatalf("%s events %+v: the full pass was dispatched as a delta check", kind, evs)
		}
	}
	why := approvalEvents(t, h, 2, "round.full_pass")
	if len(why) != 1 || eventData(t, why[0])["rule"] != "full_pass_every" {
		t.Fatalf("round.full_pass events: %+v", why)
	}
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

// sizedGit sizes PR #2 at each head of sizes ({files, lines}) for git diff
// --numstat from the round's merge base: that many source files of that
// many lines in all, and a lockfile of 5,000 lines the size leaves out.
func sizedGit(sizes map[string][2]int) func(*harness) {
	return func(h *harness) {
		stats := map[string][]gitx.FileStat{}
		for head, s := range sizes {
			files, lines := s[0], s[1]
			fs := []gitx.FileStat{{Path: "Gemfile.lock", Added: 4000, Removed: 1000}}
			for i := range files {
				n := lines / files
				if i == 0 {
					n += lines % files
				}
				fs = append(fs, gitx.FileStat{Path: fmt.Sprintf("app/models/m%d.rb", i), Added: n})
			}
			stats["base0000..."+head] = fs
		}
		h.d.Git = fakeGit{stats: stats}
	}
}

// touched is a push that adds lines code lines to each of paths, and
// changes a lockfile the churn leaves out.
func touched(lines int, paths ...string) []github.FileDelta {
	out := []github.FileDelta{{Path: "yarn.lock", Status: "modified", Lines: 900}}
	for _, p := range paths {
		d := codeDelta(lines)[0]
		d.Path = p
		out = append(out, d)
	}
	return out
}

// churnOfPR is PR n's churn since its baseline.
func churnOfPR(t *testing.T, h *harness, n int) fullPassChurn {
	t.Helper()
	c, _ := h.e.churnSince(h.ctx, h.pr(n).ID)
	return c
}

// fullPassWhyOf is the reason and the rule of PR n's last round.full_pass
// event, and the reason its last round start gives.
func fullPassWhyOf(t *testing.T, h *harness, n int) (reason, rule, start string) {
	t.Helper()
	evs := approvalEvents(t, h, n, "round.full_pass")
	if len(evs) == 0 {
		t.Fatal("no round.full_pass event")
	}
	d := eventData(t, evs[len(evs)-1])
	starts := approvalEvents(t, h, n, "engine.round_start")
	reason, _ = d["reason"].(string)
	rule, _ = d["rule"].(string)
	start, _ = eventData(t, starts[len(starts)-1])["full_pass_reason"].(string)
	return reason, rule, start
}

// Rounds measure how often the whole-PR view was lost, not how much the
// code changed. A first review of a 1,000-line PR (its lockfile left out)
// is its baseline; three re-reviews of 200 lines each change 600 lines,
// over the larger of full_pass_lines (500) and a quarter of the PR (250),
// so the fourth runs as a full pass, its reason naming the lines. The full
// pass is the new baseline, with the PR's size then, and the churn starts
// again after it.
func TestChurnSinceTheBaselineTriggersAFullPass(t *testing.T) {
	h := fullPassHarness(t, 0, sizedGit(map[string][2]int{"b1": {20, 1000}, "b5": {22, 1800}}))
	h.reviewedPR(2, "b1")
	id := h.pr(2).ID
	base, ok := h.e.readBaseline(h.ctx, kvPRReviewBaseline(id))
	if !ok || base.Target != "b1" || base.Base != "base0000" || base.Files != 20 || base.Lines != 1000 || !base.Initial {
		t.Fatalf("review baseline %+v (%v): want the first review of b1, 20 files and 1000 lines", base, ok)
	}
	heads := []string{"b1", "b2", "b3", "b4"}
	for i, head := range heads[1:] {
		reqSetFiles(h, heads[i]+"..."+head, touched(200, "app/models/coupon.rb"))
		wantOrdinary(t, "the re-review of "+head, pushAndRereview(t, h, head))
	}
	if c := churnOfPR(t, h, 2); c.Lines != 600 || c.Files != 1 || !slices.Equal(c.Paths, []string{"app/models/coupon.rb"}) || c.To != "b4" {
		t.Fatalf("churn %+v: want 600 lines in coupon.rb, the lockfile left out", c)
	}

	reqSetFiles(h, "b4...b5", touched(200, "app/models/coupon.rb"))
	wantFullPass(t, "the fourth re-review", pushAndRereview(t, h, "b5"))
	reason, rule, start := fullPassWhyOf(t, h, 2)
	if want := "600 lines changed since the first review (full_pass_lines 500; a quarter of its 1000 lines then: 250)"; reason != want || rule != "full_pass_lines" || start != want {
		t.Fatalf("full pass reason %q, rule %q, start %q: want %q", reason, rule, start, want)
	}
	if _, ok := kvValue(h, kvPRFullPassChurn(id)); ok {
		t.Fatal("the churn outlived the full pass")
	}
	if _, ok := kvValue(h, kvPRReviewBaseline(id)); ok {
		t.Fatal("the review baseline outlived the full pass")
	}
	if rec := lastFullPassOf(t, h, 2); rec.Target != "b5" || rec.Files != 22 || rec.Lines != 1800 {
		t.Fatalf("full pass record %+v: want b5 with 22 files and 1800 lines", rec)
	}

	reqSetFiles(h, "b5...b6", touched(200, "app/models/coupon.rb"))
	wantOrdinary(t, "the re-review after the full pass", pushAndRereview(t, h, "b6"))
	if c := churnOfPR(t, h, 2); c.Lines != 200 || c.To != "b6" {
		t.Fatalf("churn after the full pass %+v: want the 200 lines of b6", c)
	}
}

// The files the re-reviews touched since the baseline count once each: 4,
// then 2 more (two of the push's four were touched before), then 4 more
// reach full_pass_files (10, over a quarter of the PR's 8), and the next
// re-review is a full pass, its reason naming the files.
func TestTouchedFilesSinceTheBaselineTriggerAFullPass(t *testing.T) {
	h := fullPassHarness(t, 0, sizedGit(map[string][2]int{"b1": {8, 300}}))
	h.reviewedPR(2, "b1")
	pushes := [][]string{
		{"app/a.rb", "app/b.rb", "app/c.rb", "app/d.rb"},
		{"app/a.rb", "app/b.rb", "app/e.rb", "app/f.rb"},
		{"app/g.rb", "app/h.rb", "app/i.rb", "app/j.rb"},
	}
	heads := []string{"b1", "b2", "b3", "b4"}
	for i, head := range heads[1:] {
		reqSetFiles(h, heads[i]+"..."+head, touched(10, pushes[i]...))
		wantOrdinary(t, "the re-review of "+head, pushAndRereview(t, h, head))
	}
	if c := churnOfPR(t, h, 2); c.Files != 10 || c.Lines != 120 || len(c.Paths) != 10 {
		t.Fatalf("churn %+v: want 10 files and 120 lines", c)
	}
	reqSetFiles(h, "b4...b5", touched(10, "app/a.rb"))
	wantFullPass(t, "the fourth re-review", pushAndRereview(t, h, "b5"))
	reason, rule, _ := fullPassWhyOf(t, h, 2)
	if want := "10 files touched since the first review (full_pass_files 10; a quarter of its 8 files then: 2)"; reason != want || rule != "full_pass_files" {
		t.Fatalf("full pass reason %q, rule %q: want %q", reason, rule, want)
	}
}

// A PR of any size gets a full pass at its first re-review half a day
// after the baseline, however small the push (2026-10-15: the operator
// wants the first push of a workday read whole; a week was too rare); a
// push 11 hours after the baseline runs an ordinary re-review.
func TestAnOldPRGetsAFullPass(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files int
		age   time.Duration
		full  bool
	}{{"60 files, 12 hours", 60, 12 * time.Hour, true}, {"5 files, 12 hours", 5, 12 * time.Hour, true}, {"5 files, 11 hours", 5, 11 * time.Hour, false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := fullPassHarness(t, 0, sizedGit(map[string][2]int{"b1": {tc.files, 600}}))
			h.reviewedPR(2, "b1")
			h.advance(tc.age)
			reqSetFiles(h, "b1...b2", touched(40, "app/models/coupon.rb"))
			in := pushAndRereview(t, h, "b2")
			if !tc.full {
				wantOrdinary(t, "a re-review 11 hours after the baseline", in)
				return
			}
			wantFullPass(t, "a re-review half a day after the baseline", in)
			reason, rule, _ := fullPassWhyOf(t, h, 2)
			if want := "12h since the first review (full_pass_after 12h)"; reason != want || rule != "full_pass_after" {
				t.Fatalf("full pass reason %q, rule %q: want %q", reason, rule, want)
			}
		})
	}
}

// A push that merges the base branch is charged with the PR's own change
// only: master's 46 lines in three files are not the PR's churn, the 2
// lines of coupon.rb its own diff changed are.
func TestABaseMergeIsNotChurn(t *testing.T) {
	h := newHarness(t, noThreshold)
	mergedRereview(t, h)
	h.wantState(2, store.PRReviewed)
	if c := churnOfPR(t, h, 2); c.Lines != 2 || c.Files != 1 || !slices.Equal(c.Paths, []string{"app/models/coupon.rb"}) || c.To != mergedHead {
		t.Fatalf("churn %+v: want the 2 lines of the PR's own change in coupon.rb", c)
	}
}

// 0 turns each churn or age rule off alone: with all three due (a large
// PR, 600 lines in 16 files since a baseline 8 days old), the full pass
// names the first rule left on, and none runs with all three off.
func TestFullPassRulesOffByZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		off  func(*harness)
		rule string
	}{
		{"every rule on", func(*harness) {}, "full_pass_lines"},
		{"full_pass_lines 0", func(h *harness) { h.cfg.Pipeline.FullPassLines = 0 }, "full_pass_files"},
		{"full_pass_files 0 too", func(h *harness) { h.cfg.Pipeline.FullPassLines, h.cfg.Pipeline.FullPassFiles = 0, 0 }, "full_pass_after"},
		{"full_pass_after 0 too", func(h *harness) {
			h.cfg.Pipeline.FullPassLines, h.cfg.Pipeline.FullPassFiles, h.cfg.Pipeline.FullPassAfter.Duration = 0, 0, 0
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := fullPassHarness(t, 0, tc.off)
			h.reviewedPR(2, "b1")
			id := h.pr(2).ID
			h.e.setBaseline(h.ctx, kvPRReviewBaseline(id), fullPassRecord{Target: "b1", At: h.clock.Now().Add(-8 * 24 * time.Hour),
				Files: 60, Lines: 1200, Initial: true})
			churn := fullPassChurn{To: "b1"}
			churn.add(600, []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "b1", "b2", "b3", "b4", "b5", "b6", "b7"})
			b, err := json.Marshal(churn)
			if err != nil {
				t.Fatal(err)
			}
			h.e.setKV(h.ctx, kvPRFullPassChurn(id), string(b))
			in := pushAndRereview(t, h, "b2")
			if tc.rule == "" {
				wantOrdinary(t, "every rule off", in)
				return
			}
			wantFullPass(t, tc.name, in)
			if _, rule, _ := fullPassWhyOf(t, h, 2); rule != tc.rule {
				t.Fatalf("rule %q, want %q", rule, tc.rule)
			}
		})
	}
}

// A churn keeps at most maxChurnPaths paths; past it only the count grows.
func TestTheChurnKeepsAtMost500Paths(t *testing.T) {
	var c fullPassChurn
	var paths []string
	for i := range maxChurnPaths + 20 {
		paths = append(paths, fmt.Sprintf("app/f%04d.rb", i))
	}
	c.add(10, paths[:maxChurnPaths])
	c.add(10, paths[:5]) // touched before: no new file
	c.add(10, paths[maxChurnPaths:])
	if c.Files != maxChurnPaths+20 || len(c.Paths) != maxChurnPaths || c.Lines != 30 || !slices.IsSorted(c.Paths) {
		t.Fatalf("churn: %d files, %d paths, %d lines", c.Files, len(c.Paths), c.Lines)
	}
}
