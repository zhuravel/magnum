// Package cli wires the subcommands into a cobra command tree built per
// call. Each command lives in its own file and stays a thin layer over the
// engine, slots, inventory and store packages.
package cli

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
)

// Context carries what every command needs.
type Context struct {
	Version   string
	Layout    paths.Layout
	Config    *config.Config // nil until LoadConfig
	Stdout    io.Writer
	Stderr    io.Writer
	cfgPath   string
	layoutErr error // paths.Resolve failed; commands refuse to run
}

// LoadConfig loads config.toml once.
func (c *Context) LoadConfig() error {
	if c.Config != nil {
		return nil
	}
	cfg, err := config.Load(c.Layout, c.cfgPath)
	if err != nil {
		return err
	}
	c.Config = cfg
	return nil
}

// Main runs args (without the program name) and returns the exit code: 0 on
// success, 1 when a command fails and 2 on a usage error.
func Main(version string, args []string, stdout, stderr io.Writer) int {
	c := &Context{Version: version, Stdout: stdout, Stderr: stderr}
	c.Layout, c.layoutErr = paths.Resolve()
	return execute(c, args)
}

// Help groups of `magnum --help`.
const (
	groupInspect = "inspect"
	groupAct     = "act"
	groupDaemon  = "daemon"
)

const rootLong = "magnum runs automated GitHub pull-request reviews. A daemon watches GitHub, checks each " +
	"eligible PR out into a review worktree, runs Claude, Codex and a Codex judge inside herdr panes, posts one " +
	"review, and keeps a registry of which folder and which MySQL databases belong to which PR so storage is " +
	"released when the PR closes.\n\n" +
	"PR references are a URL, owner/repo#N, repo#N or N (in daemon.default_repo)."

// helpWidth is the column --help paragraphs wrap at.
const helpWidth = 80

// newRoot builds the command tree bound to c.
func newRoot(c *Context) *cobra.Command {
	root := &cobra.Command{
		Use:           "magnum",
		Short:         "automated GitHub PR reviews in herdr panes",
		Long:          wrapText(rootLong, helpWidth),
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if c.layoutErr != nil {
				fmt.Fprintln(c.Stderr, "magnum:", c.layoutErr)
				return exitCode(1)
			}
			return nil
		},
	}
	root.PersistentFlags().StringVar(&c.cfgPath, "config", c.cfgPath,
		"a complete config file instead of the built-in defaults (default: $MAGNUM_CONFIG; your settings go in ~/.config/magnum/config.toml)")
	_ = root.MarkPersistentFlagFilename("config", "toml")

	root.AddGroup(
		&cobra.Group{ID: groupInspect, Title: "Inspect:"},
		&cobra.Group{ID: groupAct, Title: "Act:"},
		&cobra.Group{ID: groupDaemon, Title: "Daemon:"},
	)
	root.AddCommand(
		// Inspect
		newStatusCmd(c), newPRsCmd(c), newWhereCmd(c), newSlotsCmd(c), newLogsCmd(c), newDoctorCmd(c),
		newIdentitiesCmd(c), newRolesCmd(c), newNotesCmd(c), newStatsCmd(c), newMissesCmd(c), newConfigCmd(c), newVersionCmd(c),
		// Act
		newInitCmd(c), newReviewCmd(c), newOpenCmd(c), newWatchCmd(c), newEvalCmd(c), newRetroCmd(c),
	)
	for _, k := range targetKinds {
		root.AddCommand(newTargetCmd(c, k))
	}
	root.AddCommand(
		newAbortCmd(c), newIgnoreCmd(c), newApproveCmd(c), newRequestChangesCmd(c), newUnapproveCmd(c),
		newAttentionCmd(c), newPickCmd(c), newCleanupCmd(c), newKickCmd(c),
		newPauseCmd(c), newResumeCmd(c), newUICmd(c), newPostReviewCmd(c),
		// Daemon
		newDaemonCmd(c), newDaemonRestartCmd(c), newDaemonStopCmd(c), newInstallCmd(c), newUninstallCmd(c),
	)
	noFileCompletionByDefault(root)
	return root
}

// execute runs args against a command tree bound to c.
func execute(c *Context, args []string) int {
	root := newRoot(c)
	root.SetArgs(args)
	root.SetOut(c.Stdout)
	root.SetErr(c.Stderr)
	if len(args) == 0 {
		// No command: the help on stderr and a usage error.
		root.SetOut(c.Stderr)
		root.InitDefaultHelpFlag()
		root.InitDefaultHelpCmd()
		root.InitDefaultCompletionCmd()
		_ = root.Help()
		return 2
	}
	cmd, err := root.ExecuteC()
	if err == nil {
		return 0
	}
	var code exitCode
	if errors.As(err, &code) {
		return int(code)
	}
	// cobra's own errors: an unknown command or flag, a bad flag value.
	path := root.Name()
	if cmd != nil {
		path = cmd.CommandPath()
	}
	fmt.Fprintf(c.Stderr, "%s: %v\n", path, err)
	if cmd != nil && cmd != root {
		fmt.Fprintf(c.Stderr, "usage: %s\n", cmd.UseLine())
	}
	fmt.Fprintf(c.Stderr, "Run '%s --help' for usage.\n", path)
	return 2
}

// exitCode is a command's non-zero exit status; the command has already
// printed why.
type exitCode int

func (e exitCode) Error() string { return "exit status " + strconv.Itoa(int(e)) }

// runs adapts a command body that returns an exit status to cobra's RunE.
func runs(body func(args []string) int) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, args []string) error {
		if code := body(args); code != 0 {
			return exitCode(code)
		}
		return nil
	}
}

// newCommand returns a command whose usage line is use verbatim (it names
// the flags) and whose body gets the positional arguments. Flags may come
// anywhere among them; everything after "--" is positional. Bodies check
// their own arguments so usage messages stay specific.
func newCommand(group, use, short, long string, body func(args []string) int) *cobra.Command {
	return &cobra.Command{
		Use:                   use,
		Short:                 short,
		Long:                  wrapText(long, helpWidth),
		GroupID:               group,
		Args:                  cobra.ArbitraryArgs,
		DisableFlagsInUseLine: true,
		ValidArgsFunction:     cobra.NoFileCompletions,
		RunE:                  runs(body),
	}
}

// wrapText wraps each paragraph of s at width columns.
func wrapText(s string, width int) string {
	paras := strings.Split(s, "\n\n")
	for i, p := range paras {
		var b strings.Builder
		col := 0
		for _, w := range strings.Fields(p) {
			switch {
			case col == 0:
			case col+1+len(w) > width:
				b.WriteByte('\n')
				col = 0
			default:
				b.WriteByte(' ')
				col++
			}
			b.WriteString(w)
			col += len(w)
		}
		paras[i] = b.String()
	}
	return strings.Join(paras, "\n\n")
}

// noFileCompletionByDefault keeps the shell from offering file names for
// flag values magnum does not complete (file and directory flags are marked
// where they are defined).
func noFileCompletionByDefault(cmd *cobra.Command) {
	visit := func(f *pflag.Flag) {
		if f.NoOptDefVal != "" || f.Annotations[cobra.BashCompFilenameExt] != nil || f.Annotations[cobra.BashCompSubdirsInDir] != nil {
			return
		}
		if _, ok := cmd.GetFlagCompletionFunc(f.Name); ok {
			return
		}
		_ = cmd.RegisterFlagCompletionFunc(f.Name, cobra.NoFileCompletions)
	}
	cmd.LocalNonPersistentFlags().VisitAll(visit)
	cmd.PersistentFlags().VisitAll(visit)
	for _, sub := range cmd.Commands() {
		noFileCompletionByDefault(sub)
	}
}

func newVersionCmd(c *Context) *cobra.Command {
	return newCommand(groupInspect, "version", "print the version",
		"Print the magnum version (stamped by `make build`; dev for a plain go build).",
		func([]string) int {
			fmt.Fprintln(c.Stdout, "magnum", c.Version)
			return 0
		})
}

func newConfigCmd(c *Context) *cobra.Command {
	return newCommand(groupInspect, "config", "validate config.toml and render every prompt file; print the resolved paths",
		"Validate the configuration (the built-in defaults with your ~/.config/magnum/config.toml over them), render every prompt file the "+
			"roles name with this binary's template data, and print the resolved home, config, state and judge "+
			"skill paths with the number of watches, identities and pools. Fix whatever it reports before installing "+
			"or restarting the daemon: `magnum daemon-restart` and `magnum install` run this command with the binary "+
			"that will run (bin/magnum) and refuse when it fails, and the daemon refuses to start on the same errors.",
		func([]string) int {
			if err := checkConfigAndPrompts(c); err != nil {
				fmt.Fprintln(c.Stderr, err)
				return 1
			}
			n, _ := engine.CheckPrompts(c.Config)
			checkout := c.Layout.Home
			if checkout == "" {
				checkout = "-"
			}
			skill := config.SkillPath(c.Config.JudgeFor(nil).Skill, c.Layout)
			fmt.Fprintf(c.Stdout, "checkout: %s\nconfig:   %s\ndata:     %s\nstate:    %s\nskill:    %s\nwatches: %d  identities: %d  pools: %d\nprompts: %d renders ok\n",
				checkout, configSources(c.Config, configFileInUse(c), c.Layout), c.Layout.Data(), c.Layout.State(), skill, len(c.Config.Watches), len(c.Config.Identities), len(c.Config.Pools), n)
			for _, w := range c.Config.Warnings() {
				fmt.Fprintln(c.Stderr, "warning:", w)
			}
			return 0
		})
}

// checkConfigAndPrompts loads the configuration and renders every prompt
// file it names (engine.CheckPrompts): what `magnum config` reports and the
// daemon refuses to start on.
func checkConfigAndPrompts(c *Context) error {
	if err := c.LoadConfig(); err != nil {
		return err
	}
	if _, err := engine.CheckPrompts(c.Config); err != nil {
		return fmt.Errorf("prompts do not render with this build:\n%w", err)
	}
	return nil
}
