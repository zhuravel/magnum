package agents

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	projectMergeBase = "1111111111111111111111111111111111111111"
	projectHead      = "2222222222222222222222222222222222222222"
)

// Codex 0.160's "Folder access" for a folder the session treats as
// untrusted (tui/src/onboarding/trust_directory.rs), as an embedded TUI
// renders it; a TUI on the shared daemon offers "Back to Agent Command
// Center" instead of "Quit".
const restrictedFolderScreen = `
  Folder access
  /Users/x/Projects/talkable.review1

  Config, hooks, and exec policies from untrusted folders stay
  disabled. Trusted project folders can still contribute settings.
  Skills still load, and tools follow your permission settings.
  Opening will not change saved trust.

› 1. Open restricted
  2. Quit

  enter continue · esc quit
`

// projectEnv is a PR whose checkout is made of files, with git answering
// for it: the merge base with origin/master, and changed as what differs
// under .codex/ (diff) and untracked there (ls-files).
func projectEnv(t *testing.T, files map[string]string, changed, untracked []string) (*env, string, Workspace) {
	t.Helper()
	e := newEnv(t)
	dir := t.TempDir()
	writeFiles(t, dir, files)
	git := func(args ...string) []string { return append([]string{"git", "-C", dir}, args...) }
	nul := func(paths []string) []byte {
		if len(paths) == 0 {
			return nil
		}
		return []byte(strings.Join(paths, "\x00") + "\x00")
	}
	e.run.Rules = append(e.run.Rules,
		execx.Rule{Prefix: git("merge-base"), Result: execx.Result{Stdout: []byte(projectMergeBase + "\n")}},
		execx.Rule{Prefix: git("diff"), Result: execx.Result{Stdout: nul(changed)}},
		execx.Rule{Prefix: git("ls-files"), Result: execx.Result{Stdout: nul(untracked)}},
		execx.Rule{Prefix: git("rev-parse", "--verify", "--quiet", "HEAD^{commit}"), Result: execx.Result{Stdout: []byte(projectHead + "\n")}},
	)
	ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, dir, map[string]string{"WT_BRANCH": "review1"}, "talkable#11920", e.coreRoles())
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	return e, dir, ws
}

func projectEvents(t *testing.T, e *env) []store.Event {
	t.Helper()
	all, err := e.st.EventsBySubject(e.ctx, "pr:talkable/talkable#11920", 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Event
	for _, ev := range all {
		if ev.Kind == EventCodexProjectDeclined {
			out = append(out, ev)
		}
	}
	return out
}

// untrustArgs are the codex kind's args marking dir (and its
// symlink-resolved form) untrusted for one session.
func untrustArgs(dir string) []string {
	k, _ := config.Defaults().KindSpec(KindCodex)
	return k.UntrustArgs(withRealPath(dir))
}

var judgeLaunchArgs = []string{"-c", "model_reasoning_effort=xhigh", "-c", "agents.max_concurrent_threads_per_session=2", "-c", "features.apps=false", "-c", "skills.include_instructions=false"}

// A PR controls its checkout's .codex/ (MCP servers Codex starts or sends
// the slot's secrets to, hooks, rules, settings), and magnum trusts its
// checkouts. When the PR changes .codex/ against its merge base, the judge
// (launched or resumed) gets the session flag that makes Codex treat the
// checkout as untrusted, so none of the PR's .codex/ loads; even with
// project_mcp = "off" no server of it is named. The decision is an event
// (a count, never a path) and the PR's record for the board and the judge.
func TestJudgeOfAPRChangingCodexStartsWithTheCheckoutUntrusted(t *testing.T) {
	for _, resume := range []string{"", "01a0-uuid"} {
		e, dir, ws := projectEnv(t, map[string]string{".codex/config.toml": "[mcp_servers.evil]\ncommand = \"evil-mcp\"\n"},
			[]string{".codex/config.toml"}, []string{".codex/rules/x.rules"})
		e.setKind(KindCodex, func(k *config.Kind) { k.ProjectMCP = config.ProjectMCPOff })
		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], resume); err != nil {
			t.Fatal(err)
		}
		want := slices.Concat(judgeLaunchArgs, untrustArgs(dir))
		if resume != "" {
			want = slices.Concat([]string{"resume", resume}, want)
		}
		if got := e.h.starts[0].Args; !slices.Equal(got, want) {
			t.Fatalf("resume %q: args = %q\nwant %q", resume, got, want)
		}
		if calls := e.run.CallsWithPrefix("git", "-C", dir, "merge-base", "HEAD", "origin/master"); len(calls) != 1 {
			t.Fatalf("merge-base calls = %v", calls)
		}
		evs := projectEvents(t, e)
		if len(evs) != 1 || !strings.Contains(evs[0].Message, "runs without the PR's .codex/ changes: 2 files differ from the merge base") ||
			!strings.Contains(evs[0].Message, "codex-judge") || strings.Contains(evs[0].Message, "x.rules") {
			t.Fatalf("resume %q: events = %+v", resume, evs)
		}
		var data map[string]any
		if err := json.Unmarshal(evs[0].Data, &data); err != nil || data["role"] != "codex-judge" || data["files"] != 2.0 || data["compared"] != true {
			t.Fatalf("event data = %s (%v)", evs[0].Data, err)
		}
		note, ok := ProjectDeclined(e.ctx, e.st, e.pr.ID, KindCodex)
		if !ok || note.Head != projectHead || note.Files != 2 {
			t.Fatalf("record = %+v, %v; want head %s", note, ok, projectHead)
		}
	}
}

// codex review is a Codex session too: codex-review's line marks the
// checkout untrusted the same way, after its effort, comparing with the
// round's merge base, and records the round's head.
func TestCodexReviewOfAPRChangingCodexRunsWithTheCheckoutUntrusted(t *testing.T) {
	e, dir, _ := projectEnv(t, map[string]string{".codex/config.toml": "model = \"x\"\n"}, []string{".codex/config.toml"}, nil)
	d := ShellData{BaseRef: "origin/master", BaseSHA: projectMergeBase, HeadSHA: "3333333333333333333333333333333333333333",
		ReportPath: "/r/codex.md", Marker: DoneMarker("r-1"), Checkout: dir}
	got, err := e.m.ShellLine(e.ctx, e.pr.ID, e.spec(RoleCodexReview), d)
	if err != nil {
		t.Fatal(err)
	}
	untrust := untrustArgs(dir)
	want := "command codex review -c model_reasoning_effort=high -c features.apps=false -c skills.include_instructions=false -c " + shellQuote(untrust[1]) + " --base " + projectMergeBase
	if !strings.Contains(got, want) {
		t.Fatalf("line = %q\nwant it to contain %q", got, want)
	}
	if calls := e.run.CallsWithPrefix("git", "-C", dir, "merge-base", "HEAD", projectMergeBase); len(calls) != 1 {
		t.Fatalf("merge-base calls = %v", e.run.Calls)
	}
	if note, ok := ProjectDeclined(e.ctx, e.st, e.pr.ID, KindCodex); !ok || note.Head != d.HeadSHA {
		t.Fatalf("record = %+v, %v", note, ok)
	}
	if evs := projectEvents(t, e); len(evs) != 1 || !strings.Contains(evs[0].Message, "codex-review") {
		t.Fatalf("events = %+v", evs)
	}
}

// When the PR leaves .codex/ as its merge base has it, the checkout's
// project config is the team's: it loads as before (an earlier record of a
// declined launch is cleared), and project_mcp = "off" turns its MCP
// servers off like the operator's, but for those in mcp_allow.
func TestUnchangedCodexKeepsTheTeamsServersUnlessProjectMCPIsOff(t *testing.T) {
	files := map[string]string{".codex/config.toml": "[mcp_servers.sentry]\nurl = \"https://mcp.example.com\"\n[mcp_servers.tracker]\ncommand = \"tracker-mcp\"\n"}
	e, _, ws := projectEnv(t, files, nil, nil)
	if err := e.st.SetKV(e.ctx, store.KVPRProject(e.pr.ID, KindCodex), `{"head":"`+projectHead+`","files":1}`); err != nil {
		t.Fatal(err)
	}
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
		t.Fatal(err)
	}
	if got := e.h.starts[0].Args; !slices.Equal(got, judgeLaunchArgs) {
		t.Fatalf("args = %q, want the team's servers on", got)
	}
	if _, ok := ProjectDeclined(e.ctx, e.st, e.pr.ID, KindCodex); ok {
		t.Fatal("an unchanged .codex/ must clear the record")
	}
	if evs := projectEvents(t, e); len(evs) != 0 {
		t.Fatalf("events = %+v", evs)
	}

	e, _, ws = projectEnv(t, files, nil, nil)
	e.setKind(KindCodex, func(k *config.Kind) { k.ProjectMCP, k.MCPAllow = config.ProjectMCPOff, []string{"tracker"} })
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
		t.Fatal(err)
	}
	want := slices.Concat(judgeLaunchArgs, []string{"-c", "mcp_servers.sentry.enabled=false"})
	if got := e.h.starts[0].Args; !slices.Equal(got, want) {
		t.Fatalf("project_mcp off: args = %q\nwant %q", got, want)
	}
}

// A checkout without a .codex/ directory has nothing for Codex to load,
// so no git runs; a kind without project_untrust loads a changed .codex/.
func TestCodexProjectNeedsACodexDirectoryAndProjectUntrust(t *testing.T) {
	e, dir, ws := projectEnv(t, map[string]string{"README.md": "x\n", ".codex": "a file, not a directory\n"}, []string{".codex"}, nil)
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
		t.Fatal(err)
	}
	if got := e.h.starts[0].Args; !slices.Equal(got, judgeLaunchArgs) {
		t.Fatalf("args = %q", got)
	}
	if calls := e.run.CallsWithPrefix("git", "-C", dir, "diff"); len(calls) != 0 {
		t.Fatalf("git diff ran for a checkout without .codex/: %v", calls)
	}

	e, _, ws = projectEnv(t, map[string]string{".codex/config.toml": "x = 1\n"}, []string{".codex/config.toml"}, nil)
	e.setKind(KindCodex, func(k *config.Kind) { k.ProjectUntrust = nil })
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
		t.Fatal(err)
	}
	if got := e.h.starts[0].Args; !slices.Equal(got, judgeLaunchArgs) {
		t.Fatalf("project_untrust = []: args = %q", got)
	}
}

// What magnum cannot compare it does not trust: a failing merge base
// starts the session with the checkout untrusted, and the event says why.
func TestCodexProjectIsUntrustedWhenItCannotBeCompared(t *testing.T) {
	e, dir, ws := projectEnv(t, map[string]string{".codex/config.toml": "x = 1\n"}, nil, nil)
	e.run.Rules = slices.Insert(e.run.Rules, 0, execx.Rule{Prefix: []string{"git", "-C", dir, "merge-base"},
		Result: execx.Result{Code: 1}})
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
		t.Fatal(err)
	}
	if got := e.h.starts[0].Args; !slices.Equal(got, slices.Concat(judgeLaunchArgs, untrustArgs(dir))) {
		t.Fatalf("args = %q", got)
	}
	evs := projectEvents(t, e)
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "could not compare .codex/, AGENTS.md and AGENTS.override.md with the merge base") {
		t.Fatalf("events = %+v", evs)
	}
}

// The hooks a PR's .codex/ declares stay declined as before: the judge of
// a PR adding .codex/hooks.json starts untrusted and its hooks review is
// answered "Continue without trusting".
func TestHooksOfAPRChangingCodexStayDeclined(t *testing.T) {
	e, dir, ws := projectEnv(t, map[string]string{".codex/hooks.json": "{}"}, []string{".codex/hooks.json"}, nil)
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
		t.Fatal(err)
	}
	if got := e.h.starts[0].Args; !slices.Equal(got, slices.Concat(judgeLaunchArgs, untrustArgs(dir))) {
		t.Fatalf("args = %q", got)
	}
	if choice, why := e.m.hooksChoice(KindCodex, dir); choice != hooksDeclineOption || !strings.Contains(why, "declares hooks") {
		t.Fatalf("hooks choice = %d (%s), want decline", choice, why)
	}
}

func TestDetectRestrictedFolderDialog(t *testing.T) {
	daemon := strings.NewReplacer("2. Quit", "2. Back to Agent Command Center", "esc quit", "esc back").Replace(restrictedFolderScreen)
	for name, tc := range map[string]struct {
		text string
		ok   bool
		want trustDialog
	}{
		"embedded":    {restrictedFolderScreen, true, trustDialog{kind: KindCodex, acceptSelected: true, restricted: true}},
		"daemon":      {daemon, true, trustDialog{kind: KindCodex, acceptSelected: true, restricted: true}},
		"quit chosen": {strings.NewReplacer("› 1.", "  1.", "  2. Quit", "› 2. Quit").Replace(restrictedFolderScreen), true, trustDialog{kind: KindCodex, rejectSelected: true, restricted: true}},
		// Opening an existing task keeps what it loaded while trusted: not answered.
		"existing task": {strings.Replace(daemon, "Open restricted", "Open existing task", 1), false, trustDialog{}},
		"quoted":        {"+ menu: \"› 1. Open restricted\"\n", false, trustDialog{}},
	} {
		d, ok := detectTrustDialog(tc.text)
		if ok != tc.ok || d != tc.want {
			t.Errorf("%s: detect = %+v, %v; want %+v, %v", name, d, ok, tc.want, tc.ok)
		}
	}
}

// A session told the checkout is untrusted opens on Codex's "Folder
// access" dialog, at every launch and resume: magnum answers "Open
// restricted", which loads nothing and saves no trust, so it needs no
// first-launch window: also before a prompt to a session prompted before.
// The first-launch trust dialog keeps its window.
func TestRestrictedFolderDialogIsOpenedRestricted(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace()
	name := "mg-11920-codex-judge-5d01cf"
	(&trustAgent{name: name, screen: restrictedFolderScreen, idle: codexIdleScreen}).install(e.h)
	e.h.startStatus = herdr.StatusBlocked
	e.h.startErr = &herdr.Error{Method: "agent.start", Code: "agent_not_ready", Message: "agent is blocked"}
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], "01a0-uuid"); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	if got := e.keysSent(); len(got) != 1 || got[0] != name+":enter" {
		t.Fatalf("keys = %v, want one enter", got)
	}
	evs := e.trustEvents()
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "Open restricted") {
		t.Fatalf("events = %+v", evs)
	}

	// Later, before a prompt to a session prompted long ago.
	e.h.startStatus, e.h.startErr = "", nil
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "first"); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(TrustWindow * 3)
	e.h.mu.Lock()
	e.h.reads[name] = restrictedFolderScreen
	e.h.mu.Unlock()
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunRereview, "re-review"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := e.keysSent(); len(got) != 2 || got[1] != name+":enter" {
		t.Fatalf("keys = %v, want a second enter before the prompt", got)
	}
	if n := len(e.h.prompts); n != 2 {
		t.Fatalf("prompts = %d, want 2", n)
	}
}

// Codex never loads a log file from .codex/ either: untracked logs there
// (under .codex/log/ or .codex/logs/, or named *.log or *.log.<digits>)
// leave the team's .codex/ trusted, while an untracked rule file still
// untrusts the checkout.
func TestCodexKeepsTheTeamsProjectWhenOnlyLogFilesAreUntracked(t *testing.T) {
	for untracked, declined := range map[string]bool{".codex/logs/hook.txt": false, ".codex/hooks/run.log.3": false, ".codex/rules/x.rules": true} {
		e, dir, ws := projectEnv(t, map[string]string{".codex/config.toml": "model = \"x\"\n"}, nil, []string{untracked})
		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
			t.Fatal(err)
		}
		want := judgeLaunchArgs
		if declined {
			want = slices.Concat(judgeLaunchArgs, untrustArgs(dir))
		}
		if got := e.h.starts[0].Args; !slices.Equal(got, want) {
			t.Fatalf("untracked %s: args = %q\nwant %q", untracked, got, want)
		}
		if evs := projectEvents(t, e); len(evs) != map[bool]int{true: 1}[declined] {
			t.Fatalf("untracked %s: events = %+v", untracked, evs)
		}
	}
}
