package registry_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

// A Jira plan is the effect of its task and milestone (jira:<task>:<milestone>),
// begun when the milestone comes and settled with its result; what the
// factory saw of your move after it is the effect beside it (its key and
// :seen). JiraPlans gives each plan with what was seen of it, by task and
// milestone in their order, and leaves out every other effect.
func TestJiraPlansAreTheirEffectsWithWhatWasSeenOfThem(t *testing.T) {
	t.Parallel()
	r, clk := openAt(t)
	ctx := t.Context()
	a, b := createTask(t, r, "PR-12301"), createTask(t, r, "PR-12302")
	begin := func(key, kind string, id int64) {
		t.Helper()
		if _, run, err := r.BeginEffect(ctx, key, kind, id); err != nil || !run {
			t.Fatalf("begin %s: run %v, %v", key, run, err)
		}
	}
	end := func(key, state, result string) {
		t.Helper()
		if err := r.EndEffect(ctx, key, state, result); err != nil {
			t.Fatal(err)
		}
	}
	planned := registry.JiraPlanResult{Mode: "shadow", Outcome: registry.PlanPlanned, From: "To Do", To: "In Development",
		Transition: "Start progress", At: t0.Add(time.Minute)}
	begin(registry.JiraPlanKey(a.ID, "published"), registry.EffectJiraPlan, a.ID) // stays begun: its read has not worked yet
	begin(registry.JiraPlanKey(a.ID, "start"), registry.EffectJiraPlan, a.ID)
	end(registry.JiraPlanKey(a.ID, "start"), registry.EffectOK, planned.Encode())
	seen := registry.JiraSeenResult{Moved: registry.SeenTarget, To: "In Development", At: t0.Add(19 * time.Minute), LagSecs: 18 * 60}
	begin(registry.JiraSeenKey(a.ID, "start"), registry.EffectJiraSeen, a.ID)
	end(registry.JiraSeenKey(a.ID, "start"), registry.EffectOK, seen.Encode())
	clk.Add(time.Hour)
	other := registry.JiraPlanResult{Mode: "shadow", Outcome: registry.PlanNoTransition, From: "To Do", To: "In Code Review",
		Transitions: []string{"Start progress", "Close"}, At: t0.Add(time.Hour)}
	begin(registry.JiraPlanKey(b.ID, "published"), registry.EffectJiraPlan, b.ID)
	end(registry.JiraPlanKey(b.ID, "published"), registry.EffectFail, other.Encode())
	begin("publish:2:abc", "publish", b.ID)
	begin(registry.JiraCommentKey(b.ID, "published"), registry.EffectJiraComment, b.ID)

	got, err := r.JiraPlans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []registry.JiraPlan{
		{TaskID: a.ID, Milestone: "start", State: registry.EffectOK, Begun: t0, Result: planned, Seen: &seen},
		{TaskID: a.ID, Milestone: "published", State: registry.EffectBegin, Begun: t0},
		{TaskID: b.ID, Milestone: "published", State: registry.EffectFail, Begun: t0.Add(time.Hour), Result: other},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("JiraPlans:\n%+v\nwant\n%+v", got, want)
	}
	if got[0].Seen.Lag() != 18*time.Minute {
		t.Errorf("the lag = %s, want 18m", got[0].Seen.Lag())
	}
}
