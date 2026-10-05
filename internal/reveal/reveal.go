// Package reveal brings the herdr client to the front of the user's terminal:
// focus the existing client when there is one, otherwise open a new tab or
// window running `herdr --session <name>`.
//
// It is a Go port of the terminal layer of the Raycast herdr extension
// (terminal.ts, terminal-focus.ts, process-lookup.ts, terminal-config.ts).
// herdr clients are found through `ps -axo pid=,tty=,args=` (never pgrep, see
// docs/spikes.md probe 8) and matched to a terminal tab by tty: iTerm2 and
// Terminal.app through AppleScript, Ghostty through a temporary marker title
// (`herdr terminal title set`), WezTerm through `wezterm cli`. A custom
// launcher template and a generic `open -a <App>` cover everything else.
//
// Every subprocess (ps, osascript, open, wezterm, herdr, the custom launcher)
// runs through execx.Runner. Read-only lookups are not marked Mutates; anything
// that touches the UI (focus, launch, title set, activate) is, so DryRun prints
// it instead of running it. A DryRun therefore yields Unavailable focus results
// and an "activated" outcome, never a false "focused".
//
// The package never changes herdr state: it does not start servers, create
// workspaces or panes, and only sets (then clears) the outer terminal title
// for the Ghostty flow. Focusing a pane inside herdr (`herdr agent focus`) is
// the caller's job and happens before Reveal.
package reveal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
)

const (
	cmdPS        = "/bin/ps"
	cmdOsascript = "/usr/bin/osascript"
	cmdOpen      = "/usr/bin/open"
	cmdSh        = "/bin/sh"

	// focusTimeout bounds osascript/herdr/wezterm focus calls. The extension
	// used 450 ms for a UI hot path; magnum is a CLI, and the first AppleScript
	// call after an app launch can be slow.
	focusTimeout = 5 * time.Second
	// launchTimeout bounds creating a tab (iTerm2 may be cold-starting).
	launchTimeout = 20 * time.Second
	// clearTimeout/clearRetryTimeout bound the Ghostty title clear: a quick
	// attempt, then one generous retry so the marker title does not stick.
	clearTimeout      = 2 * time.Second
	clearRetryTimeout = 5 * time.Second

	defaultSession = "default"
	defaultHerdr   = "herdr"
)

// FocusResult is the outcome of looking for an existing herdr client.
type FocusResult string

const (
	// Focused: an existing client's tab/window was brought to the front.
	Focused FocusResult = "focused"
	// Missing: the terminal is scriptable and confirmed there is no client in it.
	Missing FocusResult = "missing"
	// Unavailable: the answer could not be determined (process list failed,
	// osascript denied or timed out, terminal cannot be scripted).
	Unavailable FocusResult = "unavailable"
)

// Action says what Reveal did.
type Action string

const (
	ActionFocused   Action = "focused"   // focused an existing client
	ActionLaunched  Action = "launched"  // opened a new client tab/window
	ActionActivated Action = "activated" // could only bring the terminal app forward
)

// Options tunes Launch and Reveal.
type Options struct {
	// NewWindow opens a new window instead of a tab in the current one.
	NewWindow bool
}

// Outcome describes what Reveal did, for `magnum open` output and --json.
type Outcome struct {
	Action  Action `json:"action"`
	Kind    Kind   `json:"kind"`
	Session string `json:"session"`
	// Detail carries the reason when the result is a fallback (for example why
	// focusing was unavailable).
	Detail string `json:"detail,omitempty"`
}

// String renders the outcome as one human-readable line.
func (o Outcome) String() string {
	var s string
	switch o.Action {
	case ActionFocused:
		s = "focused existing herdr client"
	case ActionLaunched:
		s = "launched herdr client"
	default:
		s = "activated terminal app"
	}
	s += fmt.Sprintf(" (%s, session %s)", o.Kind, o.Session)
	if o.Detail != "" {
		s += ": " + o.Detail
	}
	return s
}

var errCannotFocus = errors.New("this terminal cannot be scripted to focus an existing herdr client")

// Revealer reveals herdr in one configured terminal. Build it with New.
type Revealer struct {
	run      execx.Runner
	herdrBin string
	app      string
	launcher string
	session  string
	kind     Kind

	// Test seams.
	newMarker  func() string
	wezTermBin string
}

// New builds a Revealer from config.Terminal. herdrBin is the herdr executable
// (typed into the new terminal tab and used for the Ghostty title calls); empty
// means "herdr" on PATH. An empty session means "default".
func New(run execx.Runner, cfg config.Terminal, herdrBin string) *Revealer {
	session := strings.TrimSpace(cfg.Session)
	if session == "" {
		session = defaultSession
	}
	if herdrBin == "" {
		herdrBin = defaultHerdr
	}
	return &Revealer{
		run:       run,
		herdrBin:  herdrBin,
		app:       strings.TrimSpace(cfg.App),
		launcher:  strings.TrimSpace(cfg.Launcher),
		session:   session,
		kind:      DetectKind(cfg.App),
		newMarker: randomMarker,
	}
}

// Reveal is New(run, cfg, herdrBin).Reveal(ctx, opts).
func Reveal(ctx context.Context, run execx.Runner, cfg config.Terminal, herdrBin string, opts Options) (Outcome, error) {
	return New(run, cfg, herdrBin).Reveal(ctx, opts)
}

// Kind reports the detected terminal kind.
func (r *Revealer) Kind() Kind { return r.kind }

// Session reports the herdr session being revealed.
func (r *Revealer) Session() string { return r.session }

// Reveal focuses the existing herdr client when there is one. When the
// terminal confirms there is none it launches a new client (a tab, or a window
// with opts.NewWindow). When focus cannot be determined it only brings the
// terminal app forward (opening a second client could duplicate one it cannot
// see); a custom launcher has no app to raise, so it launches.
func (r *Revealer) Reveal(ctx context.Context, opts Options) (Outcome, error) {
	out := Outcome{Kind: r.kind, Session: r.session}
	res, focusErr := r.FocusExisting(ctx)
	switch res {
	case Focused:
		out.Action = ActionFocused
		return out, nil
	case Missing:
		if err := r.Launch(ctx, opts); err != nil {
			return out, fmt.Errorf("launch herdr client in %s: %w", r.kind, err)
		}
		out.Action = ActionLaunched
		return out, nil
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if r.kind == KindCustom {
		if err := r.Launch(ctx, opts); err != nil {
			return out, fmt.Errorf("launch herdr client with custom launcher: %w", err)
		}
		out.Action = ActionLaunched
		return out, nil
	}
	if focusErr != nil {
		out.Detail = focusErr.Error()
	}
	if err := r.bringToFront(ctx); err != nil {
		return out, fmt.Errorf("activate %s: %w", r.kind, err)
	}
	out.Action = ActionActivated
	return out, nil
}

// FocusExisting looks for a herdr client of the session in the configured
// terminal and focuses it. The error accompanies Unavailable and says why.
func (r *Revealer) FocusExisting(ctx context.Context) (FocusResult, error) {
	if err := ctx.Err(); err != nil {
		return Unavailable, err
	}
	switch r.kind {
	case KindITerm, KindTerminal:
		return r.focusByTty(ctx)
	case KindGhostty:
		return r.focusGhostty(ctx)
	case KindWezTerm:
		return r.focusWezTerm(ctx)
	default:
		return Unavailable, fmt.Errorf("%w (%s)", errCannotFocus, r.kind)
	}
}

// Probe lists the ttys the configured terminal reports for its panes (iTerm2,
// Terminal.app and WezTerm only), which shows that scripting works, e.g.
// that the macOS Automation permission was granted. `magnum doctor` does not
// call it: its AppleScript starts a terminal that is not running, and before
// the permission was decided macOS asks in a dialog that takes focus, while
// doctor must not open windows (doctor names the permission instead).
func (r *Revealer) Probe(ctx context.Context) ([]string, error) {
	switch r.kind {
	case KindITerm, KindTerminal:
		script := itermTtyListScript()
		if r.kind == KindTerminal {
			script = terminalTtyListScript()
		}
		res, err := r.run.Run(ctx, execx.Cmd{
			Name: cmdOsascript, Args: []string{"-e", script}, Timeout: focusTimeout, Label: "list " + string(r.kind) + " ttys",
		})
		if err != nil {
			return nil, fmt.Errorf("list %s ttys: %w", r.kind, err)
		}
		return parseTtyList(res.Out()), nil
	case KindWezTerm:
		listing, err := r.wezTermList(ctx)
		if err != nil {
			return nil, err
		}
		ttys, ok := wezTermTtys(listing)
		if !ok {
			return nil, errors.New("wezterm cli list did not return a JSON pane array")
		}
		return ttys, nil
	default:
		return nil, fmt.Errorf("%w (%s)", errCannotFocus, r.kind)
	}
}

// focusByTty serves iTerm2 and Terminal.app: find the client ttys with ps, then
// let AppleScript select the tab that owns one of them.
func (r *Revealer) focusByTty(ctx context.Context) (FocusResult, error) {
	ttys, err := r.clientTtys(ctx)
	if err != nil {
		return Unavailable, err
	}
	if len(ttys) == 0 {
		return Missing, nil
	}
	script := itermFocusScript(ttys)
	if r.kind == KindTerminal {
		script = terminalFocusScript(ttys)
	}
	res, err := r.run.Run(ctx, execx.Cmd{
		Name: cmdOsascript, Args: []string{"-e", script}, Timeout: focusTimeout, Mutates: true,
		Label: "focus herdr client in " + string(r.kind),
	})
	if err != nil {
		return Unavailable, fmt.Errorf("focus %s tab: %w", r.kind, err)
	}
	switch out := res.Out(); out {
	case "":
		return Unavailable, fmt.Errorf("focus %s tab: osascript printed nothing", r.kind)
	case "miss":
		return Missing, nil
	default:
		return Focused, nil
	}
}

// bringToFront activates the terminal app (`open -a`).
func (r *Revealer) bringToFront(ctx context.Context) error {
	name, err := r.activationName()
	if err != nil {
		return err
	}
	_, err = r.run.Run(ctx, execx.Cmd{
		Name: cmdOpen, Args: []string{"-a", name}, Timeout: launchTimeout, Mutates: true,
		Label: "activate " + name,
	})
	return err
}

// activationName is the application name `open -a` is given. Known kinds use
// the canonical name; a generic terminal uses the configured one.
func (r *Revealer) activationName() (string, error) {
	switch r.kind {
	case KindITerm:
		return "iTerm", nil
	case KindTerminal:
		return "Terminal", nil
	case KindGhostty:
		return "Ghostty", nil
	case KindWezTerm:
		return "WezTerm", nil
	}
	if r.kind == KindGeneric && !strings.EqualFold(r.app, "generic") && r.app != "" {
		return r.app, nil
	}
	return "", fmt.Errorf("terminal.app %q does not name an application to activate", r.app)
}

func randomMarker() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("herdr-magnum-%d", time.Now().UnixNano())
	}
	return "herdr-magnum-" + hex.EncodeToString(b[:])
}

// resolveWezTermBin finds the wezterm CLI: the app bundle's binary when
// installed in the usual places, else `wezterm` on PATH.
func resolveWezTermBin() string {
	candidates := []string{"/Applications/WezTerm.app/Contents/MacOS/wezterm"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, home+"/Applications/WezTerm.app/Contents/MacOS/wezterm")
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return "wezterm"
}

func (r *Revealer) wezTerm() string {
	if r.wezTermBin == "" {
		r.wezTermBin = resolveWezTermBin()
	}
	return r.wezTermBin
}

// wezTermList runs `wezterm cli list --format json`.
func (r *Revealer) wezTermList(ctx context.Context) (string, error) {
	res, err := r.run.Run(ctx, execx.Cmd{
		Name: r.wezTerm(), Args: []string{"cli", "list", "--format", "json"}, Timeout: focusTimeout,
		Label: "list wezterm panes",
	})
	if err != nil {
		return "", fmt.Errorf("wezterm cli list: %w", err)
	}
	return res.Out(), nil
}
