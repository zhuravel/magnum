package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
)

// Codex's startup hooks review (Codex 0.160, tui/src/startup_hooks_review.rs)
// appears when a session starts or resumes in a checkout whose hooks are new
// or changed (a PR branch changed the repository's Codex hooks):
//
//	Hooks need review
//	3 hooks are new or changed.
//	Hooks can run outside the sandbox after you trust them.
//	› 1. Review hooks
//	  2. Trust all and continue
//	  3. Continue without trusting (hooks won't run)
//
// Typed into, it eats the next prompt. magnum never trusts hooks (the PR
// controls them and they run outside the sandbox): it picks "Continue without
// trusting", as it says No to permission prompts.
const (
	hooksTitle   = "Hooks need review"
	hooksDecline = "Continue without trusting (hooks won't run)"
)

var hooksOptions = []string{"Review hooks", "Trust all and continue", hooksDecline}

// EventHooksDeclined is the event kind recorded each time magnum declines a
// hooks review.
const EventHooksDeclined = "agents.hooks_declined"

// hooksDialog is a hooks review found on screen: the cursor's option and the
// decline option, as indexes into hooksOptions.
type hooksDialog struct{ cursor, decline int }

// detectHooksDialog finds the hooks review in a pane's visible text: its
// title, then its three options on adjacent lines in order, one of them
// carrying the cursor.
func detectHooksDialog(text string) (hooksDialog, bool) {
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
	d := hooksDialog{cursor: -1, decline: len(hooksOptions) - 1}
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

// declineHooks answers Codex's hooks review on ref's screen with "Continue
// without trusting": the cursor moves down (or up) to it one key at a time,
// a re-read confirms it got there, then Enter, and magnum waits for the agent
// to be idle. Nothing else on screen is touched (false, nil). It records
// EventHooksDeclined.
func (m *Manager) declineHooks(ctx context.Context, prID int64, role Role, ref paneRef) (bool, error) {
	text, err := m.readVisible(ctx, ref)
	if err != nil {
		return false, err
	}
	d, ok := detectHooksDialog(text)
	if !ok {
		return false, nil
	}
	var keys []string
	if d.cursor != d.decline {
		move, n := "down", d.decline-d.cursor
		if n < 0 {
			move, n = "up", -n
		}
		for range n {
			if err := m.sendKeys(ctx, ref, move); err != nil {
				return false, fmt.Errorf("agents: hooks review in %s: %w", ref, err)
			}
			keys = append(keys, move)
		}
		if !m.hooksCursorOnDecline(ctx, ref) {
			return false, fmt.Errorf("agents: hooks review in %s: the cursor did not reach %q; not confirming", ref, hooksDecline)
		}
	}
	if err := m.sendKeys(ctx, ref, "enter"); err != nil {
		return false, fmt.Errorf("agents: hooks review in %s: %w", ref, err)
	}
	keys = append(keys, "enter")
	msg := fmt.Sprintf("declined the Codex hooks review of %s in %s: continued without trusting the checkout's hooks (%s)",
		role, ref, strings.Join(keys, ", "))
	m.logf("agents: %s", msg)
	data, _ := json.Marshal(map[string]any{"role": string(role), "agent": ref.name, "pane": ref.pane, "keys": keys})
	subject := m.prSubject(ctx, prID)
	if _, err := m.d.Store.AppendEvent(context.WithoutCancel(ctx), store.Event{Level: "info", Subject: &subject,
		Kind: EventHooksDeclined, Message: execx.Redact(msg), Data: data}); err != nil {
		m.logf("agents: event %s: %v", EventHooksDeclined, err)
	}
	m.waitTrustReady(ctx, ref)
	return true, nil
}

// hooksCursorOnDecline re-reads the hooks review until the cursor shows on
// the decline option.
func (m *Manager) hooksCursorOnDecline(ctx context.Context, ref paneRef) bool {
	for range trustKeyRereads {
		if m.sleep(ctx, trustKeyDelay) != nil {
			return false
		}
		text, err := m.readVisible(ctx, ref)
		if err != nil {
			continue
		}
		if d, ok := detectHooksDialog(text); ok && d.cursor == d.decline {
			return true
		}
	}
	return false
}
