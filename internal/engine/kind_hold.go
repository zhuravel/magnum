package engine

// A launch failure shared across PRs holds its agent kind (DECISIONS "A
// launch failure shared across PRs holds its kind until a probe launches
// it", which reverses "Rejected: pausing a kind on a shared failure"): on
// 2026-11-05 codex-judge launches died at the TUI bootstrap for two hours,
// and every round that needed one died with them while claude-judge, the
// judge_fallback, launched fine. When two or more PRs' agents of one kind
// end with the same pane line (noteSharedFailures), the kind is held for
// kindHoldSpan: a round that needs an agent of the kind runs its watch's
// judge_fallback in the judge's place and without the kind's agent roles,
// or waits when there is none. Once the span is over, the next round that
// needs the kind runs it, the probe; each hold ends at its first good
// probe, and a probe whose launch dies on the hold's line holds the kind
// for another span (one that dies on a line of its own is its PR's
// failure, charged as such). `magnum resume --tool <kind>` lifts it at once.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// kindHoldSpan is how long a shared launch failure holds its kind before a
// round probes it, and how much longer a failed probe holds it.
const kindHoldSpan = 15 * time.Minute

// Kind hold events, under tool:<kind>: held (warn: the kind, the PRs and
// until when), a round probes it (info), a probe failed and the hold goes on
// (warn), the hold lifted (info: a good probe, with a toast, or `magnum
// resume --tool`).
const (
	EventKindHeld        = "kind.held"
	EventKindHoldProbe   = "kind.hold_probe"
	EventKindHoldRenewed = "kind.hold_renewed"
	EventKindHoldLifted  = "kind.hold_lifted"
)

// KVKindHold holds kind's hold (KindHold, JSON) while it lasts.
func KVKindHold(kind string) string { return "kind." + kind + ".shared_hold" }

// KVKindHoldLifted holds when kind's last hold was lifted: only launch
// lines after it hold the kind again, so the lines that held it, still
// within sharedFailureWindow, do not hold it again at once.
func KVKindHoldLifted(kind string) string { return "kind." + kind + ".shared_hold_lifted_at" }

// KindHold is one kind's hold: the pane line its launches ended with, how
// many PRs shared it, since when it holds and until when (then a round may
// probe), its failed probes, and the PR whose round probes it now (0: none)
// with when that round claimed it.
type KindHold struct {
	Line    string    `json:"line"`
	PRs     int       `json:"prs"`
	Since   time.Time `json:"since"`
	Until   time.Time `json:"until"`
	Probes  int       `json:"probes,omitempty"`
	ProbePR int64     `json:"probe_pr,omitempty"`
	ProbeAt time.Time `json:"probe_at,omitzero"`
}

// kindHoldOf is kind's hold (false: none, or unreadable).
func (e *Engine) kindHoldOf(ctx context.Context, kind string) (KindHold, bool) {
	h, ok, err := readKVJSON[KindHold](ctx, e, KVKindHold(kind), "kind hold")
	if err != nil {
		e.warnUnlessStopped(ctx, err, "read the kind's hold", "kind", kind)
	}
	return h, ok
}

// saveKindHold records kind's hold.
func (e *Engine) saveKindHold(ctx context.Context, kind string, h KindHold) {
	if e.d.DryRun {
		return
	}
	if err := writeKVJSON(ctx, e, KVKindHold(kind), "kind hold", h); err != nil {
		e.warnUnlessStopped(ctx, err, "record the kind's hold", "kind", kind)
	}
}

// probing reports whether h's probe is in flight: a PR claimed it and its
// round still runs.
func (e *Engine) probing(h KindHold) bool { return h.ProbePR != 0 && e.roundActive(h.ProbePR) }

// kindHeldFor reports whether kind is held for a round of the PR prID: its
// hold's span runs, or a round of another PR probes it. Once the span is
// over, the PR whose round probes it, and any PR while no round does, may
// run the kind.
func (e *Engine) kindHeldFor(ctx context.Context, kind string, prID int64) (KindHold, bool) {
	h, ok := e.kindHoldOf(ctx, kind)
	if !ok {
		return h, false
	}
	return h, e.now().Before(h.Until) || (h.ProbePR != prID && e.probing(h))
}

// heldRoles is roles for a round of the PR prID of watch w under the holds
// of their agent kinds (kindHeldFor): an agent role of a held kind is left
// out, and a held judge is replaced by the watch's judge_fallback when it
// is of a kind no hold holds. A held judge without such a fallback keeps
// roles as they are: dispatch waits the hold out (kindHoldGate).
func (e *Engine) heldRoles(ctx context.Context, w *config.Watch, prID int64, roles []config.Role) []config.Role {
	held, some := map[string]bool{}, false
	for _, k := range agentRoleKinds(roles) {
		_, held[k] = e.kindHeldFor(ctx, k, prID)
		some = some || held[k]
	}
	if !some {
		return roles
	}
	var fallback *config.Role
	for _, r := range roles {
		if r.Judge && r.IsAgent() && held[r.AgentKind()] {
			fb, ok := e.fallbackRole(CodexFlag{Kind: r.AgentKind(), Fallback: e.refusalFallback(w, r.AgentKind())})
			if !ok {
				return roles
			}
			if _, h := e.kindHeldFor(ctx, fb.AgentKind(), prID); h {
				return roles
			}
			fallback = &fb
		}
	}
	out := make([]config.Role, 0, len(roles))
	for _, r := range roles {
		switch {
		case r.Judge && fallback != nil:
			out = append(out, *fallback)
		case r.IsAgent() && held[r.AgentKind()]:
			// left out while its kind is held
		default:
			out = append(out, r)
		}
	}
	return out
}

// kindHoldGate is why a round of the PR prID that runs roles waits for a
// kind's hold: the first agent kind of roles held for it (heldRoles left
// the roles of a held kind in only for a judge without a fallback),
// "codex held (shared launch failure) until 14:00"; "" = none.
func (e *Engine) kindHoldGate(ctx context.Context, roles []config.Role, prID int64) (kind, why string) {
	for _, k := range agentRoleKinds(roles) {
		h, held := e.kindHeldFor(ctx, k, prID)
		if !held {
			continue
		}
		if !e.now().Before(h.Until) {
			return k, fmt.Sprintf("%s held (shared launch failure): another PR's round probes it", k)
		}
		return k, fmt.Sprintf("%s held (shared launch failure) until %s", k, h.Until.Local().Format("15:04"))
	}
	return "", ""
}

// agentRoleKinds lists the distinct agent kinds of roles' agent sessions
// (shell roles left out), in order.
func agentRoleKinds(roles []config.Role) []string {
	var out []string
	for _, r := range roles {
		if k := r.AgentKind(); r.IsAgent() && k != "" && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// claimProbes makes the round dispatch starts for pr, which runs roles,
// the probe of each held kind of roles whose span is over and that no
// round probes yet (kindHeldFor let it run the kind), with one
// kind.hold_probe event each.
func (e *Engine) claimProbes(ctx context.Context, roles []config.Role, pr store.PR, label string) {
	for _, k := range agentRoleKinds(roles) {
		h, ok := e.kindHoldOf(ctx, k)
		if !ok || e.now().Before(h.Until) || h.ProbePR == pr.ID || e.probing(h) {
			continue
		}
		h.ProbePR, h.ProbeAt = pr.ID, e.now()
		e.saveKindHold(ctx, k, h)
		e.event(ctx, "info", "tool:"+k, EventKindHoldProbe,
			fmt.Sprintf("%s's round probes %s, held since %s: the hold ends if its %s agent takes its prompt", label, k, h.Since.Local().Format("15:04"), k),
			map[string]any{"pr": pr.ID, "probes": h.Probes})
	}
}

// holdKind holds kind for kindHoldSpan after its agents' launches on prs
// PRs (labels) ended with line, unless a hold of it runs: one kind.held
// warning, and a toast when toast (a line agents.shared_failure named
// already today, whose own toast does not come again).
func (e *Engine) holdKind(ctx context.Context, kind, line string, labels []string, toast bool) {
	if _, ok := e.kindHoldOf(ctx, kind); ok || e.d.DryRun {
		return
	}
	now := e.now()
	h := KindHold{Line: line, PRs: len(labels), Since: now, Until: now.Add(kindHoldSpan)}
	e.saveKindHold(ctx, kind, h)
	e.event(ctx, "warn", "tool:"+kind, EventKindHeld,
		fmt.Sprintf("%s held for %s: its launches end the same way on %d PRs (%s): rounds that need it run their judge_fallback, or wait; "+
			"the first round after %s probes it", kind, humanDuration(kindHoldSpan), len(labels), strings.Join(labels, ", "), h.Until.Local().Format("15:04")),
		map[string]any{"last_line": line, "prs": len(labels), "until": store.FormatTime(h.Until)})
	if toast {
		e.urgent("kind-hold:"+kind, "magnum: "+kind+" held for "+humanDuration(kindHoldSpan),
			fmt.Sprintf("Its launches end the same way on %d PRs: %s. %s.",
				len(labels), textx.Clip(line, sharedToastRunes), heldToastWords(kind)), kindHoldSpan)
	}
}

// heldToastWords is what a toast says of kind's hold: "codex is held:
// rounds that need it run their judge_fallback, or wait, until a probe
// launches it". A round whose judge has no fallback of another kind waits,
// so the words promise no fallback judge.
func heldToastWords(kind string) string {
	return kind + " is held: rounds that need it run their judge_fallback, or wait, until a probe launches it"
}

// heldNote is a plain resume's words for kind's hold, which it leaves
// (stillPausedNote): "; codex is still held (shared launch failure) until
// 14:00: `magnum resume --tool codex` lifts it", "until a probe launches
// it" once the span is over; "" when it is not held.
func (e *Engine) heldNote(ctx context.Context, kind string) string {
	h, ok := e.kindHoldOf(ctx, kind)
	if !ok {
		return ""
	}
	until := "a probe launches it"
	if e.now().Before(h.Until) {
		until = h.Until.Local().Format("15:04")
	}
	return fmt.Sprintf("; %s is still held (shared launch failure) until %s: %s", kind, until, resumeToolHint(kind))
}

// heldWordsFor is the shared failure's words for kind's hold: "codex is
// held until 14:00: rounds that need it run their judge_fallback, or wait,
// and the first round after that probes it"; "" when it is not held.
func (e *Engine) heldWordsFor(ctx context.Context, kind string) string {
	h, ok := e.kindHoldOf(ctx, kind)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s is held until %s: rounds that need it run their judge_fallback, or wait, and the first round after that probes it",
		kind, h.Until.Local().Format("15:04"))
}

// settleProbes decides the probe of each kind the PR prID's round probes,
// at the round's end (roundEnded) or at a health check while it runs: the
// probe's agent of the kind took a prompt (probeLaunched) lifts the hold,
// with a toast, at once. Only a round that ended (or that no longer runs,
// as after a restart) decides the rest, once every run of it is in: an
// agent of the kind that ended with the hold's line since the probe began
// (probeFailed) holds it kindHoldSpan more; a round that ended with
// neither, its agents' launches ending on a line of the PR's own included,
// leaves the hold as it was and the probe to the next round, and its round
// is charged as a lone failure (sharedLaunchFailed finds no sharer). A
// health check that saw the probe's first launch die would otherwise renew
// the hold before the judge's restart launched the kind.
func (e *Engine) settleProbes(ctx context.Context, prID int64, label string, roundEnded bool) {
	for _, k := range e.kinds() {
		h, ok := e.kindHoldOf(ctx, k)
		if !ok || h.ProbePR != prID {
			continue
		}
		e.settleProbe(ctx, k, h, label, roundEnded)
	}
}

// settleProbe is settleProbes for kind's hold h.
func (e *Engine) settleProbe(ctx context.Context, kind string, h KindHold, label string, roundEnded bool) {
	switch {
	case e.probeLaunched(ctx, kind, h):
		e.liftKindHold(ctx, kind, fmt.Sprintf("%s's %s agent took its prompt: the hold ended at its first good probe", label, kind))
		e.urgent("kind-lift:"+kind, "magnum: "+kind+" launches again",
			fmt.Sprintf("The hold on %s ended at its first good probe (%s): rounds run it again.", kind, label), kindHoldSpan)
	case !roundEnded && e.roundActive(h.ProbePR):
		// The round runs: a restart of its agent may still launch the kind.
	case e.probeFailed(ctx, kind, h):
		h.Probes++
		h.Until, h.ProbePR, h.ProbeAt = e.now().Add(kindHoldSpan), 0, time.Time{}
		e.saveKindHold(ctx, kind, h)
		e.event(ctx, "warn", "tool:"+kind, EventKindHoldRenewed,
			fmt.Sprintf("%s held %s more: %s's probe ended as its launches do (probe %d, held since %s)", kind, humanDuration(kindHoldSpan), label, h.Probes,
				h.Since.Local().Format("15:04")),
			map[string]any{"last_line": h.Line, "probes": h.Probes, "until": store.FormatTime(h.Until)})
	default:
		h.ProbePR, h.ProbeAt = 0, time.Time{}
		e.saveKindHold(ctx, kind, h)
	}
}

// probeLaunched reports whether the probing PR's agent of kind took a
// prompt since its round claimed the probe: a run of a role of kind, an
// agent session, submitted at or after ProbeAt.
func (e *Engine) probeLaunched(ctx context.Context, kind string, h KindHold) bool {
	runs, err := e.st.RunsByPR(ctx, h.ProbePR)
	if err != nil {
		return false
	}
	for _, r := range runs {
		if r.SubmittedAt == nil || r.SubmittedAt.Before(h.ProbeAt) {
			continue
		}
		if role, ok := e.cfg.RoleByNameOrAlias(nil, r.Role); ok && role.IsAgent() && role.AgentKind() == kind {
			return true
		}
	}
	return false
}

// probeFailed reports whether an agent of kind of the probing PR ended with
// the hold's line since its round claimed the probe (launchLines). A line
// of its own is the PR's failure, not the kind's.
func (e *Engine) probeFailed(ctx context.Context, kind string, h KindHold) bool {
	pr, err := e.st.PRByID(ctx, h.ProbePR)
	if err != nil {
		return false
	}
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		return false
	}
	self, _, _ := prOfSubject(prSubject(repo, pr.Number))
	return slices.ContainsFunc(e.launchLines(ctx, h.ProbeAt), func(l launchLine) bool {
		return l.pr == self && l.line == h.Line && e.kindOf(l) == kind
	})
}

// liftKindHold ends kind's hold, why says how (kind.hold_lifted), and
// records when (KVKindHoldLifted).
func (e *Engine) liftKindHold(ctx context.Context, kind, why string) bool {
	h, ok := e.kindHoldOf(ctx, kind)
	if !ok || e.d.DryRun {
		return false
	}
	e.delKV(ctx, KVKindHold(kind))
	e.setKV(ctx, KVKindHoldLifted(kind), store.FormatTime(e.now()))
	e.event(ctx, "info", "tool:"+kind, EventKindHoldLifted,
		fmt.Sprintf("%s hold lifted after %s (%s): %s", kind, humanDuration(e.now().Sub(h.Since)), textx.Count(h.Probes, "failed probe", "failed probes"), why),
		map[string]any{"probes": h.Probes})
	return true
}

// checkKindHolds settles, at each health check, the probe of each held kind
// whose round runs or ended without settling it (settleProbe): a good probe
// lifts the hold before its round ends, and only a round that no longer
// runs renews it or leaves it to the next probe.
func (e *Engine) checkKindHolds(ctx context.Context) {
	for _, k := range e.kinds() {
		h, ok := e.kindHoldOf(ctx, k)
		if !ok || h.ProbePR == 0 {
			continue
		}
		label := fmt.Sprintf("PR %d", h.ProbePR)
		if pr, err := e.st.PRByID(ctx, h.ProbePR); err == nil {
			if repo, err := e.st.RepoByID(ctx, pr.RepoID); err == nil {
				label = fmt.Sprintf("%s#%d", repo.Name, pr.Number)
			}
		}
		e.settleProbe(ctx, k, h, label, false)
	}
}
