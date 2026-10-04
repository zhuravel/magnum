package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/launchd"
)

// registryReadOnlyDSN is the sqlite DSN that opens the registry at path
// read-only, never creating it. The path is URI-escaped, so a home
// directory containing '?', '#' or '%' still opens the right file.
func registryReadOnlyDSN(path string) string {
	u := url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(1000)"}
	return u.String()
}

// activeRounds lists the PRs whose review round is in flight (claiming,
// reviewing or verifying), as "owner/repo#N (state)", read-only. Stopping the
// daemon abandons those rounds: their agents keep working in herdr but nobody
// collects the result, and the next daemon starts them over. No registry yet
// means no rounds; any other read failure is an error, so callers fail closed.
func (c *Context) activeRounds(ctx context.Context) ([]string, error) {
	path := c.Layout.DB()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", registryReadOnlyDSN(path))
	if err != nil {
		return nil, err
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
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var owner, name, state string
		var n int
		if err := rows.Scan(&owner, &name, &n, &state); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s/%s#%d (%s)", owner, name, n, state))
	}
	return out, rows.Err()
}

// refuseWhileRoundsRun prints the rounds in flight and returns false when
// the command must not stop the daemon now (--now overrides). It fails
// closed: when the registry cannot be read it refuses too.
func (c *Context) refuseWhileRoundsRun(cmd string, now bool) bool {
	if now {
		return true
	}
	rounds, err := c.activeRounds(context.Background())
	switch {
	case err != nil:
		fmt.Fprintf(c.Stderr, "magnum %s: cannot tell whether review rounds are in flight (reading %s: %v)\n"+
			"stopping the daemon now may abandon them\n"+
			"fix: retry in a moment, or pass --now to stop it anyway\n", cmd, inspTilde(c.Layout.DB()), err)
		return false
	case len(rounds) == 0:
		return true
	}
	fmt.Fprintf(c.Stderr, "magnum %s: %d review round(s) in flight: %s\n"+
		"stopping the daemon now abandons them (their agents keep running, the rounds start over later)\n"+
		"fix: wait for them (`magnum status --watch`), or pass --now to interrupt\n",
		cmd, len(rounds), strings.Join(rounds, ", "))
	return false
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
