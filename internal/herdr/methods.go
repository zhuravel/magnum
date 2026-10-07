package herdr

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Split directions for PaneSplit.
const (
	SplitRight = "right"
	SplitDown  = "down"
)

// Read sources for PaneRead/AgentRead/PaneWaitOutput.
const (
	SourceVisible         = "visible"
	SourceRecent          = "recent"
	SourceRecentUnwrapped = "recent_unwrapped"
	SourceDetection       = "detection"
)

// Read formats.
const (
	FormatText = "text"
	FormatANSI = "ansi"
)

// Plugin pane placements. The CLI rejects popup; the socket accepts it.
const (
	PlacementPopup   = "popup"
	PlacementOverlay = "overlay"
	PlacementSplit   = "split"
	PlacementTab     = "tab"
	PlacementZoomed  = "zoomed"
)

func ms(d time.Duration) int64 {
	if d > 0 && d < time.Millisecond {
		return 1
	}
	return d.Milliseconds()
}

// cliSource renders a read source the way the CLI spells it (hyphens).
func cliSource(s string) string { return strings.ReplaceAll(s, "_", "-") }

// envFlags renders the --env flags of the logged CLI line, sorted by key. The
// values are never logged (role environments can carry API keys that no
// pattern-based redaction would recognise): each shows as KEY=<redacted>.
// The socket payload keeps the real values.
func envFlags(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, "--env", k+"=<redacted>")
	}
	return out
}

func focusFlag(focus bool) string {
	if focus {
		return "--focus"
	}
	return "--no-focus"
}

// Ping checks the server and returns its version and protocol.
func (c *Client) Ping(ctx context.Context) (ServerInfo, error) {
	var out ServerInfo
	err := c.do(ctx, callSpec{method: "ping", out: &out, cli: cliLine("status", "server")})
	return out, err
}

// Snapshot returns the whole live session (session.snapshot).
func (c *Client) Snapshot(ctx context.Context) (Snapshot, error) {
	var out struct {
		Snapshot Snapshot `json:"snapshot"`
	}
	err := c.do(ctx, callSpec{method: "session.snapshot", out: &out, cli: cliLine("api", "snapshot")})
	return out.Snapshot, err
}

// WorkspaceCreateOptions configures WorkspaceCreate.
type WorkspaceCreateOptions struct {
	Cwd   string
	Label string
	Env   map[string]string // pane environment for the root pane's shell
	Focus bool
}

// WorkspaceCreate opens a workspace; its root pane runs a fresh shell.
func (c *Client) WorkspaceCreate(ctx context.Context, o WorkspaceCreateOptions) (WorkspaceCreated, error) {
	params := struct {
		Cwd   string            `json:"cwd,omitempty"`
		Focus bool              `json:"focus"`
		Label string            `json:"label,omitempty"`
		Env   map[string]string `json:"env,omitempty"`
	}{o.Cwd, o.Focus, o.Label, o.Env}
	args := []string{"workspace", "create"}
	if o.Cwd != "" {
		args = append(args, "--cwd", o.Cwd)
	}
	if o.Label != "" {
		args = append(args, "--label", o.Label)
	}
	args = append(append(args, envFlags(o.Env)...), focusFlag(o.Focus))
	var out WorkspaceCreated
	err := c.do(ctx, callSpec{method: "workspace.create", params: params, out: &out, cli: cliLine(args...)})
	return out, err
}

// WorkspaceClose closes a workspace and every pane in it.
func (c *Client) WorkspaceClose(ctx context.Context, workspaceID string) error {
	params := map[string]string{"workspace_id": workspaceID}
	return c.do(ctx, callSpec{method: "workspace.close", params: params, cli: cliLine("workspace", "close", workspaceID)})
}

// WorkspaceReportMetadata publishes display-only tokens (sidebar/tab-bar)
// for a workspace under source. ttl zero means no expiry.
func (c *Client) WorkspaceReportMetadata(ctx context.Context, workspaceID, source string, tokens map[string]string, ttl time.Duration) error {
	if tokens == nil {
		tokens = map[string]string{}
	}
	params := struct {
		WorkspaceID string            `json:"workspace_id"`
		Source      string            `json:"source"`
		Tokens      map[string]string `json:"tokens"`
		TTLMS       int64             `json:"ttl_ms,omitempty"`
	}{workspaceID, source, tokens, ms(ttl)}
	args := []string{"workspace", "report-metadata", workspaceID, "--source", source}
	keys := make([]string, 0, len(tokens))
	for k := range tokens {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--token", k+"="+tokens[k])
	}
	if ttl > 0 {
		args = append(args, "--ttl-ms", strconv.FormatInt(ms(ttl), 10))
	}
	return c.do(ctx, callSpec{method: "workspace.report_metadata", params: params, cli: cliLine(args...)})
}

// SplitOptions configures PaneSplit.
type SplitOptions struct {
	Direction string // SplitRight or SplitDown
	Cwd       string
	Env       map[string]string
	Focus     bool
}

// PaneSplit splits paneID and returns the new pane (a fresh shell).
func (c *Client) PaneSplit(ctx context.Context, paneID string, o SplitOptions) (Pane, error) {
	params := struct {
		TargetPaneID string            `json:"target_pane_id"`
		Direction    string            `json:"direction"`
		Cwd          string            `json:"cwd,omitempty"`
		Focus        bool              `json:"focus"`
		Env          map[string]string `json:"env,omitempty"`
	}{paneID, o.Direction, o.Cwd, o.Focus, o.Env}
	args := []string{"pane", "split", paneID, "--direction", o.Direction}
	if o.Cwd != "" {
		args = append(args, "--cwd", o.Cwd)
	}
	args = append(append(args, envFlags(o.Env)...), focusFlag(o.Focus))
	var out struct {
		Pane Pane `json:"pane"`
	}
	err := c.do(ctx, callSpec{method: "pane.split", params: params, out: &out, cli: cliLine(args...)})
	return out.Pane, err
}

// PaneRun types command into the pane followed by Enter, atomically
// (the CLI's `pane run`, which is pane.send_input on the socket).
func (c *Client) PaneRun(ctx context.Context, paneID, command string) error {
	params := struct {
		PaneID string   `json:"pane_id"`
		Text   string   `json:"text"`
		Keys   []string `json:"keys"`
	}{paneID, command, []string{"Enter"}}
	return c.do(ctx, callSpec{method: "pane.send_input", params: params, cli: cliLine("pane", "run", paneID, command)})
}

// ReadOptions configures PaneRead and AgentRead. Zero values mean
// Source recent_unwrapped, Format text, server-default line count.
type ReadOptions struct {
	Source string
	Lines  int
	Format string
}

type readParams struct {
	PaneID    string `json:"pane_id,omitempty"`
	Target    string `json:"target,omitempty"`
	Source    string `json:"source"`
	Lines     int    `json:"lines,omitempty"`
	Format    string `json:"format"`
	StripANSI bool   `json:"strip_ansi"`
}

func (o ReadOptions) params() (readParams, []string) {
	if o.Source == "" {
		o.Source = SourceRecentUnwrapped
	}
	if o.Format == "" {
		o.Format = FormatText
	}
	// strip_ansi mirrors the CLI; format "ansi" keeps escapes regardless.
	p := readParams{Source: o.Source, Lines: o.Lines, Format: o.Format, StripANSI: true}
	flags := []string{"--source", cliSource(o.Source)}
	if o.Lines > 0 {
		flags = append(flags, "--lines", strconv.Itoa(o.Lines))
	}
	return p, append(flags, "--format", o.Format)
}

// PaneRead returns recent pane output.
func (c *Client) PaneRead(ctx context.Context, paneID string, o ReadOptions) (ReadResult, error) {
	p, flags := o.params()
	p.PaneID = paneID
	var out struct {
		Read ReadResult `json:"read"`
	}
	err := c.do(ctx, callSpec{method: "pane.read", params: p, out: &out, cli: cliLine(append([]string{"pane", "read", paneID}, flags...)...)})
	return out.Read, err
}

// PaneRename sets a pane label; "" clears it.
func (c *Client) PaneRename(ctx context.Context, paneID, label string) error {
	var l *string
	args := []string{"pane", "rename", paneID}
	if label != "" {
		l = &label
		args = append(args, label)
	}
	params := struct {
		PaneID string  `json:"pane_id"`
		Label  *string `json:"label"`
	}{paneID, l}
	return c.do(ctx, callSpec{method: "pane.rename", params: params, cli: cliLine(args...)})
}

// PaneProcessInfo returns the pane's shell pid and foreground process group.
func (c *Client) PaneProcessInfo(ctx context.Context, paneID string) (ProcessInfo, error) {
	var out struct {
		ProcessInfo ProcessInfo `json:"process_info"`
	}
	err := c.do(ctx, callSpec{method: "pane.process_info", params: map[string]string{"pane_id": paneID}, out: &out,
		cli: cliLine("pane", "process-info", "--pane", paneID)})
	return out.ProcessInfo, err
}

// WaitOutputOptions configures PaneWaitOutput.
type WaitOutputOptions struct {
	Match   string // literal substring, or a Rust regex when Regex is set
	Regex   bool
	Source  string        // "" = recent (the CLI default)
	Lines   int           // 0 = server default
	Timeout time.Duration // 0 = wait until ctx is done
}

// PaneWaitOutput blocks server-side until the pane output matches. Existing
// output counts. herdr's own timeout surfaces as code "timeout".
func (c *Client) PaneWaitOutput(ctx context.Context, paneID string, o WaitOutputOptions) (OutputMatch, error) {
	if o.Source == "" {
		o.Source = SourceRecent
	}
	kind, flag := "substring", "--match"
	if o.Regex {
		kind, flag = "regex", "--regex"
	}
	params := struct {
		PaneID    string            `json:"pane_id"`
		Source    string            `json:"source"`
		Lines     int               `json:"lines,omitempty"`
		Match     map[string]string `json:"match"`
		TimeoutMS int64             `json:"timeout_ms,omitempty"`
		StripANSI bool              `json:"strip_ansi"`
	}{paneID, o.Source, o.Lines, map[string]string{"type": kind, "value": o.Match}, ms(o.Timeout), true}
	args := []string{"pane", "wait-output", paneID, flag, o.Match, "--source", cliSource(o.Source)}
	if o.Lines > 0 {
		args = append(args, "--lines", strconv.Itoa(o.Lines))
	}
	if o.Timeout > 0 {
		args = append(args, "--timeout", strconv.FormatInt(ms(o.Timeout), 10))
	}
	var out OutputMatch
	err := c.do(ctx, callSpec{method: "pane.wait_for_output", params: params, out: &out, cli: cliLine(args...),
		wait: o.Timeout, unbounded: o.Timeout <= 0})
	return out, err
}

// PaneSendKeys sends key presses (herdr grammar: Enter, ctrl+c, "1", …).
func (c *Client) PaneSendKeys(ctx context.Context, paneID string, keys ...string) error {
	params := struct {
		PaneID string   `json:"pane_id"`
		Keys   []string `json:"keys"`
	}{paneID, keys}
	return c.do(ctx, callSpec{method: "pane.send_keys", params: params, cli: cliLine(append([]string{"pane", "send-keys", paneID}, keys...)...)})
}

// PaneGet returns one pane.
func (c *Client) PaneGet(ctx context.Context, paneID string) (Pane, error) {
	var out struct {
		Pane Pane `json:"pane"`
	}
	err := c.do(ctx, callSpec{method: "pane.get", params: map[string]string{"pane_id": paneID}, out: &out, cli: cliLine("pane", "get", paneID)})
	return out.Pane, err
}

// AgentStartOptions configures AgentStart.
type AgentStartOptions struct {
	Name    string // [a-z][a-z0-9_-]{0,31}, unique per server
	Kind    string // codex | claude | …
	PaneID  string // must be an idle shell (see WaitIdleShell)
	Timeout time.Duration
	Args    []string // extra argv after the agent command
}

// AgentStart launches an agent in an idle shell pane and names it.
func (c *Client) AgentStart(ctx context.Context, o AgentStartOptions) (AgentStarted, error) {
	params := struct {
		Name      string   `json:"name"`
		Kind      string   `json:"kind"`
		PaneID    string   `json:"pane_id"`
		Args      []string `json:"args,omitempty"`
		TimeoutMS int64    `json:"timeout_ms,omitempty"`
	}{o.Name, o.Kind, o.PaneID, o.Args, ms(o.Timeout)}
	args := []string{"agent", "start", o.Name, "--kind", o.Kind, "--pane", o.PaneID}
	if o.Timeout > 0 {
		args = append(args, "--timeout", strconv.FormatInt(ms(o.Timeout), 10))
	}
	if len(o.Args) > 0 {
		args = append(append(args, "--"), o.Args...)
	}
	wait := o.Timeout
	if wait <= 0 {
		wait = maxAgentStartTimeout
	}
	var out AgentStarted
	err := c.do(ctx, callSpec{method: "agent.start", params: params, out: &out, cli: cliLine(args...), wait: wait})
	return out, err
}

// PromptWait makes AgentPrompt wait server-side for the first matching
// status after submission. Empty Until matches idle, done or blocked;
// Timeout zero waits until ctx is done.
type PromptWait struct {
	Until   []Status
	Timeout time.Duration
}

// AgentPrompt submits text to an agent. With wait nil it returns once the
// text is accepted. Errors: agent_blocked (rejected before sending),
// agent_prompt_stalled (no working/blocked within 5 s), timeout.
func (c *Client) AgentPrompt(ctx context.Context, target, text string, wait *PromptWait) (AgentInfo, error) {
	type waitParams struct {
		Until     []Status `json:"until,omitempty"`
		TimeoutMS int64    `json:"timeout_ms,omitempty"`
	}
	params := struct {
		Target string      `json:"target"`
		Text   string      `json:"text"`
		Wait   *waitParams `json:"wait,omitempty"`
	}{Target: target, Text: text}
	args := []string{"agent", "prompt", target, text}
	sp := callSpec{method: "agent.prompt"}
	if wait != nil {
		params.Wait = &waitParams{Until: wait.Until, TimeoutMS: ms(wait.Timeout)}
		args = append(args, "--wait")
		for _, u := range wait.Until {
			args = append(args, "--until", string(u))
		}
		if wait.Timeout > 0 {
			args = append(args, "--timeout", strconv.FormatInt(ms(wait.Timeout), 10))
			sp.wait = wait.Timeout
		} else {
			sp.unbounded = true
		}
	}
	var out struct {
		Agent AgentInfo `json:"agent"`
	}
	sp.params, sp.out, sp.cli = params, &out, cliLine(args...)
	err := c.do(ctx, sp)
	return out.Agent, err
}

// AgentRead returns recent output of an agent's pane.
func (c *Client) AgentRead(ctx context.Context, target string, o ReadOptions) (ReadResult, error) {
	p, flags := o.params()
	p.Target = target
	var out struct {
		Read ReadResult `json:"read"`
	}
	err := c.do(ctx, callSpec{method: "agent.read", params: p, out: &out, cli: cliLine(append([]string{"agent", "read", target}, flags...)...)})
	return out.Read, err
}

// AgentFocus focuses the agent's pane inside herdr.
func (c *Client) AgentFocus(ctx context.Context, target string) error {
	return c.do(ctx, callSpec{method: "agent.focus", params: map[string]string{"target": target}, cli: cliLine("agent", "focus", target)})
}

// AgentRename renames an agent.
func (c *Client) AgentRename(ctx context.Context, target, name string) error {
	params := map[string]string{"target": target, "name": name}
	return c.do(ctx, callSpec{method: "agent.rename", params: params, cli: cliLine("agent", "rename", target, name)})
}

// AgentSendKeys sends key presses to an agent's pane.
func (c *Client) AgentSendKeys(ctx context.Context, target string, keys ...string) error {
	params := struct {
		Target string   `json:"target"`
		Keys   []string `json:"keys"`
	}{target, keys}
	return c.do(ctx, callSpec{method: "agent.send_keys", params: params, cli: cliLine(append([]string{"agent", "send-keys", target}, keys...)...)})
}

// NotificationShow shows a herdr toast (delivered per herdr's [ui.toast]).
func (c *Client) NotificationShow(ctx context.Context, title, body string) (NotificationResult, error) {
	params := struct {
		Title string `json:"title"`
		Body  string `json:"body,omitempty"`
	}{title, body}
	args := []string{"notification", "show", title}
	if body != "" {
		args = append(args, "--body", body)
	}
	var out NotificationResult
	err := c.do(ctx, callSpec{method: "notification.show", params: params, out: &out, cli: cliLine(args...)})
	return out, err
}

// PluginPaneOptions configures PluginPaneOpen.
type PluginPaneOptions struct {
	PluginID     string
	Entrypoint   string
	Placement    string // PlacementPopup, …; "" = manifest default
	Width        string // popup size: cells ("30") or percent ("80%")
	Height       string
	WorkspaceID  string
	TargetPaneID string
	Direction    string // for split placement
	Cwd          string
	Env          map[string]string
	Focus        bool
}

// popupSize encodes "30" as a number and "80%" as a string, as herdr expects.
func popupSize(s string) any {
	if s == "" {
		return nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return s
}

// PluginPaneOpen opens a plugin pane; only the socket accepts popup placement.
func (c *Client) PluginPaneOpen(ctx context.Context, o PluginPaneOptions) (PluginPane, error) {
	params := struct {
		PluginID     string            `json:"plugin_id"`
		Entrypoint   string            `json:"entrypoint"`
		Placement    string            `json:"placement,omitempty"`
		Width        any               `json:"width,omitempty"`
		Height       any               `json:"height,omitempty"`
		WorkspaceID  string            `json:"workspace_id,omitempty"`
		TargetPaneID string            `json:"target_pane_id,omitempty"`
		Direction    string            `json:"direction,omitempty"`
		Cwd          string            `json:"cwd,omitempty"`
		Env          map[string]string `json:"env,omitempty"`
		Focus        bool              `json:"focus"`
	}{o.PluginID, o.Entrypoint, o.Placement, popupSize(o.Width), popupSize(o.Height), o.WorkspaceID, o.TargetPaneID, o.Direction, o.Cwd, o.Env, o.Focus}
	args := []string{"plugin", "pane", "open", "--plugin", o.PluginID, "--entrypoint", o.Entrypoint}
	for _, f := range [][2]string{
		{"--placement", o.Placement}, {"--workspace", o.WorkspaceID}, {"--target-pane", o.TargetPaneID},
		{"--direction", o.Direction}, {"--cwd", o.Cwd}, {"--width", o.Width}, {"--height", o.Height},
	} {
		if f[1] != "" {
			args = append(args, f[0], f[1])
		}
	}
	args = append(args, envFlags(o.Env)...)
	if o.Focus {
		args = append(args, "--focus")
	}
	var out struct {
		PluginPane PluginPane `json:"plugin_pane"`
	}
	err := c.do(ctx, callSpec{method: "plugin.pane.open", params: params, out: &out, cli: cliLine(args...)})
	return out.PluginPane, err
}

// WorktreeList lists the git worktrees of the repository at cwd ("" lets
// herdr pick the focused workspace's repository).
func (c *Client) WorktreeList(ctx context.Context, cwd string) (WorktreeListing, error) {
	params := map[string]string{}
	args := []string{"worktree", "list"}
	if cwd != "" {
		params["cwd"] = cwd
		args = append(args, "--cwd", cwd)
	}
	var out WorktreeListing
	err := c.do(ctx, callSpec{method: "worktree.list", params: params, out: &out, cli: cliLine(args...)})
	return out, err
}

// IdleShell reports whether the pane's foreground process group is its
// shell, i.e. nothing runs in it and agent.start may type into it.
// Unknown pids (null) are never idle.
func IdleShell(pi ProcessInfo) bool {
	return pi.ShellPID > 0 && pi.ForegroundPGID == pi.ShellPID
}

// idlePollInterval paces WaitIdleShell.
var idlePollInterval = 250 * time.Millisecond

// WaitIdleShell polls pane.process_info until the pane is an idle shell or
// timeout elapses (ErrTimeout naming the foreground processes). The timeout
// bounds the whole operation, including every poll: a slow or late
// pane.process_info answer cannot outlast it. A non-positive timeout expires
// at once. It returns the last process info seen.
func (c *Client) WaitIdleShell(ctx context.Context, paneID string, timeout time.Duration) (ProcessInfo, error) {
	parent := ctx
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	// expired reports that our own deadline ran out while the caller's ctx
	// is still live. It reads the clock, not ctx.Err(): the connection's I/O
	// deadline can fire a moment before the context's timer does.
	expired := func() bool { return parent.Err() == nil && !time.Now().Before(deadline) }
	var last ProcessInfo
	for {
		pi, err := c.PaneProcessInfo(ctx, paneID)
		if err != nil {
			if IsTimeout(err) && expired() {
				return last, idleTimeoutErr(paneID, timeout, last)
			}
			return last, err
		}
		last = pi
		if IdleShell(pi) {
			return pi, nil
		}
		t := time.NewTimer(idlePollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			if err := parent.Err(); err != nil {
				return last, fmt.Errorf("herdr pane %s: wait for idle shell: %w", paneID, err)
			}
			return last, idleTimeoutErr(paneID, timeout, last)
		case <-t.C:
		}
	}
}

// idleTimeoutErr is WaitIdleShell's ErrTimeout, naming the foreground
// processes of the last poll that answered.
func idleTimeoutErr(paneID string, timeout time.Duration, pi ProcessInfo) error {
	names := make([]string, 0, len(pi.Foreground))
	for _, p := range pi.Foreground {
		names = append(names, p.Name)
	}
	fg := strings.Join(names, ",")
	if fg == "" {
		fg = "unknown"
	}
	return fmt.Errorf("herdr pane %s: shell not idle after %s (foreground: %s): %w", paneID, timeout, fg, ErrTimeout)
}
