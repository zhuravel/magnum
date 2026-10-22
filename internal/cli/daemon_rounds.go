package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/store"
)

// registryReadOnlyDSN is the sqlite DSN that opens the registry at path
// read-only, never creating it. The path is URI-escaped, so a home
// directory containing '?', '#' or '%' still opens the right file.
func registryReadOnlyDSN(path string) string {
	u := url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(1000)"}
	return u.String()
}

// daemonGate wires the gate of a skill proposal (engine.Deps.EvalCase,
// EvalInputs, EvalRuns, EvalCorpus) to the eval replays `magnum eval run`
// makes, in the daemon's process (Amendment A1: the engine cannot import
// evalrun, which runs it): a case replays with the daemon's layout, config,
// registry (for the PR's Codex flag) and logger, in a worktree of the
// gate's own (paths.Layout.LearnEvalWorktrees, never `magnum eval run`'s,
// which refuses to run while a gate does); the inputs are those of
// the config pointed at a candidate directory; the runs and the corpora are
// the ones `magnum eval` reads. A dry run, an eval's own app and a config
// that did not load wire none.
func daemonGate(c *Context, a *app.App) func(*engine.Deps) {
	return func(d *engine.Deps) {
		if a.DryRun || a.AgentTag != "" || a.Config == nil {
			return
		}
		deps := evalrun.Deps{Layout: a.Layout, Config: a.Config, Store: a.Store, Logger: a.Logger, Stderr: io.Discard,
			Warn: func(format string, args ...any) { a.Logger.Warn("skill gate: " + fmt.Sprintf(format, args...)) }}
		d.EvalCase = func(ctx context.Context, ec eval.Case, o engine.EvalGateOptions) eval.CaseRun {
			return evalrun.RunCase(ctx, deps, ec, evalrun.Options{Candidate: o.Candidate, JudgeOnly: o.JudgeOnly, RunDir: o.RunDir, AgentTag: o.AgentTag,
				Worktrees: a.Layout.LearnEvalWorktrees()})
		}
		d.EvalInputs = func(candidate string) (string, string, error) {
			cfg := a.Config
			if candidate != "" {
				var err error
				if cfg, err = evalrun.WithCandidate(a.Config, candidate); err != nil {
					return "", "", err
				}
			}
			return evalrun.Inputs(cfg), evalrun.ReviewerInputs(cfg), nil
		}
		d.EvalRuns = func() ([]eval.Run, error) { return eval.ListRuns(evalrun.RunsRoot(a.Layout)) }
		d.EvalCorpus = func() (*eval.Corpus, string, error) {
			user, learned := evalCorpusPath(c, "")
			if user != "" && !fsx.Exists(user) {
				user = ""
			}
			corpus, err := eval.LoadCorpora(user, learned)
			return corpus, user, err
		}
	}
}

// inFlight is what stopping the daemon now would cut short: the review
// rounds in flight and the background work running (the notes curation,
// engine.KVNotesCurating, the notes critique, engine.KVNotesCritic, the
// retro, engine.KVRetroRunning, the skill edit, engine.KVLearnEditing, the
// fix jobs whose fixer works, engine.KVPRFix, and the whiteboard jobs whose
// writer works, their runs of kind whiteboard).
type inFlight struct {
	rounds []string // "owner/repo#N (state)"
	work   []string // "notes curation of owner/repo (since 15:04)", "notes critique of owner/repo (proposal 3, since 15:04)", "retro (since 15:04)", "skill edit (since 15:04)", "fix of owner/repo#N (cycle 1, since 15:04)", "whiteboard of owner/repo#N (since 15:04)"
}

func (f inFlight) none() bool { return len(f.rounds) == 0 && len(f.work) == 0 }

// String lists them, the rounds first.
func (f inFlight) String() string {
	return strings.Join(append(append([]string{}, f.rounds...), f.work...), ", ")
}

// count says how much is in flight: the rounds as "N <rounds>", the work as
// "N background job(s)".
func (f inFlight) count(rounds string) string {
	var parts []string
	if len(f.rounds) > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", len(f.rounds), rounds))
	}
	if len(f.work) > 0 {
		parts = append(parts, fmt.Sprintf("%d background job(s)", len(f.work)))
	}
	return strings.Join(parts, " and ")
}

// cutShort says what stopping the daemon now does to what is in flight.
func (f inFlight) cutShort() string {
	var parts []string
	if len(f.rounds) > 0 {
		parts = append(parts, "abandons the rounds (their agents keep running, the rounds start over later)")
	}
	if len(f.work) > 0 {
		parts = append(parts, "cuts the background work short (a notes curation or a skill edit proposes nothing, a notes critique runs again later, "+
			"a retro or a skill gate runs again, a fix loop ends and needs you, a whiteboard is lost)")
	}
	return "stopping the daemon now " + strings.Join(parts, " and ")
}

// activeRounds reads what is in flight, read-only: the PRs whose review
// round is claiming, reviewing or verifying, as "owner/repo#N (state)", and
// the notes curation, the notes critique, the retro and the skill edit
// running (backgroundWork). Stopping the daemon abandons the rounds (their
// agents keep working in herdr but nobody collects the result, and the next
// daemon starts them over) and cuts the curation, the critique, the retro
// and the edit short.
// No registry yet means nothing; any other
// read failure is an error, so callers fail closed.
func (c *Context) activeRounds(ctx context.Context) (inFlight, error) {
	db, err := c.registryReadOnly()
	if db == nil || err != nil {
		return inFlight{}, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, completeTimeout)
	defer cancel()
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(prInFlight)), ", ")
	args := make([]any, len(prInFlight))
	for i, s := range prInFlight {
		args[i] = s
	}
	rows, err := db.QueryContext(ctx, `SELECT r.owner, r.name, p.number, p.state FROM prs p JOIN repos r ON r.id = p.repo_id
		WHERE p.state IN (`+marks+`) ORDER BY r.owner, r.name, p.number`, args...)
	if err != nil {
		return inFlight{}, err
	}
	defer rows.Close()
	var out inFlight
	for rows.Next() {
		var owner, name, state string
		var n int
		if err := rows.Scan(&owner, &name, &n, &state); err != nil {
			return inFlight{}, err
		}
		out.rounds = append(out.rounds, fmt.Sprintf("%s/%s#%d (%s)", owner, name, n, state))
	}
	if err := rows.Err(); err != nil {
		return inFlight{}, err
	}
	if out.work, err = backgroundWork(ctx, db); err != nil {
		return inFlight{}, err
	}
	return out, nil
}

// registryReadOnly opens the registry read-only, never creating it: nil
// (and no error) when there is none yet.
func (c *Context) registryReadOnly() (*sql.DB, error) {
	path := c.Layout.DB()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return sql.Open("sqlite", registryReadOnlyDSN(path))
}

// gateRunning names the daemon's skill gate when one runs, as
// backgroundWork does ("skill gate of proposal 3 (since 15:04)"; "" =
// none, or no registry yet): its learn.gating mark, read-only.
func (c *Context) gateRunning(ctx context.Context) (string, error) {
	db, err := c.registryReadOnly()
	if db == nil || err != nil {
		return "", err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, completeTimeout)
	defer cancel()
	work, err := backgroundWork(ctx, db)
	if err != nil {
		return "", err
	}
	for _, w := range work {
		if strings.HasPrefix(w, "skill gate") {
			return w, nil
		}
	}
	return "", nil
}

// backgroundWork reads the notes curation, the notes critique, the retro,
// the skill edit and the skill gate running from their kv marks: "notes
// curation of owner/repo (since 15:04)", "notes critique of owner/repo
// (proposal 3, since 15:04)", "retro (since 15:04)", "skill edit (since
// 15:04)" (or "revert of skill version 2 (since 15:04)", which runs in the
// edit's slot), "skill gate of proposal 3 (since 15:04)"; then the fix jobs
// whose fixer works (fixesAtWork) and the whiteboard jobs whose writer works
// (whiteboardsAtWork). A mark it cannot parse still counts.
func backgroundWork(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM kv WHERE key IN (?, ?, ?, ?, ?) AND value != '' ORDER BY key`,
		engine.KVNotesCurating, engine.KVNotesCritic, engine.KVRetroRunning, engine.KVLearnEditing, engine.KVLearnGating)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now()
	var out []string
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		what, since, detail := "retro", time.Time{}, ""
		switch key {
		case engine.KVNotesCurating:
			what = "notes curation"
			var m engine.CurateMark
			if json.Unmarshal([]byte(value), &m) == nil && m.Repo != "" {
				what, since = "notes curation of "+m.Repo, m.Started
			}
		case engine.KVNotesCritic:
			what = "notes critique"
			var m engine.CriticMark
			if json.Unmarshal([]byte(value), &m) == nil && m.Repo != "" {
				what, since = "notes critique of "+m.Repo, m.Started
				if m.Proposal != 0 {
					detail = fmt.Sprintf("proposal %d", m.Proposal)
				}
			}
		case engine.KVLearnEditing:
			what = "skill edit"
			var m engine.EditMark
			if json.Unmarshal([]byte(value), &m) == nil {
				since = m.Started
				if m.Revert != 0 {
					what = fmt.Sprintf("revert of skill version %d", m.Revert)
				}
			}
		case engine.KVLearnGating:
			what = "skill gate"
			var m engine.GateMark
			if json.Unmarshal([]byte(value), &m) == nil && m.Proposal != 0 {
				what, since = fmt.Sprintf("skill gate of proposal %d", m.Proposal), m.Started
			}
		default:
			if t, err := store.ParseTime(value); err == nil {
				since = t
			}
		}
		var parens []string
		if detail != "" {
			parens = append(parens, detail)
		}
		if !since.IsZero() {
			parens = append(parens, "since "+inspClock(now, since))
		}
		if len(parens) > 0 {
			what += " (" + strings.Join(parens, ", ") + ")"
		}
		out = append(out, what)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	fixes, err := fixesAtWork(ctx, db, now)
	if err != nil {
		return nil, err
	}
	whiteboards, err := whiteboardsAtWork(ctx, db, now)
	return append(append(out, fixes...), whiteboards...), err
}

// whiteboardsAtWork lists the whiteboard jobs whose writer works, from their
// runs of kind whiteboard not yet ended: "whiteboard of owner/repo#N (since
// 15:04)", since the prompt went out. The job keeps the PR reviewed, so no
// round state shows it, and a daemon stopped meanwhile fails the run (lost
// in a restart).
func whiteboardsAtWork(ctx context.Context, db *sql.DB, now time.Time) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT r.owner, r.name, p.number, coalesce(u.submitted_at, u.created_at) FROM runs u
		JOIN prs p ON p.id = u.pr_id JOIN repos r ON r.id = p.repo_id
		WHERE u.kind = ? AND u.state IN (?, ?, ?) ORDER BY r.owner, r.name, p.number`,
		store.RunWhiteboard, store.RunPending, store.RunSubmitted, store.RunWorking)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var owner, name, at string
		var n int
		if err := rows.Scan(&owner, &name, &n, &at); err != nil {
			return nil, err
		}
		what := fmt.Sprintf("whiteboard of %s/%s#%d", owner, name, n)
		if t, err := store.ParseTime(at); err == nil {
			what += " (since " + inspClock(now, t) + ")"
		}
		out = append(out, what)
	}
	return out, rows.Err()
}

// fixesAtWork lists the fix loops whose fixer works, from their
// engine.KVPRFix rows in phase fixing: "fix of owner/repo#N (cycle 2, since
// 15:04)", since the loop's start. The fix job keeps the PR reviewed, so no
// round state shows it, and a daemon stopped meanwhile ends the loop as
// needs attention. A loop waiting for its re-review is not listed (its
// round is, once it runs), and a row that cannot be read still counts.
func fixesAtWork(ctx context.Context, db *sql.DB, now time.Time) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT k.value, r.owner, r.name, p.number FROM kv k
		JOIN prs p ON k.key = 'pr.' || p.id || '.fix' JOIN repos r ON r.id = p.repo_id
		WHERE k.key GLOB 'pr.*.fix' ORDER BY r.owner, r.name, p.number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var value, owner, name string
		var n int
		if err := rows.Scan(&value, &owner, &name, &n); err != nil {
			return nil, err
		}
		what := fmt.Sprintf("fix of %s/%s#%d", owner, name, n)
		var st engine.FixState
		if json.Unmarshal([]byte(value), &st) == nil {
			if st.Phase != engine.FixPhaseFixing {
				continue
			}
			what += fmt.Sprintf(" (cycle %d", st.Cycle)
			if !st.StartedAt.IsZero() {
				what += ", since " + inspClock(now, st.StartedAt)
			}
			what += ")"
		}
		out = append(out, what)
	}
	return out, rows.Err()
}

// refuseWhileRoundsRun prints what is in flight and returns false when the
// command must not stop the daemon now (--now overrides). It fails closed:
// when the registry cannot be read it refuses too.
func (c *Context) refuseWhileRoundsRun(cmd string, now bool) bool {
	if now {
		return true
	}
	running, err := c.activeRounds(context.Background())
	switch {
	case err != nil:
		fmt.Fprintf(c.Stderr, "magnum %s: cannot tell whether review rounds are in flight (reading %s: %v)\n"+
			"stopping the daemon now may abandon them\n"+
			"fix: retry in a moment, or pass --now to stop it anyway\n", cmd, inspTilde(c.Layout.DB()), err)
		return false
	case running.none():
		return true
	}
	fmt.Fprintf(c.Stderr, "magnum %s: %s in flight: %s\n%s\nfix: %s\n", cmd, running.count("review round(s)"), running,
		running.cutShort(), roundsFix(cmd))
	return false
}

// roundsFix is the fix line of a stop refused while rounds or background
// work run: the ways cmd has to wait for them.
func roundsFix(cmd string) string {
	switch cmd {
	case "daemon-restart":
		return "`magnum daemon-restart --when-idle` restarts once nothing is in flight (stopping nothing), --drain also " +
			"starts nothing new meanwhile, --now interrupts them"
	case "install":
		return "`magnum install --drain` starts nothing new and waits for these, --now interrupts them"
	}
	return "wait for them (`magnum status --watch`), or pass --now to interrupt"
}

// daemonRunning reports whether a daemon runs now: launchd's job is running,
// or the pidfile names a live process. install and uninstall only guard
// rounds in flight when a daemon could be abandoning them (after a crash the
// registry may still show stale in-flight states, which the next daemon
// recovers).
func daemonRunning(ctx context.Context, c *Context, run execx.Runner) bool {
	if st, err := launchd.Status(ctx, run, daemonSys.UID(), launchd.DefaultLabel); err == nil && st.State == launchd.Running && st.PID > 0 {
		return true
	}
	pid, err := daemonSys.DaemonPID(c.Layout)
	return err != nil || pid > 0 // an unreadable pidfile counts as running (fail closed)
}
