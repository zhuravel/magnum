package pipeline

// A refusal (agents.HealthRefused; DECISIONS "A Codex safety warning ends
// the round and flags the PR"): a role's turn ended on its provider's safety
// warning about the content, Codex's "This content was flagged for possible
// cybersecurity risk" (the kind's health_patterns refused, read from the
// pane after the turn's prompt; when the pane scrolled past it, from the
// turn's error in the session's Codex rollout, Agents.TurnError). Codex
// may block an account it takes for a cyber abuser, so nothing about the
// PR is tried again: the round ends at once, whoever was refused. A refused
// reviewer or own pass cancels the stages with a *refusedError (the other
// roles are interrupted and their runs abandoned, as a lost judge's round
// does), a refused judge turn (candidates, single prompt, continue) is a
// final verdict with no nudge, and RoundResult.Refusal names the role and
// the run. The engine flags the PR (engine.CodexFlag).
//
// A helper agent (a Codex sub-agent the role's session spawned) runs in a
// rollout of its own, and its refusal leaves the session's turn ending ok:
// every ended turn of a reviewer, the own pass and the judge is checked for
// one first (helperRefusal), so a refused helper ends the round as its
// session's refusal would, whatever report or review the turn left.

import (
	"context"
	"fmt"
	"os"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
)

// Refusal is who was refused in which run (RoundResult.Refusal).
type Refusal struct {
	Role   string // the role whose turn was refused
	Kind   string // its agent kind (config.Role.AgentKind): whose provider refused it
	RunID  string // the refused turn's run
	Detail string // the pane line, or the rollout's message (redacted)
}

// Sentence is the round's error and the PR's last error: "Codex refused the
// review: content flagged as a cybersecurity risk (codex-judge, run r-…)".
func (r Refusal) Sentence() string {
	return fmt.Sprintf("%s refused the review: content flagged as a cybersecurity risk (%s, run %s)", agents.KindName(r.Kind), r.Role, r.RunID)
}

// refusedError is the cause of a stage context a refused turn cancelled,
// and what reviewers returns then: the round ends refused.
type refusedError struct{ r Refusal }

func (e *refusedError) Error() string { return e.r.Sentence() }

// refusal is the Refusal of role's run runID, refused as h says.
func refusal(role config.Role, runID string, h agents.Health) Refusal {
	return Refusal{Role: role.Name, Kind: role.AgentKind(), RunID: runID, Detail: h.Detail}
}

// reportRefusal is the Refusal a refused report stands for.
func reportRefusal(rep RoleReport) Refusal {
	return Refusal{Role: rep.Role, Kind: rep.Kind, RunID: rep.RunID, Detail: rep.Detail}
}

// refuseStages cancels the round's stages when rep is a refusal, so the
// round ends at once (the first refusal is the one named).
func refuseStages(cancel context.CancelCauseFunc, rep RoleReport) {
	if cancel != nil && rep.Status == string(agents.HealthRefused) {
		cancel(&refusedError{reportRefusal(rep)})
	}
}

// rolloutRefusal reads the error role's ended turn (run) recorded in its
// Codex rollout, or one of a helper agent the session spawned
// (Agents.TurnError): a refusal when its codex_error_info is cyber_policy
// or its message matches the kind's refused patterns. ok is false otherwise
// (no rollout, another error, none).
func (rd *round) rolloutRefusal(ctx context.Context, role config.Role, run store.Run) (agents.Health, bool) {
	te, ok := rd.r.Agents.TurnError(context.WithoutCancel(ctx), run)
	if !ok {
		return agents.Health{}, false
	}
	return rd.turnRefusal(role, te)
}

// turnRefusal is the refusal te stands for, if it is one; the detail names
// the helper agent it came from.
func (rd *round) turnRefusal(role config.Role, te agents.TurnError) (agents.Health, bool) {
	h := agents.Health{Kind: agents.HealthRefused, Detail: execx.Redact(te.Message)}
	if te.Info != agents.CodexCyberPolicy {
		if h = rd.classify(role.AgentKind(), te.Message); h.Kind != agents.HealthRefused {
			return agents.Health{}, false
		}
	}
	if te.Helper != "" {
		h.Detail = "helper agent " + te.Helper + ": " + h.Detail
	}
	return h, true
}

// helperRefusal reads the rollouts of the helper agents (Codex sub-agents)
// the session of role's run spawned for a refusal of a turn one of them
// ended within the run (DECISIONS "A refused helper agent ends the round"):
// a refused helper leaves the session's own turn ending ok, so a turn whose
// report or review is there is still a refused one. ok is false for a
// session of another kind, a run without helpers, and helpers whose turns
// ended without one.
func (rd *round) helperRefusal(ctx context.Context, role config.Role, run store.Run) (agents.Health, bool) {
	te, ok := rd.r.Agents.TurnError(context.WithoutCancel(ctx), run)
	if !ok || te.Helper == "" {
		return agents.Health{}, false
	}
	return rd.turnRefusal(role, te)
}

// helperRefused makes rep, the report of role's ended run, a refusal when
// a helper agent of it was refused (helperRefusal), and reports whether it
// did: checkReport's first check, for the reviewers and the judge's own
// pass, whose report file a refused helper does not keep from being
// written.
func (rd *round) helperRefused(ctx context.Context, role config.Role, run store.Run, rep *RoleReport) bool {
	h, ok := rd.helperRefusal(ctx, role, run)
	if !ok {
		return false
	}
	rep.Status, rep.Detail, rep.Health = string(h.Kind), h.Detail, &h
	return true
}

// helperRefusedVerdict is the final verdict of a judge turn t that was
// sent when a helper agent of it was refused (helperRefusal): the round
// ends refused before verification looks for the review, which a judge may
// post after its helper's refusal (it stays posted).
func (rd *round) helperRefusedVerdict(ctx context.Context, t turn) (verdict, bool) {
	h, ok := rd.helperRefusal(ctx, rd.judge, t.run)
	if !ok {
		return verdict{}, false
	}
	ref := refusal(rd.judge, t.run.ID, h)
	return verdict{final: true, outcome: OutcomeRefused, refusal: &ref, err: &refusedError{ref}}, true
}

// shellRefusal reads a shell role's output for its tool's refusal: the pane
// after this run's command line, then the last lines of a captured stdout
// (a refusal printed by a command that still exited 0 is no report).
func (rd *round) shellRefusal(ctx context.Context, role config.Role, run store.Run, path, anchor string) (agents.Health, bool) {
	if h := rd.paneHealth(ctx, role, run, anchor); h.Kind == agents.HealthRefused {
		return h, true
	}
	if role.Capture != config.CaptureStdout {
		return agents.Health{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return agents.Health{}, false
	}
	if h := rd.classify(role.AgentKind(), tailLines(string(b), transcriptTail)); h.Kind == agents.HealthRefused {
		return h, true
	}
	return agents.Health{}, false
}

// refused ends the round on a refusal the stages found (a reviewer, the
// judge's own pass).
func (rd *round) refused(ctx context.Context, r Refusal) (RoundResult, error) {
	rd.mu.Lock()
	rd.res.Refusal = &r
	rd.mu.Unlock()
	rd.refusedEvent(ctx, r)
	return rd.done(ctx, OutcomeRefused, &refusedError{r})
}

// refusedEvent records the refusal (round.refused).
func (rd *round) refusedEvent(ctx context.Context, r Refusal) {
	rd.event(ctx, "error", "round.refused",
		fmt.Sprintf("%s: %s; the round ends, nothing about the PR is tried again", r.Sentence(), r.Detail),
		map[string]any{"role": r.Role, "kind": r.Kind, "run": r.RunID})
}
