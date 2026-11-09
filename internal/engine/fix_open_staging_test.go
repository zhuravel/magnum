package engine

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/store"
)

// StagingFirst reads the head's own file list only, and staging's rule
// over the envs as a reader shows them: unknown holds nothing back.
func TestStagingFirstIsTheMigrationOfTheHeadsFilesThatNoStagingHolds(t *testing.T) {
	files := store.PRFiles{HeadSHA: fixHead, Paths: []string{"app/a.rb", "db/schema.rb", "db/sharded_migrate/2026_b\x1b.rb"}}
	none := []StagingEnv{{Env: "void", State: string(staging.StateNone)}, {Env: "bart", State: string(staging.StateOutdated)}}
	if got := StagingFirst(files, true, fixHead, none); got != "db/sharded_migrate/2026_b .rb" {
		t.Errorf("StagingFirst = %q, want the migration, its escape a space", got)
	}
	for name, c := range map[string]struct {
		files  store.PRFiles
		listed bool
		head   string
		shown  []StagingEnv
	}{
		"no list":         {files, false, fixHead, none},
		"another head":    {files, true, fixPushed, none},
		"no staging envs": {files, true, fixHead, nil},
		"live on one":     {files, true, fixHead, []StagingEnv{{Env: "void", State: "latest"}, {Env: "bart", State: "none"}}},
		"pushed to one":   {files, true, fixHead, []StagingEnv{{Env: "void", State: "pushed"}, {Env: "bart", State: "none"}}},
		"unknown on one":  {files, true, fixHead, []StagingEnv{{Env: "void", State: "unknown"}, {Env: "bart", State: "none"}}},
		"a word unknown":  {files, true, fixHead, []StagingEnv{{Env: "void", State: "deploying"}, {Env: "bart", State: "none"}}},
		"no migration":    {store.PRFiles{HeadSHA: fixHead, Paths: []string{"app/a.rb", "db/schema.rb", "db/shard_1_schema.rb"}}, true, fixHead, none},
	} {
		if got := StagingFirst(c.files, c.listed, c.head, c.shown); got != "" {
			t.Errorf("%s: StagingFirst = %q, want none", name, got)
		}
	}
}

// The look leaves a migration's staging deploy to the screens: your own
// approved PR with nothing open stays idle in the merge's words, with no
// need, while its head changes a migration that no staging holds. The
// factory screen (factory.deployFirst) and the board
// (tui.PRBoardRow.DeployNeedsYou) put the deploy first on that wait on
// your merge from the registry rows the tick wrote, which say it
// (StagingFirst over pr_files and the staging record); a needs_you here
// would take the factory's E-first move away.
func TestTheLookLeavesAMigrationsStagingDeployToTheScreens(t *testing.T) {
	const m = "db/sharded_migrate/20261105011921_add_metric_indexes.rb"
	h, _ := openHarness(t, findings(map[string]int{}, "COMMENTED"), withStagings(&fakeStaging{}), func(h *harness) {
		h.gh.defaultFiles = []string{"app/models/metric.rb", m, "db/shard_1_schema.rb"}
	})
	h.openOwn(prSpec{head: fixHead, gate: &github.ReviewGate{Decision: "APPROVED"}})
	h.tick() // reviewed clean
	h.pastSettle()
	h.stagingTick()
	h.stagingTick()
	v := h.wantOpen(OpenIdle, "nothing open at a1a1a1a · approved, merge is yours")
	if len(v.Needs) != 0 || strings.Contains(v.Why, "staging") {
		t.Errorf("verdict %q needs %+v, want the merge's words and no need", v.Why, v.Needs)
	}
	pr := h.pr(2)
	files, listed, err := h.st.PRFilesOf(h.ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := h.stagingRecord(2)
	now := h.clock.Now()
	if got := StagingFirst(files, listed, pr.HeadSHA, StagingShown(rec, ok, StagingEnvs(h.cfg, "talkable/talkable"), pr.HeadSHA, now, now)); got != m {
		t.Errorf("the registry's rows say the deploy first for %q, want %q", got, m)
	}
}

// A PR whose head lives in a fork, or whose head branch is master, main or
// a staging branch, never deploys to a staging, as the deploy's refusal
// says; any other branch of the repository may.
func TestStagingNeverDeploysAForkOrAProtectedBranch(t *testing.T) {
	for ref, want := range map[string]bool{"feature/index": false, "master": true, "main": true, "staging-void": true, "stagingish": false} {
		if got := StagingNeverDeploys(store.PR{HeadRef: new(ref)}); got != want {
			t.Errorf("head branch %q: never deploys %v, want %v", ref, got, want)
		}
	}
	if !StagingNeverDeploys(store.PR{HeadRef: new("feature/index"), IsCrossRepo: true}) {
		t.Error("a fork's PR may deploy to a staging")
	}
}
