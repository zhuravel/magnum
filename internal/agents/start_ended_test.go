package agents

import (
	"errors"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// A fresh Codex judge that never came up (agent.start answered ok, herdr
// listed the name at status unknown with no conversation) lost its round,
// which closed the session; 90 s later the next round's start adopted that
// agent, and its prompt found it gone 13 s later. An agent herdr lists
// under the name of a session magnum closed minutes ago, or one that never
// came up and carries no conversation while a session of that name magnum
// closed (or marked lost to start the judge again) exists, is not adopted:
// the start launches the agent anew in its pane, or, when that agent sits
// in another pane, is busy. Anything else is adopted as before.
func TestAStartDoesNotAdoptAnAgentOfAClosedSession(t *testing.T) {
	name := AgentName("talkable/talkable", 11920, RoleJudge)
	for label, tc := range map[string]struct {
		state        string        // the earlier session's
		closedAgo    time.Duration // when it was closed (0 = not closed)
		conversation string        // the agent's
		status       herdr.Status  // the agent's
		elsewhere    bool          // the agent's pane is not the start's
		otherName    bool          // the earlier session had another agent name
		adopted      bool
	}{
		"closed a minute and a half ago":                {store.SessionClosed, 90 * time.Second, "", herdr.StatusUnknown, false, false, false},
		"closed minutes ago, with a conversation":       {store.SessionClosed, 90 * time.Second, "01a0-uuid", herdr.StatusIdle, false, false, false},
		"closed hours ago, never came up":               {store.SessionClosed, 3 * time.Hour, "", herdr.StatusUnknown, false, false, false},
		"lost to the judge's restart, never came up":    {store.SessionLost, 0, "", herdr.StatusUnknown, false, false, false},
		"closed minutes ago, in another pane":           {store.SessionClosed, 90 * time.Second, "", herdr.StatusUnknown, true, false, false},
		"closed hours ago, with a conversation":         {store.SessionClosed, 3 * time.Hour, "01a0-uuid", herdr.StatusIdle, false, false, true},
		"closed hours ago, up without a conversation":   {store.SessionClosed, 3 * time.Hour, "", herdr.StatusIdle, false, false, true},
		"lost to the judge's restart, up with its turn": {store.SessionLost, 0, "01a0-uuid", herdr.StatusIdle, false, false, true},
		"closed minutes ago under another name":         {store.SessionClosed, 90 * time.Second, "", herdr.StatusUnknown, false, true, true},
	} {
		t.Run(label, func(t *testing.T) {
			e := newEnv(t)
			earlier := store.Session{PRID: e.pr.ID, Role: string(RoleJudge), AgentName: &name, AgentKind: new(KindCodex),
				State: tc.state, StartedAt: t0.Add(-tc.closedAgo - time.Minute)}
			if tc.otherName {
				earlier.AgentName = new("mg-11920-codex-judge-0ther0")
			}
			if tc.closedAgo > 0 {
				earlier.ClosedAt = new(t0.Add(-tc.closedAgo))
			}
			if _, err := e.st.CreateSession(e.ctx, earlier); err != nil {
				t.Fatal(err)
			}
			ws := e.workspace()
			pane := ws.Panes[RoleJudge]
			if tc.elsewhere {
				pane = "w1:p9"
				e.h.addPane(herdr.Pane{ID: pane, WorkspaceID: ws.WorkspaceID, Cwd: "/Users/x/Projects/talkable.review1"})
			}
			ghost := herdr.AgentInfo{Name: name, PaneID: pane, WorkspaceID: ws.WorkspaceID, Agent: KindCodex, AgentStatus: tc.status}
			if tc.conversation != "" {
				ghost.AgentSession = &herdr.AgentSession{Kind: "id", Value: tc.conversation}
			}
			e.h.addAgent(ghost)

			err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], "")
			switch {
			case tc.adopted:
				if err != nil || len(e.h.starts) != 0 || store.Deref(e.session(RoleJudge).HerdrPaneID) != pane {
					t.Fatalf("err %v, starts %+v: want the agent adopted", err, e.h.starts)
				}
			case tc.elsewhere:
				if !errors.Is(err, ErrBusy) || len(e.h.starts) != 0 {
					t.Fatalf("err %v, starts %+v: want ErrBusy and no start", err, e.h.starts)
				}
				if s := e.session(RoleJudge); s.State == store.SessionLive {
					t.Fatalf("session = %+v: the closed session's agent was adopted", s)
				}
			default:
				if err != nil || len(e.h.starts) != 1 || e.h.starts[0].PaneID != ws.Panes[RoleJudge] || e.h.starts[0].Name != name {
					t.Fatalf("err %v, starts %+v: want the agent launched anew in its pane", err, e.h.starts)
				}
				if s := e.session(RoleJudge); s.State != store.SessionLive || !s.StartedAt.Equal(t0) {
					t.Fatalf("session = %+v, want live with the new launch", s)
				}
			}
		})
	}
}
