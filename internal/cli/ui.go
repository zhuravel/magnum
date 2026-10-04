package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/BurntSushi/toml"

	"github.com/zhuravel/magnum/internal/herdr"
)

const uiUsage = "ui open <entrypoint> [--workspace id] [--width 90%] [--height 60%]"

// uiPane is a [[panes]] entry of herdr-plugin.toml.
type uiPane struct {
	ID        string `toml:"id"`
	Placement string `toml:"placement"`
	Width     string `toml:"width"`
	Height    string `toml:"height"`
}

// uiDefaults are the popup sizes herdr-plugin.toml ships, used when the
// manifest cannot be read.
var uiDefaults = map[string]uiPane{
	"picker":  {ID: "picker", Placement: "popup", Width: "90%", Height: "60%"},
	"status":  {ID: "status", Placement: "popup", Width: "95%", Height: "80%"},
	"cleanup": {ID: "cleanup", Placement: "popup", Width: "95%", Height: "80%"},
	"doctor":  {ID: "doctor", Placement: "popup", Width: "95%", Height: "80%"},
}

// uiPassEnv reaches the popup's command (herdr starts it, so it does not
// inherit this process's environment). MAGNUM_CONFIG is not in the list: uiOpen
// forwards the config file this process loaded (see daemonConfigOverride).
var uiPassEnv = []string{"MAGNUM_PICK_QUERY", "MAGNUM_HOME", "MAGNUM_BIN"}

type uiOpts struct {
	workspace, width, height string
}

func newUICmd(c *Context) *cobra.Command {
	var o uiOpts
	cmd := newCommand(groupAct, uiUsage, "open a magnum popup pane in herdr (ui open picker|status|cleanup|doctor)",
		"Open a magnum popup pane of the herdr plugin (picker, status, cleanup or doctor) over the herdr "+
			"socket. The sizes come from the [[panes]] of herdr-plugin.toml unless --width or --height is given; "+
			"the workspace defaults to the plugin context's.",
		func(pos []string) int { return runUI(c, o, pos) })
	fs := cmd.Flags()
	fs.StringVar(&o.workspace, "workspace", "", "herdr workspace to open the popup on (default: the plugin context's)")
	fs.StringVar(&o.width, "width", "", "popup width (cells or percent; default from herdr-plugin.toml)")
	fs.StringVar(&o.height, "height", "", "popup height (default from herdr-plugin.toml)")
	cmd.ValidArgsFunction = uiComplete(c)
	return cmd
}

func runUI(c *Context, o uiOpts, pos []string) int {
	if len(pos) != 2 || pos[0] != "open" {
		return actUsage(c, "ui", "want `ui open <entrypoint>`", uiUsage)
	}
	d, err := actNewDeps(c, actLight)
	if err != nil {
		return cmdFail(c, "ui", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return uiOpen(ctx, c, d, pos[1], o)
}

// uiOpen opens the plugin pane as a popup over the socket (herdr's CLI
// --placement flag rejects popup).
func uiOpen(ctx context.Context, c *Context, d *actDeps, entry string, o uiOpts) int {
	panes := uiManifest(d.Layout.Plugin())
	p, ok := panes[entry]
	if !ok {
		ids := make([]string, 0, len(panes))
		for id := range panes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return cmdFail(c, "ui", fmt.Errorf("unknown entrypoint %q: use one of %s ([[panes]] in herdr-plugin.toml)", entry, strings.Join(ids, ", ")))
	}
	opts := herdr.PluginPaneOptions{
		PluginID: d.getenv("HERDR_PLUGIN_ID"), Entrypoint: entry, Placement: herdr.PlacementPopup,
		Width: p.Width, Height: p.Height, WorkspaceID: o.workspace, Focus: true,
	}
	if opts.PluginID == "" {
		opts.PluginID = pluginID
	}
	if o.width != "" {
		opts.Width = o.width
	}
	if o.height != "" {
		opts.Height = o.height
	}
	if opts.WorkspaceID == "" {
		opts.WorkspaceID = actString(d.pluginContext(), "workspace_id")
	}
	for _, k := range uiPassEnv {
		if v := d.getenv(k); v != "" {
			if opts.Env == nil {
				opts.Env = map[string]string{}
			}
			opts.Env[k] = v
		}
	}
	// The --config flag (else $MAGNUM_CONFIG), absolute; nothing for the
	// layout's config.toml, which the popup finds on its own.
	if file := daemonConfigOverride(c); file != "" {
		if opts.Env == nil {
			opts.Env = map[string]string{}
		}
		opts.Env["MAGNUM_CONFIG"] = file
	}
	pane, err := d.Herdr.PluginPaneOpen(ctx, opts)
	if err != nil {
		return cmdFail(c, "ui", fmt.Errorf("open the %s popup: %w (is the plugin linked? `herdr plugin link %s`)", entry, d.herdrErr(err), d.Layout.Home))
	}
	if pane.Pane.ID != "" {
		fmt.Fprintf(c.Stdout, "opened %s (pane %s)\n", entry, pane.Pane.ID)
	} else {
		fmt.Fprintf(c.Stdout, "opened %s\n", entry)
	}
	return 0
}

// uiManifest reads the [[panes]] of herdr-plugin.toml, falling back to the
// shipped sizes.
func uiManifest(path string) map[string]uiPane {
	var m struct {
		Panes []uiPane `toml:"panes"`
	}
	if _, err := toml.DecodeFile(path, &m); err != nil || len(m.Panes) == 0 {
		return uiDefaults
	}
	out := map[string]uiPane{}
	for _, p := range m.Panes {
		if p.ID != "" {
			out[p.ID] = p
		}
	}
	return out
}

// uiComplete completes `ui open <entrypoint>` from herdr-plugin.toml.
func uiComplete(c *Context) cobra.CompletionFunc {
	return func(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		var out []cobra.Completion
		switch len(args) {
		case 0:
			out = append(out, cobra.CompletionWithDesc("open", "open a popup pane"))
		case 1:
			for id, p := range uiManifest(c.Layout.Plugin()) {
				out = append(out, cobra.CompletionWithDesc(id, fmt.Sprintf("%s %s x %s", p.Placement, p.Width, p.Height)))
			}
			sort.Strings(out)
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}
