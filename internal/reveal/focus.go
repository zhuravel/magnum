package reveal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/execx"
)

// focusWezTerm matches the session's client ttys to WezTerm panes and
// activates the pane.
func (r *revealer) focusWezTerm(ctx context.Context) (FocusResult, error) {
	listing, err := r.wezTermList(ctx)
	if err != nil {
		return Unavailable, err
	}
	if listing == "" {
		return Unavailable, errors.New("wezterm cli list printed nothing")
	}
	if _, ok := parseWezTermPanes(listing); !ok {
		return Unavailable, errors.New("wezterm cli list did not return a JSON pane array")
	}
	ttys, err := r.clientTtys(ctx)
	if err != nil {
		return Unavailable, err
	}
	paneID := selectWezTermPane(listing, ttys)
	if paneID == "" {
		return Missing, nil
	}
	if _, err := r.run.Run(ctx, execx.Cmd{
		Name: r.wezTerm(), Args: []string{"cli", "activate-pane", "--pane-id", paneID},
		Timeout: focusTimeout, Mutates: true, Label: "activate wezterm pane " + paneID,
	}); err != nil {
		return Unavailable, fmt.Errorf("wezterm cli activate-pane: %w", err)
	}
	if _, dry := r.run.(*execx.DryRun); dry {
		// activate-pane was only planned, so nothing was focused. Reveal then
		// plans the app activation and reports "activated", like the other kinds.
		return Unavailable, errors.New("dry-run: wezterm cli activate-pane was not run")
	}
	// Raising the pane inside WezTerm does not raise the app; best effort.
	_ = r.bringToFront(ctx)
	return Focused, nil
}

// focusGhostty runs the marker-title flow: set a unique outer terminal title
// through herdr, let AppleScript focus the Ghostty terminal with that name,
// then clear the title.
func (r *revealer) focusGhostty(ctx context.Context) (FocusResult, error) {
	marker := r.newMarker()
	// The clear is armed before the set is awaited: a client-side timeout can
	// leave the title changed server-side, and clearing an unchanged title is
	// harmless.
	mustClear := true
	defer func() {
		if mustClear {
			r.clearTitle(ctx)
		}
	}()

	res, err := r.run.Run(ctx, r.herdrCmd(focusTimeout, "set outer title", "terminal", "title", "set", marker))
	if err != nil {
		return Unavailable, fmt.Errorf("herdr terminal title set: %w", err)
	}
	changed, reason, err := parseTitleReply(string(res.Stdout))
	if err != nil {
		return Unavailable, err
	}
	if !changed {
		mustClear = false
		if reason == "no_foreground_client" {
			return Missing, nil
		}
		return Unavailable, fmt.Errorf("herdr did not change the terminal title (%s)", reason)
	}

	osa, err := r.run.Run(ctx, execx.Cmd{
		Name: cmdOsascript, Args: []string{"-e", ghosttyFocusScript(marker)}, Timeout: focusTimeout,
		Mutates: true, Label: "focus ghostty terminal",
	})
	if err != nil {
		if ae, ok := r.automationDenied(err); ok {
			return Unavailable, ae
		}
		return Unavailable, fmt.Errorf("focus ghostty terminal: %w", err)
	}
	if osa.Out() != "focused" {
		return Unavailable, errors.New("ghostty has no terminal with the marker title")
	}
	return Focused, nil
}

// herdrCmd builds a herdr CLI call that always names the session, so an
// inherited HERDR_SESSION cannot retarget it.
func (r *revealer) herdrCmd(timeout time.Duration, label string, args ...string) execx.Cmd {
	return execx.Cmd{
		Name:    r.herdrBin,
		Args:    append([]string{"--session", r.session}, args...),
		Timeout: timeout,
		Mutates: true,
		Label:   "herdr " + label,
	}
}

// clearTitle removes the marker title. It must succeed even when the caller's
// context is already cancelled, so it runs on a context detached from it, with
// one generous retry.
func (r *revealer) clearTitle(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	if _, err := r.run.Run(ctx, r.herdrCmd(clearTimeout, "clear outer title", "terminal", "title", "clear")); err == nil {
		return
	}
	_, _ = r.run.Run(ctx, r.herdrCmd(clearRetryTimeout, "clear outer title (retry)", "terminal", "title", "clear"))
}

// parseTitleReply reads `herdr terminal title set` output, which is either a
// {"result":{"changed","reason"}} envelope or the bare object.
func parseTitleReply(out string) (changed bool, reason string, err error) {
	var v struct {
		Changed bool   `json:"changed"`
		Reason  string `json:"reason"`
		Result  *struct {
			Changed bool   `json:"changed"`
			Reason  string `json:"reason"`
		} `json:"result"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return false, "", fmt.Errorf("unexpected herdr reply %q: %w", truncate(execx.Redact(strings.TrimSpace(out)), 200), err)
	}
	if v.Error != nil {
		return false, "", fmt.Errorf("herdr: %s: %s", v.Error.Code, v.Error.Message)
	}
	if v.Result != nil {
		return v.Result.Changed, v.Result.Reason, nil
	}
	return v.Changed, v.Reason, nil
}

// truncate cuts s to at most n bytes, backing up to a rune boundary so the
// result stays valid UTF-8, and appends "..." when it cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}
