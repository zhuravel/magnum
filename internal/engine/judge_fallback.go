package engine

// A PR whose judge's kind refused a round goes on without that kind
// (DECISIONS "A refused PR goes on with the fallback judge"): its flag
// names the judge role that took over (CodexFlag.Fallback, from [pipeline]
// judge_fallback or the watch's), and every place that picks a round's
// roles asks rolesForPR, which drops the flagged kind's roles (the judge,
// Codex's own review, any other role of that kind) and puts the fallback
// where the judge was. The fallback's first round finds no conversation to
// resume, so it starts fresh through the recovery path (startRoles), which
// reads the earlier reviews and threads from GitHub. A flag whose fallback
// is no longer a declared judge of another kind holds the PR as a flag
// without one does (flagHolds).

import (
	"context"

	"github.com/zhuravel/magnum/internal/config"
)

// fallbackRole is the judge role f's PR runs with: f.Fallback when it names
// a declared judge, an agent session, of another kind than the one that
// refused. ok is false for a flag without a fallback, one whose role is
// gone from the configuration, and one magnum could not read.
func (e *Engine) fallbackRole(f CodexFlag) (config.Role, bool) {
	if f.Fallback == "" || f.unread {
		return config.Role{}, false
	}
	for _, r := range e.cfg.Judges() {
		if r.Name == f.Fallback && r.IsAgent() && r.AgentKind() != f.Kind {
			return r, true
		}
	}
	return config.Role{}, false
}

// refusalFallback is the judge that takes over the PR after a refusal by
// kind: the watch's judge_fallback when it is of another kind (so a
// refusal by the fallback itself names none, and the PR is held), "" when
// none is set.
func (e *Engine) refusalFallback(w *config.Watch, kind string) string {
	fb, ok := e.cfg.JudgeFallbackFor(w)
	if !ok || !fb.IsAgent() || fb.AgentKind() == kind {
		return ""
	}
	return fb.Name
}

// rolesForPR is the roles a round of the PR runs: the watch's
// (Config.RolesFor), or, for a PR whose flag names a fallback judge, those
// without the flagged kind's roles and with the fallback in the judge's
// place.
func (e *Engine) rolesForPR(ctx context.Context, w *config.Watch, prID int64) []config.Role {
	roles := e.cfg.RolesFor(w)
	f, ok := e.codexFlag(ctx, prID)
	if !ok {
		return roles
	}
	fb, ok := e.fallbackRole(f)
	if !ok {
		return roles
	}
	out := make([]config.Role, 0, len(roles))
	for _, r := range roles {
		switch {
		case r.Judge:
			out = append(out, fb)
		case r.AgentKind() == f.Kind:
			// dropped: the kind that refused never sees the PR again
		default:
			out = append(out, r)
		}
	}
	return out
}

// judgeForPR is the judge a round of the PR runs (rolesForPR); the zero
// Role when the watch has none.
func (e *Engine) judgeForPR(ctx context.Context, w *config.Watch, prID int64) config.Role {
	for _, r := range e.rolesForPR(ctx, w, prID) {
		if r.Judge {
			return r
		}
	}
	return config.Role{}
}
