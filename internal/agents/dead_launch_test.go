package agents

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// launchCleared is the data of the one EventLaunchCleared the test's PR
// recorded.
func launchCleared(t *testing.T, e *env) (store.Event, map[string]any) {
	t.Helper()
	evs := e.eventsOf(EventLaunchCleared)
	if len(evs) != 1 {
		t.Fatalf("%s events = %+v, want one", EventLaunchCleared, evs)
	}
	var data map[string]any
	if err := json.Unmarshal(evs[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	return evs[0], data
}

// On talkable#11932 codex died at its TUI bootstrap in the judge's pane, and
// herdr kept the failed launch listed under the judge's name (launch_pending,
// status unknown) on a pane that showed only a shell prompt: every later
// start under that name failed with agent_name_taken. A start now closes the
// pane of such a launch, records pr.launch_cleared (warn: the role, the
// agent, the pane, the CLI's own error line, never the TUI's text) and
// starts in a new pane: a replacement split from the closed one, with the
// pane environment and label its session recorded, when the launch held the
// very pane the start was given (a caller still holding that pane's id
// starts in the replacement), the given pane when the launch sat elsewhere.
func TestAStartClearsADeadLaunchThatHoldsItsName(t *testing.T) {
	name := AgentName("talkable/talkable", 11920, RoleJudge)
	t.Run("in the start's own pane", func(t *testing.T) {
		e := newEnv(t)
		ws := e.workspace()
		dead := ws.Panes[RoleJudge]
		env := e.session(RoleJudge).Env
		e.h.addAgent(herdr.AgentInfo{Name: name, PaneID: dead, WorkspaceID: ws.WorkspaceID, Agent: KindCodex,
			AgentStatus: herdr.StatusUnknown, LaunchPending: true})
		e.h.reads[dead] = "codex-cli 0.163.0\nError: the TUI could not start\n    PR title in a box\n% "

		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), dead, ""); err != nil {
			t.Fatalf("start: %v", err)
		}
		if !slices.Equal(e.h.paneCloses, []string{dead}) || len(e.h.closed) != 0 {
			t.Fatalf("pane closes %v, workspace closes %v: want the dead launch's pane alone", e.h.paneCloses, e.h.closed)
		}
		split := e.h.splits[len(e.h.splits)-1]
		repl := split.New
		if split.From != dead || split.Opts.Cwd != "/Users/x/Projects/talkable.review1" || split.Opts.Env["WT_BRANCH"] != env["WT_BRANCH"] ||
			split.Opts.Env["MAGNUM_PR_URL"] != e.pr.URL || split.Opts.Focus {
			t.Fatalf("replacement split = %+v, want one from %s with its session's cwd and env", split, dead)
		}
		if got := e.h.renames[repl]; got != TaggedPaneLabel("", 11920, RoleJudge) {
			t.Fatalf("replacement label = %q", got)
		}
		if len(e.h.starts) != 1 || e.h.starts[0].PaneID != repl || e.h.starts[0].Name != name {
			t.Fatalf("starts = %+v, want the judge started in the replacement %s", e.h.starts, repl)
		}
		if s := e.session(RoleJudge); s.State != store.SessionLive || store.Deref(s.HerdrPaneID) != repl {
			t.Fatalf("judge session = %+v, want live in %s", s, repl)
		}
		ev, data := launchCleared(t, e)
		if ev.Level != "warn" || data["role"] != "codex-judge" || data["agent"] != name || data["pane"] != dead ||
			data["replacement"] != repl || data["launch_pending"] != true || data["last_line"] != "Error: the TUI could not start" {
			t.Fatalf("event %+v data %v", ev, data)
		}
		if strings.Contains(ev.Message+string(ev.Data), "PR title") {
			t.Fatalf("the event carries the pane's text: %+v", ev)
		}

		// The round's restart of a gone judge still holds the closed pane's id.
		e.h.removeAgent(name)
		if err := e.m.markLost(e.ctx, e.session(RoleJudge)); err != nil {
			t.Fatal(err)
		}
		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), dead, ""); err != nil {
			t.Fatalf("restart: %v", err)
		}
		if len(e.h.starts) != 2 || e.h.starts[1].PaneID != repl {
			t.Fatalf("starts = %+v, want the restart in the replacement %s", e.h.starts, repl)
		}
	})
	t.Run("in another pane", func(t *testing.T) {
		e := newEnv(t)
		ws := e.workspace()
		old := ws.WorkspaceID + ":p9"
		e.h.addPane(herdr.Pane{ID: old, WorkspaceID: ws.WorkspaceID, Cwd: "/Users/x/Projects/talkable.review1"})
		e.h.addAgent(herdr.AgentInfo{Name: name, PaneID: old, WorkspaceID: ws.WorkspaceID, Agent: KindCodex, AgentStatus: herdr.StatusUnknown})
		splits := len(e.h.splits)

		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
			t.Fatalf("start: %v", err)
		}
		if !slices.Equal(e.h.paneCloses, []string{old}) || len(e.h.splits) != splits {
			t.Fatalf("pane closes %v, splits %+v: want the old pane closed and no new split", e.h.paneCloses, e.h.splits[splits:])
		}
		if len(e.h.starts) != 1 || e.h.starts[0].PaneID != ws.Panes[RoleJudge] {
			t.Fatalf("starts = %+v, want the judge in its own pane", e.h.starts)
		}
		if _, data := launchCleared(t, e); data["pane"] != old || data["replacement"] != nil || data["launch_pending"] != false {
			t.Fatalf("event data %v", data)
		}
	})
	t.Run("no environment recorded for a replacement", func(t *testing.T) {
		e := newEnv(t)
		ws := e.workspace()
		dead := ws.Panes[RoleJudge]
		if err := e.st.UpdateSession(e.ctx, e.session(RoleJudge).ID, func(u *store.SessionUpdate) { u.Set("env_json", "{}") }); err != nil {
			t.Fatal(err)
		}
		e.h.addAgent(herdr.AgentInfo{Name: name, PaneID: dead, WorkspaceID: ws.WorkspaceID, Agent: KindCodex,
			AgentStatus: herdr.StatusUnknown, LaunchPending: true})

		err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), dead, "")
		if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), dead) {
			t.Fatalf("err = %v, want ErrBusy naming the closed pane", err)
		}
		if !slices.Equal(e.h.paneCloses, []string{dead}) || len(e.h.starts) != 0 {
			t.Fatalf("pane closes %v, starts %+v: want the pane closed and no start", e.h.paneCloses, e.h.starts)
		}
		if _, err := e.st.LiveSessionByPRRole(e.ctx, e.pr.ID, string(RoleJudge)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("live judge session: %v, want it lost, for the next attempt's workspace to lay out a pane", err)
		}
	})
}

// An agent that holds the role's name is never cleared while it works or is
// blocked, nor when its pane runs anything but the shell (herdr's
// pane.process_info): only a launch that never came up over an idle shell
// is.
func TestAWorkingAgentThatHoldsTheNameIsNotCleared(t *testing.T) {
	name := AgentName("talkable/talkable", 11920, RoleJudge)
	for label, tc := range map[string]struct {
		status  herdr.Status
		pending bool
		busy    bool
	}{
		"working, launch pending": {herdr.StatusWorking, true, false},
		"blocked":                 {herdr.StatusBlocked, false, false},
		"idle":                    {herdr.StatusIdle, false, false},
		"unknown, its pane busy":  {herdr.StatusUnknown, true, true},
	} {
		t.Run(label, func(t *testing.T) {
			e := newEnv(t)
			ws := e.workspace()
			pane := ws.WorkspaceID + ":p9"
			e.h.addPane(herdr.Pane{ID: pane, WorkspaceID: ws.WorkspaceID, Cwd: "/Users/x/Projects/talkable.review1"})
			e.h.addAgent(herdr.AgentInfo{Name: name, PaneID: pane, WorkspaceID: ws.WorkspaceID, Agent: KindCodex,
				AgentStatus: tc.status, LaunchPending: tc.pending})
			e.h.busy[pane] = tc.busy

			_ = e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], "")
			if len(e.h.paneCloses) != 0 || len(e.eventsOf(EventLaunchCleared)) != 0 {
				t.Fatalf("pane closes %v, events %v: want the agent left alone", e.h.paneCloses, e.eventsOf(EventLaunchCleared))
			}
			snap, _ := e.h.Snapshot(e.ctx)
			if a, ok := snap.AgentByName(name); !ok || a.PaneID != pane {
				t.Fatalf("herdr lists %+v (%v), want the agent still in %s", a, ok, pane)
			}
		})
	}
}
