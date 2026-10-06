package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
)

// modelFallback continues a session role's turn that ended on its model's
// own limit (h, a model_limit verdict on its pane): the limit is recorded
// (Agents.NoteModelLimit), the session switched to the kind's next fallback
// model not in tried (Agents.SwitchModel), and a new run of the same role
// and round (kind continue) is submitted with the model-fallback prompt and
// awaited like the original. reportPath is what the prompt names as the
// report or result file. A reviewer
// passes finishOld so its limited run ends failed with outcome model_limit;
// the judge keeps its runs for the verdict. The continuation's run id is
// added to ids (the judge's result-file check) before it is prompted.
//
// ok is false when the kind cannot switch, every fallback is used up or
// limited, or the switch failed: the caller then treats the limit as a usage
// limit (the kind pauses), as before per-model limits existed.
func (rd *round) modelFallback(ctx context.Context, role config.Role, t turn, h agents.Health, tried *[]string,
	reportPath, resultFile string, ids map[string]bool, finishOld bool) (next turn, anchor, runID string, ok bool) {
	s, found := rd.session(ctx, t.run)
	if !found || ctx.Err() != nil {
		return turn{}, "", "", false
	}
	limit, err := rd.r.Agents.NoteModelLimit(ctx, s, h)
	if err != nil {
		rd.warn(ctx, "%s: %v", role.Name, err)
	}
	model, ok := rd.r.Agents.FallbackModel(ctx, s, *tried)
	if !ok {
		return turn{}, "", "", false
	}
	if err := rd.r.Agents.SwitchModel(ctx, s, model, agents.SwitchLimitHit); err != nil {
		rd.warn(ctx, "%s: switch to %s after its model's limit: %v", role.Name, model, err)
		return turn{}, "", "", false
	}
	*tried = append(*tried, model)
	if finishOld {
		rd.finishRun(ctx, t.run.ID, store.RunFailed, string(agents.HealthModelLimit), h.Detail)
	}
	nrun, err := rd.newRun(ctx, role, store.RunContinue)
	if err != nil {
		rd.warn(ctx, "%v", err)
		return turn{}, "", "", false
	}
	rd.mu.Lock()
	if rd.cont == nil {
		rd.cont = map[string]store.Run{}
	}
	rd.cont[role.Name] = *nrun
	rd.mu.Unlock()
	text, err := rd.r.Agents.FallbackPrompt(agents.FallbackData{Model: model, Previous: limit.Model, Role: role.Name,
		URL: rd.pr.URL, HeadSHA: rd.in.TargetSHA, ReportPath: reportPath})
	if err != nil {
		rd.finishRun(ctx, nrun.ID, store.RunFailed, ReportFailed, err.Error())
		rd.warn(ctx, "%s: model-fallback prompt: %v", role.Name, err)
		return turn{}, "", "", false
	}
	if ids != nil {
		ids[nrun.ID] = true
	}
	until := ""
	if !limit.Until.IsZero() {
		until = " until " + limit.Until.Local().Format("15:04")
	}
	rd.event(ctx, "warn", "round.model_fallback",
		fmt.Sprintf("%s: %s is limited%s; switched to %s, continuing (run %s)", role.Name, limit.Model, until, model, nrun.ID),
		map[string]any{"run": nrun.ID, "previous_run": t.run.ID, "role": role.Name, "from": limit.Model, "to": model,
			"detail": execx.Redact(h.Detail)})
	return rd.submitAndWait(ctx, *nrun, text, rd.timeout(role), resultFile, ids), fallbackAnchor(text, reportPath, rd.in.TargetSHA), nrun.ID, true
}

// reviewerFallbacks continues a reviewer's ended turn on fallback models
// while its pane (after anchor) shows its model's limit and it left no
// report: up to one switch per fallback model. It returns the last turn and
// the anchor of its prompt.
func (rd *round) reviewerFallbacks(ctx context.Context, role config.Role, t turn, path, anchor string) (turn, string) {
	var tried []string
	for t.kind == waitEnded && ctx.Err() == nil {
		if _, ok := rd.reportFile(role, t.run, path); ok {
			break
		}
		h := rd.paneHealth(ctx, role, t.run, anchor)
		if h.Kind != agents.HealthModelLimit {
			break
		}
		next, nanchor, _, ok := rd.modelFallback(ctx, role, t, h, &tried, path, "", nil, true)
		if !ok {
			break
		}
		t, anchor = next, nanchor
	}
	return t, anchor
}

// paneHealth classifies the pane of run's session after anchor (HealthOK
// when it cannot be read).
func (rd *round) paneHealth(ctx context.Context, role config.Role, run store.Run, anchor string) agents.Health {
	if s, ok := rd.session(ctx, run); ok {
		if text, err := rd.r.Agents.ReadRecent(context.WithoutCancel(ctx), s, agents.HealthLines); err == nil {
			return rd.classifyAfter(role.AgentKind(), text, anchor)
		}
	}
	return agents.Health{Kind: agents.HealthOK}
}

// fallbackAnchor locates a model-fallback prompt in the pane text: the
// report path or head it names, else its first line.
func fallbackAnchor(text, reportPath, head string) string {
	for _, a := range []string{reportPath, head} {
		if a != "" && strings.Contains(text, a) {
			return a
		}
	}
	line, _, _ := strings.Cut(text, "\n")
	return strings.TrimSpace(line)
}

// asUsageLimit reads a model limit that no fallback model took over as the
// usage limit it was before per-model limits existed: the role's kind
// pauses until the reset.
func asUsageLimit(h agents.Health) agents.Health {
	if h.Kind == agents.HealthModelLimit {
		h.Kind = agents.HealthUsageLimit
	}
	return h
}
