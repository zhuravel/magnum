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
	detail := checkBuildConfig(ctx, c, run, bin)
	if detail == "" {
		return true
	}
	fmt.Fprintf(c.Stderr, "magnum %s: %s rejects the configuration, so the daemon would not start on it:\n%s\n"+
		"fix: correct ~/.config/magnum/config.toml or the prompt files (or rebuild with `make build`), "+
		"check with `%s config`, then retry\n", cmd, inspTilde(bin), detail, inspTilde(bin))
	return false
}

// checkBuildConfig runs `<bin> config` (with the --config and MAGNUM_HOME
// the daemon gets) and returns why bin rejects the configuration, redacted;
// "" when it accepts it.
func checkBuildConfig(ctx context.Context, c *Context, run execx.Runner, bin string) string {
	args := []string{"config"}
	if f := daemonConfigOverride(c); f != "" {
		args = append(args, "--config", f)
	}
	res, err := run.Run(ctx, execx.Cmd{
		Name: bin, Args: args, Env: buildCheckEnv(c),
		Timeout: verifyBuildTimeout, Label: "magnum config (the build that will run)",
	})
	if err == nil {
		return ""
	}
	detail := strings.TrimSpace(string(res.Stderr))
	var xe *execx.ExitError
	if detail == "" && errors.As(err, &xe) {
		detail = strings.TrimSpace(xe.Stderr)
	}
	if detail == "" {
		detail = err.Error()
	}
	return execx.Redact(detail)
}

// buildCheckEnv is the environment a build check runs bin in: the layout
// the daemon will use.
func buildCheckEnv(c *Context) map[string]string {
	env := map[string]string{}
	if c.Layout.CheckoutLayout() && c.Layout.Home != "" {
		env["MAGNUM_HOME"] = c.Layout.Home
	}
	return env
}

// newBuildCheck is the daemon's check of a new binary on disk before it
// restarts on it ([daemon] restart_on_new_build): `<bin> version` names the
// build, `<bin> config` must accept the configuration (verifyBuild's check).
func newBuildCheck(c *Context, run execx.Runner) func(ctx context.Context, bin string) (string, error) {
	return func(ctx context.Context, bin string) (string, error) {
		res, err := run.Run(ctx, execx.Cmd{Name: bin, Args: []string{"version"}, Env: buildCheckEnv(c),
			Timeout: verifyBuildTimeout, Label: "magnum version (a new build)"})
		if err != nil {
			return "", fmt.Errorf("%s version: %w", bin, err)
		}
		version := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(res.Out()), "magnum"))
		if detail := checkBuildConfig(ctx, c, run, bin); detail != "" {
			return version, fmt.Errorf("%s rejects the configuration: %s", bin, detail)
		}
		return version, nil
	}
}

// registryWriteDSN opens the registry at path read-write without creating
// or migrating it: --drain only sets one kv row of the running daemon's
// registry, whatever schema version this binary expects.
func registryWriteDSN(path string) string {
	u := url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: "mode=rw&_pragma=busy_timeout(5000)"}
	return u.String()
}

// startDrain sets engine.KVDaemonDraining to a drain owned by this process
// (its pid: the daemon lifts the drain of a drainer that is gone) and records
// a daemon.drain_started event. It returns the value it wrote.
func (c *Context) startDrain(ctx context.Context, cmd string) (string, error) {
	now := time.Now()
	value := engine.Drain{Since: now, PID: os.Getpid()}.Value()
	err := c.withRegistry(ctx, func(ctx context.Context, db *sql.DB) error {
		at := store.FormatTime(now)
		if _, err := db.ExecContext(ctx, `INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, engine.KVDaemonDraining, value, at); err != nil {
			return err
		}
		drainEvent(ctx, db, "daemon.drain_started",
			fmt.Sprintf("drain started by `magnum %s --drain` (pid %d): no new round starts until the restart", cmd, os.Getpid()))
		return nil
	})
	return value, err
}

// endDrain deletes the drain this process started (value: startDrain's)
// and records why in a daemon.drain_ended event. A drain already gone (the
// restarted daemon lifts it) or another command's is left alone.
func (c *Context) endDrain(ctx context.Context, value, why string) error {
	return c.withRegistry(ctx, func(ctx context.Context, db *sql.DB) error {
		res, err := db.ExecContext(ctx, "DELETE FROM kv WHERE key = ? AND value = ?", engine.KVDaemonDraining, value)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			drainEvent(ctx, db, "daemon.drain_ended", "drain ended: "+why)
		}
		return nil
	})
}

// withRegistry runs fn on the registry opened read-write without creating or
// migrating it, with a 10 s timeout.
func (c *Context) withRegistry(ctx context.Context, fn func(context.Context, *sql.DB) error) error {
	db, err := sql.Open("sqlite", registryWriteDSN(c.Layout.DB()))
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return fn(ctx, db)
}

// drainEvent appends a daemon-level event (the columns every schema has).
// It is the record, not the drain: a failed insert is ignored.
func drainEvent(ctx context.Context, db *sql.DB, kind, msg string) {
	_, _ = db.ExecContext(ctx, "INSERT INTO events (at, level, kind, message) VALUES (?, 'info', ?, ?)",
		store.FormatTime(time.Now()), kind, msg)
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
	value, err := c.startDrain(ctx, cmd)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum %s: cannot start the drain (writing %s: %v)\nfix: retry, or pass --now to restart anyway\n",
			cmd, inspTilde(c.Layout.DB()), err)
		return func() {}, false
	}
	lift := func(why string) {
		if err := c.endDrain(context.WithoutCancel(ctx), value, why); err != nil {
			fmt.Fprintf(c.Stderr, "magnum %s: warning: could not lift the drain (%v); the daemon lifts it once this command is gone\n", cmd, err)
		}
	}
	fmt.Fprintf(c.Stdout, "draining (pid %d): no new rounds start; waiting for %d round(s) in flight (at most %s): %s\n",
		os.Getpid(), len(rounds), timeout, strings.Join(rounds, ", "))
	var waited, sinceProgress time.Duration
	for {
		if ctx.Err() != nil {
			lift(fmt.Sprintf("`magnum %s --drain` was interrupted; the daemon was not restarted", cmd))
			fmt.Fprintf(c.Stderr, "magnum %s: interrupted after %s; the drain is lifted and the daemon was not restarted\n", cmd, waited)
			return func() {}, false
		}
		if waited >= timeout {
			lift(fmt.Sprintf("`magnum %s --drain` gave up after %s; the daemon was not restarted", cmd, timeout))
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
			return func() { lift(fmt.Sprintf("`magnum %s --drain` finished", cmd)) }, true
		}
		if sinceProgress >= drainProgressEvery {
			sinceProgress = 0
			fmt.Fprintf(c.Stdout, "draining: %d round(s) in flight after %s: %s\n", len(rounds), waited, strings.Join(rounds, ", "))
		}
	}
}

// waitIdle waits, without a drain and holding nothing, until no round is
// claiming, reviewing or verifying, printing a progress line every
// drainProgressEvery, for at most timeout. False when it gave up (timeout,
// interrupt).
func waitIdle(ctx context.Context, c *Context, cmd string, timeout time.Duration) bool {
	var waited, sinceProgress time.Duration
	announced := false
	for {
		rounds, err := c.activeRounds(ctx)
		switch {
		case err == nil && len(rounds) == 0:
			if announced {
				fmt.Fprintf(c.Stdout, "idle after %s: no round in flight\n", waited)
			}
			return true
		case err == nil && !announced:
			announced = true
			fmt.Fprintf(c.Stdout, "waiting for %d round(s) in flight to end, starting none of its own (at most %s): %s\n",
				len(rounds), timeout, strings.Join(rounds, ", "))
		case err == nil && sinceProgress >= drainProgressEvery:
			sinceProgress = 0
			fmt.Fprintf(c.Stdout, "waiting: %d round(s) in flight after %s: %s\n", len(rounds), waited, strings.Join(rounds, ", "))
		}
		if ctx.Err() != nil {
			fmt.Fprintf(c.Stderr, "magnum %s: stopped waiting after %s; the daemon was not restarted\n", cmd, waited)
			return false
		}
		if waited >= timeout {
			fmt.Fprintf(c.Stderr, "magnum %s: rounds were still in flight after %s; the daemon was not restarted\n"+
				"fix: retry with a longer --timeout, --drain to stop new rounds meanwhile, or --now to restart anyway (the rounds start over)\n",
				cmd, timeout)
			return false
		}
		daemonSys.Sleep(drainPollEvery)
		waited += drainPollEvery
		sinceProgress += drainPollEvery
	}
}

// guardRounds is what daemon-restart, install and migrate-home do about
// rounds in flight before they stop the daemon: refuse (default), ignore
// them (--now), drain them (--drain), or wait until none runs (--when-idle;
// the caller checks again right before the restart, see restartWhenIdle).
// The returned function ends a drain; ok false means the command stops.
// Without a running daemon there is nothing to drain or wait for: rounds the
// registry still shows in flight are a crashed daemon's, which the next one
// recovers.
func (c *Context) guardRounds(ctx context.Context, run execx.Runner, cmd string, f restartFlags) (func(), bool) {
	switch {
	case (f.drain || f.whenIdle) && !daemonRunning(ctx, c, run):
		return func() {}, true
	case f.drain:
		return drainRounds(ctx, c, cmd, f.timeout)
	case f.whenIdle:
		return func() {}, waitIdle(ctx, c, cmd, f.timeout)
	}
	return func() {}, c.refuseWhileRoundsRun(cmd, f.now)
}
