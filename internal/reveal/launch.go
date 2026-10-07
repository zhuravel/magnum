package reveal

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/zhuravel/magnum/internal/execx"
)

// detachScript runs the launcher in the background and returns at once, so a
// launcher that stays alive as the terminal itself (`kitty herdr`, `alacritty
// -e herdr`) does not hold the runner until its timeout kills the new window.
// A missing launcher executable still fails loudly.
const detachScript = `command -v "$1" >/dev/null 2>&1 || { echo "launcher not found: $1" >&2; exit 127; }
nohup "$@" >/dev/null 2>&1 &`

var paneID = regexp.MustCompile(`^\d+$`)

// clientArgv is the herdr client command line typed into a new terminal. The
// session is always named so a leaked HERDR_SESSION cannot retarget it.
func (r *Revealer) clientArgv() []string {
	return []string{r.herdrBin, "--session", r.session}
}

// launch opens a new herdr client for the session in the configured terminal:
// iTerm2 (new tab, or window with opts.NewWindow) and Terminal.app (do script),
// Ghostty (new surface configuration), WezTerm (`cli spawn`), a custom launcher
// template, or `open -a <App>` for generic terminals. A non-empty
// terminal.launcher wins over the app kind, as in the Raycast extension.
func (r *Revealer) launch(ctx context.Context, opts Options) error {
	if r.launcher != "" || r.kind == KindCustom {
		return r.launchCustom(ctx)
	}
	switch r.kind {
	case KindTerminal:
		return r.launchTerminal(ctx)
	case KindITerm:
		return r.launchITerm(ctx, opts)
	case KindGhostty:
		return r.launchGhostty(ctx, opts)
	case KindWezTerm:
		return r.launchWezTerm(ctx, opts)
	default:
		return r.bringToFront(ctx)
	}
}

func (r *Revealer) osascriptLaunch(ctx context.Context, label, script string) (execx.Result, error) {
	return r.run.Run(ctx, execx.Cmd{
		Name: cmdOsascript, Args: []string{"-e", script}, Timeout: launchTimeout, Mutates: true, Label: label,
	})
}

func (r *Revealer) launchTerminal(ctx context.Context) error {
	command := shellJoin(r.clientArgv()...)
	script := "tell application \"Terminal\"\nactivate\ndo script " + appleScriptString(command) + "\nend tell"
	if _, err := r.osascriptLaunch(ctx, "open herdr in Terminal", script); err != nil {
		return fmt.Errorf("terminal do script: %w", err)
	}
	return nil
}

func (r *Revealer) launchITerm(ctx context.Context, opts Options) error {
	placement := `if (count windows) > 0 then
  set targetWindow to current window
  set targetTab to create tab with default profile targetWindow
  set targetSession to current session of targetTab
else
  set targetWindow to create window with default profile
  set targetSession to current session of targetWindow
end if`
	if opts.NewWindow {
		placement = `set targetWindow to create window with default profile
set targetSession to current session of targetWindow`
	}
	script := "tell application \"iTerm\"\nactivate\n" + placement +
		"\ntell targetSession to write text " + appleScriptString(shellJoin(r.clientArgv()...)) + "\nend tell"
	if _, err := r.osascriptLaunch(ctx, "open herdr in iTerm2", script); err != nil {
		return fmt.Errorf("iterm create tab: %w", err)
	}
	return nil
}

func (r *Revealer) launchGhostty(ctx context.Context, opts Options) error {
	placement := `if (count windows) > 0 then
  new tab in front window with configuration cfg
else
  new window with configuration cfg
end if`
	if opts.NewWindow {
		placement = "new window with configuration cfg"
	}
	script := "tell application \"Ghostty\"\nactivate\nset cfg to new surface configuration\n" +
		"set command of cfg to " + appleScriptString(shellJoin(r.clientArgv()...)) + "\n" +
		placement + "\nreturn \"opened\"\nend tell"
	res, err := r.osascriptLaunch(ctx, "open herdr in Ghostty", script)
	if err == nil && res.Out() == "opened" {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) && r.hasClient(ctx) {
		// The script ran out of time (a cold start) but the tab it asked for
		// is there: a second Ghostty would duplicate the client.
		return nil
	}
	// Scripting unavailable (older Ghostty, Automation denied): start a new instance.
	_, err = r.run.Run(ctx, execx.Cmd{
		Name: cmdOpen, Args: append([]string{"-na", "Ghostty", "--args", "-e"}, r.clientArgv()...),
		Timeout: launchTimeout, Mutates: true, Label: "open herdr in new Ghostty",
	})
	if err != nil {
		return fmt.Errorf("open Ghostty: %w", err)
	}
	return nil
}

// hasClient reports whether a herdr client of the session is running now. A
// failing process list counts as no.
func (r *Revealer) hasClient(ctx context.Context) bool {
	ttys, err := r.clientTtys(ctx)
	return err == nil && len(ttys) > 0
}

func (r *Revealer) launchWezTerm(ctx context.Context, opts Options) error {
	placement := []string{"--new-window"}
	if !opts.NewWindow {
		if listing, err := r.wezTermList(ctx); err == nil {
			if id := selectWezTermWindow(listing); id != "" {
				placement = []string{"--window-id", id}
			}
		}
	}
	args := append([]string{"cli", "spawn"}, placement...)
	args = append(args, "--")
	args = append(args, r.clientArgv()...)
	res, err := r.run.Run(ctx, execx.Cmd{
		Name: r.wezTerm(), Args: args, Timeout: focusTimeout, Mutates: true, Label: "spawn herdr in WezTerm",
	})
	if err == nil && paneID.MatchString(res.Out()) {
		_ = r.bringToFront(ctx)
		return nil
	}
	// No running GUI to spawn into: start WezTerm itself with the command.
	_, err = r.run.Run(ctx, execx.Cmd{
		Name: cmdOpen, Args: append([]string{"-na", "WezTerm", "--args", "start", "--"}, r.clientArgv()...),
		Timeout: launchTimeout, Mutates: true, Label: "open herdr in new WezTerm",
	})
	if err != nil {
		return fmt.Errorf("open WezTerm: %w", err)
	}
	return nil
}

func (r *Revealer) launchCustom(ctx context.Context) error {
	if r.launcher == "" {
		return errors.New(`terminal.app is "custom" but terminal.launcher is empty`)
	}
	argv := r.clientArgv()
	exe, args, err := expandCustomLauncher(r.launcher, argv[0], argv[1:])
	if err != nil {
		return fmt.Errorf("terminal.launcher: %w", err)
	}
	_, err = r.run.Run(ctx, execx.Cmd{
		Name:    cmdSh,
		Args:    append([]string{"-c", detachScript, "magnum-launcher", exe}, args...),
		Timeout: focusTimeout, Mutates: true,
		Label: "custom launcher " + strings.TrimSpace(exe),
	})
	if err != nil {
		return fmt.Errorf("custom launcher %s: %w", exe, err)
	}
	return nil
}
