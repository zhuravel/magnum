package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/paths"
)

const migrateHomeUsage = "migrate-home [--now | --drain [--timeout D]] [--dry-run]"

func newMigrateHomeCmd(c *Context) *cobra.Command {
	var f restartFlags
	var dry bool
	cmd := newCommand(groupDaemon, migrateHomeUsage,
		"move the registry, reviews, notes, logs and gh config dirs from the checkout's state/ to ~/.local/share and ~/.local/state",
		"Move what magnum keeps in a checkout's state/ to where an installed magnum keeps it: the registry, review "+
			"reports and notes to $XDG_DATA_HOME/magnum (~/.local/share/magnum), the logs, the identities' gh config "+
			"dirs, the skill copies and the tab-bar file to $XDG_STATE_HOME/magnum (~/.local/state/magnum). The daemon "+
			"is stopped first (rounds in flight: refused, --now abandons them, --drain waits), every item is renamed "+
			"(nothing is copied, nothing is linked back; a failed rename puts the moved ones back), the stale pidfile "+
			"and locks are removed, an emptied state/ is removed and anything left in it is listed. A loaded launchd job "+
			"is rewritten for the new places and started again. Prompts carry the new paths from the next round on.",
		func(pos []string) int { return runMigrateHome(c, pos, f, dry) })
	addRestartFlags(cmd, &f)
	cmd.Flags().BoolVar(&dry, "dry-run", false, "print what would move and change nothing")
	return cmd
}

// homeMove is one item migrate-home renames.
type homeMove struct{ from, to string }

// homeMoves are the items of src's state/ that move to dst: the data first
// (the registry last of it, so a failure leaves the registry where it was),
// then the state.
func homeMoves(src, dst paths.Layout) []homeMove {
	var out []homeMove
	add := func(name, dir string) {
		if from := filepath.Join(src.State(), name); fileExists(from) {
			out = append(out, homeMove{from, filepath.Join(dir, name)})
		}
	}
	for _, n := range []string{"logs", "gh", "skill", "eval", "tabbar.txt"} {
		add(n, dst.State())
	}
	for _, n := range []string{"reviews", "notes", "magnum.db-wal", "magnum.db-shm", "magnum.db"} {
		add(n, dst.Data())
	}
	return out
}

// homeRuntime are the files a stopped daemon leaves that the new place
// recreates: removed, not moved.
var homeRuntime = []string{"daemon.pid", "magnum.lock", "ops.lock"}

func runMigrateHome(c *Context, pos []string, f restartFlags, dry bool) int {
	if !daemonGroupNoArgs(c, "migrate-home", migrateHomeUsage, pos) || !f.check(c, "migrate-home", migrateHomeUsage) {
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(c.Stderr, "magnum migrate-home: "+format+"\n", a...)
		return 1
	}
	src := c.Layout
	if !src.CheckoutLayout() || src.Home == "" {
		fmt.Fprintf(c.Stdout, "nothing to move: the registry is in %s already\n", inspTilde(src.Data()))
		return 0
	}
	if daemonSys.Getenv("MAGNUM_HOME") != "" {
		return fail("MAGNUM_HOME is set, which keeps everything in %s\nfix: unset MAGNUM_HOME (and drop it from your shell config), then re-run magnum migrate-home", inspTilde(src.Home))
	}
	if !fileExists(src.DB()) {
		return fail("no registry at %s", inspTilde(src.DB()))
	}
	dst, err := src.Installed()
	if err != nil {
		return fail("%v", err)
	}
	if fileExists(dst.DB()) {
		return fail("%s holds a registry already\nfix: move one of the two registries aside by hand; magnum uses %s while it exists", inspTilde(dst.DB()), inspTilde(dst.DB()))
	}
	if err := checkMovable(homeMoves(src, dst)); err != nil {
		return fail("%v", err)
	}
	if dry {
		for _, m := range homeMoves(src, dst) {
			fmt.Fprintf(c.Stdout, "would move %s → %s\n", inspTilde(m.from), inspTilde(m.to))
		}
		fmt.Fprintf(c.Stdout, "would remove %s's stale pidfile and locks, and %s once empty\n", inspTilde(src.State()), inspTilde(src.State()))
		return 0
	}

	ctx := context.Background()
	run := daemonSys.runner(false, c.Stdout)
	endDrain, ok := c.guardRounds(ctx, run, "migrate-home", f.now, f.drain, f.timeout)
	if !ok {
		return 1
	}
	uid, label := daemonSys.UID(), launchd.DefaultLabel
	st, statusErr := launchd.Status(ctx, run, uid, label)
	loaded := statusErr == nil && st.Loaded()
	if statusErr != nil || loaded {
		if err := launchd.Bootout(ctx, run, uid, label); err != nil {
			endDrain()
			return fail("stop the daemon: %v", err)
		}
	}
	if pid, err := daemonSys.DaemonPID(src); err == nil && pid > 0 && pid != st.PID {
		if err := stopMagnumDaemon(ctx, run, src, pid); err != nil {
			endDrain()
			return fail("stop the daemon running outside launchd: %v", err)
		}
	}
	endDrain() // while the registry is still where this command reads it (a starting daemon lifts it too)

	// With the daemon stopped, fold the write-ahead log into the registry:
	// the last read-write connection's close then removes -wal and -shm, so
	// the registry moves as one file (a WAL left behind would lose its writes).
	if err := checkpointRegistry(ctx, src.DB()); err != nil {
		return fail("checkpoint %s: %v\nnothing moved; fix: retry, or `magnum install` to start the daemon again", inspTilde(src.DB()), err)
	}
	moves := homeMoves(src, dst) // now: this command's own reads may have added files
	if err := checkMovable(moves); err != nil {
		return fail("%v", err)
	}
	if err := moveHome(moves); err != nil {
		code := fail("%v\nnothing moved: every item is back in %s", err, inspTilde(src.State()))
		if loaded { // load the unchanged plist again
			path := launchd.AgentPath(mustUserHome(), label)
			b, rerr := os.ReadFile(path)
			if rerr == nil {
				rerr = launchd.Install(ctx, run, uid, path, b)
			}
			if rerr != nil {
				fmt.Fprintf(c.Stderr, "magnum migrate-home: start the daemon again: %v\nfix: magnum install\n", rerr)
			}
		}
		return code
	}
	for _, m := range moves {
		fmt.Fprintf(c.Stdout, "moved %s → %s\n", inspTilde(m.from), inspTilde(m.to))
	}
	for _, n := range homeRuntime {
		_ = os.Remove(filepath.Join(src.State(), n))
	}
	reportLeftovers(c, src.State())
	noteTabBar(c, src, dst)

	c.Layout = dst
	if c.Config != nil {
		c.Config.Layout = dst
	}
	if !loaded {
		fmt.Fprintln(c.Stdout, "launchd: the job was not loaded; `magnum install` writes it for the new places and starts it")
		return 0
	}
	return installLaunchAgent(ctx, c, run, false, restartFlags{now: true, timeout: f.timeout})
}

// checkMovable refuses a move onto something that exists.
func checkMovable(moves []homeMove) error {
	for _, m := range moves {
		if fileExists(m.to) {
			return fmt.Errorf("%s exists already; not moving %s over it\nfix: remove or rename it, then re-run magnum migrate-home", inspTilde(m.to), inspTilde(m.from))
		}
	}
	return nil
}

// checkpointRegistry writes the registry's WAL into the database and
// truncates it.
func checkpointRegistry(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", registryWriteDSN(path))
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// moveHome renames every item; on a failure it renames the moved ones back
// and reports which rename failed.
func moveHome(moves []homeMove) error {
	var done []homeMove
	undo := func() {
		for _, m := range slices.Backward(done) {
			_ = os.Rename(m.to, m.from)
		}
	}
	for _, m := range moves {
		if err := os.MkdirAll(filepath.Dir(m.to), 0o700); err != nil {
			undo()
			return fmt.Errorf("create %s: %w", inspTilde(filepath.Dir(m.to)), err)
		}
		if err := os.Rename(m.from, m.to); err != nil {
			undo()
			if errors.Is(err, syscall.EXDEV) {
				return fmt.Errorf("%s and %s are on different volumes, which a rename cannot cross\nfix: move the items by hand (mv), then start magnum", inspTilde(m.from), inspTilde(filepath.Dir(m.to)))
			}
			return fmt.Errorf("move %s: %w", inspTilde(m.from), err)
		}
		done = append(done, m)
	}
	return nil
}

// reportLeftovers removes dir when nothing is left in it and lists what is
// otherwise: files magnum does not own (backups, a dev tool's directory).
func reportLeftovers(c *Context, dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	if len(ents) == 0 {
		if os.Remove(dir) == nil {
			fmt.Fprintf(c.Stdout, "removed the emptied %s\n", inspTilde(dir))
		}
		return
	}
	var names []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() {
			n += "/"
		}
		names = append(names, n)
	}
	fmt.Fprintf(c.Stdout, "left in %s (not magnum's runtime files; delete what you do not need): %s\n", inspTilde(dir), strings.Join(names, ", "))
}

// noteTabBar says how to point herdr's tab bar at the moved file when its
// config still reads the old one (magnum never edits herdr's config).
func noteTabBar(c *Context, src, dst paths.Layout) {
	home, err := daemonSys.UserHome()
	if err != nil {
		return
	}
	cfg := filepath.Join(home, ".config", "herdr", "config.toml")
	b, err := os.ReadFile(cfg)
	if err != nil || !strings.Contains(string(b), src.TabBar()) {
		return
	}
	fmt.Fprintf(c.Stdout, "herdr's tab bar still reads %s: in %s replace it with %s\n", inspTilde(src.TabBar()), inspTilde(cfg), inspTilde(dst.TabBar()))
}

// mustUserHome is the user's home for the plist path; "" when unknown.
func mustUserHome() string {
	h, _ := daemonSys.UserHome()
	return h
}
