package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/paths"
)

const installUsage = "install [--plugin] [--gh] [--no-launchd] [--now | --drain [--timeout D]] [--dry-run]"

func newInstallCmd(c *Context) *cobra.Command {
	var f installFlags
	cmd := newCommand(groupDaemon, installUsage, "create state dirs, load the launchd agent, optionally link the herdr plugin",
		"Create the state directories, write the launchd agent (~/Library/LaunchAgents/zhuravel.magnum.plist, "+
			"running `magnum daemon`, through `mise exec` while an App key comes from mise) and load it, which also restarts a "+
			"running daemon on the new binary. --plugin links this checkout as the herdr plugin; --gh writes the "+
			"gh extension shim gh-magnum, so `gh magnum <command>` runs this checkout's binary (gh-dash keybindings "+
			"use it, see docs/gh-dash.yml); --dry-run prints what would be written and run. `make install` runs "+
			"`install --plugin`; `install --gh --no-launchd` only writes the shim. bin/magnum checks the "+
			"configuration and renders every prompt first (`bin/magnum config`); the install is refused when it "+
			"fails. Reloading the agent restarts a running daemon, so while review rounds are in flight it is "+
			"refused (it abandons them); --drain stops new rounds and waits for those in flight (at most "+
			"--timeout), --now installs at once.",
		func(pos []string) int { return runInstallCmd(c, f, pos) })
	fs := cmd.Flags()
	fs.BoolVar(&f.plugin, "plugin", false, "also link this checkout as the herdr plugin "+pluginID)
	fs.BoolVar(&f.gh, "gh", false, "also write the gh extension shim (gh magnum <command>)")
	fs.BoolVar(&f.noLaunchd, "no-launchd", false, "do not write or load the launchd agent")
	fs.BoolVar(&f.dry, "dry-run", false, "print what would be written and run; change nothing")
	addRestartFlags(cmd, &f.restart)
	return cmd
}

// installFlags are the parsed `magnum install` flags.
type installFlags struct {
	plugin, gh, noLaunchd, dry bool
	restart                    restartFlags
}

func runInstallCmd(c *Context, f installFlags, pos []string) int {
	plugin, noLaunchd, dry := f.plugin, f.noLaunchd, f.dry
	if !daemonGroupNoArgs(c, "install", installUsage, pos) || !f.restart.check(c, "install", installUsage) {
		return 2
	}
	if !loadDaemonGroupConfig(c, "install") {
		return 1
	}
	ctx, stop := signalContext()
	defer stop()
	out := c.Stdout
	run := daemonSys.runner(dry, out)
	fail := func(format string, a ...any) int {
		fmt.Fprintf(c.Stderr, "magnum install: "+format+"\n", a...)
		return 1
	}

	// 1. State tree (launchd creates the log file but not its directory).
	if dry {
		fmt.Fprintf(out, "dry-run: would create %s (logs, reviews, gh config dirs)\n", c.Layout.State())
	} else {
		if err := c.Layout.EnsureDirs(); err != nil {
			return fail("create %s: %v\nfix: make the repository checkout writable, then re-run magnum install", c.Layout.State(), err)
		}
		fmt.Fprintf(out, "state: %s\n", c.Layout.State())
	}

	// 2. launchd agent.
	if !noLaunchd {
		if code := installLaunchAgent(ctx, c, run, dry, f.restart); code != 0 {
			return code
		}
	}

	// 3. herdr plugin.
	if plugin {
		if err := linkMagnumPlugin(ctx, c, run, dry); err != nil {
			return fail("%v", err)
		}
	}

	// 4. gh extension shim.
	if f.gh {
		if err := installGhShim(ctx, c, run, dry); err != nil {
			return fail("%v", err)
		}
	}

	fmt.Fprintln(out)
	fmt.Fprint(out, tabBarSnippet(c.Layout.TabBar()))
	return 0
}

// installLaunchAgent writes the plist and (re)loads it; under dry-run it
// prints the plist and the launchctl calls instead. The binary it loads
// must accept the configuration (verifyBuild). Reloading restarts a running
// daemon, so a rebuilt binary takes effect; while review rounds are in
// flight that is refused unless --now, or waited for with --drain.
func installLaunchAgent(ctx context.Context, c *Context, run execx.Runner, dry bool, rf restartFlags) int {
	out := c.Stdout
	bin := c.Layout.Binary()
	if _, err := os.Stat(bin); err != nil {
		msg := fmt.Sprintf("%s is missing\nfix: install magnum (brew install zhuravel/tap/magnum), or build it with `make build` in the checkout, then re-run magnum install", bin)
		if !dry {
			fmt.Fprintln(c.Stderr, "magnum install: "+msg)
			return 1
		}
		fmt.Fprintln(out, "warning: "+msg)
	}
	userHome, err := daemonSys.UserHome()
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum install: find your home directory: %v\nfix: set HOME, then re-run magnum install\n", err)
		return 1
	}
	prefix := launchPrefix(c.Layout, c.Config, resolveMisePath(ctx, run))
	plist := launchd.Plist(magnumPlistOptions(c.Layout, userHome, prefix, c.Config.Herdr.Socket, daemonConfigOverride(c)))
	label := launchd.DefaultLabel
	path := launchd.AgentPath(userHome, label)
	uid := daemonSys.UID()

	if !verifyBuild(ctx, c, run, "install") {
		return 1
	}
	if dry {
		fmt.Fprintf(out, "dry-run: would write %s:\n\n%s\n", path, plist)
		fmt.Fprintf(out, "dry-run: would run launchctl bootout gui/%d/%s, launchctl enable gui/%d/%s, launchctl bootstrap gui/%d %s\n",
			uid, label, uid, label, uid, path)
		return 0
	}
	if rf.drain {
		endDrain, ok := c.guardRounds(ctx, run, "install", restartFlags{drain: true, timeout: rf.timeout})
		if !ok {
			return 1
		}
		defer endDrain()
	} else if daemonRunning(ctx, c, run) && !c.refuseWhileRoundsRun("install", rf.now) {
		return 1
	}
	// A daemon started by hand (e.g. by `mise run dev`) holds the lock, so
	// launchd's copy would exit at once as "already running": stop it first.
	if pid, perr := daemonSys.DaemonPID(c.Layout); perr == nil && pid > 0 {
		if st, serr := launchd.Status(ctx, run, uid, label); serr != nil || !st.Loaded() || st.PID != pid {
			if err := stopMagnumDaemon(ctx, run, c.Layout, pid); err != nil {
				fmt.Fprintf(c.Stderr, "magnum install: %v\n", err)
				return 1
			}
			fmt.Fprintf(out, "stopped the daemon running outside launchd (pid %d)\n", pid)
		}
	}
	if err := launchd.Install(ctx, run, uid, path, plist); err != nil {
		fmt.Fprintf(c.Stderr, "magnum install: %v\nfix: check `launchctl print gui/%d/%s`, %s and %s, then re-run magnum install\n",
			err, uid, label, c.Layout.DaemonLog(), launchd.LogPath(c.Layout.Logs()))
		return 1
	}
	how := "the binary directly"
	if len(prefix) > 0 {
		how = "through `" + strings.Join(prefix, " ") + "` (an App key comes from mise)"
	}
	fmt.Fprintf(out, "launchd: loaded %s from %s, running %s\n", label, path, how)
	if st, err := launchd.Status(ctx, run, uid, label); err == nil {
		fmt.Fprintf(out, "launchd: %s\n", describeLaunchdState(st))
	}
	fmt.Fprintf(out, "logs: %s (launchd's stdout/stderr: %s)\n", c.Layout.DaemonLog(), launchd.LogPath(c.Layout.Logs()))
	return 0
}

// pluginDir is the herdr plugin magnum links: the checkout when it holds the
// manifest (development), else the copy WritePluginFiles keeps under the
// data directory.
func pluginDir(l paths.Layout) string {
	if p := l.Plugin(); p != "" && fileExists(p) {
		return l.Home
	}
	return filepath.Join(l.Data(), "herdr-plugin")
}

// writePluginFiles writes the embedded herdr plugin (manifest and script)
// into dir, each only when it differs; it reports whether it wrote any.
func writePluginFiles(dir string) (bool, error) {
	wrote := false
	for _, f := range []struct {
		rel  string
		data []byte
		mode os.FileMode
	}{{"herdr-plugin.toml", magnum.PluginManifest, 0o644}, {"scripts/magnum-ctl.sh", magnum.PluginScript, 0o755}} {
		p := filepath.Join(dir, f.rel)
		if cur, err := os.ReadFile(p); err == nil && bytes.Equal(cur, f.data) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return wrote, err
		}
		if err := os.WriteFile(p+".tmp", f.data, f.mode); err != nil {
			return wrote, err
		}
		if err := os.Rename(p+".tmp", p); err != nil {
			return wrote, err
		}
		wrote = true
	}
	return wrote, nil
}

// linkMagnumPlugin links the herdr plugin (pluginDir; an installed binary
// writes its copy first) unless it already is (idempotent: `herdr plugin
// list` first).
func linkMagnumPlugin(ctx context.Context, c *Context, run execx.Runner, dry bool) error {
	out := c.Stdout
	home := pluginDir(c.Layout)
	if home != c.Layout.Home && !dry {
		if _, err := writePluginFiles(home); err != nil {
			return fmt.Errorf("write the herdr plugin to %s: %w\nfix: check that the directory is writable, then re-run magnum install --plugin", home, err)
		}
	}
	socket := c.Config.Herdr.Socket
	p, found, err := findHerdrPlugin(ctx, run, socket, pluginID)
	if err != nil {
		return fmt.Errorf("%w\nfix: start herdr (and check `herdr plugin list`), then re-run magnum install --plugin", err)
	}
	if found {
		if !p.linkedFrom(home) {
			return fmt.Errorf("herdr plugin %s is already installed from %s, not from %s\nfix: herdr plugin unlink %s && magnum install --plugin",
				pluginID, p.Root, home, pluginID)
		}
		fmt.Fprintf(out, "herdr plugin %s: already linked from %s (re-run `herdr plugin link %s` after editing herdr-plugin.toml)\n",
			pluginID, p.Root, home)
		if !p.Enabled {
			fmt.Fprintf(out, "herdr plugin %s is disabled; enable it with `herdr plugin enable %s`\n", pluginID, pluginID)
		}
		return nil
	}
	if _, err := run.Run(ctx, herdrPluginCmd(socket, true, "plugin", "link", home)); err != nil {
		return fmt.Errorf("herdr plugin link %s: %w\nfix: start herdr, then re-run magnum install --plugin", home, err)
	}
	if !dry {
		fmt.Fprintf(out, "herdr plugin %s: linked %s\n", pluginID, home)
	}
	return nil
}

// describeLaunchdState renders a launchd job state for humans.
func describeLaunchdState(st launchd.Info) string {
	switch {
	case !st.Loaded():
		return "not loaded"
	case st.State == launchd.Running && st.PID > 0:
		return fmt.Sprintf("running, pid %d", st.PID)
	case st.Exited:
		return fmt.Sprintf("%s, last exit code %d", st.State, st.LastExitCode)
	default:
		return string(st.State)
	}
}

// ghShimName is the gh extension that runs magnum: `gh magnum <command>`.
const ghShimName = "gh-magnum"

// ghShimMarker is the shim's second line; a gh-magnum without it is not
// magnum's and is left alone.
const ghShimMarker = "# magnum gh extension shim, written by `magnum install --gh`"

// ghExtensionsDir is where gh looks for extensions: $GH_DATA_DIR/extensions,
// else $XDG_DATA_HOME/gh/extensions, else ~/.local/share/gh/extensions.
func ghExtensionsDir(userHome string) string {
	if d := daemonSys.Getenv("GH_DATA_DIR"); d != "" {
		return filepath.Join(paths.Expand(d), "extensions")
	}
	if d := daemonSys.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(paths.Expand(d), "gh", "extensions")
	}
	return filepath.Join(userHome, ".local", "share", "gh", "extensions")
}

// ghShimScript is the shim: run this checkout's binary through mise (as the
// launchd agent does), with every argument passed on.
func ghShimScript(prefix []string, bin, configOverride string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "#!/bin/sh\n%s\n", ghShimMarker)
	if configOverride != "" {
		fmt.Fprintf(&b, "MAGNUM_CONFIG=%s\nexport MAGNUM_CONFIG\n", actShellQuote(configOverride))
	}
	b.WriteString("exec ")
	for _, a := range append(slices.Clone(prefix), bin) {
		b.WriteString(actShellQuote(a) + " ")
	}
	b.WriteString("\"$@\"\n")
	return b.String()
}

// installGhShim writes <gh extensions>/gh-magnum/gh-magnum (0755) unless it
// is already current; a gh-magnum that magnum did not write is refused.
func installGhShim(ctx context.Context, c *Context, run execx.Runner, dry bool) error {
	out := c.Stdout
	userHome, err := daemonSys.UserHome()
	if err != nil {
		return fmt.Errorf("find your home directory: %w\nfix: set HOME, then re-run magnum install --gh", err)
	}
	dir := filepath.Join(ghExtensionsDir(userHome), ghShimName)
	path := filepath.Join(dir, ghShimName)
	bin := c.Layout.Binary()
	script := ghShimScript(launchPrefix(c.Layout, c.Config, resolveMisePath(ctx, run)), bin, daemonConfigOverride(c))
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(out, "warning: %s is missing; build it (`make build`) before running gh magnum\n", bin)
	}

	current := false
	if fi, err := os.Lstat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s exists and is not magnum's shim (an extension installed with `gh extension install`?)\nfix: gh extension remove magnum, then re-run magnum install --gh", dir)
		}
		old, err := os.ReadFile(path)
		switch {
		case err == nil && !bytes.Contains(old, []byte(ghShimMarker)):
			return fmt.Errorf("%s exists and is not magnum's shim\nfix: gh extension remove magnum, then re-run magnum install --gh", path)
		case err == nil:
			fi, serr := os.Stat(path)
			current = string(old) == script && serr == nil && fi.Mode().Perm() == 0o755
		case !os.IsNotExist(err):
			return fmt.Errorf("read %s: %w", path, err)
		}
	}
	switch {
	case current:
		fmt.Fprintf(out, "gh extension: %s is up to date\n", path)
	case dry:
		fmt.Fprintf(out, "dry-run: would write %s (mode 0755):\n\n%s\n", path, script)
		return nil
	default:
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		if err := os.Chmod(path, 0o755); err != nil { // WriteFile keeps an existing file's mode
			return fmt.Errorf("chmod %s: %w", path, err)
		}
		fmt.Fprintf(out, "gh extension: wrote %s\n", path)
	}
	fmt.Fprintf(out, "  gh magnum <command> runs %s: gh magnum prs, gh magnum status <ref>, gh magnum review <ref>\n", bin)
	fmt.Fprintf(out, "  gh-dash keybindings: merge %s into ~/.config/gh-dash/config.yml\n", filepath.Join(c.Layout.Home, "docs", "gh-dash.yml"))
	return nil
}
