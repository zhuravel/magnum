package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
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

// inFlight is what stopping the daemon now would cut short: the review
// rounds in flight and the background work running (the notes curation,
// engine.KVNotesCurating, and the retro, engine.KVRetroRunning).
type inFlight struct {
	rounds []string // "owner/repo#N (state)"
	work   []string // "notes curation of owner/repo (since 15:04)", "retro (since 15:04)"
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
		parts = append(parts, "cuts the background work short (a notes curation proposes nothing, a retro runs again)")
	}
	return "stopping the daemon now " + strings.Join(parts, " and ")
}

// activeRounds reads what is in flight, read-only: the PRs whose review
// round is claiming, reviewing or verifying, as "owner/repo#N (state)", and
// the notes curation and the retro running (backgroundWork). Stopping the
// daemon abandons the rounds (their agents keep working in herdr but nobody
// collects the result, and the next daemon starts them over) and cuts the
// curation and the retro short. No registry yet means nothing; any other
// read failure is an error, so callers fail closed.
func (c *Context) activeRounds(ctx context.Context) (inFlight, error) {
	path := c.Layout.DB()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return inFlight{}, nil
	} else if err != nil {
		return inFlight{}, err
	}
	db, err := sql.Open("sqlite", registryReadOnlyDSN(path))
	if err != nil {
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

// backgroundWork reads the notes curation and the retro running from their
// kv marks: "notes curation of owner/repo (since 15:04)", "retro (since
// 15:04)". A mark it cannot parse still counts.
func backgroundWork(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM kv WHERE key IN (?, ?) AND value != '' ORDER BY key`,
		engine.KVNotesCurating, engine.KVRetroRunning)
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
		what, since := "retro", time.Time{}
		switch key {
		case engine.KVNotesCurating:
			what = "notes curation"
			var m engine.CurateMark
			if json.Unmarshal([]byte(value), &m) == nil && m.Repo != "" {
				what, since = "notes curation of "+m.Repo, m.Started
			}
		default:
			if t, err := store.ParseTime(value); err == nil {
				since = t
			}
		}
		if !since.IsZero() {
			what += " (since " + inspClock(now, since) + ")"
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
