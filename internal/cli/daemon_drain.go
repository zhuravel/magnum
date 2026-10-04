package cli

// Restarts that do not abandon rounds (daemon-restart --drain, install
// --drain) and the skew guard: the binary launchd starts must accept the
// configuration and render every prompt before the daemon is restarted on it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	// defaultDrainTimeout bounds how long --drain waits for rounds in flight.
	defaultDrainTimeout = 2 * time.Hour
	drainPollEvery      = 5 * time.Second
	drainProgressEvery  = 15 * time.Second
	// verifyBuildTimeout bounds `bin/magnum config`.
	verifyBuildTimeout = time.Minute
)

// verifyBuild runs `<home>/bin/magnum config` (with the same --config the
// daemon gets), the binary launchd starts, so a configuration it rejects or
// a prompt file it cannot render is refused before the daemon is restarted
// on it (the daemon itself would refuse to start and launchd would restart
// it in a loop). A missing binary is only warned about here: install
// refuses it, and launchd reports it on a restart.
func verifyBuild(ctx context.Context, c *Context, run execx.Runner, cmd string) bool {
	bin := c.Layout.Binary()
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(c.Stderr, "magnum %s: warning: %s is missing; the configuration was not checked with the build that will run\n", cmd, bin)
		return true
	}
	args := []string{"config"}
	if f := daemonConfigOverride(c); f != "" {
		args = append(args, "--config", f)
	}
	res, err := run.Run(ctx, execx.Cmd{
		Name: bin, Args: args, Env: map[string]string{"MAGNUM_HOME": c.Layout.Home},
		Timeout: verifyBuildTimeout, Label: "magnum config (the build that will run)",
	})
	if err == nil {
		return true
	}
	detail := strings.TrimSpace(string(res.Stderr))
	var xe *execx.ExitError
	if detail == "" && errors.As(err, &xe) {
		detail = strings.TrimSpace(xe.Stderr)
	}
	if detail == "" {
		detail = err.Error()
	}
	fmt.Fprintf(c.Stderr, "magnum %s: %s rejects the configuration, so the daemon would not start on it:\n%s\n"+
		"fix: correct config.toml, config.local.toml or the prompt files (or rebuild with `make build`), "+
		"check with `%s config`, then retry\n", cmd, inspTilde(bin), execx.Redact(detail), inspTilde(bin))
	return false
}

// registryWriteDSN opens the registry at path read-write without creating
// or migrating it: --drain only sets one kv row of the running daemon's
// registry, whatever schema version this binary expects.
func registryWriteDSN(path string) string {
	u := url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: "mode=rw&_pragma=busy_timeout(5000)"}
	return u.String()
}

// setDraining sets (on) or deletes (off) engine.KVDaemonDraining.
func (c *Context) setDraining(ctx context.Context, on bool) error {
	db, err := sql.Open("sqlite", registryWriteDSN(c.Layout.DB()))
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if !on {
		_, err = db.ExecContext(ctx, "DELETE FROM kv WHERE key = ?", engine.KVDaemonDraining)
		return err
	}
	now := store.FormatTime(time.Now())
	_, err = db.ExecContext(ctx, `INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, engine.KVDaemonDraining, now, now)
	return err
}

// drainRounds stops the daemon from starting new rounds
// (engine.KVDaemonDraining) and waits until no round is in flight, printing
// a progress line every drainProgressEvery, for at most timeout. It returns
// a function that lifts the drain (the new daemon also lifts it when it
// starts), and false when it gave up (timeout, interrupt, unreadable
// registry), with the drain already lifted.
func drainRounds(ctx context.Context, c *Context, cmd string, timeout time.Duration) (func(), bool) {
	rounds, err := c.activeRounds(ctx)
	switch {
	case err != nil:
		fmt.Fprintf(c.Stderr, "magnum %s: cannot tell whether review rounds are in flight (reading %s: %v)\n"+
			"fix: retry in a moment, or pass --now to restart anyway\n", cmd, inspTilde(c.Layout.DB()), err)
		return func() {}, false
	case len(rounds) == 0:
		return func() {}, true
	}
	if err := c.setDraining(ctx, true); err != nil {
		fmt.Fprintf(c.Stderr, "magnum %s: cannot start the drain (writing %s: %v)\nfix: retry, or pass --now to restart anyway\n",
			cmd, inspTilde(c.Layout.DB()), err)
		return func() {}, false
	}
	lift := func() {
		if err := c.setDraining(context.WithoutCancel(ctx), false); err != nil {
			fmt.Fprintf(c.Stderr, "magnum %s: warning: could not lift the drain (%v); the next daemon start lifts it\n", cmd, err)
		}
	}
	fmt.Fprintf(c.Stdout, "draining: no new rounds start; waiting for %d round(s) in flight (at most %s): %s\n",
		len(rounds), timeout, strings.Join(rounds, ", "))
	var waited, sinceProgress time.Duration
	for {
		if ctx.Err() != nil {
			lift()
			fmt.Fprintf(c.Stderr, "magnum %s: interrupted after %s; the drain is lifted and the daemon was not restarted\n", cmd, waited)
			return func() {}, false
		}
		if waited >= timeout {
			lift()
			fmt.Fprintf(c.Stderr, "magnum %s: %d round(s) still in flight after %s: %s\n"+
				"the drain is lifted and the daemon was not restarted\n"+
				"fix: retry with a longer --timeout, or pass --now to restart anyway (the rounds start over)\n",
				cmd, len(rounds), timeout, strings.Join(rounds, ", "))
			return func() {}, false
		}
		daemonSys.Sleep(drainPollEvery)
		waited += drainPollEvery
		sinceProgress += drainPollEvery
		if rounds, err = c.activeRounds(ctx); err != nil {
			continue // a busy registry: try again on the next poll
		}
		if len(rounds) == 0 {
			fmt.Fprintf(c.Stdout, "drained after %s: no round in flight\n", waited)
			return lift, true
		}
		if sinceProgress >= drainProgressEvery {
			sinceProgress = 0
			fmt.Fprintf(c.Stdout, "draining: %d round(s) in flight after %s: %s\n", len(rounds), waited, strings.Join(rounds, ", "))
		}
	}
}

// guardRounds is what daemon-restart and install do about rounds in flight
// before they stop the daemon: refuse (default), ignore them (--now), or
// drain them (--drain). The returned function ends a drain; ok false means
// the command stops.
// Without a running daemon there is nothing to drain: rounds the registry
// still shows in flight are a crashed daemon's, which the next one recovers.
func (c *Context) guardRounds(ctx context.Context, run execx.Runner, cmd string, now, drain bool, timeout time.Duration) (func(), bool) {
	switch {
	case drain && !daemonRunning(ctx, c, run):
		return func() {}, true
	case drain:
		return drainRounds(ctx, c, cmd, timeout)
	}
	return func() {}, c.refuseWhileRoundsRun(cmd, now)
}
