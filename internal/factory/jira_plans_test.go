package factory

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/jira"
	"github.com/zhuravel/magnum/internal/tui"
)

// The moves of the shadow repository in these tests.
var (
	shadowStart     = config.FactoryJiraMove{To: "In Development", From: []string{"To Do", "In Progress"}}
	shadowPublished = config.FactoryJiraMove{To: "In Code Review", From: []string{"To Do", "In Progress", "In Development"}}
)

// newShadowRig is a tracking rig whose repository, example/shop, plans its
// tasks' Jira moves and writes their Jira comment in shadow.
func newShadowRig(t *testing.T, edit ...func(d *Deps)) *trackRig {
	t.Helper()
	r := newTrackRig(t, edit...)
	r.cfg.Factory.Repos = []config.FactoryRepo{{Repo: "example/shop", JiraMoves: config.JiraShadow, JiraMoveStart: shadowStart,
		JiraMovePublished: shadowPublished, JiraComment: config.JiraShadow}}
	return r
}

// issueAt is an issue in status name of category cat since three days
// before t0, assigned to you.
func issueAt(key, name, cat string) jira.IssueStatus {
	return jira.IssueStatus{Key: key, Name: name, Category: cat, StatusID: "id-" + name, Since: t0.Add(-72 * time.Hour), Searched: true,
		Assigned: true, AssigneeYou: true}
}

// toDoWorkflow is the transitions of a To Do issue in these tests.
func toDoWorkflow() []jira.Transition {
	return []jira.Transition{
		{ID: "21", Name: "Start progress", To: "In Development", ToID: "id-In Development", ToCategory: "indeterminate"},
		{ID: "31", Name: "Ready for review", To: "In Code Review", ToID: "id-In Code Review", ToCategory: "indeterminate"},
		{ID: "41", Name: "Resolve", To: "Done", ToID: "id-Done", ToCategory: "done"},
	}
}

// plans are the registry's Jira plans.
func (h *harness) plans() []registry.JiraPlan {
	h.t.Helper()
	ps, err := h.reg.JiraPlans(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return ps
}

// toStage moves task id to stage; publish publishes it as PR #n; merge
// makes it done.
func (h *harness) toStage(id int64, stage string) {
	h.t.Helper()
	if err := h.reg.TransitionTask(h.ctx, id, nil, "", func(u *registry.TaskUpdate) { u.Set("stage", stage) }); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) publish(id int64, n int) {
	h.t.Helper()
	if err := h.reg.TransitionTask(h.ctx, id, nil, registry.TaskPublished, func(u *registry.TaskUpdate) {
		u.Set("stage", "publish")
		u.Set("pr_number", n)
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) merge(id int64) {
	h.t.Helper()
	if err := h.reg.TransitionTask(h.ctx, id, nil, registry.TaskDone, nil); err != nil {
		h.t.Fatal(err)
	}
}

// transAsked are the keys the fake's Transitions was asked for.
func (f *trackJira) transReads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.transAsked)
}

// phases are the phases of events evs, in order.
func phases(evs []registry.Event) []string {
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Phase)
	}
	return out
}

// The factory's Jira only reads: its interface has the reads of a ticket,
// a search, a status and the transitions, and the client the daemon gives
// it has no method beside its reads and the token file's warning. No code
// path of the factory can send Jira a write.
func TestTheFactorysJiraOnlyReads(t *testing.T) {
	names := func(typ reflect.Type) []string {
		var out []string
		for m := range typ.Methods() {
			out = append(out, m.Name)
		}
		return out
	}
	if got, want := names(reflect.TypeFor[Jira]()), []string{"Search", "StatusSince", "Ticket", "Transitions"}; !slices.Equal(got, want) {
		t.Errorf("the factory's Jira = %q, want the reads %q", got, want)
	}
	if got, want := names(reflect.TypeFor[*jira.Client]()), []string{"Myself", "Search", "Status", "StatusSince", "Ticket", "Transitions",
		"WarnTokenFile"}; !slices.Equal(got, want) {
		t.Errorf("the Jira client's methods = %q, want its reads %q: a write is a decision of its own", got, want)
	}
}

// A task of a shadow repository with a Jira key plans its issue's move
// once at each milestone: when its coding stage begins (start), when its
// PR is published; never before its coding stage, never again on a later
// pass, never at the merge. Each plan reads the status and the
// transitions, and records its begin and its outcome as events of its
// effect; nothing goes to Jira.
func TestAShadowTaskPlansOneMoveAtEachMilestone(t *testing.T) {
	r := newShadowRig(t)
	task := r.task("PR-12360", registry.TaskActive, "investigate")
	r.jira.set("PR-12360", issueAt("PR-12360", "To Do", "new"))
	r.jira.edit(func(f *trackJira) { f.trans["PR-12360"] = toDoWorkflow() })
	r.pass()
	if ps := r.plans(); len(ps) != 0 {
		t.Fatalf("a task before its coding stage planned %+v", ps)
	}
	r.toStage(task.ID, "coding")
	r.clock.Add(time.Minute)
	r.pass()
	start := registry.JiraPlan{TaskID: task.ID, Milestone: config.JiraMilestoneStart, State: registry.EffectOK, Begun: t0.Add(time.Minute),
		Result: registry.JiraPlanResult{Mode: config.JiraShadow, Outcome: registry.PlanPlanned, From: "To Do", To: "In Development",
			Transition: "Start progress", At: t0.Add(time.Minute)}}
	if ps := r.plans(); len(ps) != 1 || !reflect.DeepEqual(ps[0], start) {
		t.Fatalf("the start's plan: %+v\nwant %+v", ps, start)
	}
	evs := r.events(task.ID, "jira_plan")
	if got := phases(evs); !slices.Equal(got, []string{registry.PhaseBegin, registry.PhaseOK}) {
		t.Fatalf("the plan's events: %q, want begin and ok", got)
	}
	for _, w := range []string{"PR-12360", "To Do", "In Development", "Start progress", "shadow", "nothing was written to Jira"} {
		if !strings.Contains(evs[1].Text, w) {
			t.Errorf("the plan's event %q lacks %q", evs[1].Text, w)
		}
	}
	for range 3 {
		r.clock.Add(trackEvery)
		r.pass()
	}
	if ps, asked := r.plans(), r.jira.transReads(); len(ps) != 1 || len(asked) != 1 {
		t.Fatalf("later passes: plans %+v, transitions read %q; want the one plan and its one read", ps, asked)
	}

	r.jira.set("PR-12360", issueAt("PR-12360", "In Development", "indeterminate"))
	r.publish(task.ID, 4530)
	r.clock.Add(time.Minute)
	r.pass()
	at := r.clock.Now()
	ps := r.plans()
	want := registry.JiraPlanResult{Mode: config.JiraShadow, Outcome: registry.PlanPlanned, From: "In Development", To: "In Code Review",
		Transition: "Ready for review", At: at}
	if len(ps) != 2 || ps[1].Milestone != config.JiraMilestonePublished || ps[1].State != registry.EffectOK || !reflect.DeepEqual(ps[1].Result, want) {
		t.Fatalf("the publish's plan: %+v\nwant %+v", ps, want)
	}
	r.merge(task.ID)
	for range 3 {
		r.clock.Add(trackEvery)
		r.pass()
	}
	if ps, asked := r.plans(), r.jira.transReads(); len(ps) != 2 || len(asked) != 2 {
		t.Errorf("after the merge: plans %+v, transitions read %q; want no move at the merge", ps, asked)
	}
	var steps []string // the plans' own steps; the info events say what was seen after them
	for _, ev := range r.events(task.ID, "jira_plan") {
		if ev.Phase != registry.PhaseInfo {
			steps = append(steps, ev.Phase)
		}
	}
	if !slices.Equal(steps, []string{registry.PhaseBegin, registry.PhaseOK, registry.PhaseBegin, registry.PhaseOK}) {
		t.Errorf("the plans' events: %q, want each plan's begin and ok", steps)
	}
}

// A plan's outcome comes from the status and the transitions: a move
// planned (from a status the move goes from, by the transition to its
// target, the assignee you or nobody); nothing to move when the issue is
// at the target or past it (a status the move does not go from); and the
// plans the factory cannot make, for you to move it: the issue is assigned
// to someone else, no transition goes to the target (naming those there
// are), the transition wants fields, or its target is done in Jira. Only a
// plan that could move it reads the transitions.
func TestEachPlanOutcomeComesFromTheStatusAndTheTransitions(t *testing.T) {
	other := issueAt("PR-12360", "To Do", "new")
	other.AssigneeYou = false
	nobody := issueAt("PR-12360", "In Progress", "indeterminate")
	nobody.Assigned, nobody.AssigneeYou = false, false
	fields := toDoWorkflow()
	fields[0].NeedsFields = true
	done := toDoWorkflow()
	done[0].ToCategory = "done"
	for name, tc := range map[string]struct {
		status jira.IssueStatus
		trans  []jira.Transition
		want   registry.JiraPlanResult
		state  string
		read   bool // the transitions were read
		words  []string
	}{
		"planned": {issueAt("PR-12360", "To Do", "new"), toDoWorkflow(), registry.JiraPlanResult{Outcome: registry.PlanPlanned, From: "To Do",
			Transition: "Start progress"}, registry.EffectOK, true, []string{"planned", "from To Do to In Development"}},
		"planned for nobody's issue": {nobody, toDoWorkflow(), registry.JiraPlanResult{Outcome: registry.PlanPlanned, From: "In Progress",
			Transition: "Start progress"}, registry.EffectOK, true, []string{"from In Progress to In Development"}},
		"at the target": {issueAt("PR-12360", "in development", "indeterminate"), toDoWorkflow(), registry.JiraPlanResult{Outcome: registry.PlanAlready,
			From: "in development"}, registry.EffectOK, false, []string{"already In Development", "nothing to move"}},
		"past it": {issueAt("PR-12360", "In Code Review", "indeterminate"), toDoWorkflow(), registry.JiraPlanResult{Outcome: registry.PlanAlready,
			From: "In Code Review"}, registry.EffectOK, false, []string{"In Code Review", "past In Development", "nothing to move"}},
		"someone else's": {other, toDoWorkflow(), registry.JiraPlanResult{Outcome: registry.PlanAssignedOther, From: "To Do"}, registry.EffectFail,
			false, []string{"assigned to someone else", "move it yourself"}},
		"no transition": {issueAt("PR-12360", "To Do", "new"), toDoWorkflow()[1:], registry.JiraPlanResult{Outcome: registry.PlanNoTransition,
			From: "To Do", Transitions: []string{"Ready for review → In Code Review", "Resolve → Done"}}, registry.EffectFail, true,
			[]string{"no transition to In Development", "Ready for review → In Code Review, Resolve → Done", "move it yourself"}},
		"no transition at all": {issueAt("PR-12360", "To Do", "new"), nil, registry.JiraPlanResult{Outcome: registry.PlanNoTransition,
			From: "To Do"}, registry.EffectFail, true, []string{"offers no transition", "Transition issues"}},
		"fields": {issueAt("PR-12360", "To Do", "new"), fields, registry.JiraPlanResult{Outcome: registry.PlanNeedsFields, From: "To Do",
			Transition: "Start progress"}, registry.EffectFail, true, []string{"wants fields", "move it yourself"}},
		"a done target": {issueAt("PR-12360", "To Do", "new"), done, registry.JiraPlanResult{Outcome: registry.PlanTargetDone, From: "To Do",
			Transition: "Start progress"}, registry.EffectFail, true, []string{"done", "never closes or resolves"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newShadowRig(t)
			task := r.task("PR-12360", registry.TaskActive, "coding")
			r.jira.set("PR-12360", tc.status)
			r.jira.edit(func(f *trackJira) { f.trans["PR-12360"] = tc.trans })
			r.pass()
			want := tc.want
			want.Mode, want.To, want.At = config.JiraShadow, "In Development", t0
			ps := r.plans()
			if len(ps) != 1 || ps[0].State != tc.state || !reflect.DeepEqual(ps[0].Result, want) {
				t.Fatalf("the plan: %+v\nwant %s %+v", ps, tc.state, want)
			}
			if asked := r.jira.transReads(); (len(asked) == 1) != tc.read {
				t.Errorf("transitions read %q, want a read %v", asked, tc.read)
			}
			evs := r.events(task.ID, "jira_plan")
			if len(evs) != 2 || evs[1].Phase != tc.state {
				t.Fatalf("the plan's events: %+v, want begin and %s", evs, tc.state)
			}
			for _, w := range tc.words {
				if !strings.Contains(evs[1].Text, w) {
					t.Errorf("the plan's event %q lacks %q", evs[1].Text, w)
				}
			}
		})
	}
}

// A plan whose read fails waits: it stays begun, its read is tried again
// as the tracking's own (10 minutes after the try), the failure on the
// issue's JIRA line; the task goes on as before. When the next milestone
// comes first, it is dropped, its last cause kept (could not plan, unread),
// and the next milestone's plan begins.
func TestAPlanWhoseReadFailsWaitsAndTheNextMilestoneDropsIt(t *testing.T) {
	r := newShadowRig(t)
	task := r.task("PR-12360", registry.TaskActive, "coding")
	r.jira.edit(func(f *trackJira) { f.refused["PR-12360"], f.trans["PR-12360"] = 404, toDoWorkflow() })
	r.pass()
	if ps := r.plans(); len(ps) != 1 || ps[0].State != registry.EffectBegin {
		t.Fatalf("a plan whose read failed: %+v, want it begun", ps)
	}
	if got := r.reload(task.ID); got.State != registry.TaskActive || got.WaitsWho != "" {
		t.Errorf("the task: %s waiting on %q, want it as it was", got.State, got.WaitsWho)
	}
	r.clock.Add(time.Minute)
	r.pass()
	if reads := r.jira.reads(); len(reads) != 1 {
		t.Fatalf("reads %q a minute after the failure, want the first alone", reads)
	}
	r.clock.Add(trackEvery)
	r.pass()
	if reads := r.jira.reads(); len(reads) != 2 {
		t.Fatalf("reads %q 10 minutes after the failure, want a second", reads)
	}
	r.publish(task.ID, 4530)
	r.clock.Add(time.Minute)
	r.pass()
	ps := r.plans()
	if len(ps) != 2 || ps[0].State != registry.EffectFail || ps[0].Result.Outcome != registry.PlanUnread ||
		!strings.Contains(ps[0].Result.Cause, "HTTP 404") || ps[1].State != registry.EffectBegin {
		t.Fatalf("after the publish: %+v, want the start's dropped (unread, its cause) and the publish's begun", ps)
	}
	evs := r.events(task.ID, "jira_plan")
	if last := evs[len(evs)-2]; last.Phase != registry.PhaseFail || !strings.Contains(last.Text, "published") || !strings.Contains(last.Text, "HTTP 404") {
		t.Errorf("the dropped plan's event: %+v", last)
	}
	r.jira.edit(func(f *trackJira) { delete(f.refused, "PR-12360") })
	r.clock.Add(trackEvery)
	r.pass()
	if ps := r.plans(); ps[1].State != registry.EffectOK || ps[1].Result.Outcome != registry.PlanPlanned || ps[1].Result.From != "To Do" {
		t.Errorf("the publish's plan once Jira answers: %+v", ps[1])
	}
	if asked := r.jira.transReads(); len(asked) != 1 {
		t.Errorf("transitions read %q, want the one read of the plan that worked", asked)
	}
}

// After a planned move, the first read that finds the issue in another
// status records what you did: your move to the plan's target, with its
// time (the changelog's, else the read's) and the lag from the plan; a
// move to another status; and, for a task that ended with no move (after
// its last read), that it did not move. Each is recorded once, as an event.
func TestTheOperatorsMoveIsMatchedToThePlan(t *testing.T) {
	for name, tc := range map[string]struct {
		next  *jira.IssueStatus // the status after the plan; nil: none, the task ends
		moved string
		to    string
		lag   time.Duration
		words string
	}{
		"to the target": {&jira.IssueStatus{Name: "In Development", Since: t0.Add(18 * time.Minute)}, registry.SeenTarget, "In Development",
			18 * time.Minute, "you moved PR-12360 to In Development 18m after the plan"},
		"to the target, since unknown": {&jira.IssueStatus{Name: "In Development"}, registry.SeenTarget, "In Development", 20 * time.Minute,
			"you moved PR-12360 to In Development 20m after the plan"},
		"elsewhere": {&jira.IssueStatus{Name: "Blocked", Since: t0.Add(5 * time.Minute)}, registry.SeenElsewhere, "Blocked", 5 * time.Minute,
			"you moved PR-12360 to Blocked, not In Development"},
		"no move by the end": {nil, registry.SeenNone, "", 0, "PR-12360 ended in To Do"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newShadowRig(t)
			task := r.task("PR-12360", registry.TaskActive, "coding")
			r.jira.set("PR-12360", issueAt("PR-12360", "To Do", "new"))
			r.jira.edit(func(f *trackJira) { f.trans["PR-12360"] = toDoWorkflow() })
			r.pass()
			if tc.next != nil {
				s := issueAt("PR-12360", tc.next.Name, "indeterminate")
				s.Since = tc.next.Since
				r.jira.set("PR-12360", s)
				r.clock.Add(20 * time.Minute)
			} else {
				r.merge(task.ID)
				r.clock.Add(lastReadAfter)
			}
			r.pass()
			r.clock.Add(45 * time.Second)
			r.pass()
			r.clock.Add(trackEvery)
			r.pass()
			ps := r.plans()
			if len(ps) != 1 || ps[0].Seen == nil {
				t.Fatalf("the plan: %+v, want what was seen of it", ps)
			}
			if s := ps[0].Seen; s.Moved != tc.moved || s.To != tc.to || s.Lag() != tc.lag {
				t.Errorf("seen %+v, want %s to %q after %s", s, tc.moved, tc.to, tc.lag)
			}
			evs := r.events(task.ID, "jira_plan")
			if len(evs) != 3 || evs[2].Phase != registry.PhaseInfo || !strings.Contains(evs[2].Text, tc.words) {
				t.Errorf("the plan's events: %+v, want an info event saying %q", evs, tc.words)
			}
		})
	}
}

// A restart between a plan's steps plans once: the plan begun, its read
// made, and the daemon gone before it took the answer, the next daemon
// reads it again and settles the one plan, with one begin and one end.
func TestARestartBetweenThePlansStepsPlansOnce(t *testing.T) {
	r := newShadowRig(t)
	task := r.task("PR-12360", registry.TaskActive, "coding")
	r.jira.set("PR-12360", issueAt("PR-12360", "To Do", "new"))
	r.jira.edit(func(f *trackJira) { f.trans["PR-12360"] = toDoWorkflow() })
	r.tick()
	r.e.waitBackground() // its answers wait on the loop of a daemon that stops now
	if ps := r.plans(); len(ps) != 1 || ps[0].State != registry.EffectBegin {
		t.Fatalf("before the restart: %+v, want the plan begun", ps)
	}
	r.e.stopBackground()
	r.e = New(r.deps)
	t.Cleanup(r.e.stopBackground)
	for _, typ := range []string{config.FactoryStageAgents, config.FactoryStageGate, config.FactoryStageReview, config.FactoryStageDecision,
		config.FactoryStageEffect, config.FactoryStageQA} {
		r.stage(typ, idleStage)
	}
	r.clock.Add(time.Minute)
	for range 3 {
		r.pass()
		r.clock.Add(trackEvery)
	}
	ps := r.plans()
	if len(ps) != 1 || ps[0].State != registry.EffectOK || ps[0].Result.Outcome != registry.PlanPlanned || !ps[0].Result.At.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after the restart: %+v, want the one plan, made by the next daemon", ps)
	}
	if got := phases(r.events(task.ID, "jira_plan")); !slices.Equal(got, []string{registry.PhaseBegin, registry.PhaseOK}) {
		t.Errorf("the plan's events: %q, want one begin and one ok", got)
	}
	if asked := r.jira.transReads(); len(asked) != 2 {
		t.Errorf("transitions read %q, want the first daemon's and the next's", asked)
	}
}

// No plan without its keys: a repository whose jira_moves is off, one
// whose block sets no Jira keys, a task of your words, a key of another
// tracker and a factory without [jira] plan nothing and read no
// transitions; the issue's status is tracked as before.
func TestNoPlanWithoutShadowOrTheTasksOwnJiraKey(t *testing.T) {
	for name, edit := range map[string]func(r *trackRig){
		"off": func(r *trackRig) {
			r.cfg.Factory.Repos[0].JiraMoves, r.cfg.Factory.Repos[0].JiraComment = config.JiraOff, config.JiraOff
		},
		"no keys": func(r *trackRig) {
			r.cfg.Factory.Repos[0] = config.FactoryRepo{Repo: "example/shop"}
		},
		"another repository's keys": func(r *trackRig) { r.cfg.Factory.Repos[0].Repo = "example/other" },
		"another tracker's key":     func(r *trackRig) { r.cfg.Board.Trackers = []string{"https://linear.app/example/issue/PR-{num}"} },
	} {
		t.Run(name, func(t *testing.T) {
			r := newShadowRig(t)
			edit(r)
			r.task("PR-12360", registry.TaskActive, "coding")
			if _, err := r.reg.CreateTask(r.ctx, registry.Task{Prompt: "fix the digest", Kind: "bug", Repo: "example/shop", Title: "fix",
				State: registry.TaskActive, Stage: "coding"}); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				r.pass()
				r.clock.Add(trackEvery)
			}
			if ps, asked := r.plans(), r.jira.transReads(); len(ps) != 0 || len(asked) != 0 {
				t.Errorf("plans %+v, transitions read %q; want none", ps, asked)
			}
			for id := range int64(2) {
				if _, err := os.Stat(filepath.Join(r.layout.FactoryTaskDir(id+1), jiraCommentFile)); err == nil {
					t.Errorf("task %d has a comment file without the keys", id+1)
				}
			}
		})
	}
	r := newShadowRig(t, func(d *Deps) { d.Jira = nil })
	r.task("PR-12360", registry.TaskActive, "coding")
	r.pass()
	if ps := r.plans(); len(ps) != 0 {
		t.Errorf("[jira] off: plans %+v", ps)
	}
}

// The mirror comment in shadow: at the start, the publish and the merge
// the factory writes the comment it would keep on the issue to the task's
// folder, jira-comment.md, rewritten at each milestone, once, with an
// event: the factory's own words alone (the task, the PR's repository and
// number with its link, the task's state and stage, the time), never the
// PR's, the ticket's or a finding's text.
func TestTheCommentFileAtEachMilestoneHoldsTheFactorysWordsAlone(t *testing.T) {
	r := newShadowRig(t)
	task, err := r.reg.CreateTask(r.ctx, registry.Task{JiraKey: "PR-12360", Kind: "bug", Repo: "example/shop", Title: "Payouts secret title",
		State: registry.TaskActive, Stage: "investigate"})
	if err != nil {
		t.Fatal(err)
	}
	dir := r.layout.FactoryTaskDir(task.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ticketFile), []byte("# PR-12360\n\nPayouts secret ticket\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, jiraCommentFile))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	r.pass()
	if _, err := os.Stat(filepath.Join(dir, jiraCommentFile)); err == nil {
		t.Fatal("a comment before the coding stage")
	}
	r.toStage(task.ID, "coding")
	r.clock.Add(time.Minute)
	r.pass()
	want := "The factory started coding this issue (magnum factory, task 1).\nPR: none yet\nState: active, stage coding\nUpdated: 2026-11-04 09:01 UTC\n"
	if got := read(); got != want {
		t.Errorf("the start's comment:\n%s\nwant\n%s", got, want)
	}
	r.clock.Add(time.Hour)
	r.pass()
	if got := read(); got != want {
		t.Errorf("a later pass rewrote the comment:\n%s", got)
	}
	r.publish(task.ID, 4530)
	r.clock.Add(time.Minute)
	r.pass()
	want = "The factory published its PR for review (magnum factory, task 1).\nPR: example/shop#4530 https://github.com/example/shop/pull/4530\n" +
		"State: published, stage publish\nUpdated: 2026-11-04 10:02 UTC\n"
	if got := read(); got != want {
		t.Errorf("the publish's comment:\n%s\nwant\n%s", got, want)
	}
	r.merge(task.ID)
	r.clock.Add(time.Minute)
	r.pass()
	want = "The factory's PR is merged (magnum factory, task 1).\nPR: example/shop#4530 https://github.com/example/shop/pull/4530\n" +
		"State: done, stage publish\nUpdated: 2026-11-04 10:03 UTC\n"
	got := read()
	if got != want {
		t.Errorf("the merge's comment:\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "Payouts") {
		t.Errorf("the comment quotes the task's or the ticket's text:\n%s", got)
	}
	evs := r.events(task.ID, "jira_comment")
	if got := phases(evs); !slices.Equal(got, []string{registry.PhaseBegin, registry.PhaseOK, registry.PhaseBegin, registry.PhaseOK,
		registry.PhaseBegin, registry.PhaseOK}) {
		t.Errorf("the comment's events: %q, want a begin and an ok at each milestone", got)
	}
	if !strings.Contains(evs[1].Text, "shadow") || !strings.Contains(evs[1].Text, jiraCommentFile) {
		t.Errorf("the comment's event %q", evs[1].Text)
	}
	// A task merged before the comment was on never gets one.
	old, err := r.reg.CreateTask(r.ctx, registry.Task{JiraKey: "PR-12361", Kind: "bug", Repo: "example/shop", Title: "old", State: registry.TaskActive})
	if err != nil {
		t.Fatal(err)
	}
	r.publish(old.ID, 4531)
	r.merge(old.ID)
	r.pass()
	if _, err := os.Stat(filepath.Join(r.layout.FactoryTaskDir(old.ID), jiraCommentFile)); err == nil {
		t.Error("a task merged before its first comment got one at the merge")
	}
}

// The task card carries the task's latest plan, its outcome and what was
// seen after it, for the card's one line; a task without one has none.
func TestTheTaskCardCarriesTheLatestJiraPlan(t *testing.T) {
	r := newShadowRig(t)
	task := r.task("PR-12360", registry.TaskActive, "coding")
	bare := r.task("PR-12361", registry.TaskActive, "investigate")
	r.jira.set("PR-12360", issueAt("PR-12360", "To Do", "new"))
	r.jira.edit(func(f *trackJira) { f.trans["PR-12360"] = toDoWorkflow() })
	r.pass()
	r.jira.set("PR-12360", jira.IssueStatus{Key: "PR-12360", Name: "In Development", StatusID: "id-In Development", Since: t0.Add(18 * time.Minute),
		Searched: true})
	r.clock.Add(20 * time.Minute)
	r.pass()
	r.clock.Add(45 * time.Second)
	r.pass()
	d, err := ScreenData(r.ctx, r.reg, nil, r.clock.Now(), time.Time{}, ScreenOptions{Config: r.cfg, Layout: r.layout})
	if err != nil {
		t.Fatal(err)
	}
	want := &tui.FactoryJiraPlan{Milestone: config.JiraMilestoneStart, Mode: config.JiraShadow, Outcome: registry.PlanPlanned, From: "To Do",
		To: "In Development", Transition: "Start progress", Begun: t0, At: t0, Moved: registry.SeenTarget, MovedTo: "In Development",
		MovedAt: t0.Add(18 * time.Minute)}
	if got := d.Cards[task.ID].Task.JiraPlan; !reflect.DeepEqual(got, want) {
		t.Errorf("the card's plan: %+v\nwant %+v", got, want)
	}
	if got := d.Cards[bare.ID].Task.JiraPlan; got != nil {
		t.Errorf("a task without a plan: %+v", got)
	}
}

// A plan whose read never worked is dropped when its task ends (stopped
// here): could not plan, unread, with the read's last cause; no plan
// begins for an ended task.
func TestABegunPlanIsDroppedWhenItsTaskEnds(t *testing.T) {
	r := newShadowRig(t)
	task := r.task("PR-12360", registry.TaskActive, "coding")
	r.jira.edit(func(f *trackJira) { f.down = 28 })
	r.pass()
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, registry.TaskStopped, nil); err != nil {
		t.Fatal(err)
	}
	r.clock.Add(time.Minute)
	r.pass()
	ps := r.plans()
	if len(ps) != 1 || ps[0].State != registry.EffectFail || ps[0].Result.Outcome != registry.PlanUnread || ps[0].Result.Cause == "" {
		t.Fatalf("the plan of a stopped task: %+v, want it dropped with its cause", ps)
	}
	evs := r.events(task.ID, "jira_plan")
	if last := evs[len(evs)-1]; last.Phase != registry.PhaseFail || !strings.Contains(last.Text, "the task ended first") {
		t.Errorf("the dropped plan's event: %+v", last)
	}
	if asked := r.jira.transReads(); len(asked) != 0 {
		t.Errorf("transitions read %q for a plan whose status never answered", asked)
	}
}
