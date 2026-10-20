package cli

// `magnum settings`: the settings screen of internal/tui over the user
// config and the runtime pauses. It writes only through the screen, each
// save after the operator's y.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/tui"
)

const settingsUsage = "settings [--watch N | --repo owner/name] [--history] [--restore N]"

// settingsNoTerminal is the refusal off a terminal, where no screen can
// ask before a save.
const settingsNoTerminal = "magnum settings needs a terminal; edit ~/.config/magnum/config.toml and run `magnum config`"

type settingsOpts struct {
	watch, restore       int
	repo                 string
	history              bool
	watchSet, restoreSet bool // the flags were given (0 is refused, not taken for "none")
}

func newSettingsCmd(c *Context) *cobra.Command {
	var o settingsOpts
	var cmd *cobra.Command
	cmd = newCommand(groupAct, settingsUsage, "edit config.toml and the runtime pauses on a screen",
		"Open the settings screen: the settings of ~/.config/magnum/config.toml by section, each with its default, "+
			"where its value comes from and whether it applies now or after a daemon restart, and the Runtime "+
			"section's pauses (automation, each agent kind, each watch owner), which apply at once as `magnum pause` "+
			"and `magnum resume` do. A save shows the changed lines and asks y/N first, keeps the previous file "+
			"in the history and can ask the daemon to restart on it. --watch N opens the screen on the N-th "+
			"[[watch]] (from 1) and --repo on a repository; --history lists the kept versions and --restore N "+
			"opens the save that puts the N-th back (1 = the newest). `,` on the board and the dashboard opens "+
			"the same screen. It needs a terminal (--history does not): anywhere else, edit the file and check it with "+
			"`magnum config`. --history and --restore work when a hand edit broke the file.",
		func(pos []string) int {
			o.watchSet, o.restoreSet = cmd.Flags().Changed("watch"), cmd.Flags().Changed("restore")
			return runSettings(c, o, pos)
		})
	fs := cmd.Flags()
	fs.IntVar(&o.watch, "watch", 0, "open on the N-th [[watch]] (from 1)")
	fs.StringVar(&o.repo, "repo", "", "open on a repository (owner/name)")
	fs.BoolVar(&o.history, "history", false, "list the kept versions of config.toml")
	fs.IntVar(&o.restore, "restore", 0, "open the save that puts the N-th kept version back (1 = the newest)")
	return cmd
}

func runSettings(c *Context, o settingsOpts, pos []string) int {
	if len(pos) > 0 {
		return actUsage(c, "settings", "takes no arguments", settingsUsage)
	}
	opts, msg := o.screen()
	if msg != "" {
		return actUsage(c, "settings", msg, settingsUsage)
	}
	if o.history { // a listing: it needs no terminal
		if err := settingsHistory(c); err != nil {
			return cmdFail(c, "settings", err)
		}
		return 0
	}
	if code, refused := refuseInAgentPane(c, "settings"); refused {
		return code
	}
	if !inspScreen(c) {
		fmt.Fprintln(c.Stderr, settingsNoTerminal)
		return 1
	}
	acts := newScreenActions(c)
	defer acts.Close()
	ctx, stop := signalContext()
	defer stop()
	if err := tuiSettings(ctx, newSettingsSource(c, acts), screenRuntime{acts}, opts); err != nil {
		return cmdFail(c, "settings", err)
	}
	return 0
}

// screen is the screen's options for o, or why o is refused.
func (o settingsOpts) screen() (tui.SettingsOptions, string) {
	var s tui.SettingsOptions
	repo := strings.TrimSpace(o.repo)
	switch {
	case o.watchSet && repo != "":
		return s, "use either --watch or --repo"
	case o.watchSet && o.watch < 1:
		return s, "--watch takes a [[watch]]'s position, from 1"
	case o.watchSet:
		s.Scope = "watch:" + strconv.Itoa(o.watch)
	case repo != "":
		if owner, name, ok := strings.Cut(repo, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return s, fmt.Sprintf("--repo %q: want owner/name", o.repo)
		}
		s.Scope = "repo:" + repo
	}
	if o.restoreSet {
		if o.restore < 1 {
			return s, "--restore takes a kept version's number, from 1 (the newest)"
		}
		s.Restore = o.restore
	}
	if o.history && (o.watchSet || repo != "" || o.restoreSet) {
		return s, "--history lists the kept versions and takes no other flag"
	}
	return s, ""
}
