package cli

// Shared helpers of the daemon command group: daemon, install, uninstall and
// daemon-restart.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/paths"
)

// defaultMisePath is used when `command -v mise` finds nothing.
const defaultMisePath = "/opt/homebrew/bin/mise"

// daemonGroupSys holds the process-level seams of the daemon command group.
// Tests replace daemonSys; production uses the real system.
type daemonGroupSys struct {
	// Runner runs every subprocess; nil means execx.Real.
	Runner execx.Runner
	// UID is the launchd GUI domain owner (os.Getuid).
	UID func() int
	// UserHome is the user's home directory (LaunchAgents, PATH, HOME).
	UserHome func() (string, error)
	// Getenv reads MAGNUM_CONFIG and HERDR_BIN_PATH.
	Getenv func(string) string
	// Kill signals a process (syscall.Kill).
	Kill func(pid int, sig syscall.Signal) error
	// DaemonPID reads the daemon pidfile (engine.DaemonPID).
	DaemonPID func(paths.Layout) (int, error)
	// Sleep waits between polls.
	Sleep func(time.Duration)
	// RunEngine runs the daemon in the foreground (app.New + engine.Run).
	RunEngine func(ctx context.Context, c *Context, o daemonOptions) (daemonDryRunReport, error)
}

var daemonSys = daemonGroupSys{
	UID:       os.Getuid,
	UserHome:  os.UserHomeDir,
	Getenv:    os.Getenv,
	Kill:      syscall.Kill,
	DaemonPID: engine.DaemonPID,
	Sleep:     time.Sleep,
	RunEngine: runDaemonEngine,
}

// runner returns the subprocess runner for one command. Under --dry-run it
// is wrapped in execx.DryRun, so read-only commands still run and mutating
// ones are printed to out instead.
func (s daemonGroupSys) runner(dry bool, out io.Writer) execx.Runner {
	base := s.Runner
	if base == nil {
		base = &execx.Real{}
	}
	if !dry {
		return base
	}
	return &execx.DryRun{Inner: base, Out: daemonGroupPrinter{w: out}}
}

// daemonGroupPrinter writes execx.DryRun's "would run" lines to a command's
// stdout (the lines arrive already redacted).
type daemonGroupPrinter struct{ w io.Writer }

func (p daemonGroupPrinter) Printf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	line = strings.Replace(line, " (dir=)", "", 1)
	fmt.Fprintln(p.w, strings.TrimRight(line, "\n"))
}

// daemonGroupNoArgs refuses positional arguments; false means the command
// returns 2.
func daemonGroupNoArgs(c *Context, name, usage string, pos []string) bool {
	if len(pos) > 0 {
		fmt.Fprintf(c.Stderr, "magnum %s: unexpected argument %q\nusage: magnum %s\n", name, pos[0], usage)
		return false
	}
	return true
}

// loadDaemonGroupConfig loads config.toml and explains the fix on failure.
func loadDaemonGroupConfig(c *Context, cmd string) bool {
	if err := c.LoadConfig(); err != nil {
		file := c.Layout.Config()
		if file == "" || !fsx.Exists(file) {
			file = c.Layout.UserConfig
		}
		fmt.Fprintf(c.Stderr, "magnum %s: %v\nfix: correct the config file (%s), then check it with `magnum config`\n",
			cmd, err, inspTilde(file))
		return false
	}
	return true
}

// daemonConfigOverride is the config file the daemon must be told about: the
// --config flag or $MAGNUM_CONFIG, made absolute, or "" for the layout's
// default config.toml.
func daemonConfigOverride(c *Context) string {
	file := c.cfgPath
	if file == "" {
		file = daemonSys.Getenv("MAGNUM_CONFIG")
	}
	if file == "" {
		return ""
	}
	file = paths.Expand(file)
	if abs, err := filepath.Abs(file); err == nil {
		file = abs
	}
	if file == c.Layout.Config() {
		return ""
	}
	return file
}

// configFileInUse is the config file config.Load reads for c (--config,
// else $MAGNUM_CONFIG, else the layout's config.toml), made absolute: the
// file `magnum config` and `magnum doctor` report as validated.
func configFileInUse(c *Context) string {
	if f := daemonConfigOverride(c); f != "" {
		return f
	}
	return c.Layout.Config()
}

// resolveMisePath finds mise the way the brief asks (`command -v mise` in a
// plain sh, read-only), falling back to Homebrew's location.
func resolveMisePath(ctx context.Context, run execx.Runner) string {
	res, err := run.Run(ctx, execx.Cmd{
		Name:    "/bin/sh",
		Args:    []string{"-c", "command -v mise"},
		Timeout: 10 * time.Second,
		Label:   "command -v mise",
	})
	if err == nil {
		if p := res.Out(); filepath.IsAbs(p) && !strings.ContainsAny(p, "\n") {
			return p
		}
	}
	return defaultMisePath
}

// launchPrefix is what a bare environment (the LaunchAgent, the gh
// extension) puts before the binary: `mise -C <checkout> exec --` while an
// App identity reads its key from an env var (private_key_env without
// private_key_file) that the checkout's mise environment provides, else
// nothing, so an install whose keys are files runs the binary directly.
func launchPrefix(layout paths.Layout, cfg *config.Config, misePath string) []string {
	if layout.Home == "" || cfg == nil || misePath == "" {
		return nil
	}
	if !fsx.Exists(filepath.Join(layout.Home, ".mise.toml")) && !fsx.Exists(filepath.Join(layout.Home, ".mise.local.toml")) {
		return nil
	}
	for _, id := range cfg.Identities {
		if id.Kind == "app" && id.PrivateKeyFile == "" && id.PrivateKeyEnv != "" {
			return []string{misePath, "-C", layout.Home, "exec", "--"}
		}
	}
	return nil
}

// launchdPATH is the PATH the LaunchAgent gets (launchd's own is bare).
// It carries mise's shims (codex, claude, node installed with mise), which a
// launchd job running the binary directly, not through `mise exec`, gets no
// other way: the daemon runs `codex login status` and the like itself.
func launchdPATH(userHome string) string {
	shims := filepath.Join(userHome, ".local", "share", "mise", "shims")
	if d := daemonSys.Getenv("MISE_DATA_DIR"); filepath.IsAbs(d) {
		shims = filepath.Join(d, "shims")
	}
	return strings.Join([]string{
		filepath.Join(userHome, ".local", "bin"), shims,
		"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin",
	}, ":")
}

// magnumPlistOptions describes the LaunchAgent: `<prefix> <binary> daemon`
// (prefix: launchPrefix), restarted only after an abnormal exit. The
// daemon writes and rotates logs/daemon.log itself; launchd's capture of
// stdout/stderr (crashes, output before the logger exists) goes to
// logs/launchd.log so the two never write one file. MAGNUM_HOME is set only
// for the checkout layout, which it selects. configFile is non-empty only
// for a non-default config (see daemonConfigOverride). Never put secrets
// here: the plist is world-readable; keys are files or come from mise.
func magnumPlistOptions(layout paths.Layout, userHome string, prefix []string, herdrSocket, configFile string) launchd.Options {
	env := map[string]string{
		"PATH":              launchdPATH(userHome),
		"HOME":              userHome,
		"HERDR_SOCKET_PATH": herdrSocket,
	}
	if layout.CheckoutLayout() && layout.Home != "" {
		env["MAGNUM_HOME"] = layout.Home
	}
	workDir := layout.Home
	if workDir == "" {
		workDir = userHome
	}
	if configFile != "" {
		env["MAGNUM_CONFIG"] = configFile
	}
	// launchd gives the daemon Apple's ssh-agent socket, not the shell's: a
	// user whose keys live in another agent (1Password, Strongbox, a forwarded
	// agent) gets "Permission denied (publickey)" on every fetch. Carry the
	// installing shell's socket over unless it is Apple's own listener, which
	// launchd provides anyway.
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" && !strings.Contains(sock, "/com.apple.launchd.") {
		env["SSH_AUTH_SOCK"] = sock
	}
	return launchd.Options{
		Label:            launchd.DefaultLabel,
		ProgramArguments: append(slices.Clone(prefix), layout.Binary(), "daemon"),
		WorkingDir:       workDir,
		Env:              env,
		StdoutPath:       launchd.LogPath(layout.Logs()),
		StderrPath:       launchd.LogPath(layout.Logs()),
		KeepAlive:        true,
		ThrottleSeconds:  10,
	}
}

// herdrPluginEntry is the part of one `herdr plugin list --json` entry magnum
// reads.
type herdrPluginEntry struct {
	ID       string `json:"plugin_id"`
	Root     string `json:"plugin_root"`
	Manifest string `json:"manifest_path"`
	Enabled  bool   `json:"enabled"`
	Version  string `json:"version"`
	Source   struct {
		Kind string `json:"kind"`
	} `json:"source"`
}

// herdrPluginBin is the herdr binary: $HERDR_BIN_PATH (set inside plugin actions)
// or herdr on PATH.
func herdrPluginBin() string {
	if p := daemonSys.Getenv("HERDR_BIN_PATH"); p != "" {
		return p
	}
	return "herdr"
}

// herdrPluginCmd builds a herdr CLI call aimed at the configured server socket.
func herdrPluginCmd(socket string, mutates bool, args ...string) execx.Cmd {
	cmd := execx.Cmd{
		Name:    herdrPluginBin(),
		Args:    args,
		Timeout: time.Minute,
		Mutates: mutates,
		Label:   "herdr " + strings.Join(args[:min(2, len(args))], " "),
	}
	if socket != "" {
		cmd.Env = map[string]string{"HERDR_SOCKET_PATH": socket}
	}
	return cmd
}

// findHerdrPlugin looks the plugin up with `herdr plugin list --plugin <id>
// --json` (read-only).
func findHerdrPlugin(ctx context.Context, run execx.Runner, socket, id string) (herdrPluginEntry, bool, error) {
	res, err := run.Run(ctx, herdrPluginCmd(socket, false, "plugin", "list", "--plugin", id, "--json"))
	if err != nil {
		return herdrPluginEntry{}, false, fmt.Errorf("herdr plugin list: %w", err)
	}
	var out struct {
		Result struct {
			Plugins []herdrPluginEntry `json:"plugins"`
		} `json:"result"`
	}
	if err := json.Unmarshal(res.Stdout, &out); err != nil {
		return herdrPluginEntry{}, false, fmt.Errorf("herdr plugin list: unexpected output: %w", err)
	}
	for _, p := range out.Result.Plugins {
		if p.ID == id {
			return p, true, nil
		}
	}
	return herdrPluginEntry{}, false, nil
}

// linkedFrom reports whether the plugin entry is this checkout (link mode
// keeps the checkout itself as plugin_root).
func (p herdrPluginEntry) linkedFrom(dir string) bool {
	return sameResolvedPath(p.Root, dir) || (p.Manifest != "" && sameResolvedPath(filepath.Dir(p.Manifest), dir))
}

// sameResolvedPath compares two directories after resolving symlinks.
func sameResolvedPath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	norm := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		return filepath.Clean(p)
	}
	return norm(a) == norm(b)
}

// tabBarSnippet is the optional herdr tab-bar entry that shows
// state/tabbar.txt. magnum only prints it; herdr's config.toml is the user's.
func tabBarSnippet(tabBarFile string) string {
	entry := fmt.Sprintf("{ type = 'command', command = %s, interval_seconds = 6, timeout_seconds = 2 },",
		tabBarTOMLString(tabBarCommand(tabBarFile)))
	return "Optional: show magnum's status in herdr's tab bar. Add this entry to the tab_bar_right array\n" +
		"under [ui] in ~/.config/herdr/config.toml (magnum never edits that file); it shows \"magnum down\"\n" +
		"when the daemon stopped writing the file (an entry that only runs `cat` keeps the last line):\n\n  " + entry + "\n"
}

// tabBarCommand is the sh command herdr runs for the tab bar: the status line
// of the file engine.WriteTabBarFile writes ("<written> <max age>" then the
// line), or "magnum down" when the file is missing, has no timestamp (an
// older daemon) or is older than its max age (three poll intervals, which
// the daemon writes into the file): the daemon is not ticking.
func tabBarCommand(tabBarFile string) string {
	return "f=" + tabBarShellQuote(tabBarFile) + `; { read -r at max && IFS= read -r line; } 2>/dev/null <"$f"; ` +
		`case "$at$max" in ""|*[!0-9]*) echo "magnum down";; *) if [ $(( $(date +%s) - at )) -gt "$max" ]; ` +
		`then echo "magnum down"; else printf "%s\n" "$line"; fi;; esac`
}

// tabBarShellQuote quotes s for sh inside double quotes.
func tabBarShellQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")
	return `"` + r.Replace(s) + `"`
}

// tabBarTOMLString renders s as a TOML literal string when possible, else as a
// basic string.
func tabBarTOMLString(s string) string {
	if !strings.ContainsAny(s, "'\n\r") {
		return "'" + s + "'"
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`)
	return `"` + r.Replace(s) + `"`
}

// migrateGuard is the CLI's store.Options.BeforeMigrate. A build that expects
// a newer registry schema must not migrate the file while a magnum daemon runs
// on it: the daemon exits when the schema changes under it
// (store.SchemaChanged), launchd restarts it at once, and the restart cuts the
// agent turns of its rounds and preempts any drain. The new build takes over
// through `daemon-restart --drain`, which never migrates (it writes one kv
// row) and whose new daemon migrates on start. A stale pidfile naming a
// recycled pid does not count; a live pid that ps cannot identify does.
func migrateGuard(layout paths.Layout) func(from, to int) error {
	return func(from, to int) error {
		pid, err := daemonSys.DaemonPID(layout)
		if err != nil || pid == 0 {
			return nil
		}
		comm, args, err := engine.ProcessCommand(context.Background(), daemonSys.runner(false, nil), pid)
		if err == nil && !engine.LooksLikeDaemon(comm, args) {
			return nil
		}
		return fmt.Errorf("the registry is at schema %d and this build needs %d, but the daemon (pid %d) still runs on it; "+
			"migrating now would make it exit mid-round\n"+
			"fix: run `magnum daemon-restart --drain` (it waits for the rounds in flight and restarts the daemon on this build, which migrates), then retry",
			from, to, pid)
	}
}
