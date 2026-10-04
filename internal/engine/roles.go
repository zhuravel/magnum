package engine

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// The review pipeline is configuration ([[role]], [kinds.*]): the engine
// lays out and starts what a round's roles need (pipeline.RolesToRun), and
// pauses, backoffs and preflights are keyed by agent kind name (codex,
// claude, droid, omp, any [kinds.<name>]).

// kinds lists the agent kinds that can be paused and preflighted: every
// declared [kinds.<name>] (config.KindNames, sorted).
func (e *Engine) kinds() []string { return e.cfg.KindNames() }

// isKind reports whether name is a declared agent kind.
func (e *Engine) isKind(name string) bool { return slices.Contains(e.kinds(), name) }

// agentKinds lists the distinct agent kinds of roles (config.Role.AgentKind,
// "" and "shell" left out), in order.
func agentKinds(roles []config.Role) []string {
	var out []string
	for _, r := range roles {
		if k := r.AgentKind(); k != "" && k != config.KindShell && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// isJudge reports whether role (a sessions.role or runs.role value) names a
// configured judge.
func (e *Engine) isJudge(role string) bool {
	return slices.ContainsFunc(e.cfg.RolesFor(nil), func(r config.Role) bool { return r.Judge && r.Name == role })
}

// judgeOf is the name of the judge of the watch covering the PR (the first
// configured judge when the PR or its watch is gone).
func (e *Engine) judgeOf(ctx context.Context, prID int64) string {
	var w *config.Watch
	if pr, err := e.st.PRByID(ctx, prID); err == nil {
		if repo, err := e.st.RepoByID(ctx, pr.RepoID); err == nil {
			w = e.cfg.WatchFor(repo.FullName())
		}
	}
	if j := e.cfg.JudgeFor(w); j.Name != "" {
		return j.Name
	}
	return store.RoleJudge
}

// kvPRRequested holds the role names (a JSON list) requested for the PR's
// next round (store.KVPRRoles).
func kvPRRequested(id int64) string { return store.KVPRRoles(id) }

// requestedRoles reads the roles requested for the PR's next round. A
// "simplify" request an older daemon stored (kvPRSimplify) counts as the
// role answering to "simplify".
func (e *Engine) requestedRoles(ctx context.Context, prID int64) []string {
	var out []string
	if v, ok := e.getKV(ctx, kvPRRequested(prID)); ok && v != "" {
		if err := json.Unmarshal([]byte(v), &out); err != nil {
			e.log.Warn("requested roles", "pr", prID, "value", v, "err", err)
		}
	}
	if v, _ := e.getKV(ctx, kvPRSimplify(prID)); v == "1" && !slices.Contains(out, "simplify") {
		out = append(out, "simplify")
	}
	return out
}

// addRequested adds role names to the PR's requested roles.
func (e *Engine) addRequested(ctx context.Context, prID int64, names ...string) {
	if len(names) == 0 {
		return
	}
	cur := e.requestedRoles(ctx, prID)
	for _, n := range names {
		if !slices.Contains(cur, n) {
			cur = append(cur, n)
		}
	}
	b, err := json.Marshal(cur)
	if err != nil {
		e.log.Warn("requested roles", "pr", prID, "err", err)
		return
	}
	e.setKV(ctx, kvPRRequested(prID), string(b))
}

// clearRequested forgets the PR's requested roles.
func (e *Engine) clearRequested(ctx context.Context, prID int64) {
	e.delKV(ctx, kvPRRequested(prID), kvPRSimplify(prID))
}
