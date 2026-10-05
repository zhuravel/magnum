package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// cleanupParse parses cleanup's flags and arguments as the command does.
func cleanupParse(c *Context, args []string) (*cleanupFlags, error) {
	f := &cleanupFlags{cmd: "cleanup", review: true}
	fs := pflag.NewFlagSet("cleanup", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cleanupDefineFlags(fs, f)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if err := cleanupArgs(c, fs.Args()); err != nil {
		return nil, err
	}
	return f, nil
}

type fakePlanner struct {
	plan      cleanup.Plan
	gotOpts   cleanup.Options
	gotPlan   cleanup.Plan // what Apply got
	applied   bool
	confirmed bool
	report    cleanup.Report
	applyErr  error
}

func (f *fakePlanner) Plan(_ context.Context, o cleanup.Options) (cleanup.Plan, error) {
	f.gotOpts = o
	p := f.plan
	p.Options = o
	p.DryRun = o.DryRun
	return p, nil
}

func (f *fakePlanner) Apply(_ context.Context, p cleanup.Plan, confirmed bool) (cleanup.Report, error) {
	f.applied, f.confirmed, f.gotPlan = true, confirmed, p
	return f.report, f.applyErr
}

func TestCleanupFlagsToOptions(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{nil, `{"options":{}}`},
		{[]string{"--pr", "11920"}, `{"options":{"pr":{"repo":"talkable/talkable","number":11920}}}`},
		{[]string{"--pr", "zhuravel/app#3", "--force"}, `{"options":{"pr":{"repo":"zhuravel/app","number":3},"force":true}}`},
		{[]string{"--shrink"}, `{"options":{"shrink":0}}`},
		{[]string{"--shrink=4", "--idle"}, `{"options":{"shrink":4,"idle":true}}`},
		{[]string{"--orphans", "--slug", "pr27087fix"}, `{"options":{"orphans":true,"slug":"pr27087fix"}}`},
		{[]string{"--external", "--slot", "repo3"}, `{"options":{"slot":"repo3","external":true}}`},
		{[]string{"--slot", "review2", "--remove", "--dry-run"}, `{"options":{"dry_run":true,"slot":"review2","remove":true}}`},
	}
	for _, tc := range cases {
		f := newInspFixture(t)
		cf, err := cleanupParse(f.Ctx, tc.args)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		o, err := cf.options(context.Background(), nil, "talkable/talkable")
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		b, _ := json.Marshal(struct {
			O cleanup.Options `json:"options"`
		}{o})
		if string(b) != tc.want {
			t.Errorf("%v: options = %s, want %s", tc.args, b, tc.want)
		}
	}
	f := newInspFixture(t)
	if _, err := cleanupParse(f.Ctx, []string{"--shrink=-1"}); err == nil {
		t.Fatal("negative shrink must fail")
	}
	if _, err := cleanupParse(f.Ctx, []string{"extra"}); err == nil {
		t.Fatal("positional args must fail")
	}
	cf, _ := cleanupParse(f.Ctx, []string{"--pr", "nope!"})
	if _, err := cf.options(context.Background(), nil, "talkable/talkable"); err == nil {
		t.Fatal("bad --pr must fail")
	}
}

func cleanupPlanFixture() cleanup.Plan {
	return cleanup.Plan{
		Actions: []cleanup.Action{
			{Kind: cleanup.KindRelease, Subject: "talkable/talkable#11700", Why: "merged 14m ago", Slot: "review5", Bytes: 2 << 30},
			{Kind: cleanup.KindDropDBs, Subject: "slug:pr27087fix", Why: "orphan databases", Slug: "pr27087fix",
				DBNames: []string{"talkable_test__pr27087fix"}, DBBytes: 800 << 20, Confirm: true},
		},
		Skipped: []cleanup.Skip{{Subject: "talkable/talkable#11701", Reason: cleanup.SkipGraceLeft, Detail: "6m left"}},
		Totals:  cleanup.Totals{Actions: 2, DiskBytes: 2 << 30, MySQLBytes: 800 << 20, Databases: 1},
	}
}

func TestCleanupDryRunNeverApplies(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	fp := &fakePlanner{plan: cleanupPlanFixture()}
	cf, _ := cleanupParse(f.Ctx, []string{"--dry-run", "--yes"})
	if code := cleanupExec(context.Background(), f.Ctx, fp, st, cf, "talkable/talkable"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if fp.applied || !fp.gotOpts.DryRun {
		t.Fatalf("applied=%v opts=%+v", fp.applied, fp.gotOpts)
	}
	if !strings.Contains(f.Out.String(), "release") || !strings.Contains(f.Out.String(), "grace_left") {
		t.Fatalf("plan not rendered:\n%s", f.Out.String())
	}
}

func TestCleanupNeedsConfirmationOffTTY(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	fp := &fakePlanner{plan: cleanupPlanFixture()}
	cf, _ := cleanupParse(f.Ctx, nil)
	if code := cleanupExec(context.Background(), f.Ctx, fp, st, cf, "talkable/talkable"); code != 1 {
		t.Fatalf("code %d", code)
	}
	if fp.applied || !strings.Contains(f.Err.String(), "--yes") {
		t.Fatalf("applied=%v err=%s", fp.applied, f.Err.String())
	}
}

func TestCleanupAppliesInProcess(t *testing.T) {
	cases := []struct {
		name, input   string
		yes           bool
		applied, conf bool
	}{
		{name: "tty yes + slug", input: "y\npr27087fix\n", applied: true, conf: true},
		{name: "tty yes + wrong slug", input: "y\nreview9\n", applied: true, conf: false},
		{name: "tty no", input: "n\n", applied: false},
		{name: "--yes off tty", yes: true, applied: true, conf: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newInspFixture(t)
			st := f.store()
			if tc.input != "" {
				f.tty(tc.input)
			}
			fp := &fakePlanner{plan: cleanupPlanFixture(), report: cleanup.Report{Done: 1, Unconfirmed: 1,
				Results: []cleanup.Result{{Action: cleanupPlanFixture().Actions[0], Status: cleanup.StatusDone}}}}
			var args []string
			if tc.yes {
				args = append(args, "--yes")
			}
			cf, _ := cleanupParse(f.Ctx, args)
			code := cleanupExec(context.Background(), f.Ctx, fp, st, cf, "talkable/talkable")
			if fp.applied != tc.applied || fp.confirmed != tc.conf {
				t.Fatalf("applied=%v confirmed=%v code=%d out=%s err=%s", fp.applied, fp.confirmed, code, f.Out.String(), f.Err.String())
			}
			if tc.applied && (code != 0 || !strings.Contains(f.Out.String(), "done")) {
				t.Fatalf("code=%d out=%s", code, f.Out.String())
			}
		})
	}
}

func TestCleanupApplyFailureExitsOne(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	fp := &fakePlanner{plan: cleanupPlanFixture(), report: cleanup.Report{Failed: 1}, applyErr: errors.New("boom")}
	cf, _ := cleanupParse(f.Ctx, []string{"--yes"})
	if code := cleanupExec(context.Background(), f.Ctx, fp, st, cf, "talkable/talkable"); code != 1 {
		t.Fatalf("code %d", code)
	}
}

func TestCleanupHandsOffToRunningDaemon(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	unlock, held, err := engine.AcquireLock(f.Ctx.Layout.Lock())
	if err != nil || held {
		t.Fatalf("lock: held=%v err=%v", held, err)
	}
	defer unlock()
	fp := &fakePlanner{plan: cleanupPlanFixture()}
	cf, _ := cleanupParse(f.Ctx, []string{"--yes"})
	if code := cleanupExec(context.Background(), f.Ctx, fp, st, cf, "talkable/talkable"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if fp.applied {
		t.Fatal("the CLI must not apply while the daemon holds the lock")
	}
	req, err := nextPendingRequest(context.Background(), st)
	if err != nil || req.Kind != engine.ReqCleanup {
		t.Fatalf("request = %+v err %v", req, err)
	}
	var p engine.CleanupPayload
	if err := json.Unmarshal(req.Payload, &p); err != nil || p.Plan == nil || len(p.Plan.Actions) != 2 || p.Confirmed {
		t.Fatalf("payload = %+v err %v", p, err)
	}
	if !strings.Contains(f.Out.String(), "queued as request") {
		t.Fatalf("out = %s", f.Out.String())
	}
}

func TestCleanupJSON(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	fp := &fakePlanner{plan: cleanupPlanFixture()}
	cf, _ := cleanupParse(f.Ctx, []string{"--json"})
	if code := cleanupExec(context.Background(), f.Ctx, fp, st, cf, "talkable/talkable"); code != 0 {
		t.Fatalf("code %d", code)
	}
	var out struct {
		Plan   cleanup.Plan    `json:"plan"`
		Report *cleanup.Report `json:"report"`
	}
	if err := json.Unmarshal(f.Out.Bytes(), &out); err != nil || len(out.Plan.Actions) != 2 || out.Report != nil || fp.applied {
		t.Fatalf("json out=%s err=%v applied=%v", f.Out.String(), err, fp.applied)
	}
}

func TestCleanupCommandEmptyPlan(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	inspSeedPR(t, st, "talkable/talkable", 1, store.PRReviewed, nil)
	st.Close()
	if code := f.run("cleanup", "--dry-run"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	if code := f.run("cleanup", "--slug", "x"); code != 2 || !strings.Contains(f.Err.String(), "--slug needs --orphans") {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
}

// withCleanupScreen puts cleanup on a terminal; decide is the user's answer
// to the plan the screen shows (nil: the screen must not run).
func withCleanupScreen(t *testing.T, decide func(tui.CleanupPlan) tui.CleanupOutcome) *[]tui.CleanupPlan {
	t.Helper()
	onScreen(t)
	var shown []tui.CleanupPlan
	old := tuiCleanupPlan
	tuiCleanupPlan = func(_ context.Context, p tui.CleanupPlan) (tui.CleanupOutcome, error) {
		if decide == nil {
			t.Fatal("the review screen ran")
		}
		shown = append(shown, p)
		return decide(p), nil
	}
	t.Cleanup(func() { tuiCleanupPlan = old })
	return &shown
}

func TestCleanupReviewScreen(t *testing.T) {
	cases := []struct {
		name      string
		out       tui.CleanupOutcome
		applied   bool
		confirmed bool
		kinds     []string
		totals    cleanup.Totals
	}{
		{name: "the slug drop only", out: tui.CleanupOutcome{Apply: true, SelectedIDs: []string{"1"}}, applied: true, confirmed: true,
			kinds: []string{cleanup.KindDropDBs}, totals: cleanup.Totals{Actions: 1, MySQLBytes: 800 << 20, Databases: 1}},
		{name: "the release only", out: tui.CleanupOutcome{Apply: true, SelectedIDs: []string{"0"}}, applied: true,
			kinds: []string{cleanup.KindRelease}, totals: cleanup.Totals{Actions: 1, DiskBytes: 2 << 30}},
		{name: "both", out: tui.CleanupOutcome{Apply: true, SelectedIDs: []string{"0", "1"}}, applied: true, confirmed: true,
			kinds: []string{cleanup.KindRelease, cleanup.KindDropDBs}, totals: cleanupPlanFixture().Totals},
		{name: "cancelled", out: tui.CleanupOutcome{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newInspFixture(t)
			st := f.store()
			shown := withCleanupScreen(t, func(tui.CleanupPlan) tui.CleanupOutcome { return tc.out })
			fp := &fakePlanner{plan: cleanupPlanFixture(), report: cleanup.Report{Done: 1}}
			cf, _ := cleanupParse(f.Ctx, nil)
			if code := cleanupExec(context.Background(), f.Ctx, fp, st, cf, "talkable/talkable"); code != 0 {
				t.Fatalf("code %d err %s", code, f.Err.String())
			}
			p := (*shown)[0]
			if len(p.Actions) != 2 || p.Actions[0].ID != "0" || p.Actions[0].NeedsTypedConfirm != "" ||
				p.Actions[1].ID != "1" || p.Actions[1].NeedsTypedConfirm != "pr27087fix" || p.Actions[1].DBBytes != 800<<20 ||
				p.Actions[0].Subject != "talkable/talkable#11700" || p.Actions[0].Why != "merged 14m ago" {
				t.Fatalf("screen actions %+v", p.Actions)
			}
			if len(p.Skipped) != 1 || p.Skipped[0] != (tui.CleanupSkip{Subject: "talkable/talkable#11701", Reason: "grace_left (6m left)"}) ||
				p.Totals != (tui.CleanupTotals{Disk: 2 << 30, MySQL: 800 << 20}) {
				t.Fatalf("screen skipped %+v totals %+v", p.Skipped, p.Totals)
			}
			if strings.Contains(f.Out.String(), "Plan:") {
				t.Errorf("the plan was printed as text too:\n%s", f.Out.String())
			}
			if fp.applied != tc.applied || fp.confirmed != tc.confirmed {
				t.Fatalf("applied=%v confirmed=%v", fp.applied, fp.confirmed)
			}
			if !tc.applied {
				actContains(t, f.Out.String(), "Not applied.")
				return
			}
			var kinds []string
			for _, a := range fp.gotPlan.Actions {
				kinds = append(kinds, a.Kind)
			}
			if !slices.Equal(kinds, tc.kinds) || fp.gotPlan.Totals != tc.totals {
				t.Fatalf("applied %v totals %+v", kinds, fp.gotPlan.Totals)
			}
		})
	}
}

func TestCleanupReviewScreenHandsTheSelectionToTheDaemon(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	unlock, held, err := engine.AcquireLock(f.Ctx.Layout.Lock())
	if err != nil || held {
		t.Fatalf("lock: held=%v err=%v", held, err)
	}
	defer unlock()
	withCleanupScreen(t, func(tui.CleanupPlan) tui.CleanupOutcome {
		return tui.CleanupOutcome{Apply: true, SelectedIDs: []string{"1"}}
	})
	fp := &fakePlanner{plan: cleanupPlanFixture()}
	cf, _ := cleanupParse(f.Ctx, nil)
	if code := cleanupExec(context.Background(), f.Ctx, fp, st, cf, "talkable/talkable"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	req, err := nextPendingRequest(context.Background(), st)
	if err != nil || req.Kind != engine.ReqCleanup {
		t.Fatalf("request = %+v err %v", req, err)
	}
	var p engine.CleanupPayload
	if err := json.Unmarshal(req.Payload, &p); err != nil || p.Plan == nil || len(p.Plan.Actions) != 1 || !p.Confirmed ||
		p.Plan.Actions[0].Slug != "pr27087fix" {
		t.Fatalf("payload = %+v err %v", p, err)
	}
}

func TestCleanupTextPathsSkipTheScreen(t *testing.T) {
	for _, args := range [][]string{{"--yes"}, {"--dry-run"}, {"--json"}} {
		f := newInspFixture(t)
		st := f.store()
		withCleanupScreen(t, nil)
		fp := &fakePlanner{plan: cleanupPlanFixture()}
		cf, _ := cleanupParse(f.Ctx, args)
		if code := cleanupExec(context.Background(), f.Ctx, fp, st, cf, "talkable/talkable"); code != 0 {
			t.Fatalf("%v: code %d err %s", args, code, f.Err.String())
		}
		if args[0] == "--yes" && (!fp.applied || len(fp.gotPlan.Actions) != 2) {
			t.Fatalf("--yes applies the whole plan: %+v", fp.gotPlan)
		}
	}
	// `slots remove` shares the code path but keeps its y/N question.
	f := newInspFixture(t)
	st := f.store()
	withCleanupScreen(t, nil)
	fp := &fakePlanner{plan: cleanupPlanFixture()}
	sf := &cleanupFlags{cmd: "slots remove", remove: true, slot: "review5"}
	if code := cleanupExec(context.Background(), f.Ctx, fp, st, sf, "talkable/talkable"); code != 1 || fp.applied {
		t.Fatalf("slots remove off a confirming terminal: code %d applied %v", code, fp.applied)
	}
}

func TestCleanupPROptionUsesTheRegisteredOwner(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	inspSeedPR(t, st, "zhuravel/widgets", 7, store.PRReviewed, nil)
	cf, _ := cleanupParse(f.Ctx, []string{"--pr", "widgets#7"})
	o, err := cf.options(context.Background(), st, "talkable/talkable")
	if err != nil || o.PR == nil || o.PR.Repo != "zhuravel/widgets" || o.PR.Number != 7 {
		t.Fatalf("options %+v err %v", o.PR, err)
	}
	// Unknown repositories keep the default owner.
	cf, _ = cleanupParse(f.Ctx, []string{"--pr", "gadgets#3"})
	if o, err := cf.options(context.Background(), st, "talkable/talkable"); err != nil || o.PR.Repo != "talkable/gadgets" {
		t.Fatalf("options %+v err %v", o.PR, err)
	}
}
