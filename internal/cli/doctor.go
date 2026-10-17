package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const doctorUsage = "[--json]"

func newDoctorCmd(c *Context) *cobra.Command {
	var asJSON bool
	cmd := newCommand(groupInspect, "doctor "+doctorUsage, "check herdr, the agent CLIs, prompts, gh, MySQL, clones, SSH, disk, launchd and the registry",
		"Check everything magnum depends on: config, herdr (version and socket methods), every agent CLI the "+
			"configured roles use (on PATH in zsh, logged in, shell wrapper), herdr's Codex session hook (the SessionStart hook in "+
			"$CODEX_HOME/hooks.json herdr learns session ids from: missing, or disabled in config.toml's [hooks.state]), the judge skill, the fix role, the skill editor "+
			"(whether [learn] editor can run, and why it is off), the Whiteboard "+
			"CLI and server (when a watch has whiteboards on), the role prompts and "+
			"prompts_dir, gh and each identity, MySQL (when a [[pool]] declares databases), the main clones and their SSH "+
			"origins, a pool's reset_db when it names schema_paths, mise (when a pool, the LaunchAgent or your PATH uses it), the login shell "+
			"(whether the `zsh -lc` shell the agents run their tools in sees mise's shims), free disk, codex staging, the launchd agent "+
			"and the mise it starts the daemon through, the terminal's Automation permission on macOS (named, never probed: "+
			"doctor opens no window), the herdr plugin link and the registry. "+
			"Each check prints PASS, WARN, FAIL or SKIP with the exact fix. Read-only; exits 1 when a check fails.",
		func(pos []string) int { return runDoctor(c, asJSON, pos) })
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// Check statuses.
const (
	doctorPass = "PASS"
	doctorWarn = "WARN"
	doctorFail = "FAIL"
	doctorSkip = "SKIP" // not applicable or not run (no login check, the CLI is missing)
)

// doctorCheck is one PASS/WARN/FAIL line.
type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// doctorHerdr is the part of *herdr.Client doctor uses.
type doctorHerdr interface {
	Ping(ctx context.Context) (herdr.ServerInfo, error)
	Call(ctx context.Context, method string, params, out any) error
}

// doctorAgents is the part of *agents.Manager doctor uses.
type doctorAgents interface {
	Wrapper(ctx context.Context, kind string) (bool, error)
	CodexHome(role config.Role) string
}

// doctorMySQL is the part of *mysqlx.Client doctor uses.
type doctorMySQL interface {
	Ping(ctx context.Context) error
	ListSuffixed(ctx context.Context) ([]mysqlx.Database, error)
}

// doctorDeps are doctor's sources; every check only reads.
type doctorDeps struct {
	Config     *config.Config
	Layout     paths.Layout
	ConfigFile string // the config file that was loaded; "" means Layout.Config()
	Store      *store.Store
	Run        execx.Runner
	Herdr      doctorHerdr
	Agents     doctorAgents
	MySQL      doctorMySQL // nil: the DSN is invalid
	Inventory  statusScanner
	Launchd    func(ctx context.Context) (launchd.Info, error)
	DiskFree   func(path string) (uint64, error)
	Getenv     func(string) string
	UserHome   string
}

// doctorDiskFree reads the free bytes of a path (a seam for tests).
var doctorDiskFree = inspDiskFree

func runDoctor(c *Context, asJSON bool, pos []string) int {
	if len(pos) > 0 {
		return inspUsage(c, "doctor", "takes no arguments", doctorUsage)
	}
	a, err := inspOpenApp(c, false)
	if err != nil {
		return doctorPrint(c, []doctorCheck{doctorOpenFailed(err)}, asJSON)
	}
	defer a.Close()
	ctx, cancel := signalContext()
	defer cancel()
	d := doctorDeps{
		Config: a.Config, Layout: a.Layout, ConfigFile: configFileInUse(c), Store: a.Store, Run: a.Runner, Herdr: a.Herdr, Agents: a.Agents,
		Inventory: a.Inventory,
		Launchd: func(ctx context.Context) (launchd.Info, error) {
			return launchd.Status(ctx, a.Runner, os.Getuid(), launchd.DefaultLabel)
		},
		DiskFree: doctorDiskFree, Getenv: os.Getenv, UserHome: inspHome(),
	}
	if a.MySQL != nil {
		d.MySQL = a.MySQL
	}
	return doctorPrint(c, doctorRun(ctx, d), asJSON)
}

// doctorOpenFailed is the one check doctor reports when it cannot open the
// config and the registry: a migration the CLI refuses under a running
// daemon (migrateGuard) names the registry and daemon-restart --drain; any
// other failure points at config.toml.
func doctorOpenFailed(err error) doctorCheck {
	if r, ok := errors.AsType[*migrateRefusal](err); ok {
		return doctorFailed("registry", r.reason(), migrateRefusalFix)
	}
	return doctorFailed("config", "config/state: "+err.Error(), "fix config.toml (`magnum config` validates it)")
}

// doctorPrint prints the checks; exit code 1 when any failed.
func doctorPrint(c *Context, cs []doctorCheck, asJSON bool) int {
	counts := map[string]int{}
	for _, ch := range cs {
		counts[ch.Status]++
	}
	if asJSON {
		if err := writeJSON(c.Stdout, cs); err != nil {
			return cmdFail(c, "doctor", err)
		}
	} else {
		for _, ch := range cs {
			fmt.Fprintf(c.Stdout, "%s  %s\n", ch.Status, ch.Detail)
			if ch.Fix != "" {
				fmt.Fprintf(c.Stdout, "      fix: %s\n", ch.Fix)
			}
		}
		summary := fmt.Sprintf("%d PASS, %d WARN, %d FAIL", counts[doctorPass], counts[doctorWarn], counts[doctorFail])
		if counts[doctorSkip] > 0 {
			summary += fmt.Sprintf(", %d SKIP", counts[doctorSkip])
		}
		fmt.Fprintf(c.Stdout, "\n%s\n", summary)
	}
	if counts[doctorFail] > 0 {
		return 1
	}
	return 0
}

// doctorRun runs every check group concurrently and returns the checks in
// a fixed order.
func doctorRun(ctx context.Context, d doctorDeps) []doctorCheck {
	groups := []func(context.Context, doctorDeps) []doctorCheck{
		doctorConfig, doctorHerdrChecks, doctorAgentChecks, doctorSessionHooks, doctorSkill, doctorFixRole, doctorWhiteboard, doctorPrompts, doctorGH, doctorIdentities, doctorMySQLCheck,
		doctorClones, doctorSchemaReset, doctorMise, doctorLoginShell, doctorDisk, doctorStaging, doctorLaunchd, doctorLaunchdMise, doctorTerminal, doctorPluginCheck,
		doctorLearnEditor, doctorRegistry,
	}
	out := make([][]doctorCheck, len(groups))
	var wg sync.WaitGroup
	for i, g := range groups {
		wg.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					out[i] = []doctorCheck{{Name: fmt.Sprintf("check %d", i), Status: doctorFail, Detail: fmt.Sprintf("check crashed: %v", r)}}
				}
			}()
			out[i] = g(ctx, d)
		})
	}
	wg.Wait()
	var all []doctorCheck
	for _, cs := range out {
		all = append(all, cs...)
	}
	return all
}

func doctorOK(name, detail string) doctorCheck {
	return doctorCheck{Name: name, Status: doctorPass, Detail: detail}
}
func doctorWarned(name, detail, fix string) doctorCheck {
	return doctorCheck{Name: name, Status: doctorWarn, Detail: detail, Fix: fix}
}
func doctorFailed(name, detail, fix string) doctorCheck {
	return doctorCheck{Name: name, Status: doctorFail, Detail: detail, Fix: fix}
}
func doctorSkipped(name, detail string) doctorCheck {
	return doctorCheck{Name: name, Status: doctorSkip, Detail: detail}
}

// doctorExec runs a read-only probe and returns trimmed stdout.
// doctorExecGit runs git with the redirecting GIT_* variables scrubbed, like
// every other git call magnum makes.
func doctorExecGit(ctx context.Context, d doctorDeps, timeout time.Duration, env map[string]string, args ...string) (string, error) {
	return doctorCmd(ctx, d, execx.Cmd{Name: "git", Args: args, Env: env, Unset: gitx.ScrubbedEnv(), Timeout: timeout, Label: "doctor"})
}

// doctorShellProbe asks an interactive zsh (`zsh -ic script`) what the user's
// shell config defines, detached from the terminal doctor runs in (NoTTY),
// as the daemon asks it under launchd.
func doctorShellProbe(ctx context.Context, d doctorDeps, timeout time.Duration, script string) (string, error) {
	return doctorCmd(ctx, d, execx.Cmd{Name: "zsh", Args: []string{"-ic", script}, Timeout: timeout, NoTTY: true, Label: "doctor"})
}

func doctorExec(ctx context.Context, d doctorDeps, timeout time.Duration, dir string, env map[string]string, name string, args ...string) (string, error) {
	return doctorCmd(ctx, d, execx.Cmd{Name: name, Args: args, Dir: dir, Env: env, Timeout: timeout, Label: "doctor"})
}

// doctorCmd runs a read-only probe cmd through doctor's runner and returns
// trimmed stdout; a failed exit's error is the first line of its stderr,
// redacted, when it wrote one.
func doctorCmd(ctx context.Context, d doctorDeps, cmd execx.Cmd) (string, error) {
	if d.Run == nil {
		return "", errors.New("no runner")
	}
	res, err := d.Run.Run(ctx, cmd)
	if err != nil {
		var ee *execx.ExitError
		if errors.As(err, &ee) && strings.TrimSpace(ee.Stderr) != "" {
			return res.Out(), fmt.Errorf("%s", textx.FirstLine(execx.Redact(ee.Stderr)))
		}
		return res.Out(), err
	}
	return res.Out(), nil
}

// doctorWhiteboard checks what the whiteboard job needs once a watch has
// whiteboards on (config.WhiteboardFor; off everywhere is a SKIP that runs
// nothing): the `whiteboard` CLI on PATH, with the version it reports
// (`whiteboard version --json`), and a Whiteboard server that answers
// (engine.WhiteboardStatusCmd: Whiteboard Desktop, or `whiteboard server
// start`). A missing CLI fails: the job cannot run at all. A server that is
// down only warns: the daemon skips the whiteboard of each review it posts
// until one runs, and the next posted review is the next chance.
func doctorWhiteboard(ctx context.Context, d doctorDeps) []doctorCheck {
	const name = "whiteboard"
	cfg := d.Config
	var on []string
	for i := range cfg.Watches {
		if cfg.WhiteboardFor(&cfg.Watches[i]) {
			on = append(on, cfg.Watches[i].Owner)
		}
	}
	if len(on) == 0 {
		return []doctorCheck{doctorSkipped(name, "whiteboards are off (whiteboard = false in [pipeline] and every [[watch]])")}
	}
	who := fmt.Sprintf("whiteboards are on for %s %s", textx.Plural(len(on), "watch", "watches"), strings.Join(on, ", "))
	out, err := doctorCmd(ctx, d, execx.Cmd{Name: engine.WhiteboardCommand, Args: []string{"version", "--json"}, Timeout: 15 * time.Second, Probe: true, Label: "doctor"})
	if err != nil {
		return []doctorCheck{doctorFailed(name, fmt.Sprintf("`whiteboard` is not on PATH (%s): %s", who, err.Error()),
			"install dev.fast Whiteboard and put its `whiteboard` CLI on PATH")}
	}
	label := engine.WhiteboardCommand
	var v struct {
		Version string `json:"version"`
	}
	if json.Unmarshal([]byte(out), &v) == nil && strings.TrimSpace(v.Version) != "" {
		label += " " + textx.FirstLine(strings.TrimSpace(v.Version))
	}
	if _, err := doctorCmd(ctx, d, engine.WhiteboardStatusCmd()); err != nil {
		return []doctorCheck{doctorWarned(name, fmt.Sprintf("Whiteboard is not running: the job skips whiteboards until Whiteboard Desktop or `whiteboard server start` runs (%s is on PATH; %s; %s)",
			label, who, err.Error()), "start Whiteboard Desktop, or run `whiteboard server start`")}
	}
	return []doctorCheck{doctorOK(name, fmt.Sprintf("%s is on PATH and Whiteboard runs (%s)", label, who))}
}

func doctorConfig(ctx context.Context, d doctorDeps) []doctorCheck {
	cfg := d.Config
	sources := configSources(cfg, d.ConfigFile, d.Layout)
	detail := fmt.Sprintf("config %s: %d watches, %d identities, %d pools", sources, len(cfg.Watches),
		len(cfg.Identities), len(cfg.Pools))
	if d.Store != nil {
		if v, err := d.Store.SchemaVersion(ctx); err == nil {
			detail += fmt.Sprintf("; registry %s (schema v%d)", inspTilde(d.Layout.DB()), v)
		}
	}
	out := []doctorCheck{doctorOK("config", detail)}
	for _, s := range cfg.Sources {
		if legacy := d.Layout.Config(); legacy != "" && s == legacy {
			out = append(out, doctorWarned("config location", inspTilde(s)+" replaces the built-in defaults (a checkout from before they were built in)",
				"keep your settings in "+inspTilde(d.Layout.UserConfig)+" and remove "+inspTilde(s)+" unless it is a complete config on purpose"))
		}
	}
	for _, w := range cfg.Warnings() {
		out = append(out, doctorWarned("config warning", w, ""))
	}
	if _, err := os.Stat(d.Layout.Binary()); err != nil {
		fix := "brew reinstall zhuravel/tap/magnum"
		if d.Layout.Home != "" {
			fix = "cd " + inspTilde(d.Layout.Home) + " && go build -o bin/magnum ./cmd/magnum"
		}
		out = append(out, doctorWarned("binary", "no binary at "+inspTilde(d.Layout.Binary())+" (launchd runs it)", fix))
	}
	return out
}

// configSources names what the configuration was read from: "base + user"
// (cfg.Sources), else the file given, else the layout's default.
func configSources(cfg *config.Config, file string, l paths.Layout) string {
	if cfg != nil && len(cfg.Sources) > 0 {
		parts := make([]string, len(cfg.Sources))
		for i, s := range cfg.Sources {
			parts[i] = inspTilde(s)
		}
		return strings.Join(parts, " + ")
	}
	if file != "" {
		return inspTilde(file)
	}
	return config.BuiltinDefaults
}
