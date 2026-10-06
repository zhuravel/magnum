package cli

// A review round in flight on the board and the dashboard: when it started
// and which of its roles work, have worked or have not started yet, read
// from its runs (the state cell's "simplify · 17m", the card's timeline).

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// roundRunningStates are the PR states whose last_round_started_at is the
// round in flight's start; a claiming PR's is still its previous round's.
var roundRunningStates = []string{store.PRReviewing, store.PRVerifying}

// boardRoundProgress fills the rows' Progress (ids[i] is rows[i]'s PR id):
// one query for the PRs reviewing or verifying and one for their rounds'
// runs, none when no row is. It runs after boardRoundFacts: a role the
// round named (RoundWhy, when its round_start is not older than the round)
// without a run yet is listed as not started.
func boardRoundProgress(ctx context.Context, st *store.Store, cfg *config.Config, ids []int64, rows []tui.PRBoardRow) error {
	n := min(len(ids), len(rows))
	if !slices.ContainsFunc(rows[:n], func(r tui.PRBoardRow) bool { return slices.Contains(roundRunningStates, r.State) }) {
		return nil
	}
	prs, err := st.ListPRs(ctx, store.PRFilter{States: roundRunningStates})
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = config.Defaults()
	}
	progress, err := roundProgressOf(ctx, st, cfg, prs)
	if err != nil {
		return err
	}
	for i := range n {
		g := progress[ids[i]]
		listNamedRoles(cfg, g, rows[i].RoundWhy)
		rows[i].Progress = g
	}
	return nil
}

// listNamedRoles adds to g, as not started, each role w named that has no
// entry of its own in g, the judge last: a judge whose own pass alone has
// runs is one of them, its own pass's entry not standing for its main run.
// Nothing changes when either is nil or w is older than g (the previous
// round's round_start, before this one's was written).
func listNamedRoles(cfg *config.Config, g *tui.RoundProgress, w *tui.RoundWhy) {
	if g == nil || w == nil || w.At.Before(g.StartedAt) {
		return
	}
	for _, role := range w.Roles {
		if !slices.ContainsFunc(g.Roles, func(r tui.RoleProgress) bool { return r.Role == role && !r.OwnPass }) {
			g.Roles = append(g.Roles, newRoleProgress(cfg, role))
		}
	}
	judgeLast(g.Roles)
}

// roundProgressOf is the round each PR of prs runs, by PR id: its start
// (last_round_started_at) and its roles from the round's runs, the runs of
// the PR's highest round not created before that start (the rule
// reviewAttachStart follows: an older run belongs to an earlier round).
// Only a PR in roundRunningStates with a start has one. One query for the
// runs of every PR.
func roundProgressOf(ctx context.Context, st *store.Store, cfg *config.Config, prs []store.PR) (map[int64]*tui.RoundProgress, error) {
	starts := map[int64]time.Time{}
	var ids []int64
	for _, pr := range prs {
		if slices.Contains(roundRunningStates, pr.State) && pr.LastRoundStartedAt != nil {
			starts[pr.ID] = *pr.LastRoundStartedAt
			ids = append(ids, pr.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	byPR, err := st.LatestRoundRuns(ctx, ids...)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]*tui.RoundProgress, len(ids))
	for _, id := range ids {
		start := starts[id]
		runs := slices.DeleteFunc(byPR[id], func(r store.Run) bool { return r.CreatedAt.Before(start) })
		out[id] = &tui.RoundProgress{StartedAt: start, Roles: roleProgress(cfg, runs)}
	}
	return out, nil
}

// roleProgress is the roles of a round's runs (oldest first) in the order
// their first runs were created, the judge last. The judge's own pass (its
// runs of kind own_pass, a model fallback's continuation included) is an
// entry of its own, right before the judge's, so its runs never decide when
// the judge's main run started, works, ended or failed. A role started with
// its earliest prompted run (runStarted), works while one of its runs is
// submitted or working, and otherwise its latest run says when it ended
// and whether it failed.
func roleProgress(cfg *config.Config, runs []store.Run) []tui.RoleProgress {
	var out []tui.RoleProgress
	for _, r := range runs {
		own := r.Kind == store.RunOwnPass
		i := slices.IndexFunc(out, func(p tui.RoleProgress) bool { return p.Role == r.Role && p.OwnPass == own })
		if i < 0 {
			i = len(out)
			p := newRoleProgress(cfg, r.Role)
			p.Judge, p.OwnPass = p.Judge || own, own
			out = append(out, p)
		}
		p := &out[i]
		if at := runStarted(r); !at.IsZero() && (p.Started.IsZero() || at.Before(p.Started)) {
			p.Started = at
		}
		p.Working = p.Working || r.State == store.RunSubmitted || r.State == store.RunWorking
		p.Failed = r.State == store.RunFailed || r.State == store.RunAbandoned
		p.Ended = time.Time{}
		if r.EndedAt != nil {
			p.Ended = *r.EndedAt
		}
	}
	for i := range out {
		if out[i].Working {
			out[i].Ended, out[i].Failed = time.Time{}, false
		}
	}
	judgeLast(out)
	return out
}

// runStarted is when a run was prompted: submitted_at, else when it was
// seen working, else its creation for a run past pending without either;
// zero while it is pending.
func runStarted(r store.Run) time.Time {
	switch {
	case r.SubmittedAt != nil:
		return *r.SubmittedAt
	case r.WorkingSeenAt != nil:
		return *r.WorkingSeenAt
	case r.State == store.RunPending:
		return time.Time{}
	}
	return r.CreatedAt
}

func newRoleProgress(cfg *config.Config, role string) tui.RoleProgress {
	return tui.RoleProgress{Role: role, Label: stageLabel(cfg, role), Judge: actIsJudge(cfg, role)}
}

// judgeLast moves the judge after the other roles, keeping their order, its
// own pass right before its main entry.
func judgeLast(roles []tui.RoleProgress) {
	rank := func(r tui.RoleProgress) int {
		switch {
		case !r.Judge:
			return 0
		case r.OwnPass:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(roles, func(a, b tui.RoleProgress) int { return cmp.Compare(rank(a), rank(b)) })
}

// stageLabel is the short name the board's state cell gives a role while
// it works ("simplify · 17m"): the shortest of its configured name and
// aliases, in runes, the name winning a tie and then the aliases in their
// order (claude-simplify → simplify, codex-review → codex, claude-review →
// claude). Roles are data: whatever a [[role]] is called, its shortest
// name is its label. A role the configuration does not know keeps its
// stored name; nil cfg means the built-in roles. The board calls the judge
// "judge" whatever its names (tui.RoleProgress).
func stageLabel(cfg *config.Config, role string) string {
	if cfg == nil {
		cfg = config.Defaults()
	}
	r, ok := cfg.RoleByNameOrAlias(nil, role)
	if strings.TrimSpace(role) == "" || !ok {
		return role
	}
	label := r.Name
	for _, a := range r.Aliases {
		if utf8.RuneCountInString(a) < utf8.RuneCountInString(label) {
			label = a
		}
	}
	return label
}
