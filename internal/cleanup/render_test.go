package cleanup

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

func TestRenderPlan(t *testing.T) {
	f := newFixture(t)
	pr := f.closedPR(f.talkable, 11700, store.GHMerged, -time.Minute)
	f.poolSlot("review5", store.SlotHeld, pr.ID, true, nil)
	young := f.closedPR(f.talkable, 11701, store.GHMerged, 6*time.Minute)
	f.poolSlot("review6", store.SlotHeld, young.ID, true, nil)
	foo := f.closedPR(f.foo, 3, store.GHClosed, -time.Minute)
	f.prSlot(foo, store.SlotHeld, true)
	f.orphan("review9", 100)
	f.orphan("pr27087fix", 1)

	out := Render(f.plan(Options{}))
	for _, want := range []string{
		"Plan: free ~4M disk, 800M MySQL (3 actions)",
		"release",
		"talkable#11700 (merged 14m ago) review5",
		"remove_worktree",
		"foo#3 (closed 2h ago)",
		"drop_dbs",
		"slug:review9",
		"8 databases",
		"Skipped:",
		"talkable#11701",
		"grace_left (6m left)",
		"slug:pr27087fix",
		"not_managed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "((") || strings.Contains(out, "))") {
		t.Errorf("nested parentheses:\n%s", out)
	}
	if strings.Contains(out, "talkable/talkable#") {
		t.Errorf("owner/name with the same name should be shortened:\n%s", out)
	}
}

func TestRenderSlotSubjects(t *testing.T) {
	p := Plan{Actions: []Action{
		{Kind: KindRemoveSlot, Subject: "slot:review2", Why: "requested", Slot: "review2", Bytes: 2 << 20},
		{Kind: KindRelease, Subject: "talkable/talkable#9", Why: "merged 1h ago, no slot", PRID: 9},
	}}
	out := Render(p)
	if strings.Contains(out, "review2 review2") || strings.Contains(out, "(requested) review2") {
		t.Errorf("slot repeated:\n%s", out)
	}
	if !strings.Contains(out, "talkable#9 (merged 1h ago, no slot)\n") {
		t.Errorf("slot-less release:\n%s", out)
	}
}

func TestRenderEmptyAndFlags(t *testing.T) {
	if out := Render(Plan{}); !strings.Contains(out, "Nothing to clean up") {
		t.Fatalf("empty = %q", out)
	}
	p := Plan{DryRun: true, Actions: []Action{{Kind: KindDropDBs, Subject: "slug:x", Slug: "x", DBNames: []string{"talkable_test__x"}, Confirm: true}},
		Warnings: []string{"mysql: down"}}
	p.Totals = totals(p.Actions)
	out := Render(p)
	for _, want := range []string{"dry run", "typed confirmation", "Warnings:", "mysql: down"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

func TestRenderReport(t *testing.T) {
	rep := Report{Results: []Result{
		{Action: Action{Kind: KindRelease, Subject: "talkable/talkable#1", Slot: "review1"}, Status: StatusDone},
		{Action: Action{Kind: KindDropDBs, Subject: "slug:x"}, Status: StatusFailed, Error: "boom"},
		{Action: Action{Kind: KindDropDBs, Subject: "slug:y", Confirm: true}, Status: StatusUnconfirmed},
	}, Done: 1, Failed: 1, Unconfirmed: 1}
	out := RenderReport(rep)
	for _, want := range []string{"done", "talkable#1", "failed", "boom", "unconfirmed", "1 done, 1 failed, 1 unconfirmed"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

func TestHumanFormats(t *testing.T) {
	for in, want := range map[int64]string{0: "0B", 512: "512B", 2048: "2K", 5 << 20: "5M", 2254857830: "2.1G"} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[time.Duration]string{
		20 * time.Second: "<1m", 14 * time.Minute: "14m", 2*time.Hour + 5*time.Minute: "2h", 72 * time.Hour: "3d",
	} {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestPlanJSON(t *testing.T) {
	f := newFixture(t)
	f.orphan("review9", 1)
	b, err := json.Marshal(f.plan(Options{}))
	if err != nil {
		t.Fatal(err)
	}
	var back Plan
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Actions) != 1 || back.Actions[0].Kind != KindDropDBs || !strings.Contains(string(b), `"db_names"`) {
		t.Fatalf("json = %s", b)
	}
	// A plan that went through JSON (CLI -> daemon request) still applies.
	rep, err := f.p.Apply(f.ctx, back, false)
	if err != nil || rep.Done != 1 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
}
