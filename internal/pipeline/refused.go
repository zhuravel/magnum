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
// Codex rollout: a refusal when its codex_error_info is cyber_policy or its
// message matches the kind's refused patterns. ok is false otherwise (no
// rollout, another error, none).
func (rd *round) rolloutRefusal(ctx context.Context, role config.Role, run store.Run) (agents.Health, bool) {
	te, ok := rd.r.Agents.TurnError(context.WithoutCancel(ctx), run)
	if !ok {
		return agents.Health{}, false
	}
	if te.Info == agents.CodexCyberPolicy {
		return agents.Health{Kind: agents.HealthRefused, Detail: execx.Redact(te.Message)}, true
	}
	if h := rd.classify(role.AgentKind(), te.Message); h.Kind == agents.HealthRefused {
		return h, true
	}
	return agents.Health{}, false
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
