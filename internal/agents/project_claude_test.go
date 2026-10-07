package agents

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
)

// claudeLaunchArgs are claude-review's args in the test env (a wrapper, so
// no kind args): its name and effort, and none of the operator's MCP
// servers.
var claudeLaunchArgs = []string{"--name", "PR #11920 claude-review - talkable", "--effort", "high", "--strict-mcp-config"}

// userSettingsOnly is the claude kind's project_untrust.
var userSettingsOnly = []string{"--setting-sources", "user"}

func claudeProjectEvents(t *testing.T, e *env) []store.Event {
	t.Helper()
	all, err := e.st.EventsBySubject(e.ctx, "pr:talkable/talkable#11920", 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Event
	for _, ev := range all {
		if ev.Kind == EventClaudeProjectDeclined {
			out = append(out, ev)
		}
	}
	return out
}

// A PR controls its checkout's .claude/ (settings with hooks Claude Code
// runs outside any sandbox, env, plugins; skills, commands, agents) and
// .mcp.json, and magnum trusts its checkouts and skips Claude's
// permissions. When the PR changes them against its merge base, a claude
// role (launched or resumed) loads the user's settings only, so none of
// the PR's project config loads. The decision is an event (counts, never a
// path) and the PR's claude record with the head, for the board and the
// judge.
func TestClaudeOfAPRChangingClaudeConfigLoadsOnlyTheUserSettings(t *testing.T) {
	for _, resume := range []string{"", "0f1e-uuid"} {
		e, dir, ws := projectEnv(t, map[string]string{
			".claude/settings.json": `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"evil"}]}]}}`,
			".mcp.json":             `{"mcpServers":{"evil":{"command":"evil-mcp"}}}`,
		}, []string{".claude/settings.json", ".mcp.json"}, []string{".claude/settings.local.json"})
		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], resume); err != nil {
			t.Fatal(err)
		}
		want := slices.Concat(claudeLaunchArgs, userSettingsOnly)
		if resume != "" {
			want = slices.Concat([]string{"--resume", resume}, want)
		}
		if got := e.h.starts[0].Args; !slices.Equal(got, want) {
			t.Fatalf("resume %q: args = %q\nwant %q", resume, got, want)
		}
		if calls := e.run.CallsWithPrefix("git", "-C", dir, "diff", "--name-only", "-z", "--no-renames", "--no-ext-diff", projectMergeBase, "--",
			":(top,literal,icase).claude", ":(top,literal,icase).mcp.json"); len(calls) != 1 {
			t.Fatalf("diff calls = %v", e.run.Calls)
		}
		evs := claudeProjectEvents(t, e)
		if len(evs) != 1 || !strings.Contains(evs[0].Message, "3 files under .claude/ or in .mcp.json differ from the merge base") ||
			!strings.Contains(evs[0].Message, "claude-review") || strings.Contains(evs[0].Message, "settings.local") {
			t.Fatalf("resume %q: events = %+v", resume, evs)
		}
		var data map[string]any
		if err := json.Unmarshal(evs[0].Data, &data); err != nil || data["role"] != "claude-review" || data["files"] != 3.0 || data["compared"] != true {
			t.Fatalf("event data = %s (%v)", evs[0].Data, err)
		}
		note, ok := ProjectDeclined(e.ctx, e.st, e.pr.ID, KindClaude)
		if !ok || note.Head != projectHead || note.Files != 3 {
			t.Fatalf("record = %+v, %v; want head %s", note, ok, projectHead)
		}
		if _, ok := ProjectDeclined(e.ctx, e.st, e.pr.ID, KindCodex); ok {
			t.Fatal("a Claude decision must not be recorded as Codex's")
		}
	}
}

// A PR that only changes .mcp.json (a server Claude Code starts, or one
// pointed at another URL with the slot's tokens) is declined the same way;
// a checkout with .mcp.json and no .claude/ is compared too.
func TestClaudeOfAPRChangingOnlyTheMCPConfigLoadsOnlyTheUserSettings(t *testing.T) {
	e, _, ws := projectEnv(t, map[string]string{".mcp.json": `{"mcpServers":{}}`}, []string{".mcp.json"}, nil)
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
		t.Fatal(err)
	}
	if got := e.h.starts[0].Args; !slices.Equal(got, slices.Concat(claudeLaunchArgs, userSettingsOnly)) {
		t.Fatalf("args = %q", got)
	}
	if evs := claudeProjectEvents(t, e); len(evs) != 1 || !strings.Contains(evs[0].Message, "1 file under .claude/ or in .mcp.json differs") {
		t.Fatalf("events = %+v", evs)
	}
}

// When the PR leaves .claude/ and .mcp.json as its merge base has them,
// the project config is the team's: it loads as before, and the launch
// clears an earlier Claude record but never the Codex one, which a Codex
// session of the same head keeps.
func TestUnchangedClaudeConfigKeepsTheTeamsSettings(t *testing.T) {
	e, _, ws := projectEnv(t, map[string]string{".claude/settings.json": "{}", ".mcp.json": "{}"}, nil, nil)
	for _, kind := range []string{KindClaude, KindCodex} {
		if err := e.st.SetKV(e.ctx, store.KVPRProject(e.pr.ID, kind), `{"head":"`+projectHead+`","files":1}`); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
		t.Fatal(err)
	}
	if got := e.h.starts[0].Args; !slices.Equal(got, claudeLaunchArgs) {
		t.Fatalf("args = %q, want the team's settings on", got)
	}
	if _, ok := ProjectDeclined(e.ctx, e.st, e.pr.ID, KindClaude); ok {
		t.Fatal("an unchanged .claude/ must clear the Claude record")
	}
	if _, ok := ProjectDeclined(e.ctx, e.st, e.pr.ID, KindCodex); !ok {
		t.Fatal("a Claude launch cleared the Codex record")
	}
	if evs := claudeProjectEvents(t, e); len(evs) != 0 {
		t.Fatalf("events = %+v", evs)
	}
}

// A checkout with neither .claude/ nor .mcp.json gives Claude Code nothing
// of the PR's to load, so no git runs (a .claude file is no directory); a
// claude kind with project_untrust = [] loads a changed .claude/.
func TestClaudeProjectNeedsItsConfigAndProjectUntrust(t *testing.T) {
	e, dir, ws := projectEnv(t, map[string]string{"README.md": "x\n", ".claude": "a file, not a directory\n"}, []string{".claude"}, nil)
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
		t.Fatal(err)
	}
	if got := e.h.starts[0].Args; !slices.Equal(got, claudeLaunchArgs) {
		t.Fatalf("args = %q", got)
	}
	if calls := e.run.CallsWithPrefix("git", "-C", dir, "diff"); len(calls) != 0 {
		t.Fatalf("git diff ran for a checkout without .claude/ or .mcp.json: %v", calls)
	}

	e, _, ws = projectEnv(t, map[string]string{".claude/settings.json": "{}"}, []string{".claude/settings.json"}, nil)
	e.setKind(KindClaude, func(k *config.Kind) { k.ProjectUntrust = nil })
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
		t.Fatal(err)
	}
	if got := e.h.starts[0].Args; !slices.Equal(got, claudeLaunchArgs) {
		t.Fatalf("project_untrust = []: args = %q", got)
	}
	if evs := claudeProjectEvents(t, e); len(evs) != 0 {
		t.Fatalf("project_untrust = []: events = %+v", evs)
	}
}

// What magnum cannot compare it does not load: a failing merge base starts
// Claude with the user's settings only, and the event says why.
func TestClaudeProjectIsLeftOutWhenItCannotBeCompared(t *testing.T) {
	e, dir, ws := projectEnv(t, map[string]string{".claude/settings.json": "{}"}, nil, nil)
	e.run.Rules = slices.Insert(e.run.Rules, 0, execx.Rule{Prefix: []string{"git", "-C", dir, "merge-base"},
		Result: execx.Result{Code: 1}})
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
		t.Fatal(err)
	}
	if got := e.h.starts[0].Args; !slices.Equal(got, slices.Concat(claudeLaunchArgs, userSettingsOnly)) {
		t.Fatalf("args = %q", got)
	}
	evs := claudeProjectEvents(t, e)
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "could not compare .claude/ and .mcp.json with the merge base") {
		t.Fatalf("events = %+v", evs)
	}
	if note, ok := ProjectDeclined(e.ctx, e.st, e.pr.ID, KindClaude); !ok || note.Compared {
		t.Fatalf("record = %+v, %v", note, ok)
	}
}

// The board's card and the judge learn of each agent's decision for the
// round's head: one sentence per CLI that ran without the PR's project
// config, Codex first; a record of another head says nothing.
func TestDeclinedProjectsOfTheHead(t *testing.T) {
	e := newEnv(t)
	set := func(kind, head string) {
		if err := e.st.SetKV(e.ctx, store.KVPRProject(e.pr.ID, kind), `{"head":"`+head+`","files":1,"compared":true}`); err != nil {
			t.Fatal(err)
		}
	}
	set(KindClaude, projectHead)
	if got := ProjectSentences(e.ctx, e.st, e.pr.ID, projectHead); got != ClaudeProjectSentence {
		t.Fatalf("sentences = %q", got)
	}
	round := []string{KindCodex, KindClaude, KindClaude}
	var jd JudgeData
	NoteDeclinedProjects(e.ctx, e.st, e.pr.ID, projectHead, round, &jd)
	if !jd.ClaudeProjectDeclined || jd.CodexProjectDeclined {
		t.Fatalf("judge data: codex %v, claude %v", jd.CodexProjectDeclined, jd.ClaudeProjectDeclined)
	}
	set(KindCodex, projectHead)
	if got := ProjectSentences(e.ctx, e.st, e.pr.ID, projectHead); got != CodexProjectSentence+" "+ClaudeProjectSentence {
		t.Fatalf("sentences = %q", got)
	}
	// A round none of whose roles is a Claude one hears only of Codex.
	jd = JudgeData{}
	NoteDeclinedProjects(e.ctx, e.st, e.pr.ID, projectHead, []string{KindCodex, KindCodex}, &jd)
	if jd.ClaudeProjectDeclined || !jd.CodexProjectDeclined {
		t.Fatalf("a round without Claude: judge data: codex %v, claude %v", jd.CodexProjectDeclined, jd.ClaudeProjectDeclined)
	}
	if got := ProjectSentences(e.ctx, e.st, e.pr.ID, projectMergeBase); got != "" {
		t.Fatalf("another head: %q", got)
	}
	jd = JudgeData{}
	NoteDeclinedProjects(e.ctx, e.st, e.pr.ID, projectMergeBase, round, &jd)
	if jd.ClaudeProjectDeclined || jd.CodexProjectDeclined {
		t.Fatalf("another head: judge data %+v", jd)
	}
}

// Claude Code reloads its settings files and skills while it runs, and
// loads a .claude/settings.json the checkout gains later, so a session that
// started with the project config loaded would take a later head's from
// disk. ReloadsProject names those sessions for the engine, which quits
// them before it moves the checkout: a Claude session launched with the
// project config loaded, or one magnum did not launch (adopted: it cannot
// tell); never one launched with the user's settings only, a Codex session
// (Codex reads its config at start) or a claude kind whose project_untrust
// is [] (the operator lets a changed config load anyway).
func TestReloadsProjectNamesTheClaudeSessionsThatLoadedTheProjectConfig(t *testing.T) {
	e, _, ws := projectEnv(t, map[string]string{".claude/settings.json": "{}"}, nil, nil)
	for _, r := range []Role{RoleClaude, RoleJudge} {
		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(r), ws.Panes[r], ""); err != nil {
			t.Fatal(err)
		}
	}
	if !e.m.ReloadsProject(e.ctx, e.session(RoleClaude)) {
		t.Error("a Claude session with the project config loaded must be quit before the checkout moves")
	}
	if e.m.ReloadsProject(e.ctx, e.session(RoleJudge)) {
		t.Error("a Codex session does not reload its project config")
	}
	e.setKind(KindClaude, func(k *config.Kind) { k.ProjectUntrust = nil })
	if e.m.ReloadsProject(e.ctx, e.session(RoleClaude)) {
		t.Error("project_untrust = []: a changed config loads anyway, so nothing to quit for")
	}

	e, _, ws = projectEnv(t, map[string]string{".claude/settings.json": "{}"}, []string{".claude/settings.json"}, nil)
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
		t.Fatal(err)
	}
	if e.m.ReloadsProject(e.ctx, e.session(RoleClaude)) {
		t.Error("a Claude session with the user's settings only never loads the project config")
	}

	adopted, err := e.st.CreateSession(e.ctx, store.Session{PRID: e.pr.ID, Role: "claude-simplify", AgentName: store.Ptr("mg-x"),
		AgentKind: store.Ptr(KindClaude), State: store.SessionLive})
	if err != nil {
		t.Fatal(err)
	}
	if !e.m.ReloadsProject(e.ctx, adopted) {
		t.Error("a Claude session magnum did not launch may have the project config loaded")
	}
}

// An agent StartAgent adopts instead of launching (its name already taken,
// or a conversation herdr restored by itself without magnum's flags) may
// run with the project config loaded whatever an earlier launch on the same
// session row did: adopting it drops the mark, so the engine quits it before
// the checkout moves.
func TestAnAdoptedClaudeAgentCountsAsReloadingTheProjectConfig(t *testing.T) {
	e, _, ws := projectEnv(t, map[string]string{".claude/settings.json": "{}"}, []string{".claude/settings.json"}, nil)
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
		t.Fatal(err)
	}
	if e.m.ReloadsProject(e.ctx, e.session(RoleClaude)) {
		t.Fatal("launched with the user's settings only")
	}
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
		t.Fatal(err)
	}
	if n := len(e.h.starts); n != 1 {
		t.Fatalf("agent starts = %d, want the second call to adopt", n)
	}
	if !e.m.ReloadsProject(e.ctx, e.session(RoleClaude)) {
		t.Fatal("an adopted agent kept the mark of the launch before it")
	}
}

// A team's own Claude Code hook (the base branch's .claude/settings.json)
// logs tool use to gitignored files under the checkout's .claude/log/, so
// every slot it ran in has untracked files there. Claude Code never loads a
// log file as configuration, so an untracked one (a base name ending in
// .log or .log.<digits>, or a file under .claude/log/ or .claude/logs/)
// leaves the team's settings, skills and CLAUDE.md loaded. Every other
// untracked file still declines, ignored or not (settings.local.json, a
// skill, even one named logs), and so does a tracked change, a log file's
// too.
func TestClaudeKeepsTheTeamsSettingsWhenOnlyLogFilesAreUntracked(t *testing.T) {
	for _, c := range []struct {
		name               string
		changed, untracked []string
		declined           bool
	}{
		{"the hook's logs", nil, []string{".claude/log/tool_use.log", ".claude/log/tool_use.log.1"}, false},
		{"a log anywhere under .claude/", nil, []string{".claude/hooks/run.log", ".claude/skills/x/out.log.12", ".claude/logs/today.txt"}, false},
		{"settings.local.json", nil, []string{".claude/log/tool_use.log", ".claude/settings.local.json"}, true},
		{"a skill", nil, []string{".claude/skills/x/SKILL.md"}, true},
		{"a skill named logs", nil, []string{".claude/skills/logs/SKILL.md"}, true},
		{"a log-like name that is none", nil, []string{".claude/tool_use.log.old", ".claude/catalog.md"}, true},
		{"the PR's settings", []string{".claude/settings.json"}, []string{".claude/log/tool_use.log"}, true},
		{"a tracked log", []string{".claude/log/tool_use.log"}, nil, true},
		{"an untracked .mcp.json", nil, []string{".mcp.json"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _, ws := projectEnv(t, map[string]string{".claude/settings.json": "{}", ".claude/log/tool_use.log": "x\n"}, c.changed, c.untracked)
			if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
				t.Fatal(err)
			}
			want := claudeLaunchArgs
			if c.declined {
				want = slices.Concat(claudeLaunchArgs, userSettingsOnly)
			}
			if got := e.h.starts[0].Args; !slices.Equal(got, want) {
				t.Fatalf("args = %q\nwant %q", got, want)
			}
			_, recorded := ProjectDeclined(e.ctx, e.st, e.pr.ID, KindClaude)
			if evs := claudeProjectEvents(t, e); recorded != c.declined || len(evs) != map[bool]int{true: 1}[c.declined] {
				t.Fatalf("record %v, events %+v; want declined %v", recorded, evs, c.declined)
			}
		})
	}
}

// macOS's filesystem ignores case, and folds Unicode too ("ſ" is "s"), so
// Claude Code opening .claude/settings.json or .mcp.json reads the
// .Claude/settings.json or .mcp.jſon a PR added: such a path is the
// project config like the real one. A tracked .Claude/settings.json, an
// untracked .CLAUDE/ skill or .Claude/Settings.Local.JSON decline; an
// untracked .CLAUDE/LOG/ log does not. Every root entry that folds to a
// path is compared by its own name too, since git's case matching is
// ASCII-only and would miss .mcp.jſon.
func TestClaudeProjectPathsMatchInAnyCase(t *testing.T) {
	longS := ".mcp.jſon"
	for _, c := range []struct {
		name               string
		files              map[string]string
		changed, untracked []string
		declined           bool
	}{
		{"a tracked .Claude/settings.json", map[string]string{".Claude/settings.json": "{}"}, []string{".Claude/settings.json"}, nil, true},
		{"an untracked .CLAUDE/ skill", map[string]string{".CLAUDE/skills/x/SKILL.md": "x"}, nil, []string{".CLAUDE/skills/x/SKILL.md"}, true},
		{"an untracked .Claude/Settings.Local.JSON", map[string]string{".Claude/Settings.Local.JSON": "{}"}, nil, []string{".Claude/Settings.Local.JSON"}, true},
		{"an untracked .CLAUDE/LOG/ log", map[string]string{".CLAUDE/LOG/tool_use.LOG": "x"}, nil, []string{".CLAUDE/LOG/tool_use.LOG"}, false},
		{"a .mcp.json with a long s", map[string]string{longS: "{}"}, []string{longS}, nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, dir, ws := projectEnv(t, c.files, c.changed, c.untracked)
			if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleClaude), ws.Panes[RoleClaude], ""); err != nil {
				t.Fatal(err)
			}
			want := claudeLaunchArgs
			if c.declined {
				want = slices.Concat(claudeLaunchArgs, userSettingsOnly)
			}
			if got := e.h.starts[0].Args; !slices.Equal(got, want) {
				t.Fatalf("args = %q\nwant %q", got, want)
			}
			// Each path, then each root entry folding to it by its own name.
			specs := []string{":(top,literal,icase).claude"}
			for f := range c.files {
				if top, _, _ := strings.Cut(f, "/"); top != ".claude" && strings.EqualFold(top, ".claude") {
					specs = append(specs, ":(top,literal,icase)"+top)
				}
			}
			specs = append(specs, ":(top,literal,icase).mcp.json")
			if _, ok := c.files[longS]; ok {
				specs = append(specs, ":(top,literal,icase)"+longS)
			}
			diff := slices.Concat([]string{"git", "-C", dir, "diff", "--name-only", "-z", "--no-renames", "--no-ext-diff", projectMergeBase, "--"}, specs)
			if calls := e.run.CallsWithPrefix(diff...); len(calls) != 1 || len(calls[0].Args) != len(diff)-1 {
				t.Fatalf("diff calls = %v\nwant %q", e.run.Calls, diff)
			}
		})
	}
}

// Codex too opens a .Codex/config.toml for .codex/config.toml on macOS: a
// PR adding one untrusts the checkout. A PR's file list names the project
// config in any case, so the engine parks a Claude session before such a
// head too; a longer name is another path.
func TestCodexProjectAndThePRsFileListMatchInAnyCase(t *testing.T) {
	e, dir, ws := projectEnv(t, map[string]string{".Codex/config.toml": "[mcp_servers.evil]\n"}, []string{".Codex/config.toml"}, nil)
	if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
		t.Fatal(err)
	}
	if got, want := e.h.starts[0].Args, slices.Concat(judgeLaunchArgs, untrustArgs(dir)); !slices.Equal(got, want) {
		t.Fatalf("args = %q\nwant %q", got, want)
	}
	for kind, paths := range map[string][]string{
		KindClaude: {".Claude/settings.json", ".MCP.JSON", ".mcp.jſon", ".CLAUDE"},
		KindCodex:  {".Codex/config.toml", ".CODEX/hooks.json"},
	} {
		for _, p := range paths {
			if !ProjectTouched(kind, []string{"app/x.rb", p}) {
				t.Errorf("%s: %s must touch the project config", kind, p)
			}
		}
	}
	for _, p := range []string{".claudex/settings.json", "app/.claude/settings.json", ".mcp.json.bak", ".MCP.JSON/x"} {
		if ProjectTouched(KindClaude, []string{p}) {
			t.Errorf("%s touches no Claude project config", p)
		}
	}
}
