package herdr

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// typedCase drives one convenience method and states the exact socket
// request it must produce (copied from what the herdr 0.9.3 CLI sends, see
// testdata/schema-excerpt.json "cli_requests") and the CLI line it logs.
type typedCase struct {
	name   string
	call   func(ctx context.Context, c *Client) error
	method string
	params string // JSON; compared semantically
	cli    string // expected logged CLI line (prefix match)
	result any    // what the fake server answers
}

func typedCases() []typedCase {
	ok := map[string]any{"type": "ok"}
	pane := map[string]any{"type": "pane_info", "pane": map[string]any{"pane_id": "w1:p2", "workspace_id": "w1", "tab_id": "w1:t1", "agent_status": "unknown"}}
	agent := map[string]any{"type": "agent_info", "agent": map[string]any{"pane_id": "w1:p1", "name": "mg-x", "agent": "codex", "agent_status": "idle"}}
	read := map[string]any{"type": "pane_read", "read": map[string]any{"pane_id": "w1:p1", "text": "hello", "source": "recent_unwrapped", "format": "text"}}
	return []typedCase{
		{
			name:   "snapshot",
			call:   func(ctx context.Context, c *Client) error { _, err := c.Snapshot(ctx); return err },
			method: "session.snapshot", params: `{}`, cli: "herdr api snapshot",
			result: map[string]any{"type": "session_snapshot", "snapshot": map[string]any{"version": "0.9.1", "protocol": 22}},
		},
		{
			name: "workspace create",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorkspaceCreate(ctx, WorkspaceCreateOptions{Cwd: "/tmp", Label: "repo#1", Env: map[string]string{"A": "B"}})
				return err
			},
			method: "workspace.create", params: `{"cwd":"/tmp","focus":false,"label":"repo#1","env":{"A":"B"}}`,
			cli:    "herdr workspace create --cwd /tmp --label 'repo#1' --env 'A=<redacted>' --no-focus",
			result: map[string]any{"type": "workspace_created", "workspace": map[string]any{"workspace_id": "w9"}, "tab": map[string]any{"tab_id": "w9:t1"}, "root_pane": map[string]any{"pane_id": "w9:p1"}},
		},
		{
			name:   "workspace close",
			call:   func(ctx context.Context, c *Client) error { return c.WorkspaceClose(ctx, "w1") },
			method: "workspace.close", params: `{"workspace_id":"w1"}`, cli: "herdr workspace close w1", result: ok,
		},
		{
			name:   "workspace rename",
			call:   func(ctx context.Context, c *Client) error { return c.WorkspaceRename(ctx, "w1", "newlabel") },
			method: "workspace.rename", params: `{"workspace_id":"w1","label":"newlabel"}`, cli: "herdr workspace rename w1 newlabel",
			result: map[string]any{"type": "workspace_info", "workspace": map[string]any{"workspace_id": "w1"}},
		},
		{
			name: "workspace report metadata",
			call: func(ctx context.Context, c *Client) error {
				return c.WorkspaceReportMetadata(ctx, "w1", "magnum", map[string]string{"a": "b"}, 0)
			},
			method: "workspace.report_metadata", params: `{"workspace_id":"w1","source":"magnum","tokens":{"a":"b"}}`,
			cli: "herdr workspace report-metadata w1 --source magnum --token a=b", result: ok,
		},
		{
			name: "workspace report metadata ttl",
			call: func(ctx context.Context, c *Client) error {
				return c.WorkspaceReportMetadata(ctx, "w1", "magnum", map[string]string{"a": "b"}, 90*time.Second)
			},
			method: "workspace.report_metadata", params: `{"workspace_id":"w1","source":"magnum","tokens":{"a":"b"},"ttl_ms":90000}`,
			cli: "herdr workspace report-metadata w1 --source magnum --token a=b --ttl-ms 90000", result: ok,
		},
		{
			name: "tab create",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.TabCreate(ctx, TabCreateOptions{WorkspaceID: "w1", Cwd: "/tmp", Label: "lab"})
				return err
			},
			method: "tab.create", params: `{"workspace_id":"w1","cwd":"/tmp","focus":false,"label":"lab"}`,
			cli:    "herdr tab create --workspace w1 --cwd /tmp --label lab --no-focus",
			result: map[string]any{"type": "tab_created", "tab": map[string]any{"tab_id": "w1:t2"}, "root_pane": map[string]any{"pane_id": "w1:p3"}},
		},
		{
			name: "pane split",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.PaneSplit(ctx, "w1:p1", SplitOptions{Direction: SplitRight, Cwd: "/tmp", Env: map[string]string{"A": "B"}})
				return err
			},
			method: "pane.split", params: `{"target_pane_id":"w1:p1","direction":"right","cwd":"/tmp","focus":false,"env":{"A":"B"}}`,
			cli: "herdr pane split w1:p1 --direction right --cwd /tmp --env 'A=<redacted>' --no-focus", result: pane,
		},
		{
			name:   "pane run",
			call:   func(ctx context.Context, c *Client) error { return c.PaneRun(ctx, "w1:p1", "echo hi") },
			method: "pane.send_input", params: `{"pane_id":"w1:p1","text":"echo hi","keys":["Enter"]}`,
			cli: "herdr pane run w1:p1 'echo hi'", result: ok,
		},
		{
			name: "pane read",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.PaneRead(ctx, "w1:p1", ReadOptions{Source: SourceRecentUnwrapped, Lines: 50, Format: FormatANSI})
				return err
			},
			method: "pane.read", params: `{"pane_id":"w1:p1","source":"recent_unwrapped","lines":50,"format":"ansi","strip_ansi":true}`,
			cli: "herdr pane read w1:p1 --source recent-unwrapped --lines 50 --format ansi", result: read,
		},
		{
			name: "pane read defaults",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.PaneRead(ctx, "w1:p1", ReadOptions{})
				return err
			},
			method: "pane.read", params: `{"pane_id":"w1:p1","source":"recent_unwrapped","format":"text","strip_ansi":true}`,
			cli: "herdr pane read w1:p1 --source recent-unwrapped --format text", result: read,
		},
		{
			name:   "pane rename",
			call:   func(ctx context.Context, c *Client) error { return c.PaneRename(ctx, "w1:p1", "PR #1 judge") },
			method: "pane.rename", params: `{"pane_id":"w1:p1","label":"PR #1 judge"}`, cli: "herdr pane rename w1:p1 'PR #1 judge'", result: pane,
		},
		{
			name:   "pane rename clear",
			call:   func(ctx context.Context, c *Client) error { return c.PaneRename(ctx, "w1:p1", "") },
			method: "pane.rename", params: `{"pane_id":"w1:p1","label":null}`, cli: "herdr pane rename w1:p1", result: pane,
		},
		{
			name:   "pane process info",
			call:   func(ctx context.Context, c *Client) error { _, err := c.PaneProcessInfo(ctx, "w1:p1"); return err },
			method: "pane.process_info", params: `{"pane_id":"w1:p1"}`, cli: "herdr pane process-info --pane w1:p1",
			result: map[string]any{"type": "pane_process_info", "process_info": map[string]any{"pane_id": "w1:p1"}},
		},
		{
			name: "pane wait output substring",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.PaneWaitOutput(ctx, "w1:p1", WaitOutputOptions{Match: "MAGNUM_DONE", Timeout: time.Minute})
				return err
			},
			method: "pane.wait_for_output", params: `{"pane_id":"w1:p1","source":"recent","match":{"type":"substring","value":"MAGNUM_DONE"},"timeout_ms":60000,"strip_ansi":true}`,
			cli:    "herdr pane wait-output w1:p1 --match MAGNUM_DONE --source recent --timeout 60000",
			result: map[string]any{"type": "output_matched", "pane_id": "w1:p1", "revision": 1, "matched_line": "MAGNUM_DONE_r1", "read": map[string]any{"text": "x"}},
		},
		{
			name: "pane wait output regex",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.PaneWaitOutput(ctx, "w1:p1", WaitOutputOptions{Match: "DONE_[0-9]+", Regex: true, Lines: 20})
				return err
			},
			method: "pane.wait_for_output", params: `{"pane_id":"w1:p1","source":"recent","lines":20,"match":{"type":"regex","value":"DONE_[0-9]+"},"strip_ansi":true}`,
			cli:    "herdr pane wait-output w1:p1 --regex 'DONE_[0-9]+' --source recent --lines 20",
			result: map[string]any{"type": "output_matched", "pane_id": "w1:p1", "revision": 1, "read": map[string]any{"text": "x"}},
		},
		{
			name:   "pane send keys",
			call:   func(ctx context.Context, c *Client) error { return c.PaneSendKeys(ctx, "w1:p1", "ctrl+c", "Enter") },
			method: "pane.send_keys", params: `{"pane_id":"w1:p1","keys":["ctrl+c","Enter"]}`, cli: "herdr pane send-keys w1:p1 ctrl+c Enter", result: ok,
		},
		{
			name:   "pane get",
			call:   func(ctx context.Context, c *Client) error { _, err := c.PaneGet(ctx, "w1:p1"); return err },
			method: "pane.get", params: `{"pane_id":"w1:p1"}`, cli: "herdr pane get w1:p1", result: pane,
		},
		{
			name:   "pane list",
			call:   func(ctx context.Context, c *Client) error { _, err := c.PaneList(ctx, ""); return err },
			method: "pane.list", params: `{}`, cli: "herdr pane list",
			result: map[string]any{"type": "pane_list", "panes": []any{}},
		},
		{
			name:   "pane list workspace",
			call:   func(ctx context.Context, c *Client) error { _, err := c.PaneList(ctx, "w1"); return err },
			method: "pane.list", params: `{"workspace_id":"w1"}`, cli: "herdr pane list --workspace w1",
			result: map[string]any{"type": "pane_list", "panes": []any{}},
		},
		{
			name: "agent start",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.AgentStart(ctx, AgentStartOptions{Name: "mg-x-1-judge", Kind: "codex", PaneID: "w1:p1", Timeout: time.Minute, Args: []string{"resume", "abc", "--flag"}})
				return err
			},
			method: "agent.start", params: `{"name":"mg-x-1-judge","kind":"codex","pane_id":"w1:p1","args":["resume","abc","--flag"],"timeout_ms":60000}`,
			cli:    "herdr agent start mg-x-1-judge --kind codex --pane w1:p1 --timeout 60000 -- resume abc --flag",
			result: map[string]any{"type": "agent_started", "agent": map[string]any{"pane_id": "w1:p1", "name": "mg-x-1-judge"}, "argv": []string{"codex", "resume", "abc"}},
		},
		{
			name: "agent start minimal",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.AgentStart(ctx, AgentStartOptions{Name: "mg-x-1-claude", Kind: "claude", PaneID: "w1:p1"})
				return err
			},
			method: "agent.start", params: `{"name":"mg-x-1-claude","kind":"claude","pane_id":"w1:p1"}`,
			cli:    "herdr agent start mg-x-1-claude --kind claude --pane w1:p1",
			result: map[string]any{"type": "agent_started", "agent": map[string]any{"pane_id": "w1:p1"}, "argv": []string{"claude"}},
		},
		{
			name: "agent prompt wait",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.AgentPrompt(ctx, "mg-x", "hello there", &PromptWait{Until: []Status{StatusWorking, StatusBlocked}, Timeout: time.Minute})
				return err
			},
			method: "agent.prompt", params: `{"target":"mg-x","text":"hello there","wait":{"until":["working","blocked"],"timeout_ms":60000}}`,
			cli:    "herdr agent prompt mg-x 'hello there' --wait --until working --until blocked --timeout 60000",
			result: map[string]any{"type": "agent_prompted", "agent": map[string]any{"pane_id": "w1:p1", "agent_status": "working"}},
		},
		{
			name: "agent prompt no wait",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.AgentPrompt(ctx, "mg-x", "hello", nil)
				return err
			},
			method: "agent.prompt", params: `{"target":"mg-x","text":"hello"}`, cli: "herdr agent prompt mg-x hello",
			result: map[string]any{"type": "agent_prompted", "agent": map[string]any{"pane_id": "w1:p1"}},
		},
		{
			name: "agent prompt default wait",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.AgentPrompt(ctx, "mg-x", "hello", &PromptWait{})
				return err
			},
			method: "agent.prompt", params: `{"target":"mg-x","text":"hello","wait":{}}`, cli: "herdr agent prompt mg-x hello --wait",
			result: map[string]any{"type": "agent_prompted", "agent": map[string]any{"pane_id": "w1:p1"}},
		},
		{
			name:   "agent get",
			call:   func(ctx context.Context, c *Client) error { _, err := c.AgentGet(ctx, "mg-x"); return err },
			method: "agent.get", params: `{"target":"mg-x"}`, cli: "herdr agent get mg-x", result: agent,
		},
		{
			name: "agent read",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.AgentRead(ctx, "mg-x", ReadOptions{Lines: 40})
				return err
			},
			method: "agent.read", params: `{"target":"mg-x","source":"recent_unwrapped","lines":40,"format":"text","strip_ansi":true}`,
			cli: "herdr agent read mg-x --source recent-unwrapped --lines 40 --format text", result: read,
		},
		{
			name:   "agent list",
			call:   func(ctx context.Context, c *Client) error { _, err := c.AgentList(ctx); return err },
			method: "agent.list", params: `{}`, cli: "herdr agent list",
			result: map[string]any{"type": "agent_list", "agents": []any{}},
		},
		{
			name:   "agent focus",
			call:   func(ctx context.Context, c *Client) error { return c.AgentFocus(ctx, "mg-x") },
			method: "agent.focus", params: `{"target":"mg-x"}`, cli: "herdr agent focus mg-x", result: agent,
		},
		{
			name:   "agent rename",
			call:   func(ctx context.Context, c *Client) error { return c.AgentRename(ctx, "mg-x", "mg-y") },
			method: "agent.rename", params: `{"target":"mg-x","name":"mg-y"}`, cli: "herdr agent rename mg-x mg-y", result: agent,
		},
		{
			name:   "agent send keys",
			call:   func(ctx context.Context, c *Client) error { return c.AgentSendKeys(ctx, "mg-x", "ctrl+c") },
			method: "agent.send_keys", params: `{"target":"mg-x","keys":["ctrl+c"]}`, cli: "herdr agent send-keys mg-x ctrl+c", result: ok,
		},
		{
			name: "notification show",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.NotificationShow(ctx, "Title here", "Body text")
				return err
			},
			method: "notification.show", params: `{"title":"Title here","body":"Body text"}`,
			cli:    "herdr notification show 'Title here' --body 'Body text'",
			result: map[string]any{"type": "notification_show", "shown": true, "reason": "shown"},
		},
		{
			name: "plugin pane popup",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.PluginPaneOpen(ctx, PluginPaneOptions{PluginID: "zhuravel.magnum", Entrypoint: "picker", Placement: PlacementPopup, Width: "80%", Height: "30", Focus: true, Env: map[string]string{"X": "1"}})
				return err
			},
			method: "plugin.pane.open", params: `{"plugin_id":"zhuravel.magnum","entrypoint":"picker","placement":"popup","width":"80%","height":30,"focus":true,"env":{"X":"1"}}`,
			cli:    "herdr plugin pane open --plugin zhuravel.magnum --entrypoint picker --placement popup --width 80% --height 30 --env 'X=<redacted>' --focus",
			result: map[string]any{"type": "plugin_pane_opened", "plugin_pane": map[string]any{"plugin_id": "zhuravel.magnum", "entrypoint": "picker", "pane": map[string]any{"pane_id": "w1:p9"}}},
		},
		{
			name:   "terminal title set",
			call:   func(ctx context.Context, c *Client) error { _, err := c.TerminalTitleSet(ctx, "marker123"); return err },
			method: "client.window_title.set", params: `{"title":"marker123"}`, cli: "herdr terminal title set marker123",
			result: map[string]any{"type": "client_window_title", "changed": true, "reason": "set"},
		},
		{
			name:   "terminal title clear",
			call:   func(ctx context.Context, c *Client) error { _, err := c.TerminalTitleClear(ctx); return err },
			method: "client.window_title.clear", params: `{}`, cli: "herdr terminal title clear",
			result: map[string]any{"type": "client_window_title", "changed": true, "reason": "cleared"},
		},
		{
			name:   "worktree list",
			call:   func(ctx context.Context, c *Client) error { _, err := c.WorktreeList(ctx, "/tmp"); return err },
			method: "worktree.list", params: `{"cwd":"/tmp"}`, cli: "herdr worktree list --cwd /tmp",
			result: map[string]any{"type": "worktree_list", "source": map[string]any{"repo_root": "/r"}, "worktrees": []any{}},
		},
		{
			name:   "worktree list default",
			call:   func(ctx context.Context, c *Client) error { _, err := c.WorktreeList(ctx, ""); return err },
			method: "worktree.list", params: `{}`, cli: "herdr worktree list",
			result: map[string]any{"type": "worktree_list", "source": map[string]any{"repo_root": "/r"}, "worktrees": []any{}},
		},
		{
			name: "workspace create env order",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorkspaceCreate(ctx, WorkspaceCreateOptions{Cwd: "/w", Env: map[string]string{"Z": "1", "A": "2"}, Focus: true})
				return err
			},
			method: "workspace.create", params: `{"cwd":"/w","focus":true,"env":{"A":"2","Z":"1"}}`,
			cli:    "herdr workspace create --cwd /w --env 'A=<redacted>' --env 'Z=<redacted>' --focus",
			result: map[string]any{"type": "workspace_created", "workspace": map[string]any{"workspace_id": "w9"}, "tab": map[string]any{"tab_id": "w9:t1"}, "root_pane": map[string]any{"pane_id": "w9:p1"}},
		},
		{
			name:   "ping",
			call:   func(ctx context.Context, c *Client) error { _, err := c.Ping(ctx); return err },
			method: "ping", params: `{}`, cli: "herdr status server",
			result: map[string]any{"type": "pong", "version": "0.9.1", "protocol": 22},
		},
	}
}

func jsonEqual(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("bad got json %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad want json %s: %v", want, err)
	}
	return reflect.DeepEqual(g, w)
}

func TestTypedMethodsSendCLIEquivalentRequests(t *testing.T) {
	for _, tc := range typedCases() {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeServer(t)
			s.reply(tc.method, tc.result)
			log := &recLogger{}
			c := s.client()
			c.Logger = log
			if err := tc.call(context.Background(), c); err != nil {
				t.Fatalf("call: %v", err)
			}
			req := s.last()
			if req.Method != tc.method {
				t.Fatalf("method = %s, want %s", req.Method, tc.method)
			}
			if !jsonEqual(t, req.Params, tc.params) {
				t.Fatalf("params = %s\nwant     %s", req.Params, tc.params)
			}
			lines := log.all()
			if len(lines) != 1 || !strings.Contains(lines[0], tc.cli) {
				t.Fatalf("log = %q, want it to contain %q", lines, tc.cli)
			}
		})
	}
}

func TestTypedMethodsReturnServerErrors(t *testing.T) {
	for _, tc := range typedCases() {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeServer(t)
			s.handle(tc.method, func(json.RawMessage) (any, *Error) { return nil, &Error{Code: "pane_not_found", Message: "nope"} })
			log := &recLogger{}
			c := s.client()
			c.Logger = log
			err := tc.call(context.Background(), c)
			if !IsCode(err, CodePaneNotFound) {
				t.Fatalf("want pane_not_found, got %v", err)
			}
			if lines := log.all(); len(lines) != 1 || !strings.Contains(lines[0], "pane_not_found") {
				t.Fatalf("error not logged: %q", lines)
			}
		})
	}
}

func TestLogTruncatesLongArguments(t *testing.T) {
	s := newFakeServer(t)
	s.reply("agent.prompt", map[string]any{"type": "agent_prompted", "agent": map[string]any{"pane_id": "w1:p1"}})
	log := &recLogger{}
	c := s.client()
	c.Logger = log
	long := strings.Repeat("x", 500) + "\nsecond line ghp_abcdefghijklmnopqrstu"
	if _, err := c.AgentPrompt(context.Background(), "mg-x", long, nil); err != nil {
		t.Fatal(err)
	}
	line := log.all()[0]
	if len(line) > 400 || strings.Contains(line, "\n") || strings.Contains(line, "ghp_") {
		t.Fatalf("log line not truncated/flattened/redacted: %q", line)
	}
	if got := string(s.last().Params); !strings.Contains(got, `second line ghp_abcdefghijklmnopqrstu`) {
		t.Fatalf("the request itself must carry the full text: %s", got)
	}
}

// TestLogRedactsCLIArguments covers cliLine's own redaction: a short token
// survives truncation, so only Redact keeps it out of the log.
func TestLogRedactsCLIArguments(t *testing.T) {
	const token = "ghs_abcdefghijklmnopqrstuvwxyz0123"
	s := newFakeServer(t)
	s.reply("agent.prompt", map[string]any{"type": "agent_prompted", "agent": map[string]any{"pane_id": "w1:p1"}})
	s.reply("pane.send_input", map[string]any{"type": "ok"})
	log := &recLogger{}
	c := s.client()
	c.Logger = log
	if _, err := c.AgentPrompt(context.Background(), "mg-x", "clone with "+token, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.PaneRun(context.Background(), "w1:p1", "export GH_TOKEN="+token); err != nil {
		t.Fatal(err)
	}
	lines := log.all()
	if len(lines) != 2 {
		t.Fatalf("want 2 log lines, got %q", lines)
	}
	for _, line := range lines {
		if strings.Contains(line, token) || !strings.Contains(line, "<redacted>") || !strings.HasPrefix(line, "debug herdr: herdr ") {
			t.Fatalf("CLI log line must carry <redacted> and no token: %q", line)
		}
	}
	if got := string(s.requests()[0].Params); !strings.Contains(got, token) {
		t.Fatalf("the request itself must carry the full text: %s", got)
	}
}

// TestEnvValuesAreNeverLogged: environment values (API keys in role
// environments) show as KEY=<redacted> in the logged CLI line while the
// socket payload keeps them.
func TestEnvValuesAreNeverLogged(t *testing.T) {
	const secret = "sk-ant-secret"
	env := map[string]string{"ANTHROPIC_API_KEY": secret, "PLAIN": "hunter2"}
	calls := []struct {
		name, method string
		result       any
		call         func(ctx context.Context, c *Client) error
	}{
		{"workspace create", "workspace.create", map[string]any{"type": "workspace_created", "workspace": map[string]any{"workspace_id": "w1"}, "tab": map[string]any{"tab_id": "w1:t1"}, "root_pane": map[string]any{"pane_id": "w1:p1"}},
			func(ctx context.Context, c *Client) error {
				_, err := c.WorkspaceCreate(ctx, WorkspaceCreateOptions{Cwd: "/w", Env: env})
				return err
			}},
		{"tab create", "tab.create", map[string]any{"type": "tab_created", "tab": map[string]any{"tab_id": "w1:t2"}, "root_pane": map[string]any{"pane_id": "w1:p3"}},
			func(ctx context.Context, c *Client) error {
				_, err := c.TabCreate(ctx, TabCreateOptions{WorkspaceID: "w1", Env: env})
				return err
			}},
		{"pane split", "pane.split", map[string]any{"type": "pane_info", "pane": map[string]any{"pane_id": "w1:p2", "workspace_id": "w1", "tab_id": "w1:t1", "agent_status": "unknown"}},
			func(ctx context.Context, c *Client) error {
				_, err := c.PaneSplit(ctx, "w1:p1", SplitOptions{Direction: SplitRight, Env: env})
				return err
			}},
		{"plugin pane open", "plugin.pane.open", map[string]any{"type": "plugin_pane_opened", "plugin_pane": map[string]any{"pane_id": "w1:p4"}},
			func(ctx context.Context, c *Client) error {
				_, err := c.PluginPaneOpen(ctx, PluginPaneOptions{PluginID: "p", Entrypoint: "e", Env: env})
				return err
			}},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeServer(t)
			s.reply(tc.method, tc.result)
			log := &recLogger{}
			c := s.client()
			c.Logger = log
			if err := tc.call(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			lines := log.all()
			if len(lines) != 1 {
				t.Fatalf("want 1 log line, got %q", lines)
			}
			line := lines[0]
			if strings.Contains(line, secret) || strings.Contains(line, "hunter2") {
				t.Fatalf("env value leaked into the log: %q", line)
			}
			if !strings.Contains(line, "--env 'ANTHROPIC_API_KEY=<redacted>' --env 'PLAIN=<redacted>'") {
				t.Fatalf("env keys must be logged with redacted values: %q", line)
			}
			if got := string(s.last().Params); !strings.Contains(got, secret) {
				t.Fatalf("the socket payload must keep the real env value: %s", got)
			}
		})
	}
}

func TestSnapshotDecodesRealShape(t *testing.T) {
	raw := `{"type":"session_snapshot","snapshot":{"version":"0.9.1","protocol":22,
	 "focused_workspace_id":"w1H","focused_tab_id":null,"focused_pane_id":null,
	 "workspaces":[{"active_tab_id":"w1F:t1","agent_status":"idle","focused":false,"label":"repo1","number":18,"pane_count":1,"tab_count":1,"tokens":{"space_label":"repo1"},"workspace_id":"w1F","worktree":{"checkout_path":"/p/talkable.repo1","is_linked_worktree":true,"repo_key":"/p/talkable/.git","repo_name":"talkable","repo_root":"/p/talkable"}}],
	 "tabs":[{"agent_status":"unknown","focused":false,"label":"1","number":3,"pane_count":1,"tab_id":"w21:t3","workspace_id":"w21"}],
	 "panes":[{"agent_status":"unknown","cwd":"/Users/x","focused":false,"foreground_cwd":"/Users/x","pane_id":"w21:p3","revision":1433,"scroll":{"max_offset_from_bottom":681,"offset_from_bottom":0,"viewport_rows":85},"tab_id":"w21:t3","terminal_id":"term_1","terminal_title":"x@host:~","terminal_title_stripped":"x@host:~","tokens":{"harness_logo":""},"workspace_id":"w21"},
	          {"agent":"codex","agent_session":{"agent":"codex","kind":"id","source":"herdr:codex","value":"01a0fbf5-fd9f-7dc3-8b78-022b19c1eddd"},"agent_status":"idle","cwd":"/p/talkable.repo3","focused":false,"label":"PR #1 judge","pane_id":"w1H:p1","revision":23388,"tab_id":"w1H:t1","terminal_id":"term_2","terminal_title":"PR #11920 | talkable.repo3","workspace_id":"w1H"}],
	 "agents":[{"agent":"codex","name":"mg-talkable-1-judge","agent_session":{"agent":"codex","kind":"id","source":"herdr:codex","value":"01a0fbf5-fd9f-7dc3-8b78-022b19c1eddd"},"agent_status":"working","completion_seq":null,"interactive_ready":true,"cwd":"/p/talkable.repo3","focused":false,"pane_id":"w1H:p1","revision":23388,"state_change_seq":368,"tab_id":"w1H:t1","terminal_id":"term_2","workspace_id":"w1H"}],
	 "layouts":[]}}`
	s := newFakeServer(t)
	s.reply("session.snapshot", json.RawMessage(raw))
	snap, err := s.client().Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Version != "0.9.1" || snap.Protocol != 22 || snap.FocusedWorkspaceID != "w1H" || snap.FocusedPaneID != "" {
		t.Fatalf("header = %+v", snap)
	}
	ws := snap.Workspaces[0]
	if ws.ID != "w1F" || ws.Label != "repo1" || ws.Worktree == nil || ws.Worktree.CheckoutPath != "/p/talkable.repo1" || !ws.Worktree.IsLinkedWorktree {
		t.Fatalf("workspace = %+v", ws)
	}
	if snap.Tabs[0].ID != "w21:t3" || snap.Tabs[0].WorkspaceID != "w21" {
		t.Fatalf("tab = %+v", snap.Tabs[0])
	}
	shell := snap.Panes[0]
	if shell.Agent != "" || shell.AgentSession != nil || shell.Cwd != "/Users/x" || shell.TerminalTitle != "x@host:~" {
		t.Fatalf("shell pane = %+v", shell)
	}
	codex := snap.Panes[1]
	if codex.Agent != "codex" || codex.Label != "PR #1 judge" || codex.AgentSession == nil ||
		codex.AgentSession.Kind != "id" || codex.AgentSession.Source != "herdr:codex" || codex.AgentSession.Value != "01a0fbf5-fd9f-7dc3-8b78-022b19c1eddd" {
		t.Fatalf("codex pane = %+v (session %+v)", codex, codex.AgentSession)
	}
	a := snap.Agents[0]
	if a.Name != "mg-talkable-1-judge" || a.PaneID != "w1H:p1" || a.AgentStatus != StatusWorking || !a.InteractiveReady || a.CompletionSeq != nil || a.StateChangeSeq != 368 {
		t.Fatalf("agent = %+v", a)
	}
	if p, ok := snap.PaneBySession("01a0fbf5-fd9f-7dc3-8b78-022b19c1eddd"); !ok || p.ID != "w1H:p1" {
		t.Fatalf("PaneBySession = %+v %v", p, ok)
	}
	if _, ok := snap.PaneBySession("nope"); ok {
		t.Fatal("PaneBySession found a missing session")
	}
	if ag, ok := snap.AgentByName("mg-talkable-1-judge"); !ok || ag.PaneID != "w1H:p1" {
		t.Fatalf("AgentByName = %+v %v", ag, ok)
	}
}

func TestTypedResultsDecode(t *testing.T) {
	ctx := context.Background()
	s := newFakeServer(t)
	c := s.client()

	s.reply("workspace.create", json.RawMessage(`{"type":"workspace_created","workspace":{"workspace_id":"w9","label":"talkable#1","number":4,"focused":false,"pane_count":1,"tab_count":1,"active_tab_id":"w9:t1","agent_status":"unknown"},"tab":{"tab_id":"w9:t1","workspace_id":"w9","number":1,"label":"1","focused":true,"pane_count":1,"agent_status":"unknown"},"root_pane":{"pane_id":"w9:p1","terminal_id":"t","workspace_id":"w9","tab_id":"w9:t1","focused":true,"agent_status":"unknown","revision":0}}`))
	wc, err := c.WorkspaceCreate(ctx, WorkspaceCreateOptions{Cwd: "/x"})
	if err != nil || wc.Workspace.ID != "w9" || wc.Tab.ID != "w9:t1" || wc.RootPane.ID != "w9:p1" {
		t.Fatalf("WorkspaceCreate = %+v, %v", wc, err)
	}

	s.reply("tab.create", json.RawMessage(`{"type":"tab_created","tab":{"tab_id":"w9:t2","workspace_id":"w9"},"root_pane":{"pane_id":"w9:p4"}}`))
	tc, err := c.TabCreate(ctx, TabCreateOptions{WorkspaceID: "w9"})
	if err != nil || tc.Tab.ID != "w9:t2" || tc.RootPane.ID != "w9:p4" {
		t.Fatalf("TabCreate = %+v, %v", tc, err)
	}

	s.reply("pane.split", json.RawMessage(`{"type":"pane_info","pane":{"pane_id":"w9:p2","workspace_id":"w9","tab_id":"w9:t1","agent_status":"unknown"}}`))
	sp, err := c.PaneSplit(ctx, "w9:p1", SplitOptions{Direction: SplitDown})
	if err != nil || sp.ID != "w9:p2" {
		t.Fatalf("PaneSplit = %+v, %v", sp, err)
	}

	s.reply("pane.process_info", json.RawMessage(`{"type":"pane_process_info","process_info":{"foreground_process_group_id":27602,"foreground_processes":[{"argv":["codex","--search"],"argv0":"codex","cmdline":"codex --search","cwd":"/p","name":"node","pid":27871}],"pane_id":"w2J:p1","shell_pid":18310,"tty":"/dev/ttys007"}}`))
	pi, err := c.PaneProcessInfo(ctx, "w2J:p1")
	if err != nil || pi.PaneID != "w2J:p1" || pi.ShellPID != 18310 || pi.ForegroundPGID != 27602 || pi.TTY != "/dev/ttys007" ||
		len(pi.Foreground) != 1 || pi.Foreground[0].PID != 27871 || pi.Foreground[0].Name != "node" || pi.Foreground[0].Cwd != "/p" ||
		!reflect.DeepEqual(pi.Foreground[0].Argv, []string{"codex", "--search"}) || pi.Foreground[0].Cmdline != "codex --search" {
		t.Fatalf("PaneProcessInfo = %+v, %v", pi, err)
	}

	s.reply("agent.start", json.RawMessage(`{"type":"agent_started","agent":{"pane_id":"w9:p1","name":"mg-a","agent":"codex","agent_status":"idle","launch_pending":true},"argv":["codex","resume","u"]}`))
	st, err := c.AgentStart(ctx, AgentStartOptions{Name: "mg-a", Kind: "codex", PaneID: "w9:p1"})
	if err != nil || st.Agent.Name != "mg-a" || !st.Agent.LaunchPending || !reflect.DeepEqual(st.Argv, []string{"codex", "resume", "u"}) {
		t.Fatalf("AgentStart = %+v, %v", st, err)
	}

	s.reply("agent.prompt", json.RawMessage(`{"type":"agent_prompted","agent":{"pane_id":"w9:p1","name":"mg-a","agent_status":"working"}}`))
	ap, err := c.AgentPrompt(ctx, "mg-a", "hi", &PromptWait{Until: []Status{StatusWorking}})
	if err != nil || ap.AgentStatus != StatusWorking {
		t.Fatalf("AgentPrompt = %+v, %v", ap, err)
	}

	s.reply("agent.list", json.RawMessage(`{"type":"agent_list","agents":[{"pane_id":"a:p1","agent":"codex","agent_status":"working"},{"pane_id":"b:p1","agent":"claude","agent_status":"idle"}]}`))
	al, err := c.AgentList(ctx)
	if err != nil || len(al) != 2 || al[0].Agent != "codex" || al[1].AgentStatus != StatusIdle {
		t.Fatalf("AgentList = %+v, %v", al, err)
	}

	s.reply("pane.read", json.RawMessage(`{"type":"pane_read","read":{"pane_id":"w9:p1","workspace_id":"w9","tab_id":"w9:t1","source":"recent_unwrapped","format":"text","text":"line1\nline2","revision":0,"truncated":true}}`))
	rd, err := c.PaneRead(ctx, "w9:p1", ReadOptions{})
	if err != nil || rd.Text != "line1\nline2" || !rd.Truncated || rd.Source != "recent_unwrapped" {
		t.Fatalf("PaneRead = %+v, %v", rd, err)
	}

	s.reply("pane.wait_for_output", json.RawMessage(`{"type":"output_matched","pane_id":"w9:p1","revision":7,"matched_line":"MAGNUM_DONE_r1","read":{"pane_id":"w9:p1","text":"...MAGNUM_DONE_r1"}}`))
	om, err := c.PaneWaitOutput(ctx, "w9:p1", WaitOutputOptions{Match: "MAGNUM_DONE_r1"})
	if err != nil || om.MatchedLine != "MAGNUM_DONE_r1" || om.Revision != 7 || om.Read.Text != "...MAGNUM_DONE_r1" {
		t.Fatalf("PaneWaitOutput = %+v, %v", om, err)
	}

	s.reply("notification.show", json.RawMessage(`{"type":"notification_show","shown":false,"reason":"rate_limited"}`))
	nr, err := c.NotificationShow(ctx, "t", "")
	if err != nil || nr.Shown || nr.Reason != "rate_limited" {
		t.Fatalf("NotificationShow = %+v, %v", nr, err)
	}
	if got := string(s.last().Params); got != `{"title":"t"}` {
		t.Fatalf("empty body must be omitted: %s", got)
	}

	s.reply("client.window_title.set", json.RawMessage(`{"type":"client_window_title","changed":false,"reason":"no_foreground_client"}`))
	wt, err := c.TerminalTitleSet(ctx, "m")
	if err != nil || wt.Changed || wt.Reason != "no_foreground_client" {
		t.Fatalf("TerminalTitleSet = %+v, %v", wt, err)
	}

	s.reply("worktree.list", json.RawMessage(`{"type":"worktree_list","source":{"repo_key":"/p/t/.git","repo_name":"talkable","repo_root":"/p/t","source_checkout_path":"/p/t","source_workspace_id":"wM"},"worktrees":[{"branch":"main","is_bare":false,"is_detached":false,"is_linked_worktree":false,"is_prunable":false,"label":"talkable","open_workspace_id":"wM","path":"/p/t"},{"branch":null,"is_bare":false,"is_detached":true,"is_linked_worktree":true,"is_prunable":false,"label":"talkable.review1","path":"/p/t.review1"}]}`))
	wl, err := c.WorktreeList(ctx, "")
	if err != nil || wl.Source.RepoRoot != "/p/t" || len(wl.Worktrees) != 2 || wl.Worktrees[0].OpenWorkspaceID != "wM" ||
		!wl.Worktrees[1].IsDetached || wl.Worktrees[1].Branch != "" || wl.Worktrees[1].Path != "/p/t.review1" {
		t.Fatalf("WorktreeList = %+v, %v", wl, err)
	}
	if got := string(s.last().Params); got != `{}` {
		t.Fatalf("empty cwd must be omitted: %s", got)
	}

	s.reply("plugin.pane.open", json.RawMessage(`{"type":"plugin_pane_opened","plugin_pane":{"plugin_id":"zhuravel.magnum","entrypoint":"picker","pane":{"pane_id":"w1:p9"}}}`))
	pp, err := c.PluginPaneOpen(ctx, PluginPaneOptions{PluginID: "zhuravel.magnum", Entrypoint: "picker", Placement: PlacementPopup})
	if err != nil || pp.Pane.ID != "w1:p9" || pp.PluginID != "zhuravel.magnum" {
		t.Fatalf("PluginPaneOpen = %+v, %v", pp, err)
	}

	s.reply("ping", json.RawMessage(`{"type":"pong","version":"0.9.1","protocol":22,"capabilities":{"live_handoff":true}}`))
	pong, err := c.Ping(ctx)
	if err != nil || pong.Version != "0.9.1" || pong.Protocol != 22 {
		t.Fatalf("Ping = %+v, %v", pong, err)
	}
}

func TestServerSideWaitsExtendTheClientDeadline(t *testing.T) {
	ctx := context.Background()
	s := newFakeServer(t)
	c := s.client()
	c.Timeout = 100 * time.Millisecond

	s.reply("agent.prompt", slow{300 * time.Millisecond, map[string]any{"type": "agent_prompted", "agent": map[string]any{"pane_id": "p"}}})
	if _, err := c.AgentPrompt(ctx, "mg-x", "hi", &PromptWait{Timeout: 400 * time.Millisecond}); err != nil {
		t.Fatalf("prompt with wait must outlive Client.Timeout: %v", err)
	}
	if _, err := c.AgentPrompt(ctx, "mg-x", "hi", nil); !IsTimeout(err) {
		t.Fatalf("prompt without wait must use Client.Timeout, got %v", err)
	}

	s.reply("pane.wait_for_output", slow{300 * time.Millisecond, map[string]any{"type": "output_matched", "pane_id": "p", "revision": 1, "read": map[string]any{}}})
	if _, err := c.PaneWaitOutput(ctx, "p", WaitOutputOptions{Match: "x", Timeout: 400 * time.Millisecond}); err != nil {
		t.Fatalf("wait-output must outlive Client.Timeout: %v", err)
	}

	s.reply("agent.start", slow{300 * time.Millisecond, map[string]any{"type": "agent_started", "agent": map[string]any{"pane_id": "p"}, "argv": []string{}}})
	if _, err := c.AgentStart(ctx, AgentStartOptions{Name: "a", Kind: "codex", PaneID: "p", Timeout: 400 * time.Millisecond}); err != nil {
		t.Fatalf("agent start must outlive Client.Timeout: %v", err)
	}

	// An unbounded server wait (no timeout) is bounded only by ctx.
	s.reply("pane.wait_for_output", slow{300 * time.Millisecond, map[string]any{"type": "output_matched", "pane_id": "p", "revision": 1, "read": map[string]any{}}})
	if _, err := c.PaneWaitOutput(ctx, "p", WaitOutputOptions{Match: "x"}); err != nil {
		t.Fatalf("unbounded wait-output must not use Client.Timeout: %v", err)
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := c.PaneWaitOutput(short, "p", WaitOutputOptions{Match: "x"}); !IsTimeout(err) {
		t.Fatalf("ctx deadline must still apply, got %v", err)
	}
}
