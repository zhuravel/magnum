package factory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// fakePRWork is the fix lane as the PR requests see it: it records each
// call (its kind and its payload) and answers with plan for a Plan.
type fakePRWork struct {
	mu    sync.Mutex
	calls []prCall
	plan  FixAsked
	plans map[string]FixAsked // by kind, over plan
	err   error               // what every call answers (nil: words)
}

type prCall struct {
	kind    string
	payload any
}

func (f *fakePRWork) note(kind string, p any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, prCall{kind, p})
	if f.err != nil {
		return "", f.err
	}
	return "did " + kind, nil
}

func (f *fakePRWork) Plan(_ context.Context, kind string, pr PRRef) (FixPlan, error) {
	if _, err := f.note("plan "+kind, pr); err != nil {
		return FixPlan{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	asked, ok := f.plans[kind]
	if !ok {
		asked = f.plan
	}
	return FixPlan{Asked: asked, Text: "dry run: would " + kind + " " + pr.Repo}, nil
}

func (f *fakePRWork) Fix(_ context.Context, p FixPayload) (string, error) { return f.note(ReqFix, p) }
func (f *fakePRWork) SwitchFix(_ context.Context, p FixSwitchPayload, off bool) (string, error) {
	if off {
		return f.note(ReqFixOff, p)
	}
	return f.note(ReqFixOn, p)
}
func (f *fakePRWork) FixDone(_ context.Context, p FixDonePayload) (string, error) {
	return f.note(ReqFixDone, p)
}
func (f *fakePRWork) FixDecide(_ context.Context, p FixDecidePayload) (string, error) {
	return f.note(ReqFixDecide, p)
}
func (f *fakePRWork) FixCI(_ context.Context, p FixCIPayload) (string, error) {
	return f.note(ReqFixCI, p)
}
func (f *fakePRWork) MergeParent(_ context.Context, p MergeParentPayload) (string, error) {
	return f.note(ReqMergeParent, p)
}
func (f *fakePRWork) StopFix(_ context.Context, p FixStopPayload) (string, error) {
	return f.note(ReqFixStop, p)
}
func (f *fakePRWork) Handoff(_ context.Context, p HandoffPayload) (string, error) {
	return f.note(ReqHandoff, p)
}

func (f *fakePRWork) kinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.kind)
	}
	return out
}

// shopPR is the PR the requests name.
var shopPR = PRRef{Repo: "example/shop", Number: 7}

// headA and headB are two heads of the PR.
var (
	headA = strings.Repeat("a", 40)
	headB = strings.Repeat("b", 40)
)

// validPRRequests are a request of each PR kind as a screen sends it, each
// one the factory would take.
func validPRRequests(at time.Time) map[string]any {
	merge := &FixAsked{Head: headA, Base: "master", Tip: headB, At: at}
	return map[string]any{
		ReqFix:         FixPayload{PRRef: shopPR, Surface: "cli"},
		ReqFixOff:      FixSwitchPayload{PRRef: shopPR},
		ReqFixOn:       FixSwitchPayload{PRRef: shopPR},
		ReqFixDone:     FixDonePayload{PRRef: shopPR, Seen: "a1b2c3"},
		ReqFixDecide:   FixDecidePayload{PRRef: shopPR, Decide: []FixDecision{{Thread: 11, Reply: "Keep it as it is."}}},
		ReqFixCI:       FixCIPayload{PRRef: shopPR, Asked: &FixAsked{Head: headA, Entries: []string{"Completion"}, Count: 1, At: at}},
		ReqMergeParent: MergeParentPayload{PRRef: shopPR, Asked: merge},
		ReqFixStop:     FixStopPayload{PRRef: shopPR},
		ReqHandoff:     HandoffPayload{PRRef: shopPR, Asked: &FixAsked{Head: headA, Assignee: "rev-ann", At: at}},
	}
}

// ask enqueues a request of kind with payload p and answers the pending
// requests, as a tick does first; it returns the answered request.
func (h *harness) ask(kind string, p any) registry.Request {
	h.t.Helper()
	id, err := h.reg.EnqueueRequest(h.ctx, kind, p)
	if err != nil {
		h.t.Fatal(err)
	}
	h.e.handleRequests(h.ctx)
	return h.request(id)
}

// While [factory] fix is off (the default until the switch day), every PR
// request of the fix lane, each kind with a payload the factory would take
// and a dry run alike, is answered that the factory does not fix PRs yet,
// naming the setting, and the fix lane is never asked.
func TestEveryFixRequestIsAnsweredWhileFixIsOff(t *testing.T) {
	work := &fakePRWork{}
	h := newHarness(t, func(d *Deps) { d.PRWork = work })
	if h.cfg.Factory.Fix != config.FactoryFixOff {
		t.Fatalf("[factory] fix = %q in the harness, want the default off", h.cfg.Factory.Fix)
	}
	reqs := validPRRequests(t0)
	if len(reqs) != len(FixKinds) {
		t.Fatalf("%d requests for %d kinds", len(reqs), len(FixKinds))
	}
	dry := map[string]any{ReqFix: FixPayload{PRRef: shopPR, DryRun: true}, ReqFixCI: FixCIPayload{PRRef: shopPR, DryRun: true},
		ReqMergeParent: MergeParentPayload{PRRef: shopPR, DryRun: true}, ReqHandoff: HandoffPayload{PRRef: shopPR, DryRun: true}}
	for _, kind := range FixKinds {
		for _, p := range []any{reqs[kind], dry[kind]} {
			if p == nil {
				continue
			}
			q := h.ask(kind, p)
			if q.State != registry.RequestFailed || !strings.Contains(q.Result, "the factory does not fix PRs yet") ||
				!strings.Contains(q.Result, `[factory] fix is "off"`) {
				t.Errorf("%s %+v: %s %q, want the answer that the factory does not fix yet, naming [factory] fix", kind, p, q.State, q.Result)
			}
		}
	}
	if calls := work.kinds(); len(calls) != 0 {
		t.Fatalf("the fix lane was asked %v while fix is off", calls)
	}
	// Each kind is the factory's: none is answered as unknown.
	q := h.ask("fix_something", FixPayload{PRRef: shopPR})
	if !strings.Contains(q.Result, "unknown request kind") {
		t.Fatalf("an unknown kind is answered %q", q.Result)
	}
}

// With [factory] fix on, each PR request reaches its call of the fix lane
// with its payload; a build with no fix lane answers that it does not fix
// PRs yet.
func TestAFixRequestReachesTheFixLaneOnlyWhenFixIsOn(t *testing.T) {
	h := newHarness(t)
	h.cfg.Factory.Fix = config.FactoryFixOn
	q := h.ask(ReqFix, FixPayload{PRRef: shopPR})
	if q.State != registry.RequestFailed || !strings.Contains(q.Result, "this factory build does not fix PRs yet") {
		t.Fatalf("a fix with no fix lane: %s %q", q.State, q.Result)
	}

	work := &fakePRWork{}
	h = newHarness(t, func(d *Deps) { d.PRWork = work })
	h.cfg.Factory.Fix = config.FactoryFixOn
	reqs := validPRRequests(t0)
	work.plans = map[string]FixAsked{ReqFixCI: *reqs[ReqFixCI].(FixCIPayload).Asked, ReqMergeParent: *reqs[ReqMergeParent].(MergeParentPayload).Asked,
		ReqHandoff: *reqs[ReqHandoff].(HandoffPayload).Asked}
	for _, kind := range FixKinds {
		q := h.ask(kind, validPRRequests(t0)[kind])
		if q.State != registry.RequestDone || q.Result != "did "+kind {
			t.Errorf("%s: %s %q, want the fix lane's answer", kind, q.State, q.Result)
		}
	}
	want := []string{ReqFix, ReqFixOff, ReqFixOn, ReqFixDone, ReqFixDecide, "plan " + ReqFixCI, ReqFixCI, "plan " + ReqMergeParent, ReqMergeParent,
		ReqFixStop, "plan " + ReqHandoff, ReqHandoff}
	if got := work.kinds(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the fix lane's calls %v, want %v", got, want)
	}
	if p, ok := work.calls[4].payload.(FixDecidePayload); !ok || p.Decide[0].Reply != "Keep it as it is." || p.Repo != "example/shop" {
		t.Errorf("the decide reached the lane as %+v", work.calls[4].payload)
	}
	decide, _ := h.reg.RequestByID(h.ctx, 5)
	if strings.Contains(string(decide.Payload), "Keep it") {
		t.Errorf("the decide's payload keeps your words after its answer: %s", decide.Payload)
	}
	work.err = errors.New("PR #7 is not yours")
	if q := h.ask(ReqFixStop, FixStopPayload{PRRef: shopPR}); q.State != registry.RequestFailed || q.Result != "PR #7 is not yours" {
		t.Errorf("the lane's refusal is answered %s %q", q.State, q.Result)
	}
}

// A dry run answers the plan as magnum's merge of the parent does: its JSON
// on the first line (what the y/N asks about), its words after. The y that
// sends the plan back binds the request to it: the same plan runs, and a
// head, a tip, a held merge, the failed checks or the assignee that moved
// since, or a question 10 minutes old or older, is refused with what moved,
// and the fix lane does nothing.
func TestADryRunAnswersThePlanAndTheYIsBoundToIt(t *testing.T) {
	work := &fakePRWork{}
	h := newHarness(t, func(d *Deps) { d.PRWork = work })
	h.cfg.Factory.Fix = config.FactoryFixOn
	planned := FixAsked{Head: headA, Base: "master", Tip: headB, Conflicts: 2, At: t0}
	work.plan = planned

	q := h.ask(ReqMergeParent, MergeParentPayload{PRRef: shopPR, DryRun: true})
	plan, ok := ParseFixPlan(q.Result)
	if q.State != registry.RequestDone || !ok || plan.Ref != "example/shop#7" || plan.Kind != ReqMergeParent || plan.Asked.Tip != headB ||
		!strings.HasSuffix(q.Result, "\ndry run: would merge_parent example/shop") {
		t.Fatalf("the dry run's answer %s %q (parsed %+v, %v)", q.State, q.Result, plan, ok)
	}
	if got := work.kinds(); len(got) != 1 || got[0] != "plan "+ReqMergeParent {
		t.Fatalf("a dry run asked the lane %v, want its plan alone", got)
	}

	asked := plan.Asked
	h.clock.Add(time.Minute)
	if q := h.ask(ReqMergeParent, MergeParentPayload{PRRef: shopPR, Asked: &asked}); q.State != registry.RequestDone {
		t.Fatalf("the y bound to the plan: %s %q", q.State, q.Result)
	}

	for name, tc := range map[string]struct {
		moved func(a *FixAsked)
		want  string
	}{
		"the head":   {func(a *FixAsked) { a.Head = headB }, "its head moved from aaaaaaa to bbbbbbb since you asked"},
		"the tip":    {func(a *FixAsked) { a.Tip = headA }, "master moved from bbbbbbb to aaaaaaa since you asked"},
		"conflicts":  {func(a *FixAsked) { a.Conflicts = 3 }, "the merge conflicts in 3 files now, and you were asked about 2"},
		"held merge": {func(a *FixAsked) { a.Merge = headB }, "the factory holds the merge bbbbbbb now"},
		"the base":   {func(a *FixAsked) { a.Base = "main" }, "its base moved from master to main"},
	} {
		t.Run(name, func(t *testing.T) {
			work.mu.Lock()
			work.plan, work.calls = planned, nil
			tc.moved(&work.plan)
			work.mu.Unlock()
			q := h.ask(ReqMergeParent, MergeParentPayload{PRRef: shopPR, Asked: &asked})
			if q.State != registry.RequestFailed || !strings.Contains(q.Result, tc.want) || !strings.Contains(q.Result, "nothing was done") {
				t.Fatalf("%s %q, want the refusal naming what moved", q.State, q.Result)
			}
			if got := work.kinds(); len(got) != 1 {
				t.Fatalf("the lane was asked %v, want its plan alone", got)
			}
		})
	}

	work.plan = FixAsked{Head: headA, Entries: []string{"Completion", "Lint"}, Count: 1, At: t0}
	q = h.ask(ReqFixCI, FixCIPayload{PRRef: shopPR, Asked: &FixAsked{Head: headA, Entries: []string{"Completion"}, Count: 1, At: h.clock.Now()}})
	if !strings.Contains(q.Result, "the failed checks are Completion, Lint (CI fix 1) now, and you were asked about Completion (CI fix 1)") {
		t.Errorf("a CI fix whose failed checks moved: %q", q.Result)
	}
	work.plan = FixAsked{Head: headA, Assignee: "rev-bob", At: t0}
	q = h.ask(ReqHandoff, HandoffPayload{PRRef: shopPR, Asked: &FixAsked{Head: headA, Assignee: "rev-ann", At: h.clock.Now()}})
	if !strings.Contains(q.Result, "it goes to rev-bob now, and you were asked about rev-ann") {
		t.Errorf("a hand-off whose assignee moved: %q", q.Result)
	}

	work.mu.Lock()
	work.plan, work.calls = planned, nil
	work.mu.Unlock()
	h.clock.Add(10 * time.Minute)
	q = h.ask(ReqMergeParent, MergeParentPayload{PRRef: shopPR, Asked: &asked})
	if q.State != registry.RequestFailed || !strings.Contains(q.Result, "asked 11 minutes ago, longer than a y binds its plan (10 minutes)") {
		t.Fatalf("a y asked 11 minutes ago: %s %q", q.State, q.Result)
	}
	if got := work.kinds(); len(got) != 1 {
		t.Fatalf("the lane was asked %v, want its plan alone", got)
	}
}

// Each PR request checks its payload before the fix lane sees it: a PR
// named without its owner or number, a done that names no attention you
// saw, a decide with no reply, an empty one, one with no thread or one
// thread twice, a dry run that carries a binding, a hand-off without one,
// a binding that names no plan of its kind, and a field this build does not
// know (a newer CLI's) are refused, and the lane is never asked.
func TestThePRRequestsCheckTheirPayloads(t *testing.T) {
	work := &fakePRWork{}
	h := newHarness(t, func(d *Deps) { d.PRWork = work })
	h.cfg.Factory.Fix = config.FactoryFixOn
	noOwner := PRRef{Repo: "shop", Number: 7}
	for _, tc := range []struct {
		kind string
		p    any
		want string
	}{
		{ReqFix, FixPayload{PRRef: noOwner}, "names no pull request"},
		{ReqFixStop, FixStopPayload{PRRef: PRRef{Repo: "example/shop"}}, "names no pull request"},
		{ReqFixOff, FixSwitchPayload{PRRef: PRRef{Repo: "example/shop/x", Number: 7}}, "names no pull request"},
		{ReqFixDone, FixDonePayload{PRRef: shopPR}, "names no attention you saw"},
		{ReqFixDecide, FixDecidePayload{PRRef: shopPR}, "carries no reply"},
		{ReqFixDecide, FixDecidePayload{PRRef: shopPR, Decide: []FixDecision{{Thread: 11, Reply: "  "}}}, "says nothing"},
		{ReqFixDecide, FixDecidePayload{PRRef: shopPR, Decide: []FixDecision{{Reply: "yes"}}}, "names no thread"},
		{ReqFixDecide, FixDecidePayload{PRRef: shopPR, Decide: []FixDecision{{Thread: 11, Reply: "a"}, {Thread: 11, Reply: "b"}}}, "named twice"},
		{ReqFixDecide, FixDecidePayload{PRRef: shopPR, Decide: []FixDecision{{Thread: 11, Reply: strings.Repeat("x", maxReplyBytes+1)}}},
			"longer than GitHub takes"},
		{ReqMergeParent, MergeParentPayload{PRRef: shopPR, DryRun: true, Asked: &FixAsked{Head: headA, At: t0}}, "a dry run binds nothing"},
		{ReqHandoff, HandoffPayload{PRRef: shopPR}, "needs the plan its y/N asked about"},
		{ReqMergeParent, MergeParentPayload{PRRef: shopPR, Asked: &FixAsked{Head: headA, At: t0}}, "no the parent and its tip"},
		{ReqFixCI, FixCIPayload{PRRef: shopPR, Asked: &FixAsked{Head: headA, At: t0}}, "no the failed checks"},
		{ReqHandoff, HandoffPayload{PRRef: shopPR, Asked: &FixAsked{Head: headA, At: t0}}, "no whom the PR goes to"},
		{ReqFixCI, FixCIPayload{PRRef: shopPR, Asked: &FixAsked{Entries: []string{"Completion"}}}, "no its head and its time"},
		{ReqFixStop, map[string]any{"Repo": "example/shop", "Number": 7, "Force": true}, "this factory daemon predates"},
	} {
		q := h.ask(tc.kind, tc.p)
		if q.State != registry.RequestFailed || !strings.Contains(q.Result, tc.want) {
			t.Errorf("%s %+v: %s %q, want a refusal with %q", tc.kind, tc.p, q.State, q.Result, tc.want)
		}
	}
	if got := work.kinds(); len(got) != 0 {
		t.Fatalf("the lane was asked %v about requests it never takes", got)
	}
}
