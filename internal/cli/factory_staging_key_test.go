package cli

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
	"github.com/zhuravel/magnum/internal/tui"
)

// E on the factory screen runs the board's own staging deploy
// (screenActions.StagingPlan, StagingDeploy): the plan is the daemon's dry
// run in magnum's registry, the deploy the request so bound, both answered
// by magnum's daemon. The factory daemon has no part in it: the factory's
// registry is not even made. A screen built without the board's actions
// refuses both, and queues nothing.
func TestTheFactoryScreensStagingDeployIsTheBoards(t *testing.T) {
	h, prs := stagingHarness(t)
	h.pid = 4242
	h.stagingDaemon(prs[5].HeadSHA)
	board := newScreenActions(h.c)
	defer board.Close()
	src := &factoryScreenSource{c: h.c, now: inspNow}
	act := newFactoryActions(h.c, src, board)
	ctx := context.Background()
	const ref = "talkable/talkable#5"

	res, err := act.StagingPlan(ctx, ref, []string{"void", "bart"})
	if err != nil || res.Ask == "" || res.Staging == nil || res.Staging.Head != prs[5].HeadSHA || len(res.Staging.Envs) != 1 ||
		res.Staging.Envs[0] != "void" {
		t.Fatalf("the plan = %+v, %v (%s)", res, err, h.errb.String())
	}
	if res, err = act.StagingDeploy(ctx, ref, *res.Staging); err != nil || res.Ask != "" {
		t.Fatalf("the deploy = %+v, %v", res, err)
	}
	reqs := h.requests()
	if len(reqs) != 2 || reqs[0].Kind != engine.ReqStagingDeploy || reqs[1].Kind != engine.ReqStagingDeploy {
		t.Fatalf("requests %+v, want the plan and the deploy in magnum's registry", reqs)
	}
	if p := actDecode[engine.StagingDeployPayload](t, reqs[0].Payload); !p.DryRun {
		t.Errorf("the plan's request %+v, want a dry run", p)
	}
	if p := actDecode[engine.StagingDeployPayload](t, reqs[1].Payload); p.DryRun || p.Head != prs[5].HeadSHA || len(p.Seen) != 1 {
		t.Errorf("the deploy's request %+v, want the deploy bound to the plan", p)
	}
	if fsx.Exists(h.c.Layout.FactoryDB()) {
		t.Error("E made the factory's registry")
	}

	bare := newFactoryActions(h.c, src, nil)
	if _, err := bare.StagingPlan(ctx, ref, []string{"void"}); !errors.Is(err, errNoBoard) {
		t.Errorf("the plan without the board's actions: %v, want %v", err, errNoBoard)
	}
	if _, err := bare.StagingDeploy(ctx, ref, tui.StagingAsked{Envs: []string{"void"}, Head: prs[5].HeadSHA}); !errors.Is(err, errNoBoard) {
		t.Errorf("the deploy without the board's actions: %v, want %v", err, errNoBoard)
	}
	if n := len(h.requests()); n != 2 {
		t.Errorf("%d requests after the refusals, want 2", n)
	}
}

// The screen's rows carry their PRs' staging deploys from the records the
// board reads (engine.KVStagingDeploys in magnum's registry), the live ones
// first: one read of every record per frame (prFacts reads them once,
// before its loop over the rows), each row given its own PR's alone. A
// record of a PR no row has shows nowhere.
func TestTheFactoryScreenReadsItsPRsStagingDeploys(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	cfg := *dt.ctx.Config
	stagingConfig(&cfg)
	cfg.Watches = []config.Watch{{Owner: "talkable", Include: []string{"talkable"}, Identity: "zhuravel"}} // a staging repo is a watched one
	dt.ctx.Config = &cfg
	ctx := context.Background()
	st := storetest.OpenFile(t, dt.ctx.Layout.DB())
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	head := "abcdef1234567890abcdef1234567890abcdef12"
	for _, rec := range []engine.StagingDeployRecord{
		{Effect: "void-5-20261103t090000-abcd", Repo: "talkable/talkable", Number: 5, Head: head, Env: "void", Step: "watch", Phase: "fail",
			State: "failed", Why: "ci.yml failed on the merge", Seq: 9, At: at},
		{Effect: "bart-5-20261103t100000-abcd", Repo: "talkable/talkable", Number: 5, Head: head, Env: "bart", Step: "push", Phase: "begin",
			State: "running", Text: "push of 6b6b6b6", Seq: 4, At: at.Add(50 * time.Minute)},
		{Effect: "void-7-20261103t100000-abcd", Repo: "talkable/talkable", Number: 7, Head: head, Env: "void", Step: "push", Phase: "begin",
			State: "running", Seq: 2, At: at},
	} {
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetKV(ctx, engine.KVStagingDeploy(rec.Effect), string(b)); err != nil {
			t.Fatal(err)
		}
	}
	own := func(n int, id int64) factory.PRState {
		return factory.PRState{Ref: factory.PRRef{Repo: "talkable/talkable", Number: n}, ID: id, Title: "a PR", Found: true, DaemonUp: true,
			Watched: true, GHState: store.GHOpen, State: store.PRReviewed, AuthorLogin: "zhuravel"}
	}
	src := &factoryScreenSource{c: dt.ctx, now: time.Now, magnum: ownOnlyMagnum{own: []factory.PRState{own(5, 15), own(6, 16)}}}
	defer src.close()
	d, err := src.Frame(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deploys := map[string][]tui.StagingDeploy{}
	for _, r := range d.Rows {
		if r.PR != nil {
			deploys[r.PR.Ref] = r.PR.StagingDeploys
		}
	}
	if len(deploys) != 2 {
		t.Fatalf("rows with a PR %v, want #5 and #6", deploys)
	}
	ds := deploys["talkable/talkable#5"]
	if len(ds) != 2 || ds[0].Effect != "bart-5-20261103t100000-abcd" || !ds[0].Live || ds[0].Text != "push of 6b6b6b6" ||
		ds[1].Effect != "void-5-20261103t090000-abcd" || ds[1].Live || ds[1].Why != "ci.yml failed on the merge" || !ds[1].At.Equal(at) {
		t.Errorf("#5's deploys %+v, want the live one to bart, then the failed one to void", ds)
	}
	if ds := deploys["talkable/talkable#6"]; len(ds) != 0 {
		t.Errorf("#6's deploys %+v, want none: the records are #5's and #7's", ds)
	}
}

// The screen's rows carry the staging envs the config names for their
// PRs' repositories (tui.FactoryPR.StagingEnvs, config.Stagings), which E's
// chooser lists on a row without staging cells: your own PR's in a staging
// repository, in the config's order, and none for a repository without
// any. The config is all it reads: without magnum's registry the rows
// still carry them.
func TestTheFactoryScreenGivesItsPRsTheConfigsStagingEnvs(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	cfg := *dt.ctx.Config
	stagingConfig(&cfg)
	dt.ctx.Config = &cfg
	own := func(repo string, n int, id int64) factory.PRState {
		return factory.PRState{Ref: factory.PRRef{Repo: repo, Number: n}, ID: id, Title: "a PR", Found: true, DaemonUp: true,
			Watched: true, GHState: store.GHOpen, State: store.PRReviewed, AuthorLogin: "zhuravel"}
	}
	src := &factoryScreenSource{c: dt.ctx, now: time.Now, magnum: ownOnlyMagnum{own: []factory.PRState{own("talkable/talkable", 5, 15),
		own("example/shop", 6, 16)}}}
	defer src.close()
	d, err := src.Frame(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	envs := map[string][]string{}
	for _, r := range d.Rows {
		if r.PR != nil {
			envs[r.PR.Ref] = r.PR.StagingEnvs
		}
	}
	if len(envs) != 2 {
		t.Fatalf("rows with a PR %v, want #5 and #6", envs)
	}
	if got := envs["talkable/talkable#5"]; !slices.Equal(got, []string{"void", "bart"}) {
		t.Errorf("#5's staging envs %q, want the config's void and bart", got)
	}
	if got := envs["example/shop#6"]; got != nil {
		t.Errorf("#6's staging envs %q, want none: its repository has none", got)
	}
	if fsx.Exists(dt.ctx.Layout.DB()) {
		t.Error("the frame made magnum's registry")
	}
}
