package engine

// herdr's Codex session hook (DECISIONS "Doctor and the daemon tell when
// herdr's Codex session hook stops giving ids"): herdr learns a Codex
// session's id from the SessionStart hook it installs in hooks.json, and
// without the id a judge cannot resume and can stop early. On 2026-10-22 a
// rewrite of ~/.codex/hooks.json moved that hook behind two others, most
// Codex sessions got no id for hours, and the daemon said nothing but one
// log line per session. Once a minute, with the usage read, the tick reads
// the registry's session ids (a run of Codex sessions without one warns
// once, and the first session with one after it says so) and the
// modification time of each Codex home's hooks.json (a change that leaves
// herdr's hook failing doctor's checks warns once). The checks are agents',
// which doctor runs too; magnum never edits hooks.json or Codex's trust.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// The events of herdr's Codex session hook.
const (
	// EventCodexSessionIDsMissing (warn) says the newest Codex sessions in
	// a row have no session id, once a run.
	EventCodexSessionIDsMissing = "codex.session_ids_missing"
	// EventCodexSessionIDsBack (info) says a Codex session has an id again
	// after such a run.
	EventCodexSessionIDsBack = "codex.session_ids_back"
	// EventCodexHooksChanged (warn) says a Codex home's hooks.json changed
	// and herdr's hook now fails a check, once a change.
	EventCodexHooksChanged = "codex.hooks_changed"
)

// KVCodexSessionIDsMissing is when the run of Codex sessions without a
// session id the daemon warned of started (store.FormatTime); absent when
// there is no such run.
const KVCodexSessionIDsMissing = "codex.session_ids_missing"

const (
	// codexSessionIDWait is how old a Codex session must be for the daemon
	// to count it without an id: herdr's hook reports the id at the start
	// of its first turn, which comes right after the start.
	codexSessionIDWait = 5 * time.Minute
	// codexSessionIDRun is how many of the newest Codex sessions in a row
	// must have no id for the daemon to warn.
	codexSessionIDRun = 2
	// codexHookToastWindow bounds how often one warning toasts.
	codexHookToastWindow = 24 * time.Hour
)

// checkCodexHook runs once a minute (usageEvery) while the codex kind's
// session ids come from herdr and a session role starts it
// (agents.CodexHookHomes): the sessions' ids (checkCodexSessionIDs), then
// each Codex home's hooks.json (checkCodexHooksFile).
func (e *Engine) checkCodexHook(ctx context.Context) {
	homes, users := agents.CodexHookHomes(e.cfg, func(r config.Role) string {
		if e.d.CodexHome == nil {
			return ""
		}
		return e.d.CodexHome(r)
	})
	if len(homes) == 0 {
		return
	}
	now := e.now()
	if !e.codexHookRead.IsZero() && now.Sub(e.codexHookRead) < usageEvery {
		return
	}
	e.codexHookRead = now
	e.checkCodexSessionIDs(ctx, now)
	for _, home := range homes {
		if home != "" {
			e.checkCodexHooksFile(ctx, home, users[home])
		}
	}
}

// checkCodexSessionIDs reads the newest Codex sessions at least
// codexSessionIDWait old that ran an agent (agents.RecentSessionIDs): when
// codexSessionIDRun or more in a row, of any PRs, have no session id, it
// writes one EventCodexSessionIDsMissing warning and one toast and keeps the
// run's start (KVCodexSessionIDsMissing), which says nothing more until a
// session started since it began has an id (herdr's report, or the
// observer's recovery from the rollout): then one EventCodexSessionIDsBack
// info event, and the mark goes.
func (e *Engine) checkCodexSessionIDs(ctx context.Context, now time.Time) {
	sessions, err := agents.RecentSessionIDs(ctx, e.st, agents.KindCodex, now, codexSessionIDWait)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "codex session ids")
		return
	}
	gap := agents.MissingSessionIDs(sessions)
	warned, had := e.kvTime(ctx, KVCodexSessionIDsMissing)
	switch {
	case had:
		if gap.Streak >= len(sessions) || sessions[gap.Streak].StartedAt.Before(warned) {
			return // no session of the run, or started since, has an id
		}
		s := sessions[gap.Streak]
		e.delKV(ctx, KVCodexSessionIDsMissing)
		e.event(ctx, "info", "tool:"+agents.KindCodex, EventCodexSessionIDsBack,
			fmt.Sprintf("Codex sessions get session ids again: %s (session %d, started %s) has one; the sessions started from %s had none",
				s.Role, s.ID, s.StartedAt.Local().Format("15:04"), warned.Local().Format("15:04")),
			map[string]any{"session": s.ID, "role": s.Role, "since": store.FormatTime(warned)})
	case gap.Streak >= codexSessionIDRun:
		run := sessions[:gap.Streak]
		since := run[len(run)-1].StartedAt
		ids, roles := make([]int64, len(run)), make([]string, len(run))
		for i, s := range run {
			ids[i], roles[i] = s.ID, s.Role
		}
		msg := fmt.Sprintf("Codex sessions get no session id since %s (%d sessions). Judges cannot resume and can stop early. Run `magnum doctor`.",
			since.Local().Format("15:04"), len(run))
		e.setKV(ctx, KVCodexSessionIDsMissing, store.FormatTime(since))
		e.event(ctx, "warn", "tool:"+agents.KindCodex, EventCodexSessionIDsMissing, msg,
			map[string]any{"sessions": ids, "roles": roles, "since": store.FormatTime(since)})
		e.urgent("codex-session-ids:"+store.FormatTime(since), "magnum: Codex sessions get no session id", msg, codexHookToastWindow)
	}
}

// checkCodexHooksFile compares the modification time of home's hooks.json
// with the one the last check saw (codexHooksSeen; the first check only
// records it): when it changed and herdr's hook then fails a check of
// doctor's (agents.CodexSessionHook.Problems: missing, disabled, its place
// or its trust), one EventCodexHooksChanged warning and one toast name the
// file and the failed checks, never the home's path (a pane env value but
// the default ~/.codex). A file that cannot be read is said the same way.
func (e *Engine) checkCodexHooksFile(ctx context.Context, home string, roles []string) {
	file := filepath.Join(home, "hooks.json")
	var mtime time.Time
	switch fi, err := os.Stat(file); {
	case err == nil:
		mtime = fi.ModTime()
	case !errors.Is(err, fs.ErrNotExist):
		e.log.Debug("codex hooks.json", "err", err)
		return
	}
	prev, seen := e.codexHooksSeen[home]
	if e.codexHooksSeen == nil {
		e.codexHooksSeen = map[string]time.Time{}
	}
	e.codexHooksSeen[home] = mtime
	if !seen || prev.Equal(mtime) || mtime.IsZero() {
		return
	}
	hooksName, configName := agents.CodexFileName(home, "hooks.json"), agents.CodexFileName(home, "config.toml")
	h, ok, err := agents.ReadCodexSessionHook(home)
	var problems []agents.CodexHookProblem
	switch {
	case err != nil:
		problems = []agents.CodexHookProblem{{Check: "read", Text: "herdr's Codex session hook cannot be judged: " +
			strings.ReplaceAll(err.Error(), home, strings.TrimSuffix(agents.CodexFileName(home, ""), "/"))}}
	case !ok:
		return
	default:
		problems = h.Problems(hooksName, configName)
	}
	if len(problems) == 0 {
		e.log.Info("codex hooks.json changed; herdr's session hook passes", "at", mtime.Local().Format("15:04"))
		return
	}
	checks := make([]string, len(problems))
	for i, p := range problems {
		checks[i] = p.Check
	}
	msg := fmt.Sprintf("%s changed at %s: %s. Codex sessions may get no session id: run `magnum doctor`.",
		hooksName, mtime.Local().Format("15:04"), agents.JoinCodexHookProblems(problems))
	e.event(ctx, "warn", "tool:"+agents.KindCodex, EventCodexHooksChanged, msg,
		map[string]any{"checks": checks, "roles": roles, "changed": store.FormatTime(mtime)})
	e.urgent("codex-hooks:"+store.FormatTime(mtime), "magnum: herdr's Codex session hook may not run", msg, codexHookToastWindow)
}
