package agents

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
)

// Codex's startup hooks review (Codex 0.160, tui/src/startup_hooks_review.rs)
// appears when a session starts or resumes and hooks Codex has not trusted yet
// are new or changed: the user's own (CODEX_HOME's hooks.json or config.toml,
// an installed plugin's hooks/hooks.json; a tool that installs hooks rewrote
// them) or a repository's .codex/ hooks (which a PR controls):
//
//	Hooks need review
//	3 hooks are new or changed.
//	Hooks can run outside the sandbox after you trust them.
//	› 1. Review hooks
//	  2. Trust all and continue
//	  3. Continue without trusting (hooks won't run)
//
// Typed into, it eats the next prompt. The dialog does not say where a hook
// comes from, but the checkout does: with the kind's on_hooks_review
// "trust_own" (default) magnum trusts them when the checkout declares no
// hooks of its own (checkoutHooks), so every hook listed is the user's, and
// declines them otherwise, as it says No to permission prompts. Declining
// holds for the one session and records no distrust.
const (
	hooksTitle   = "Hooks need review"
	hooksTrust   = "Trust all and continue"
	hooksDecline = "Continue without trusting (hooks won't run)"
)

var hooksOptions = []string{"Review hooks", hooksTrust, hooksDecline}

// The options magnum picks, as indexes into hooksOptions.
const (
	hooksTrustOption   = 1
	hooksDeclineOption = 2
)

// Event kinds recorded each time magnum answers a hooks review.
const (
	EventHooksTrusted  = "agents.hooks_trusted"
	EventHooksDeclined = "agents.hooks_declined"
)

// hooksDialog is a hooks review found on screen: the cursor's option, as an
// index into hooksOptions, and key, the dialog's identity (promptKey of its
// lines from the title to the last option), which tells a re-read whether
// the screen still shows the same dialog.
type hooksDialog struct {
	cursor int
	key    string
}

// detectHooksDialog finds a live hooks review at the bottom of a pane's
// visible text, as the trust and permission dialogs are found: its title
// (the last one on screen), then its three options on adjacent lines in
// order, one of them carrying the cursor, with nothing below them but blank
// and key-hint lines. Dialog text that output, the composer or another
// dialog follows is not the dialog, and a screen showing a permission prompt
// (detectPermissionPrompt) has none: no key magnum presses for the hooks
// review may land on an approval.
func detectHooksDialog(text string) (hooksDialog, bool) {
	if _, ok := detectPermissionPrompt(text); ok {
		return hooksDialog{}, false
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	title := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == hooksTitle {
			title = i
		}
	}
	if title < 0 {
		return hooksDialog{}, false
	}
	first := -1
	for i := title + 1; i < len(lines); i++ {
		if m := trustOptionLine.FindStringSubmatch(lines[i]); m != nil && m[2] == hooksOptions[0] {
			first = i
			break
		}
	}
	if first < 0 || first+len(hooksOptions) > len(lines) {
		return hooksDialog{}, false
	}
	last := first + len(hooksOptions) - 1
	for _, l := range lines[last+1:] {
		if t := stripBox(l); t != "" && !permHint.MatchString(t) {
			return hooksDialog{}, false
		}
	}
	d := hooksDialog{cursor: -1, key: promptKey(lines[title : last+1])}
	for k, want := range hooksOptions {
		m := trustOptionLine.FindStringSubmatch(lines[first+k])
		if m == nil || m[2] != want {
			return hooksDialog{}, false
		}
		if m[1] != "" {
			if d.cursor >= 0 {
				return hooksDialog{}, false // two cursors: not this dialog
			}
			d.cursor = k
		}
	}
	if d.cursor < 0 {
		return hooksDialog{}, false
	}
	return d, true
}

// hooksChoice is the option magnum picks on a hooks review of a kind's agent
// working in dir, and why.
func (m *Manager) hooksChoice(kind, dir string) (int, string) {
	if k, ok := m.kindSpec(kind); !ok || k.OnHooksReview != config.HooksTrustOwn {
		return hooksDeclineOption, "on_hooks_review is not " + config.HooksTrustOwn
	}
	switch file, err := checkoutHooks(dir); {
	case err != nil:
		return hooksDeclineOption, err.Error()
	case file != "":
		return hooksDeclineOption, "the checkout declares hooks in " + file
	}
	return hooksTrustOption, dir + " declares no hooks, so every hook listed is your own (CODEX_HOME or an installed plugin)"
}

// answerHooks answers Codex's hooks review on ref's screen with the option
// hooksChoice picks for the checkout dir: the cursor moves down (or up) to it
// one key at a time, then the screen is read again and Enter goes only to the
// same dialog with the cursor on the choice; magnum then waits for the agent
// to be idle. Nothing else on screen is touched (false, nil). It records
// EventHooksTrusted or EventHooksDeclined.
func (m *Manager) answerHooks(ctx context.Context, prID int64, role Role, kind string, ref paneRef, dir string) (bool, error) {
	text, err := m.readVisible(ctx, ref)
	if err != nil {
		return false, err
	}
	d, ok := detectHooksDialog(text)
	if !ok {
		return false, nil
	}
	choice, why := m.hooksChoice(kind, dir)
	var keys []string
	moved := d.cursor != choice
	if moved {
		move, n := "down", choice-d.cursor
		if n < 0 {
			move, n = "up", -n
		}
		for range n {
			if err := m.sendKeys(ctx, ref, move); err != nil {
				return false, fmt.Errorf("agents: hooks review in %s: %w", ref, err)
			}
			keys = append(keys, move)
		}
	}
	if !m.hooksCursorOn(ctx, ref, d.key, choice, moved) {
		what := "the screen no longer shows the same hooks review"
		if moved {
			what = fmt.Sprintf("the cursor did not reach %q", hooksOptions[choice])
		}
		return false, fmt.Errorf("agents: hooks review in %s: %s; not confirming", ref, what)
	}
	if err := m.sendKeys(ctx, ref, "enter"); err != nil {
		return false, fmt.Errorf("agents: hooks review in %s: %w", ref, err)
	}
	keys = append(keys, "enter")
	kindEv, msg := EventHooksTrusted, fmt.Sprintf("trusted the Codex hooks review of %s in %s: %s (%s)", role, ref, why, strings.Join(keys, ", "))
	if choice == hooksDeclineOption {
		kindEv, msg = EventHooksDeclined, fmt.Sprintf("declined the Codex hooks review of %s in %s: %s; this session runs without the "+
			"untrusted hooks; trust them once in your own Codex to stop the dialog (%s)", role, ref, why, strings.Join(keys, ", "))
	}
	m.event(ctx, m.prSubject(ctx, prID), "info", kindEv, msg, map[string]any{"role": string(role), "agent": ref.name,
		"pane": ref.pane, "keys": keys, "option": hooksOptions[choice], "checkout": dir})
	m.waitTrustReady(ctx, ref)
	return true, nil
}

// hooksCursorOn re-reads ref's screen until it shows the hooks review
// identified by key with the cursor on option: once, right away, when the
// cursor did not move; after a move up to trustKeyRereads times, each after
// trustKeyDelay. The last read is the one Enter follows.
func (m *Manager) hooksCursorOn(ctx context.Context, ref paneRef, key string, option int, moved bool) bool {
	tries := 1
	if moved {
		tries = trustKeyRereads
	}
	for range tries {
		if moved && m.sleep(ctx, trustKeyDelay) != nil {
			return false
		}
		text, err := m.readVisible(ctx, ref)
		if err != nil {
			continue
		}
		if d, ok := detectHooksDialog(text); ok && d.key == key && d.cursor == option {
			return true
		}
	}
	return false
}

// hooksKeys are the config.toml keys through which a project layer brings
// hooks into a session: hooks themselves, the features that turn them on and
// plugins (and their marketplaces), which carry their own.
var hooksKeys = []string{"hooks", "plugin_hooks", "plugins", "marketplaces"}

// codexConfigMax caps the .codex/config.toml checkoutHooks reads: the PR
// controls it, and a project config is a few KiB.
const codexConfigMax = 1 << 20

// checkoutHooks names the file through which the checkout at dir declares
// Codex hooks of its own: .codex/hooks.json, or a hooksKeys key at any depth
// of .codex/config.toml, in each project layer Codex reads (dir and its
// parents up to the first one holding .git; dir alone outside a
// repository). "" = none. An error means magnum cannot tell: dir unknown or
// missing, a file unreadable or not TOML, or a config.toml it does not read
// (fsx.ReadRegular: a symlink, a special file, past codexConfigMax).
// Untracked files count: a previous round's agent may have written them.
func checkoutHooks(dir string) (string, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return "", errors.New("the checkout is unknown")
	}
	dir = filepath.Clean(dir)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("the checkout %s is not a directory", dir)
	}
	layers := []string{dir}
	for d := dir; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(d)
		if parent == d {
			layers = []string{dir} // no repository: Codex reads dir alone
			break
		}
		d = parent
		layers = append(layers, d)
	}
	for _, d := range layers {
		p := filepath.Join(d, ".codex", "hooks.json")
		if _, err := os.Lstat(p); err == nil {
			return p, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("cannot read %s: %w", p, err)
		}
		p = filepath.Join(d, ".codex", "config.toml")
		b, err := fsx.ReadRegular(p, codexConfigMax)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			if pe, ok := errors.AsType[*fs.PathError](err); ok {
				err = pe.Err // the path is named once
			}
			return "", fmt.Errorf("cannot read %s: %w", p, err)
		}
		var doc map[string]any
		if _, err := toml.Decode(string(b), &doc); err != nil {
			return "", fmt.Errorf("%s is not valid TOML", p) // the parser's message may quote the file
		}
		if declaresHooks(doc) {
			return p, nil
		}
	}
	return "", nil
}

// declaresHooks reports whether a hooksKeys key appears at any depth of a
// decoded TOML document.
func declaresHooks(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, sub := range x {
			if slices.Contains(hooksKeys, k) || declaresHooks(sub) {
				return true
			}
		}
	case []map[string]any:
		for _, sub := range x {
			if declaresHooks(sub) {
				return true
			}
		}
	case []any:
		if slices.ContainsFunc(x, declaresHooks) {
			return true
		}
	}
	return false
}
