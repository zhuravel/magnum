package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// setFiles makes author pr's author, gate its review gate and fork
// whether its head lives in a fork, and stores its file list at its head,
// as the poll's Details read writes them (pr_files).
func (h *actHarness) setFiles(pr store.PR, author string, gate *store.ReviewGate, fork bool, paths ...string) {
	h.t.Helper()
	if _, err := h.st.UpsertPRFromGitHub(h.ctx, store.GitHubPR{RepoID: pr.RepoID, NodeID: pr.NodeID, Number: pr.Number, URL: pr.URL,
		HeadSHA: pr.HeadSHA, AuthorLogin: new(author), GHState: store.GHOpen, ReviewGate: gate, IsCrossRepo: new(fork),
		Files: &store.PRFiles{HeadSHA: pr.HeadSHA, Paths: paths}}); err != nil {
		h.t.Fatal(err)
	}
}

// setOpenVerdict stores the look's verdict on pr as the engine writes it.
func (h *actHarness) setOpenVerdict(pr store.PR, v engine.OpenVerdict) {
	h.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, engine.KVPROpen(pr.ID), string(b)); err != nil {
		h.t.Fatal(err)
	}
}

// The board says a PR must go to a staging before its merge from the
// registry alone: the head's file list (pr_files) names a migration and
// every env of its staging record holds none of the head. Your own PR whose
// fresh verdict leaves the merge to you (the look's idle verdict, as the
// engine writes it: approved, or a base that requires no review) needs you
// for the deploy (DeployNeedsYou: --needs-me lists it, its printed state
// says deploy-first, the JSON says it); your PR with no approval yet and a
// colleague's carry the same words, never a needs-you. A PR whose head
// lives in a fork, which never deploys to a staging, has no line. A push to
// one env takes the line away on the next read, as does a record of another
// head; a repository without staging envs has none.
func TestTheBoardSaysDeployBeforeMergeForAMigrationNoStagingHolds(t *testing.T) {
	h := newActHarness(t)
	stagingConfig(h.d.Cfg)
	mine := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	theirs := h.seedPR("talkable/talkable", 6, store.PRReviewed)
	elsewhere := h.seedPR("zhuravel/magnum", 7, store.PRReviewed)
	waiting := h.seedPR("talkable/talkable", 8, store.PRReviewed)
	fork := h.seedPR("talkable/talkable", 9, store.PRReviewed)
	free := h.seedPR("talkable/talkable", 10, store.PRReviewed)
	const migration = "db/sharded_migrate/20261105011921_add_metric_indexes_to_custom_activities.rb"
	approved := &store.ReviewGate{Decision: "APPROVED"}
	h.setFiles(mine, "zhuravel", approved, false, "app/models/metric.rb", migration, "db/shard_1_schema.rb")
	h.setFiles(theirs, "alice", approved, false, "db/migrate/20261109_add_index.rb")
	h.setFiles(elsewhere, "zhuravel", approved, false, "db/migrate/20261109_add_index.rb")
	h.setFiles(waiting, "zhuravel", &store.ReviewGate{Decision: "REVIEW_REQUIRED"}, false, "db/migrate/20261109_add_index.rb")
	h.setFiles(fork, "zhuravel", approved, true, "db/migrate/20261109_add_index.rb")
	h.setFiles(free, "zhuravel", &store.ReviewGate{}, false, "db/migrate/20261109_add_index.rb")
	none := func(head string) engine.StagingRecord {
		return engine.StagingRecord{Head: head, Envs: []engine.StagingEnv{{Env: "void", State: "none"}, {Env: "bart", State: "outdated", InTip: "0ld0000"}}}
	}
	for _, pr := range []store.PR{mine, theirs, waiting, fork, free} {
		h.setStagingRecord(pr.ID, none(pr.HeadSHA))
	}
	now := time.Now()
	if err := h.st.SetKV(h.ctx, store.KVDaemonLastTick, store.FormatTime(now)); err != nil {
		t.Fatal(err)
	}
	const words = "deploy to a staging before the merge (it changes " + migration + ")"
	idle := func(pr store.PR, why string) {
		h.setOpenVerdict(pr, engine.OpenVerdict{Verdict: engine.OpenIdle, Why: "nothing open at abc1234 · " + why, Head: pr.HeadSHA, At: now})
	}
	idle(mine, "approved, merge is yours")
	idle(waiting, "no approval yet")
	idle(free, "no review required, merge is yours")
	h.setOpenVerdict(fork, engine.OpenVerdict{Verdict: engine.OpenOff, Why: "its head lives in a fork", Head: fork.HeadSHA, At: now, Off: engine.OpenOffFork})
	rows := func(o prsOptions) map[int]tui.PRBoardRow {
		t.Helper()
		out, err := o.source(prsSource(h.st, h.d.Cfg, store.BoardFilter{}, []string{"zhuravel"}, h.d.Layout))(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		by := map[int]tui.PRBoardRow{}
		for _, r := range out {
			by[r.Number] = r
		}
		return by
	}
	got := rows(prsOptions{})
	if r := got[5]; r.DeployFirst != words || !r.DeployNeedsYou() || r.Open == nil || !r.Open.Merge {
		t.Errorf("your #5: deploy first %q, needs you %v, verdict %+v; want %q and needs you", r.DeployFirst, r.DeployNeedsYou(), r.Open, words)
	}
	const theirWords = "deploy to a staging before the merge (it changes db/migrate/20261109_add_index.rb)"
	if r := got[10]; r.DeployFirst != theirWords || !r.DeployNeedsYou() {
		t.Errorf("your #10 on a base that requires no review: deploy first %q, needs you %v; want both", r.DeployFirst, r.DeployNeedsYou())
	}
	for _, n := range []int{6, 8} {
		if r := got[n]; r.DeployFirst != theirWords || r.DeployNeedsYou() {
			t.Errorf("#%d: deploy first %q, needs you %v; want the words and no needs-you", n, r.DeployFirst, r.DeployNeedsYou())
		}
	}
	if r := got[7]; r.DeployFirst != "" {
		t.Errorf("#7 of a repository without staging envs: %q", r.DeployFirst)
	}
	if r := got[9]; r.DeployFirst != "" || r.DeployNeedsYou() || len(r.Stagings) == 0 {
		t.Errorf("your #9 from a fork: deploy first %q (needs you %v), stagings %v; want no line and its stagings", r.DeployFirst, r.DeployNeedsYou(), r.Stagings)
	}
	js := prsJSONOf(got[5])
	if js.DeployBeforeMerge != words || js.Open == nil || !js.Open.Deploy {
		t.Errorf("tui --json: deploy_before_merge %q, open %+v", js.DeployBeforeMerge, js.Open)
	}
	b, err := json.Marshal(js)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, `"deploy_before_merge":"`+words+`"`) || !strings.Contains(s, `"deploy":true`) {
		t.Errorf("tui --json lacks deploy_before_merge or open.deploy: %s", s)
	}
	if js := prsJSONOf(got[8]); js.Open == nil || js.Open.Deploy {
		t.Errorf("your unapproved #8's tui --json: open %+v, want no deploy", js.Open)
	}
	if js := prsJSONOf(got[6]); js.DeployBeforeMerge != theirWords || js.Open != nil {
		t.Errorf("a colleague's tui --json: deploy_before_merge %q, open %+v", js.DeployBeforeMerge, js.Open)
	}
	if s := prsStateCell(got[5], now); !strings.Contains(s, ",deploy-first") {
		t.Errorf("your #5's printed state %q lacks deploy-first", s)
	}
	for _, n := range []int{6, 8} {
		if s := prsStateCell(got[n], now); strings.Contains(s, "deploy-first") || strings.Contains(s, "needs-you") {
			t.Errorf("#%d's printed state %q needs you", n, s)
		}
	}
	if needs := rows(prsOptions{NeedsMe: true}); len(needs) != 2 || needs[5].Number != 5 || needs[10].Number != 10 {
		t.Errorf("--needs-me lists %v, want your #5 and #10 alone", needs)
	}

	h.setStagingRecord(mine.ID, engine.StagingRecord{Head: mine.HeadSHA, Envs: []engine.StagingEnv{
		{Env: "void", State: "pushed", Detail: "ci_running", InTip: mine.HeadSHA}, {Env: "bart", State: "none"}}})
	h.setStagingRecord(theirs.ID, none("0ther000"))
	h.setStagingRecord(free.ID, none("0ther000"))
	got = rows(prsOptions{})
	if r := got[5]; r.DeployFirst != "" || r.DeployNeedsYou() {
		t.Errorf("pushed to void: #5 still says %q (needs you %v)", r.DeployFirst, r.DeployNeedsYou())
	}
	if r := got[6]; r.DeployFirst != "" {
		t.Errorf("a record of another head: #6 says %q", r.DeployFirst)
	}
	if needs := rows(prsOptions{NeedsMe: true}); len(needs) != 0 {
		t.Errorf("--needs-me lists %v once void holds the head", needs)
	}
}
