package factory

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// theMigration is the migration the deploy-first tests' changes add.
const theMigration = "db/migrate/20261109010203_add_metric_rows.rb"

// stagedRig is the screen rig whose repository has theStagings, reading
// magnum's registry (the fake).
func stagedRig(t *testing.T) *screenRig {
	t.Helper()
	r := newScreenRig(t)
	r.cfg.Factory.Repos[0].Staging = theStagings
	r.withMagnum()
	return r
}

// changes writes task id's ready card, the change it pushed at the PR's
// head (prHead), with paths.
func (r *screenRig) changes(id int64, paths ...string) {
	r.t.Helper()
	r.changesAt(id, prHead, paths...)
}

// changesAt writes task id's ready card of head, with paths.
func (r *screenRig) changesAt(id int64, head string, paths ...string) {
	r.t.Helper()
	c := ReadyCardFile{Head: head, Base: strings.Repeat("b", 40)}
	for _, p := range paths {
		c.Files = append(c.Files, CardFile{Path: p, Added: 3, InPlan: true})
	}
	r.card(id, c)
}

// stagingFile writes staging.json as the factory daemon does, task id's
// PR number pr standing in theStagings' order as states say, each seen an
// hour ago.
func (r *screenRig) stagingFile(id int64, pr int, states ...string) {
	r.t.Helper()
	e := stagingScreenTask{PR: pr}
	for i, s := range states {
		e.Cells = append(e.Cells, stagingFileCell{Name: theStagings[i].Name, State: s, Seen: screenNow.Add(-time.Hour)})
	}
	b, err := json.Marshal(stagingScreen{Tasks: map[int64]stagingScreenTask{id: e}})
	if err != nil {
		r.t.Fatal(err)
	}
	p := r.layout.FactoryStaging()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// approvedIdle makes PR v idle and approved: nothing open at its head, a
// person's approval, your merge (the look's verdict).
func approvedIdle(v *PRState) {
	v.HasOpen, v.Approved, v.Open = true, true, verdict(engine.OpenIdle, approvedWhy)
}

const approvedWhy = "nothing open at a1b2c3d · approved, merge is yours"

// A published task whose PR waits on your merge, whose change (the ready
// card it pushed) adds a migration, and whose PR is on no staging (every
// env read, none or outdated) says to deploy it to a staging first: the
// row's and the card's needs-you line start with staging's words, the
// short form is their first clause, the move is E, and the row says the
// deploy comes first (DeployFirst), so the screen offers E first. The look's
// verdict and the row's own words (magnum and a person approved) both say
// it. Before, the row said "merge #4534 on GitHub" for a PR no staging had
// run the migration of (talkable#12203 and #12204).
func TestAPublishedTaskWhoseMigrationIsOnNoStagingDeploysBeforeItsMerge(t *testing.T) {
	words := staging.DeployBeforeMergeWords([]string{theMigration})
	for name, c := range map[string]struct {
		edit func(*PRState)
		why  string
	}{
		"the look's verdict": {approvedIdle, approvedWhy},
		"magnum's row": {func(v *PRState) {
			reviewedAt(v, [4]int{}, store.VerdictClean)
			v.Review, v.ReviewEvent = nil, "APPROVED"
			v.Gate = &store.ReviewGate{Decision: "APPROVED", Opinions: []store.LatestReview{{Login: "rev-ann", State: "APPROVED", CommitSHA: prHead}}}
		}, "magnum approved a1b2c3d · approved by rev-ann, merge is yours"},
	} {
		t.Run(name, func(t *testing.T) {
			r := stagedRig(t)
			task := r.publishedTask("PR-12324", 4534)
			r.changes(task.ID, "app/models/metric.rb", theMigration, "spec/models/metric_spec.rb")
			r.stagingFile(task.ID, 4534, "none", "outdated", "none")
			r.pr(4534, c.edit)
			d := r.data(time.Time{})
			row := rowOf(t, d, "PR-12324")
			want := tui.WaitsOn{Who: tui.WaitsOnYou, What: words + " · " + c.why, Short: "deploy to a staging before the merge", OnYou: true}
			if row.Group != tui.GroupNeedsYou || len(row.WaitsOn) != 1 || row.WaitsOn[0] != want {
				t.Fatalf("the row: %+v, want NEEDS YOU with %+v", row, want)
			}
			if row.Move != "E deploys to a staging" || row.MoveShort != "E deploys it" || !row.DeployFirst {
				t.Errorf("its move %q / %q, deploy first %v: want E's deploy first", row.Move, row.MoveShort, row.DeployFirst)
			}
			if want := "you: " + words + " · " + c.why; row.Help != want {
				t.Errorf("its help %q, want %q", row.Help, want)
			}
			if card := d.Cards[task.ID].Task; card.WaitingFor != "you: "+words+" · "+c.why {
				t.Errorf("its card waits for %q, want staging's words first", card.WaitingFor)
			}
		})
	}
}

// A staging that holds the PR's head (latest, or pushed: CI, CI ✗,
// deploying) or may hold it (unknown, and every cell while no factory
// daemon runs) keeps the merge words: the screen says only what the mirror
// backs.
func TestAStagingThatMayHoldTheHeadKeepsTheMergeWords(t *testing.T) {
	for name, state := range map[string]string{"latest": "latest", "pushed, CI runs": "ci", "pushed, CI failed on it": "ci-fail",
		"deploying": "deploying", "unknown": "unknown"} {
		t.Run(name, func(t *testing.T) {
			r := stagedRig(t)
			task := r.publishedTask("PR-12324", 4534)
			r.changes(task.ID, theMigration)
			r.stagingFile(task.ID, 4534, "none", state, "none")
			r.pr(4534, approvedIdle)
			wantMerge(t, rowOf(t, r.data(time.Time{}), "PR-12324"))
		})
	}
	r := stagedRig(t)
	task := r.publishedTask("PR-12324", 4534)
	r.changes(task.ID, theMigration)
	r.stagingFile(task.ID, 4534, "none", "none", "none")
	r.pr(4534, approvedIdle)
	d, err := ScreenData(r.ctx, r.reg, r.agents, screenNow, time.Time{}, ScreenOptions{Config: r.cfg, Layout: r.layout, Magnum: r.magnum,
		DaemonDown: true})
	if err != nil {
		t.Fatal(err)
	}
	wantMerge(t, rowOf(t, d, "PR-12324"))
}

// wantMerge checks that row waits on your merge of #4534 as before.
func wantMerge(t *testing.T, row tui.FactoryRow) {
	t.Helper()
	want := tui.WaitsOn{Who: tui.WaitsOnYou, What: approvedWhy, Short: "nothing open at a1b2c3d", OnYou: true}
	if row.Group != tui.GroupNeedsYou || len(row.WaitsOn) != 1 || row.WaitsOn[0] != want {
		t.Errorf("the row: %+v, want NEEDS YOU with %+v", row, want)
	}
	if row.Move != "merge #4534 on GitHub" || row.MoveShort != "merge it" || row.DeployFirst {
		t.Errorf("its move %q / %q, deploy first %v: want the merge", row.Move, row.MoveShort, row.DeployFirst)
	}
}

// A change that adds no migration, a task without the ready card it pushed
// (no change known: the screen never runs git for one), and a repository
// without staging envs keep the merge words.
func TestATaskWithoutAMigrationKeepsTheMergeWords(t *testing.T) {
	for name, set := range map[string]func(r *screenRig, id int64){
		"no migration": func(r *screenRig, id int64) {
			r.changes(id, "app/models/metric.rb", "db/schema.rb")
			r.stagingFile(id, 4534, "none", "none", "none")
		},
		"no ready card": func(r *screenRig, id int64) { r.stagingFile(id, 4534, "none", "none", "none") },
		"no staging envs": func(r *screenRig, id int64) {
			r.cfg.Factory.Repos[0].Staging = nil
			r.changes(id, theMigration)
			r.stagingFile(id, 4534, "none", "none", "none")
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := stagedRig(t)
			task := r.publishedTask("PR-12324", 4534)
			set(r, task.ID)
			r.pr(4534, approvedIdle)
			wantMerge(t, rowOf(t, r.data(time.Time{}), "PR-12324"))
		})
	}
}

// Only a wait on your merge puts the deploy first: a PR whose CI failed,
// whose reviewer asked for changes or that no reviewer was asked of keeps
// its own next step, migration and stagings aside.
func TestOnlyTheMergeWaitPutsTheDeployFirst(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(*PRState)
		move string
	}{
		"CI failed": {func(v *PRState) {
			reviewedAt(v, [4]int{}, store.VerdictClean)
			v.CI = &tui.CIInfo{State: "failed", Failed: 1, Required: []tui.CheckState{{Name: "Completion", Label: "Completion", State: "failed"}}}
		}, "fix the CI"},
		"changes asked": {func(v *PRState) {
			reviewedAt(v, [4]int{}, store.VerdictClean)
			v.Gate = &store.ReviewGate{Decision: "CHANGES_REQUESTED", Opinions: []store.LatestReview{{Login: "rev-ann", State: "CHANGES_REQUESTED"}}}
		}, "answer rev-ann"},
		"nobody asked": {func(v *PRState) {
			reviewedAt(v, [4]int{}, store.VerdictClean)
			v.Gate = &store.ReviewGate{Decision: "REVIEW_REQUIRED"}
		}, "ask a reviewer"},
	} {
		t.Run(name, func(t *testing.T) {
			r := stagedRig(t)
			task := r.publishedTask("PR-12324", 4534)
			r.changes(task.ID, theMigration)
			r.stagingFile(task.ID, 4534, "none", "none", "none")
			r.pr(4534, c.edit)
			row := rowOf(t, r.data(time.Time{}), "PR-12324")
			if row.Move != c.move || row.DeployFirst || strings.Contains(row.WaitsOn[0].What, "deploy to a staging") {
				t.Errorf("the row: %+v, want its move %q and no deploy first", row, c.move)
			}
		})
	}
}

// Your own PR that waits on your merge says the same, by magnum's records
// of it: the paths magnum's poller listed at its head and the review
// daemon's staging record (PRState.Files, Stagings). Its row and card now
// show where it stands on each staging env, from that record, so E's
// chooser lists them as the row's line says them. One env that holds the
// head keeps the merge words. Before, your PR's envs read "not read yet".
func TestYourPRWhoseMigrationIsOnNoStagingDeploysBeforeItsMerge(t *testing.T) {
	r := stagedRig(t)
	words := staging.DeployBeforeMergeWords([]string{theMigration})
	envs := func(states ...string) []engine.StagingEnv {
		out := make([]engine.StagingEnv, len(states))
		for i, s := range states {
			out[i] = engine.StagingEnv{Env: theStagings[i].Name, State: s}
		}
		return out
	}
	r.own(91, 4601, func(v *PRState) {
		approvedIdle(v)
		v.Files, v.Stagings = []string{"app/models/metric.rb", theMigration}, envs("none", "outdated", "none")
	})
	r.own(92, 4602, func(v *PRState) {
		approvedIdle(v)
		v.Files, v.Stagings = []string{theMigration}, envs("none", "latest", "none")
	})
	r.own(93, 4603, func(v *PRState) {
		approvedIdle(v)
		v.Files = []string{theMigration}
		v.Stagings = []engine.StagingEnv{{Env: "void", State: "pushed", Detail: string(staging.DetailCIFailedOurs)}, {Env: "bart", State: "none"},
			{Env: "bastion", State: "unknown", Why: "not read yet"}}
	})
	d := r.data(time.Time{})
	first := rowOf(t, d, "#4601")
	want := tui.WaitsOn{Who: tui.WaitsOnYou, What: words + " · " + approvedWhy, Short: "deploy to a staging before the merge", OnYou: true}
	if first.Group != tui.GroupNeedsYou || first.WaitsOn[0] != want || first.Move != "E deploys to a staging" || !first.DeployFirst {
		t.Errorf("your PR on no staging: %+v, want %+v and E's deploy first", first, want)
	}
	if got := cellStates(first.Stagings); !slices.Equal(got, []string{"void=none", "bart=outdated", "bastion=none"}) {
		t.Errorf("its cells %q, want the review daemon's record", got)
	}
	if c := d.Cards[-91].Task; c.WaitingFor != "you: "+words+" · "+approvedWhy || !slices.Equal(cellStates(c.Stagings), cellStates(first.Stagings)) {
		t.Errorf("its card waits for %q with stagings %q", c.WaitingFor, cellStates(c.Stagings))
	}
	held := rowOf(t, d, "#4602")
	if held.Move != "merge #4602 on GitHub" || held.DeployFirst || held.WaitsOn[0].What != approvedWhy {
		t.Errorf("your PR live on bart: %+v, want the merge", held)
	}
	if got := cellStates(rowOf(t, d, "#4603").Stagings); !slices.Equal(got, []string{"void=ci-fail", "bart=none", "bastion=unknown"}) {
		t.Errorf("cells %q, want each env in the screen's words", got)
	}
}

// OwnPRs reads, for each of your PRs, the paths magnum's poller listed at
// its head (pr_files) and, in a repository with staging envs, the review
// daemon's staging record of it, as the board shows it
// (engine.StagingShown): each env of the config in its order, unknown
// where the record says nothing. A list of an older head lists nothing,
// and a record of an older head says every env unknown, as does one
// without magnum's daemon ticking. The registry stays as it was.
func TestOwnPRsReadTheReviewDaemonsStagingRecordAndTheHeadsPaths(t *testing.T) {
	l, st := magnumLayout(t)
	ctx := t.Context()
	now := time.Now()
	head, older := strings.Repeat("ab", 20), strings.Repeat("cd", 20)
	cfg := selfConfig()
	cfg.Factory.Repos = []config.FactoryRepo{{Repo: "example/shop", Base: "master", Staging: theStagings}}
	seven := magnumPR(t, st, 7, head, func(g *store.GitHubPR) {
		g.Files = &store.PRFiles{HeadSHA: head, Paths: []string{"app/a.rb", theMigration}}
	})
	eight := magnumPR(t, st, 8, head, func(g *store.GitHubPR) {
		g.Files = &store.PRFiles{HeadSHA: older, Paths: []string{theMigration}}
	})
	setKV(t, st, store.KVDaemonLastTick, store.FormatTime(now))
	setKV(t, st, engine.KVPRStaging(seven.ID), engine.StagingRecord{Head: head, Envs: []engine.StagingEnv{{Env: "void", State: "none"},
		{Env: "bart", State: "latest", InTip: head}}})
	setKV(t, st, engine.KVPRStaging(eight.ID), engine.StagingRecord{Head: older, Envs: []engine.StagingEnv{{Env: "void", State: "none"}}})
	if _, err := st.DB().Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	before := registryBytes(t, l)
	prs, err := NewMagnum(l, cfg).OwnPRs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 2 {
		t.Fatalf("OwnPRs = %+v, want #7 and #8", prs)
	}
	states := func(v PRState) []string {
		var out []string
		for _, e := range v.Stagings {
			out = append(out, e.Env+"="+e.State)
		}
		return out
	}
	if v := prs[0]; !slices.Equal(v.Files, []string{"app/a.rb", theMigration}) ||
		!slices.Equal(states(v), []string{"void=none", "bart=latest", "bastion=unknown"}) {
		t.Errorf("#7: files %q, stagings %q", v.Files, states(v))
	}
	if v := prs[1]; v.Files != nil || !slices.Equal(states(v), []string{"void=unknown", "bart=unknown", "bastion=unknown"}) {
		t.Errorf("#8, its list and record of an older head: files %q, stagings %q", v.Files, states(v))
	}
	if !bytes.Equal(before, registryBytes(t, l)) {
		t.Fatal("OwnPRs changed magnum's registry")
	}

	setKV(t, st, store.KVDaemonLastTick, store.FormatTime(now.Add(-time.Hour)))
	if prs, err = NewMagnum(l, cfg).OwnPRs(ctx); err != nil || len(prs) != 2 {
		t.Fatalf("OwnPRs = %+v, %v", prs, err)
	}
	if got := states(prs[0]); !slices.Equal(got, []string{"void=unknown", "bart=unknown", "bastion=unknown"}) {
		t.Errorf("#7 with magnum's last tick an hour old: stagings %q, want each unknown", got)
	}
}

// olderHead is a head of the PR before prHead: the one its ready card was
// drawn at before magnum's fixer pushed.
var olderHead = "eeeeeee" + strings.Repeat("0", 33)

// A task's changed paths are those of the PR's head, the head the rest of
// its row and card read: the ready card's when it was drawn at that head,
// else the paths magnum's poller listed at it (PRState.Files), else the
// card's, the only change known; a PR without a ready card (one the factory
// adopted) has the head's list alone. A migration the card names that the
// head's list does not (the fixer removed it) asks no deploy, and one the
// head's list names that the card lacks (the fixer added it) does. Before,
// the card's paths stood at whatever head the PR had.
func TestATasksPathsAreThoseOfThePRsHead(t *testing.T) {
	words := staging.DeployBeforeMergeWords([]string{theMigration})
	for name, c := range map[string]struct {
		cardHead string
		card     []string
		files    []string
		first    bool
	}{
		"a migration the card names and the head's list does not": {olderHead, []string{theMigration}, []string{"app/models/metric.rb"}, false},
		"a migration the head's list names and the card lacks":    {olderHead, []string{"app/models/metric.rb"}, []string{"app/models/metric.rb", theMigration}, true},
		"no list of the head: the card's paths":                   {olderHead, []string{theMigration}, nil, true},
		"no ready card (an adopted PR): the head's list":          {olderHead, nil, []string{theMigration}, true},
		"a card of the head wins over the head's list":            {prHead, []string{theMigration}, []string{"app/models/metric.rb"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := stagedRig(t)
			task := r.publishedTask("PR-12324", 4534)
			if c.card != nil {
				r.changesAt(task.ID, c.cardHead, c.card...)
			}
			r.stagingFile(task.ID, 4534, "none", "none", "none")
			r.pr(4534, func(v *PRState) {
				approvedIdle(v)
				v.Files = c.files
			})
			row := rowOf(t, r.data(time.Time{}), "PR-12324")
			if !c.first {
				wantMerge(t, row)
				return
			}
			if !row.DeployFirst || row.WaitsOn[0].What != words+" · "+approvedWhy {
				t.Errorf("the row: %+v, want the deploy first", row)
			}
		})
	}
}

// The deploy first keeps what the merge move said after it: the optional
// calls magnum's fixer left you, seen at the merge. The short move stays
// E's alone.
func TestTheDeployFirstMoveKeepsTheOptionalCalls(t *testing.T) {
	const link = "https://github.com/example/talkable/pull/4534#discussion_r4302866759"
	r := stagedRig(t)
	task := r.publishedTask("PR-12324", 4534)
	r.changes(task.ID, theMigration)
	r.stagingFile(task.ID, 4534, "none", "none", "none")
	r.pr(4534, func(v *PRState) {
		v.HasOpen, v.Approved, v.Open = true, true, verdict(engine.OpenIdle, approvedWhy+" · 1 optional call: "+link)
		v.Open.Optional = []string{link}
	})
	row := rowOf(t, r.data(time.Time{}), "PR-12324")
	if row.Move != "E deploys to a staging · 1 optional call: "+link || row.MoveShort != "E deploys it" || !row.DeployFirst {
		t.Errorf("its move %q / %q, deploy first %v: want E's with the optional call", row.Move, row.MoveShort, row.DeployFirst)
	}
}

// PRStates and OwnPRs read the paths magnum's poller listed at a PR's head
// only in a repository with staging envs, where the rule can use them: in
// one without, neither the paths nor the staging record are read.
func TestThePathsAreReadOnlyInARepositoryWithStagingEnvs(t *testing.T) {
	l, st := magnumLayout(t)
	ctx := t.Context()
	head := strings.Repeat("ab", 20)
	seven := magnumPR(t, st, 7, head, func(g *store.GitHubPR) {
		g.Files = &store.PRFiles{HeadSHA: head, Paths: []string{theMigration}}
	})
	setKV(t, st, store.KVDaemonLastTick, store.FormatTime(time.Now()))
	setKV(t, st, engine.KVPRStaging(seven.ID), engine.StagingRecord{Head: head, Envs: []engine.StagingEnv{{Env: "void", State: "none"}}})
	ref := PRRef{Repo: "example/shop", Number: 7}
	staged := selfConfig()
	staged.Factory.Repos = []config.FactoryRepo{{Repo: "example/shop", Base: "master", Staging: theStagings}}
	got, err := NewMagnum(l, staged).PRStates(ctx, []PRRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	if v := got[ref]; !slices.Equal(v.Files, []string{theMigration}) || v.Stagings != nil {
		t.Errorf("a task's PR in a repository with envs: files %q, stagings %+v; want the head's paths and no record", v.Files, v.Stagings)
	}
	got, err = NewMagnum(l, selfConfig()).PRStates(ctx, []PRRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	if v := got[ref]; v.Files != nil || v.Stagings != nil {
		t.Errorf("a repository without envs: files %q, stagings %+v; want neither read", v.Files, v.Stagings)
	}
	prs, err := NewMagnum(l, selfConfig()).OwnPRs(ctx)
	if err != nil || len(prs) != 1 || prs[0].Files != nil || prs[0].Stagings != nil {
		t.Errorf("OwnPRs without envs = %+v, %v; want #7 with neither read", prs, err)
	}
}

// A read of a PR's paths that fails (a list magnum's registry cannot
// decode) hides no PR: OwnPRs and PRStates go on, the PR's paths are not
// known (no nudge: the rule finds no migration), and its staging record is
// still read; a record that does not decode reads as none, each env
// unknown. Before, the failure failed OwnPRs, and every one of your PRs
// left home ("your PRs not read").
func TestAFailedReadOfThePathsHidesNoPR(t *testing.T) {
	l, st := magnumLayout(t)
	ctx := t.Context()
	head := strings.Repeat("ab", 20)
	cfg := selfConfig()
	cfg.Factory.Repos = []config.FactoryRepo{{Repo: "example/shop", Base: "master", Staging: theStagings}}
	seven := magnumPR(t, st, 7, head, func(g *store.GitHubPR) {
		g.Files = &store.PRFiles{HeadSHA: head, Paths: []string{theMigration}}
	})
	eight := magnumPR(t, st, 8, head, nil)
	if _, err := st.DB().Exec("UPDATE pr_files SET paths_json = '{not json' WHERE pr_id = ?", seven.ID); err != nil {
		t.Fatal(err)
	}
	setKV(t, st, store.KVDaemonLastTick, store.FormatTime(time.Now()))
	setKV(t, st, engine.KVPRStaging(seven.ID), engine.StagingRecord{Head: head, Envs: []engine.StagingEnv{{Env: "void", State: "none"}}})
	setKV(t, st, engine.KVPRStaging(eight.ID), "{not a record")
	prs, err := NewMagnum(l, cfg).OwnPRs(ctx)
	if err != nil || len(prs) != 2 {
		t.Fatalf("OwnPRs = %+v, %v; want #7 and #8", prs, err)
	}
	states := func(v PRState) []string {
		var out []string
		for _, e := range v.Stagings {
			out = append(out, e.Env+"="+e.State)
		}
		return out
	}
	if v := prs[0]; v.Files != nil || !slices.Equal(states(v), []string{"void=none", "bart=unknown", "bastion=unknown"}) {
		t.Errorf("#7, its paths not read: files %q, stagings %q", v.Files, states(v))
	}
	if v := prs[1]; !slices.Equal(states(v), []string{"void=unknown", "bart=unknown", "bastion=unknown"}) {
		t.Errorf("#8, a record that does not decode: stagings %q", states(v))
	}
	ref := PRRef{Repo: "example/shop", Number: 7}
	if got, err := NewMagnum(l, cfg).PRStates(ctx, []PRRef{ref}); err != nil || !got[ref].Found || got[ref].Files != nil {
		t.Errorf("PRStates = %+v, %v; want #7 found, its paths not known", got[ref], err)
	}
}

// A PR that can never deploy to a staging (its head lives in a fork, or its
// head branch is master, main or a staging branch: the deploy's own
// refusal) gets no deploy-first line, a task's or your own: E is not
// offered where magnum's daemon would refuse it. The row says the merge, as
// before the rule.
func TestAPRThatNeverDeploysToAStagingGetsNoDeployFirstLine(t *testing.T) {
	r := stagedRig(t)
	task := r.publishedTask("PR-12324", 4534)
	r.changes(task.ID, theMigration)
	r.stagingFile(task.ID, 4534, "none", "none", "none")
	r.pr(4534, func(v *PRState) { approvedIdle(v); v.NeverDeploys = true })
	wantMerge(t, rowOf(t, r.data(time.Time{}), "PR-12324"))

	r = stagedRig(t)
	none := []engine.StagingEnv{{Env: "void", State: "none"}, {Env: "bart", State: "none"}, {Env: "bastion", State: "none"}}
	r.own(91, 4601, func(v *PRState) {
		approvedIdle(v)
		v.Files, v.Stagings, v.NeverDeploys = []string{theMigration}, none, true
	})
	r.own(92, 4602, func(v *PRState) { approvedIdle(v); v.Files, v.Stagings = []string{theMigration}, none })
	d := r.data(time.Time{})
	if row := rowOf(t, d, "#4601"); row.DeployFirst || row.Move != "merge #4601 on GitHub" || row.WaitsOn[0].What != approvedWhy {
		t.Errorf("your PR that never deploys: %+v, want the merge and no deploy first", row)
	}
	if row := rowOf(t, d, "#4602"); !row.DeployFirst || row.Move != "E deploys to a staging" {
		t.Errorf("your PR that may deploy: %+v, want E's deploy first", row)
	}
}

// A PR whose staging deploy of its head is live (queued or running) gets no
// deploy-first line while it runs: E would be refused ("already
// deploying") and the push lands by itself, as the board says it. The row
// says the merge meanwhile; the line is back once the deploy is not live.
func TestADeployOfThePRsHeadThatRunsTakesTheDeployFirstAway(t *testing.T) {
	r := stagedRig(t)
	task := r.publishedTask("PR-12324", 4534)
	r.changes(task.ID, theMigration)
	r.stagingFile(task.ID, 4534, "none", "none", "none")
	r.pr(4534, func(v *PRState) { approvedIdle(v); v.DeployingHead = true })
	wantMerge(t, rowOf(t, r.data(time.Time{}), "PR-12324"))

	r = stagedRig(t)
	none := []engine.StagingEnv{{Env: "void", State: "none"}, {Env: "bart", State: "none"}, {Env: "bastion", State: "none"}}
	r.own(91, 4601, func(v *PRState) {
		approvedIdle(v)
		v.Files, v.Stagings, v.DeployingHead = []string{theMigration}, none, true
	})
	if row := rowOf(t, r.data(time.Time{}), "#4601"); row.DeployFirst || row.Move != "merge #4601 on GitHub" || row.WaitsOn[0].What != approvedWhy {
		t.Errorf("your PR deploying its head: %+v, want the merge and no deploy first", row)
	}
}

// PRStates and OwnPRs say from magnum's registry whether a PR can never
// deploy to a staging (PRState.NeverDeploys: a fork's, or a head branch
// master, main or staging-*), as the deploy's refusal does.
func TestPRStatesSayWhichPRsNeverDeployToAStaging(t *testing.T) {
	l, st := magnumLayout(t)
	head := strings.Repeat("ab", 20)
	cfg := selfConfig()
	cfg.Factory.Repos = []config.FactoryRepo{{Repo: "example/shop", Base: "master", Staging: theStagings}}
	for n, c := range map[int]struct {
		ref   string
		fork  bool
		never bool
	}{7: {"feature/index", false, false}, 8: {"feature/index", true, true}, 9: {"staging-void", false, true},
		10: {"master", false, true}, 11: {"main", false, true}} {
		magnumPR(t, st, n, head, func(g *store.GitHubPR) { g.HeadRef, g.IsCrossRepo = new(c.ref), new(c.fork) })
	}
	want := map[int]bool{7: false, 8: true, 9: true, 10: true, 11: true}
	prs, err := NewMagnum(l, cfg).OwnPRs(t.Context())
	if err != nil || len(prs) != len(want) {
		t.Fatalf("OwnPRs = %+v, %v; want %d PRs", prs, err, len(want))
	}
	var refs []PRRef
	for _, v := range prs {
		refs = append(refs, v.Ref)
		if v.NeverDeploys != want[v.Ref.Number] {
			t.Errorf("OwnPRs #%d: never deploys %v, want %v", v.Ref.Number, v.NeverDeploys, want[v.Ref.Number])
		}
	}
	got, err := NewMagnum(l, cfg).PRStates(t.Context(), refs)
	if err != nil {
		t.Fatal(err)
	}
	for ref, v := range got {
		if v.NeverDeploys != want[ref.Number] {
			t.Errorf("PRStates #%d: never deploys %v, want %v", ref.Number, v.NeverDeploys, want[ref.Number])
		}
	}
}

// PRStates and OwnPRs say whether a staging deploy of a PR's head is live
// (PRState.DeployingHead), from the records the board reads (the review
// daemon's staging.deploy.* rows), in a repository with staging envs: a
// deploy queued or running for the head is; one that stopped for you (needs
// you), one that ended, one of an older head and another PR's are not, and
// the line they leave stays. A repository without staging envs reads none.
func TestPRStatesSayWhetherADeployOfThePRsHeadRuns(t *testing.T) {
	l, st := magnumLayout(t)
	head, older := strings.Repeat("ab", 20), strings.Repeat("cd", 20)
	cfg := selfConfig()
	cfg.Factory.Repos = []config.FactoryRepo{{Repo: "example/shop", Base: "master", Staging: theStagings}}
	deploy := func(effect string, n int, head, state string) {
		setKV(t, st, engine.KVStagingDeploy(effect), engine.StagingDeployRecord{Effect: effect, Repo: "example/shop", Number: n, Head: head,
			Env: "void", Step: "merge", Phase: "begin", State: state, Seq: 2, At: time.Now()})
	}
	want := map[int]bool{7: true, 8: true, 9: false, 10: false, 11: false, 12: false}
	for n := range want {
		magnumPR(t, st, n, head, nil)
	}
	deploy("void-7-a", 7, head, "queued")
	deploy("void-8-a", 8, head, "running")
	deploy("void-9-a", 9, head, "needs you")
	deploy("void-10-a", 10, head, "done")
	deploy("void-11-a", 11, older, "running")
	deploy("void-99-a", 99, head, "running") // another PR's: #12 has none
	prs, err := NewMagnum(l, cfg).OwnPRs(t.Context())
	if err != nil || len(prs) != len(want) {
		t.Fatalf("OwnPRs = %+v, %v; want %d PRs", prs, err, len(want))
	}
	var refs []PRRef
	for _, v := range prs {
		refs = append(refs, v.Ref)
		if v.DeployingHead != want[v.Ref.Number] {
			t.Errorf("OwnPRs #%d: deploying its head %v, want %v", v.Ref.Number, v.DeployingHead, want[v.Ref.Number])
		}
	}
	got, err := NewMagnum(l, cfg).PRStates(t.Context(), refs)
	if err != nil {
		t.Fatal(err)
	}
	for ref, v := range got {
		if v.DeployingHead != want[ref.Number] {
			t.Errorf("PRStates #%d: deploying its head %v, want %v", ref.Number, v.DeployingHead, want[ref.Number])
		}
	}
	if got, err = NewMagnum(l, selfConfig()).PRStates(t.Context(), refs); err != nil || got[refs[0]].DeployingHead {
		t.Errorf("a repository without staging envs: %+v, %v; want no deploy read", got[refs[0]], err)
	}
}

// A change of several migrations says the first (byte order) and counts the
// rest, on the row, its help and the card; the short form stays the first
// clause, and your own PR says the same by its list of paths.
func TestSeveralMigrationsAreNamedByTheFirstAndCounted(t *testing.T) {
	const second, third = "db/migrate/20261109010204_backfill_metric_rows.rb", "engines/billing/db/sharded_migrate/20261101_index.rb"
	words := staging.DeployBeforeMergeWords([]string{theMigration, second, third})
	if want := "deploy to a staging before the merge (it changes " + theMigration + " and 2 more migrations)"; words != want {
		t.Fatalf("the rule's words %q, want %q", words, want)
	}
	r := stagedRig(t)
	task := r.publishedTask("PR-12324", 4534)
	r.changes(task.ID, third, "app/models/metric.rb", second, theMigration)
	r.stagingFile(task.ID, 4534, "none", "none", "none")
	r.pr(4534, approvedIdle)
	none := []engine.StagingEnv{{Env: "void", State: "none"}, {Env: "bart", State: "none"}, {Env: "bastion", State: "none"}}
	r.own(91, 4601, func(v *PRState) {
		approvedIdle(v)
		v.Files, v.Stagings = []string{second, theMigration}, none
	})
	d := r.data(time.Time{})
	row := rowOf(t, d, "PR-12324")
	want := tui.WaitsOn{Who: tui.WaitsOnYou, What: words + " · " + approvedWhy, Short: "deploy to a staging before the merge", OnYou: true}
	if len(row.WaitsOn) != 1 || row.WaitsOn[0] != want || row.Help != "you: "+words+" · "+approvedWhy || !row.DeployFirst {
		t.Errorf("the row: %+v, want %+v and its help with the words", row, want)
	}
	if got := d.Cards[task.ID].Task.WaitingFor; got != "you: "+words+" · "+approvedWhy {
		t.Errorf("its card waits for %q, want the words", got)
	}
	own := rowOf(t, d, "#4601")
	if want := "deploy to a staging before the merge (it changes " + theMigration + " and 1 more migration) · " + approvedWhy; own.WaitsOn[0].What != want {
		t.Errorf("your PR waits on %q, want %q", own.WaitsOn[0].What, want)
	}
}
