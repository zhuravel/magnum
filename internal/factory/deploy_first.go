package factory

// A PR that waits on your merge, changes a migration and is on no staging
// needs a staging deploy first (staging.DeployBeforeMerge, which names every
// migration it changes: the first, and how many more): a staging
// deploy is where the migration runs before production (Jenkins migrates
// on it), and the PR template asks for one. Its row and card on home then
// say the deploy first, in staging's words (DeployBeforeMergeWords), and
// their move is E, the staging deploy, offered first
// (tui.FactoryRow.DeployFirst), with the y/N E already asks. A published
// task's changed paths are those of its PR's head: its ready card's when the
// card is of that head, else the paths magnum's poller listed at the head
// (PRState.Files), else the card's (taskDeployFirst); its envs are its row's
// staging cells, as the factory daemon last read them (staging.json). Your
// own PR's are magnum's records of it, read with its row (Magnum.OwnPRs):
// the paths the poller listed at its head and the review daemon's staging
// record. The poller keeps a PR's first 100 paths only, so a migration past
// them gives no nudge. Only a wait on the merge changes: CI,
// a reviewer, findings stay the next step. A PR that can never deploy to a
// staging (PRState.NeverDeploys: a fork's, or a head branch master, main
// or staging-*) gets no line, as on the board: E would be refused, and the
// merge words stay. One whose deploy of its head is live (PRState.
// DeployingHead: queued or running; one that stopped for you is not) is a
// wait on others while it runs (deploying): "deploying to a staging (it
// changes ...)", no move. Nothing here runs git.

import (
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/tui"
)

// The move of a PR whose staging deploy comes before its merge, long and
// short.
const (
	deployMove      = "E deploys to a staging"
	deployMoveShort = "E deploys it"
)

// deployFirst is w, a wait on your merge (w.merge) of a PR whose changed
// files are paths and whose staging status is envs, with the staging
// deploy first when staging.DeployBeforeMerge says the PR needs one: its
// words lead the blocker's (their first clause its short form) and the
// help line, and E is the move, with what the merge move said after it
// (the optional calls magnum's fixer left you; its short form is E's
// alone). Any other wait, and a PR that needs no deploy, is w.
func deployFirst(w prWait, paths []string, envs []staging.EnvStatus) prWait {
	if !w.merge {
		return w
	}
	migrations, needed := staging.DeployBeforeMerge(paths, envs)
	if !needed {
		return w
	}
	words := staging.DeployBeforeMergeWords(migrations)
	first := func(s string) string {
		if s == "" {
			return ""
		}
		return words + " · " + s
	}
	w.on.What, w.on.Short, w.on.Long, w.help = first(w.on.What), firstClause(words), first(w.on.Long), first(w.help)
	move := deployMove
	if _, rest, ok := strings.Cut(w.move, " · "); ok { // what the merge move said after it: the optional calls
		move += " · " + rest
	}
	w.move, w.moveShort, w.deploy = move, deployMoveShort, true
	return w
}

// deploying is w, a wait on your merge (w.merge) while a staging deploy of
// the PR's head runs (queued or running), of a PR whose changed files are
// paths and whose staging status is envs: a wait on others, not on you,
// "deploying to a staging (it changes <the migrations>)" in the rule's
// count words (staging.MigrationWords), with no move, for magnum's daemon
// runs the deploy. The merge is not yours until a staging ran the
// migration, so the row leaves NEEDS YOU as the board's does, and never
// says "merge is yours" meanwhile; E would only say it is already
// deploying. It still waits so when the push has landed (an env reads
// pushed: CI runs, the migration has not), until the deploy is not live:
// done, the staging holds the head and the merge words come back (the rule
// decides again); stopped for you, the deploy-first line comes back with E.
// A PR that changes no migration, or that an env already holds (latest, so
// the migration ran there), keeps w.
func deploying(w prWait, paths []string, envs []staging.EnvStatus) prWait {
	migrations, _ := staging.DeployBeforeMerge(paths, envs)
	if len(migrations) == 0 || slices.ContainsFunc(envs, func(e staging.EnvStatus) bool { return e.State == staging.StateLatest }) {
		return w
	}
	d := waitsWait(waitMagnum, "deploying to a staging ("+staging.MigrationWords(migrations)+")", "deploying to a staging", "")
	d.since = w.since
	return d
}

// deployWait is w, a wait on your merge, as the staging deploy of PR st
// changes it, st's changed files being paths and its staging status envs: a
// PR that can never deploy to a staging (st.NeverDeploys: E would be
// refused) keeps w; while a deploy of its head runs (st.DeployingHead) it is
// a wait on others (deploying); else the deploy comes first where the rule
// says so (deployFirst). The board leaves its line out the same two times.
func deployWait(w prWait, st PRState, paths []string, envs []staging.EnvStatus) prWait {
	switch {
	case !w.merge || st.NeverDeploys:
		return w
	case st.DeployingHead:
		return deploying(w, paths, envs)
	}
	return deployFirst(w, paths, envs)
}

// taskDeployFirst is deployWait on published task v's wait w, whose PR
// magnum's registry tells as st: its row's staging cells (staging.json, as
// stagingCellsOf reads them: none in a repository without envs, each
// unknown while no factory daemon runs) and the paths of the PR's head,
// st.Head, the head the rest of its row and card read: the ready card's
// (ready-card.json, the change you pushed) when it was drawn at that head,
// else the paths magnum's poller listed there (st.Files: magnum's fixer
// may have pushed since), else the card's, the only change known; none
// without either. It reads the card only for a wait on the merge in a
// repository with staging envs, of a PR that can deploy to a staging. Cells
// not written yet (the factory daemon has not read the PR) leave the
// deploy-first line out, but not the wait on a deploy that magnum's
// records say runs.
func (s *screen) taskDeployFirst(v *taskView, st PRState, w prWait) prWait {
	if !w.merge || st.NeverDeploys {
		return w
	}
	cells := stagingCellsOf(s.cfg, s.stagings, v.t, prRef(v).Number, s.down)
	if len(cells) == 0 && !st.DeployingHead {
		return w
	}
	c, err := ReadReadyCard(s.l, v.t.ID)
	var paths []string
	switch {
	case err == nil && c.Head == st.Head, err == nil && st.Files == nil:
		for _, f := range c.Files {
			paths = append(paths, f.Path)
		}
	case st.Files != nil:
		paths = st.Files
	default:
		return w
	}
	return deployWait(w, st, paths, cellEnvs(cells))
}

// cellEnvs are staging cells as internal/staging's statuses, stagingState
// the other way round: latest is latest; CI, CI ✗ and deploying (the head
// on the staging branch) are pushed; outdated and none are themselves; any
// other state is unknown.
func cellEnvs(cells []tui.StagingCell) []staging.EnvStatus {
	out := make([]staging.EnvStatus, len(cells))
	for i, c := range cells {
		out[i] = staging.EnvStatus{Env: staging.Env{Name: c.Name}, State: staging.StateUnknown}
		switch c.State {
		case "latest":
			out[i].State = staging.StateLatest
		case "ci", "ci-fail", "deploying":
			out[i].State = staging.StatePushed
		case "outdated":
			out[i].State = staging.StateOutdated
		case "none":
			out[i].State = staging.StateNone
		}
	}
	return out
}

// recordEnvs are the review daemon's staging record of a PR, as
// engine.StagingShown shows it, as internal/staging's statuses: a state
// word this build does not know is unknown.
func recordEnvs(rec []engine.StagingEnv) []staging.EnvStatus {
	out := make([]staging.EnvStatus, len(rec))
	for i, e := range rec {
		st := staging.State(e.State)
		switch st {
		case staging.StateLatest, staging.StatePushed, staging.StateOutdated, staging.StateNone:
		default:
			st = staging.StateUnknown
		}
		out[i] = staging.EnvStatus{Env: staging.Env{Name: e.Env}, State: st, Detail: staging.Detail(e.Detail)}
	}
	return out
}

// recordCells are the review daemon's staging record of your PR as its
// row's staging cells, in the painter's words (stagingState), with no age:
// the record keeps no time of the read that first saw a state.
func recordCells(rec []engine.StagingEnv) []tui.StagingCell {
	if len(rec) == 0 {
		return nil
	}
	envs := recordEnvs(rec)
	out := make([]tui.StagingCell, len(envs))
	for i, e := range envs {
		out[i] = tui.StagingCell{Name: e.Env.Name, State: stagingState(e)}
	}
	return out
}
