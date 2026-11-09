package cli

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// stagingConfig gives talkable/talkable the staging envs void and bart.
func stagingConfig(cfg *config.Config) {
	cfg.Factory.Repos = []config.FactoryRepo{{Repo: "talkable/talkable", Staging: []config.FactoryStaging{
		{Name: "void", Branch: "staging-void"}, {Name: "bart", Branch: "staging-bart"}}}}
}

// setStagingRecord stores PR prID's staging record as the daemon writes it.
func (h *actHarness) setStagingRecord(prID int64, rec engine.StagingRecord) {
	h.t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, engine.KVPRStaging(prID), string(b)); err != nil {
		h.t.Fatal(err)
	}
}

// The board's rows carry each open PR's staging status from the daemon's
// record, per env of the config in its order; a PR of a repository without
// staging envs carries none, and a record of an older head reads unknown.
func TestTheBoardReadsEachOpenPRsStagingStatusFromItsRecord(t *testing.T) {
	h := newActHarness(t)
	stagingConfig(h.d.Cfg)
	pushed := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	other := h.seedPR("zhuravel/magnum", 6, store.PRReviewed)
	h.setStagingRecord(pr.ID, engine.StagingRecord{Head: pr.HeadSHA, Envs: []engine.StagingEnv{
		{Env: "bart", State: "pushed", Detail: "ci_running", InTip: pr.HeadSHA, PushedAt: pushed, RunURL: "https://github.com/talkable/talkable/actions/runs/8"},
		{Env: "void", State: "latest", InTip: pr.HeadSHA, PushedAt: pushed, DeployedAt: pushed.Add(20 * time.Minute)},
	}})
	h.setStagingRecord(other.ID, engine.StagingRecord{Head: other.HeadSHA, Envs: []engine.StagingEnv{{Env: "void", State: "latest"}}})
	if err := h.st.SetKV(h.ctx, store.KVDaemonLastTick, store.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	rows := func() map[int]tui.PRBoardRow {
		t.Helper()
		out, err := prsSource(h.st, h.d.Cfg, store.BoardFilter{}, nil, h.d.Layout)(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		by := map[int]tui.PRBoardRow{}
		for _, r := range out {
			by[r.Number] = r
		}
		return by
	}
	got := rows()
	want := []tui.StagingInfo{
		{Env: "void", State: "latest", InTip: pr.HeadSHA, PushedAt: pushed, DeployedAt: pushed.Add(20 * time.Minute)},
		{Env: "bart", State: "pushed", Detail: "ci_running", InTip: pr.HeadSHA, PushedAt: pushed, RunURL: "https://github.com/talkable/talkable/actions/runs/8"},
	}
	if s := got[5].Stagings; len(s) != 2 || s[0].Env != want[0].Env || s[0].State != want[0].State || !s[0].DeployedAt.Equal(want[0].DeployedAt) ||
		s[1].Detail != "ci_running" || s[1].RunURL != want[1].RunURL || !s[1].PushedAt.Equal(pushed) || s[1].InTip != pr.HeadSHA {
		t.Fatalf("#5's stagings %+v, want %+v", s, want)
	}
	if s := got[6].Stagings; s != nil {
		t.Fatalf("a PR of a repository without staging envs carries %+v", s)
	}
	if js := prsJSONOf(got[5]); len(js.Stagings) != 2 || js.Stagings[0].Env != "void" || js.Stagings[1].RunURL == "" {
		t.Fatalf("tui --json stagings %+v", js.Stagings)
	}
	if js := prsJSONOf(got[6]); js.Stagings == nil {
		t.Fatal("tui --json prints null for no stagings, want []")
	}

	h.setStagingRecord(pr.ID, engine.StagingRecord{Head: "0ld0000", Envs: []engine.StagingEnv{{Env: "void", State: "latest"}}})
	if s := rows()[5].Stagings; len(s) != 2 || s[0].State != "unknown" || s[0].Why != "the head moved since the last read" {
		t.Fatalf("a record of an older head: %+v", s)
	}
}

// setDeployRecord stores a staging deploy's record as the daemon writes it.
func (h *actHarness) setDeployRecord(rec engine.StagingDeployRecord) {
	h.t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, engine.KVStagingDeploy(rec.Effect), string(b)); err != nil {
		h.t.Fatal(err)
	}
}

// The board's rows carry each PR's staging deploys from their records, the
// live ones first, read once for every row; a PR of a repository without
// staging envs carries none; tui --json prints them.
func TestTheBoardReadsEachPRsStagingDeploysFromTheirRecords(t *testing.T) {
	h := newActHarness(t)
	stagingConfig(h.d.Cfg)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.seedPR("talkable/talkable", 6, store.PRReviewed)
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	h.setDeployRecord(engine.StagingDeployRecord{Effect: "void-5-20261005t090000-abcd", Repo: "talkable/talkable", Number: 5, Head: pr.HeadSHA,
		Env: "void", Step: "watch", Phase: "fail", State: "failed", Why: "ci.yml failed on the merge", Seq: 9, At: at})
	h.setDeployRecord(engine.StagingDeployRecord{Effect: "bart-5-20261005t100000-abcd", Repo: "talkable/talkable", Number: 5, Head: pr.HeadSHA,
		Env: "bart", Step: "push", Phase: "begin", State: "running", Text: "push of 6b6b6b6", Seq: 4, At: at.Add(50 * time.Minute)})
	out, err := prsSource(h.st, h.d.Cfg, store.BoardFilter{}, nil, h.d.Layout)(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	by := map[int]tui.PRBoardRow{}
	for _, r := range out {
		by[r.Number] = r
	}
	ds := by[5].StagingDeploys
	if len(ds) != 2 || ds[0].Effect != "bart-5-20261005t100000-abcd" || !ds[0].Live || ds[0].Step != "push" || ds[0].Text != "push of 6b6b6b6" ||
		ds[1].Env != "void" || ds[1].Live || ds[1].Why != "ci.yml failed on the merge" || !ds[1].At.Equal(at) {
		t.Fatalf("#5's deploys %+v", ds)
	}
	if ds := by[6].StagingDeploys; len(ds) != 0 {
		t.Fatalf("#6's deploys %+v", ds)
	}
	if js := prsJSONOf(by[5]); len(js.StagingDeploys) != 2 || js.StagingDeploys[0].Effect != "bart-5-20261005t100000-abcd" || !js.StagingDeploys[0].Live {
		t.Fatalf("tui --json staging_deploys %+v", js.StagingDeploys)
	}
	if js := prsJSONOf(by[6]); js.StagingDeploys == nil {
		t.Fatal("tui --json prints null for no deploys, want []")
	}
}
