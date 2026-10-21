package cli

// The settings screen's Runtime section: the pauses `magnum status` lists
// (statusGatherPauses), and pause and resume through the bodies of
// `magnum pause` and `magnum resume`, captured as the screens' other keys are.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/tui"
)

// screenRuntime is the settings screen's runtime over the screens' actions,
// whose act deps it shares.
type screenRuntime struct{ *screenActions }

var _ tui.SettingsRuntime = screenRuntime{}

// Runtime reads the automation pause, each agent kind's pause and each
// watch owner's identity-leak pause from the registry, as `magnum status`
// lists them, and whether a daemon runs (its pidfile, without waking it).
func (r screenRuntime) Runtime(ctx context.Context) (tui.RuntimeState, error) {
	// The lock covers building the deps only: an action in flight may wait
	// half a minute for the daemon's answer under it, and reading the pauses
	// needs no turn (as Requests reads the requests).
	r.mu.Lock()
	d, err := r.deps()
	r.mu.Unlock()
	if err != nil {
		return tui.RuntimeState{}, err
	}
	pid := 0
	if d.Running != nil {
		pid, _ = d.Running() // a pidfile that cannot be read counts as no daemon
	}
	rep := statusReport{GeneratedAt: d.now()}
	statusGatherPauses(statusDeps{Store: d.Store, Config: d.Cfg, Now: d.Now}, statusKV{ctx: ctx, st: d.Store}, &rep)
	s := settingsRuntimeState(d.Cfg, rep.Pauses, rep.GeneratedAt, pid > 0)
	for i, k := range s.Kinds { // x on its row restarts them (`magnum restart-sessions`)
		live, _, err := engine.RestartableSessions(ctx, d.Store, d.Cfg, k.Name, 0)
		if err != nil {
			return tui.RuntimeState{}, err
		}
		s.Kinds[i].Live = len(live)
	}
	return s, nil
}

// RestartSessions parks the live sessions of an agent kind (or all) as
// `magnum restart-sessions --tool` does, the screen having asked first; the
// text is the daemon's answer.
func (s *screenActions) RestartSessions(ctx context.Context, tool string) (string, error) {
	res, err := s.do(ctx, "restart-sessions", func(ctx context.Context, c *Context, d *actDeps) int {
		if msg := resumeToolRefusal(d.Cfg, tool); msg != "" {
			return cmdFail(c, "restart-sessions", errors.New(msg))
		}
		return restartSessionsMain(ctx, c, d, restartSessionsOpts{tool: tool, yes: true})
	})
	return res.Text, err
}

// settingsRuntimeState is the Runtime section from the pauses status
// gathered at now. Automation is held by `magnum pause` (scope daemon) and
// by an infrastructure pause, which `magnum resume` lifts with it.
func settingsRuntimeState(cfg *config.Config, pauses []statusPause, now time.Time, daemon bool) tui.RuntimeState {
	byScope := map[string][]statusPause{}
	for _, p := range pauses {
		byScope[p.Scope] = append(byScope[p.Scope], p)
	}
	s := tui.RuntimeState{Daemon: daemon}
	s.Automation = settingsRuntimeEntry("automation", "", slices.Concat(byScope["daemon"], byScope["infra"]), now)
	if !s.Automation.Paused {
		s.Automation.State, s.Automation.Detail = "running", "automatic reviews start as PRs need them"
	}
	for _, k := range rolesAllKinds(cfg) {
		s.Kinds = append(s.Kinds, settingsRuntimeEntry(k, "", byScope[k], now))
	}
	seen := map[string]bool{}
	for _, w := range cfg.Watches { // once per owner, as statusGatherPauses scopes them
		if owner := strings.ToLower(w.Owner); !seen[owner] {
			seen[owner] = true
			label := statusSafe("watch "+w.Owner, 0) // the screen numbers it as its scopes number the file's watches
			s.Watches = append(s.Watches, settingsRuntimeEntry(w.Owner, label, byScope["watch:"+w.Owner], now))
		}
	}
	return s
}

// settingsRuntimeEntry is one pausable thing from its pauses: paused by any
// of them but a model limit ("<model> limited": its sessions run a fallback
// model meanwhile), which the detail names instead. The state is each
// pause's text without its detail; the detail is each one's detail and fix.
func settingsRuntimeEntry(name, label string, pauses []statusPause, now time.Time) tui.RuntimeEntry {
	e := tui.RuntimeEntry{Name: name, Label: label, State: "no pause"}
	var states, details []string
	for _, p := range pauses {
		if strings.HasSuffix(p.Reason, " limited") {
			details = append(details, statusPauseText(p, now))
			continue
		}
		e.Paused = true
		detail := p.Detail
		p.Detail = ""
		states = append(states, statusPauseText(p, now))
		for _, s := range []string{statusSafe(detail, 100), statusSafe(p.Fix, 0)} {
			if s != "" {
				details = append(details, s)
			}
		}
	}
	if e.Paused {
		e.State = "paused: " + strings.Join(states, "; ")
	}
	e.Detail = strings.Join(details, "; ")
	return e
}

// Pause pauses automation as `magnum pause` does (pauseMain, after the
// checks of its flags); the text is its answer. The screen asked first.
func (s *screenActions) Pause(ctx context.Context, p tui.PauseSpec) (string, error) {
	o := pauseOpts{dur: p.For, until: p.Until, reason: p.Reason}
	if msg := o.refusal(); msg != "" {
		return "", errors.New(msg)
	}
	res, err := s.do(ctx, "pause", func(ctx context.Context, c *Context, d *actDeps) int {
		if o.until != "" { // pauseMain's own refusal ends with its usage line
			if _, err := pauseParseUntil(o.until, d.now()); err != nil {
				return cmdFail(c, "pause", err)
			}
		}
		return pauseMain(ctx, c, d, o)
	})
	return res.Text, err
}

// Resume lifts a pause as `magnum resume` does (resumeMain, after its
// check of the agent kind); the text is its answer. The screen asked first.
func (s *screenActions) Resume(ctx context.Context, r tui.ResumeSpec) (string, error) {
	if r.Tool != "" && r.Watch != "" {
		return "", errors.New("resume an agent kind or a watch, not both")
	}
	res, err := s.do(ctx, "resume", func(ctx context.Context, c *Context, d *actDeps) int {
		if msg := resumeToolRefusal(d.Cfg, r.Tool); msg != "" {
			return cmdFail(c, "resume", errors.New(msg))
		}
		return resumeMain(ctx, c, d, resumeOpts{tool: r.Tool, watch: r.Watch})
	})
	return res.Text, err
}
