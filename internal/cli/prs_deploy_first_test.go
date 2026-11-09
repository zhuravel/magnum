package cli

import (
	"encoding/json"
	"maps"
	"slices"
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

// A deploy of the head that magnum runs (live: queued behind another effect
// on the env, or running, the conflict resolver's work included) takes the
// deploy-before-merge line away until the push lands and a staging read says
// pushed: E is already refused ("already deploying"), so the row is no
// needs-you, --needs-me leaves it out, and the JSON has no
// deploy_before_merge and no open.deploy. A deploy of the head that stopped
// for you keeps the line (E resumes it), as does a live deploy of an older
// head.
func TestTheBoardLeavesDeployBeforeMergeOutWhileADeployOfTheHeadRuns(t *testing.T) {
	h := newActHarness(t)
	stagingConfig(h.d.Cfg)
	queued := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	resolving := h.seedPR("talkable/talkable", 6, store.PRReviewed)
	stopped := h.seedPR("talkable/talkable", 7, store.PRReviewed)
	older := h.seedPR("talkable/talkable", 8, store.PRReviewed)
	const migration = "db/migrate/20261109_add_index.rb"
	now := time.Now()
	if err := h.st.SetKV(h.ctx, store.KVDaemonLastTick, store.FormatTime(now)); err != nil {
		t.Fatal(err)
	}
	for _, pr := range []store.PR{queued, resolving, stopped, older} {
		h.setFiles(pr, "zhuravel", &store.ReviewGate{Decision: "APPROVED"}, false, "app/models/metric.rb", migration)
		h.setStagingRecord(pr.ID, engine.StagingRecord{Head: pr.HeadSHA, Envs: []engine.StagingEnv{{Env: "void", State: "none"}, {Env: "bart", State: "none"}}})
		h.setOpenVerdict(pr, engine.OpenVerdict{Verdict: engine.OpenIdle, Why: "nothing open at abc1234 · approved, merge is yours", Head: pr.HeadSHA, At: now})
	}
	at := now.Add(-20 * time.Minute).UTC().Truncate(time.Second)
	deploy := func(pr store.PR, effect, head, step, phase, state string) {
		h.setDeployRecord(engine.StagingDeployRecord{Effect: effect, Repo: "talkable/talkable", Number: pr.Number, Head: head, Env: "void",
			Step: step, Phase: phase, State: state, Seq: 3, At: at})
	}
	deploy(queued, "void-5-20261109t100000-abcd", queued.HeadSHA, "claim", "begin", "queued")
	deploy(resolving, "void-6-20261109t100000-abcd", resolving.HeadSHA, "merge", "begin", "running")
	deploy(stopped, "void-7-20261109t100000-abcd", stopped.HeadSHA, "verify", "fail", "needs you")
	deploy(older, "void-8-20261109t100000-abcd", "0ld0000def5678", "merge", "begin", "running")
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
	for _, n := range []int{5, 6} {
		r := got[n]
		if r.DeployFirst != "" || r.DeployNeedsYou() || len(r.StagingDeploys) != 1 || !r.StagingDeploys[0].Live {
			t.Errorf("#%d deploying its head: deploy first %q, needs you %v, deploys %+v; want no line beside its live deploy",
				n, r.DeployFirst, r.DeployNeedsYou(), r.StagingDeploys)
		}
		if js := prsJSONOf(r); js.DeployBeforeMerge != "" || js.Open == nil || js.Open.Deploy {
			t.Errorf("#%d deploying its head: tui --json deploy_before_merge %q, open %+v", n, js.DeployBeforeMerge, js.Open)
		}
		if s := prsStateCell(r, now); strings.Contains(s, "deploy-first") {
			t.Errorf("#%d deploying its head: printed state %q", n, s)
		}
	}
	const words = "deploy to a staging before the merge (it changes " + migration + ")"
	for _, n := range []int{7, 8} {
		if r := got[n]; r.DeployFirst != words || !r.DeployNeedsYou() {
			t.Errorf("#%d: deploy first %q, needs you %v; want %q and needs you", n, r.DeployFirst, r.DeployNeedsYou(), words)
		}
	}
	if needs := slices.Sorted(maps.Keys(rows(prsOptions{NeedsMe: true}))); !slices.Equal(needs, []int{7, 8}) {
		t.Errorf("--needs-me lists %v, want #7 (its deploy needs you) and #8 (a deploy of an older head) alone", needs)
	}
}
