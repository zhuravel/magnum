package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/launchd"
)

const uninstallUsage = "uninstall [--now] [--dry-run]"

func newUninstallCmd(c *Context) *cobra.Command {
	var dry, now bool
	cmd := newCommand(groupDaemon, uninstallUsage, "unload and delete the launchd agent and unlink the herdr plugin (keeps state)",
		"Unload and delete the launchd agent, which stops the daemon it runs, and unlink the herdr plugin. The "+
			"state directory (registry, logs, review reports) and the slots are kept. A broken config does not "+
			"block it. While review rounds, a notes curation or the retro are in flight it is refused, because stopping the daemon abandons them; "+
			"--now uninstalls anyway.",
		func(pos []string) int { return runUninstallCmd(c, dry, now, pos) })
	cmd.Flags().BoolVar(&dry, "dry-run", false, "print what would be removed; change nothing")
	cmd.Flags().BoolVar(&now, "now", false, "stop the daemon even while review rounds, a notes curation or the retro are in flight (rounds start over)")
	return cmd
}

func runUninstallCmd(c *Context, dry, now bool, pos []string) int {
	if !daemonGroupNoArgs(c, "uninstall", uninstallUsage, pos) {
		return 2
	}
	// A broken config must not block removal; only the herdr socket comes
	// from it, and the herdr CLI has its own default.
	socket := ""
	if err := c.LoadConfig(); err == nil {
		socket = c.Config.Herdr.Socket
	} else {
		fmt.Fprintf(c.Stderr, "magnum uninstall: warning: %v (continuing with herdr's default socket)\n", err)
	}
	ctx := context.Background()
	out := c.Stdout
	run := daemonSys.runner(dry, out)
	if !dry && daemonRunning(ctx, c, run) && !c.refuseWhileRoundsRun("uninstall", now) {
		return 1
	}
	failed := false

	// 1. launchd agent.
	label := launchd.DefaultLabel
	uid := daemonSys.UID()
	if userHome, err := daemonSys.UserHome(); err != nil {
		fmt.Fprintf(c.Stderr, "magnum uninstall: find your home directory: %v\nfix: set HOME, then re-run magnum uninstall\n", err)
		failed = true
	} else {
		path := launchd.AgentPath(userHome, label)
		switch {
		case dry:
			fmt.Fprintf(out, "dry-run: would unload gui/%d/%s and delete %s\n", uid, label, path)
		default:
			if err := launchd.Uninstall(ctx, run, uid, label, path); err != nil {
				fmt.Fprintf(c.Stderr, "magnum uninstall: %v\nfix: run `launchctl bootout gui/%d/%s`, then re-run magnum uninstall\n", err, uid, label)
				failed = true
			} else {
				fmt.Fprintf(out, "launchd: unloaded gui/%d/%s and removed %s\n", uid, label, path)
			}
		}
	}

	// 2. herdr plugin.
	p, found, err := findHerdrPlugin(ctx, run, socket, pluginID)
	switch {
	case err != nil:
		fmt.Fprintf(c.Stderr, "magnum uninstall: %v\nfix: start herdr and run `herdr plugin unlink %s` if the plugin is still listed\n", err, pluginID)
		failed = true
	case !found:
		fmt.Fprintf(out, "herdr plugin %s: not linked\n", pluginID)
	default:
		if _, err := run.Run(ctx, herdrPluginCmd(socket, true, "plugin", "unlink", pluginID)); err != nil {
			fmt.Fprintf(c.Stderr, "magnum uninstall: herdr plugin unlink %s: %v\nfix: run `herdr plugin unlink %s` (or `herdr plugin uninstall %s` if it was not linked from %s)\n",
				pluginID, err, pluginID, pluginID, p.Root)
			failed = true
		} else if !dry {
			fmt.Fprintf(out, "herdr plugin %s: unlinked (was %s)\n", pluginID, p.Root)
		}
	}

	kept := []string{c.Layout.Data() + " (registry, reviews, notes)"}
	if c.Layout.State() != c.Layout.Data() {
		kept = append(kept, c.Layout.State()+" (logs, gh config dirs)")
	}
	if c.Layout.UserConfig != "" {
		kept = append(kept, c.Layout.UserConfig)
	}
	fmt.Fprintf(out, "kept %s; delete them by hand to remove every trace\n", strings.Join(kept, ", "))
	if failed {
		return 1
	}
	return 0
}
