package herdr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeView is a herdr session as a guard reads it; asked records the panes
// whose processes it was asked for.
type fakeView struct {
	snap  Snapshot
	asked []string
}

func (f *fakeView) Snapshot(context.Context) (Snapshot, error) { return f.snap, nil }

func (f *fakeView) PaneProcessInfo(_ context.Context, paneID string) (ProcessInfo, error) {
	f.asked = append(f.asked, paneID)
	return ProcessInfo{PaneID: paneID, ShellPID: 10, ForegroundPGID: 10}, nil
}

// oneWorkspace is a session that numbers its first workspace, tab, pane
// and agent as every herdr server does: from w1.
func oneWorkspace(agent string) Snapshot {
	return Snapshot{
		Workspaces: []Workspace{{ID: "w1", ActiveTabID: "w1:t1"}, {ID: "w2"}},
		Tabs:       []Tab{{ID: "w1:t1", WorkspaceID: "w1"}},
		Panes:      []Pane{{ID: "w1:p1", WorkspaceID: "w1", TabID: "w1:t1", Agent: agent}},
		Agents:     []AgentInfo{{PaneID: "w1:p1", WorkspaceID: "w1", TabID: "w1:t1", Name: agent}},
	}
}

// Each herdr server numbers its panes from w1, so one id can name a pane of
// magnum's session and one of yours. The guard's view keeps the ids of its
// own session, the ones its store records, and prefixes yours; an id that
// is "" stays "".
func TestTheGuardViewPrefixesYourSessionsIds(t *testing.T) {
	own, yours := &fakeView{snap: oneWorkspace("mg-1-judge")}, &fakeView{snap: oneWorkspace("claude")}
	got, err := GuardView{Own: own, Yours: yours}.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := Snapshot{
		Workspaces: []Workspace{{ID: "w1", ActiveTabID: "w1:t1"}, {ID: "w2"}, {ID: "yours:w1", ActiveTabID: "yours:w1:t1"}, {ID: "yours:w2"}},
		Tabs:       []Tab{{ID: "w1:t1", WorkspaceID: "w1"}, {ID: "yours:w1:t1", WorkspaceID: "yours:w1"}},
		Panes: []Pane{{ID: "w1:p1", WorkspaceID: "w1", TabID: "w1:t1", Agent: "mg-1-judge"},
			{ID: "yours:w1:p1", WorkspaceID: "yours:w1", TabID: "yours:w1:t1", Agent: "claude"}},
		Agents: []AgentInfo{{PaneID: "w1:p1", WorkspaceID: "w1", TabID: "w1:t1", Name: "mg-1-judge"},
			{PaneID: "yours:w1:p1", WorkspaceID: "yours:w1", TabID: "yours:w1:t1", Name: "claude"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the view is\n%+v\nwant\n%+v", got, want)
	}

	// One session (yours is the same socket) is read once, as it is.
	if got, err := (GuardView{Own: own}).Snapshot(t.Context()); err != nil || !reflect.DeepEqual(got, own.snap) {
		t.Fatalf("one session reads %+v (%v), want its snapshot as it is", got, err)
	}
}

// A guard asks the processes of a pane the view showed of the session the
// pane is of: yours, without the prefix, for a prefixed id, else its own;
// the answer keeps the id the guard asked for.
func TestTheGuardViewAsksTheSessionAPaneIsOf(t *testing.T) {
	own, yours := &fakeView{}, &fakeView{}
	g := GuardView{Own: own, Yours: yours}
	pi, err := g.ProcessInfo(t.Context(), "yours:w1:p1")
	if err != nil || pi.PaneID != "yours:w1:p1" || pi.ShellPID != 10 {
		t.Fatalf("your pane's processes %+v (%v), want yours, under the id the guard asked for", pi, err)
	}
	if pi, err := g.ProcessInfo(t.Context(), "w1:p1"); err != nil || pi.PaneID != "w1:p1" {
		t.Fatalf("its own pane's processes %+v (%v)", pi, err)
	}
	if !slices.Equal(yours.asked, []string{"w1:p1"}) || !slices.Equal(own.asked, []string{"w1:p1"}) {
		t.Fatalf("your session was asked for %q and its own for %q, want w1:p1 each", yours.asked, own.asked)
	}
	if _, err := (GuardView{Own: own}).ProcessInfo(t.Context(), "yours:w1:p1"); err == nil {
		t.Fatal("a view without your session answered for your pane")
	}
}

// Your herdr server not running (no socket, or a stale one) with nothing
// saved beside its socket has no panes, so it holds nothing: the guard sees
// its own session's panes alone. Its own session down is still an error: the panes its store
// records cannot be checked.
func TestYourSessionWithNoServerHasNoPanes(t *testing.T) {
	dir := shortTempDir(t)
	own := &fakeView{snap: oneWorkspace("mg-1-judge")}
	gone := &Client{Socket: filepath.Join(dir, "missing.sock")}
	got, err := GuardView{Own: own, Yours: gone}.Snapshot(t.Context())
	if err != nil || !reflect.DeepEqual(got, own.snap) {
		t.Fatalf("with your server gone the view is %+v (%v), want its own session's panes alone", got, err)
	}
	if _, err := (GuardView{Own: gone, Yours: own}).Snapshot(t.Context()); err == nil || !NoServer(err) {
		t.Fatalf("its own session with no server reads %v, want the error", err)
	}
}

// Your herdr server that does not answer in time may have busy panes: the
// view is an error, and the guard holds off as on its own herdr.
func TestYourSessionThatTimesOutHoldsTheGuard(t *testing.T) {
	s := newFakeServer(t)
	s.handle("session.snapshot", func(json.RawMessage) (any, *Error) { return hang{}, nil })
	slowYours := s.client()
	slowYours.Timeout = 50 * time.Millisecond
	got, err := GuardView{Own: &fakeView{snap: oneWorkspace("mg-1-judge")}, Yours: slowYours}.Snapshot(t.Context())
	if !IsTimeout(err) {
		t.Fatalf("with your server not answering the view is %+v (%v), want the timeout", got, err)
	}
}

// savedSession writes herdr's saved state of a session (session.json beside
// its socket, version 3) with one workspace: a shell in /work/shell and a
// Claude conversation in slot, as herdr keeps them while its server is
// stopped. It returns the session's socket, where no server runs.
func savedSession(t *testing.T, slot string) string {
	t.Helper()
	dir := shortTempDir(t)
	state := `{"version": 3, "active": 0, "selected": 0, "workspaces": [{"id": "wAB", "active_tab": 1, "custom_name": null,
		"identity_cwd": "/work", "tabs": [
			{"custom_name": null, "root_pane": 1, "panes": {"1": {"cwd": "/work/shell", "agent_session": null, "label": "notes", "launch_argv": null}}},
			{"custom_name": null, "root_pane": 6, "panes": {"6": {"cwd": "` + slot + `", "label": null, "launch_argv": null,
				"agent_session": {"agent": "claude", "kind": "id", "source": "herdr:claude", "value": "conv-1"}}}}]}]}`
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "herdr.sock")
}

// herdr restores a stopped session's panes when its server starts, in their
// directories, and resumes their agents' conversations. So your session with
// no server running is not empty for a guard: each pane herdr saved with an
// agent conversation is an idle agent of yours in its directory, which holds
// the checkout it is in as a live one does. A saved shell comes back an idle
// shell, which holds nothing, so it is not shown.
func TestYourSessionWithNoServerShowsTheAgentsHerdrSavedAsIdle(t *testing.T) {
	own := &fakeView{snap: oneWorkspace("mg-1-judge")}
	yours := &Client{Socket: savedSession(t, "/slots/review1")}
	got, err := GuardView{Own: own, Yours: yours}.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	conv := &AgentSession{Agent: "claude", Kind: "id", Source: "herdr:claude", Value: "conv-1"}
	want := oneWorkspace("mg-1-judge")
	want.Workspaces = append(want.Workspaces, Workspace{ID: "yours:saved:wAB"})
	want.Panes = append(want.Panes, Pane{ID: "yours:saved:wAB:p6", WorkspaceID: "yours:saved:wAB", Agent: "claude",
		AgentStatus: StatusIdle, AgentSession: conv, Cwd: "/slots/review1"})
	want.Agents = append(want.Agents, AgentInfo{PaneID: "yours:saved:wAB:p6", WorkspaceID: "yours:saved:wAB", Agent: "claude",
		AgentStatus: StatusIdle, AgentSession: conv, Cwd: "/slots/review1"})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("with your server stopped the view is\n%+v\nwant\n%+v", got, want)
	}
}

// A saved state that cannot be read, or that a herdr magnum does not know
// wrote (another version), may hold an agent of yours in any checkout: the
// view is an error, and the guard holds off as on a server that does not
// answer.
func TestASavedSessionThatCannotBeReadHoldsTheGuard(t *testing.T) {
	for name, state := range map[string]string{
		"broken":        `{"version": 3, "workspaces": [`,
		"other version": `{"version": 4, "workspaces": []}`,
		"no workspaces": `{"version": 3}`,
	} {
		t.Run(name, func(t *testing.T) {
			socket := savedSession(t, "/slots/review1")
			if err := os.WriteFile(filepath.Join(filepath.Dir(socket), "session.json"), []byte(state), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := GuardView{Own: &fakeView{}, Yours: &Client{Socket: socket}}.Snapshot(t.Context())
			if err == nil || !strings.Contains(err.Error(), "session.json") {
				t.Fatalf("the view is %+v (%v), want an error naming the saved state", got, err)
			}
		})
	}
}

// The factory's local review opens its rounds in magnum's review session,
// with their directories in the factory's copies: a pane you open there
// holds a copy only if the copies' guard reads that session too. Its ids are
// prefixed apart from yours (each server numbers from w1), and a guard asks
// a pane's processes of the session it is of.
func TestTheGuardViewReadsMagnumsReviewSessionAsAThirdSession(t *testing.T) {
	own, yours, review := &fakeView{snap: oneWorkspace("mg-1-judge")}, &fakeView{snap: oneWorkspace("claude")},
		&fakeView{snap: oneWorkspace("codex")}
	g := GuardView{Own: own, Yours: yours, Review: review}
	got, err := g.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var panes []string
	for _, p := range got.Panes {
		panes = append(panes, p.ID+"="+p.Agent)
	}
	if want := []string{"w1:p1=mg-1-judge", "yours:w1:p1=claude", "review:w1:p1=codex"}; !slices.Equal(panes, want) {
		t.Fatalf("the view's panes are %q, want %q", panes, want)
	}
	if pi, err := g.ProcessInfo(t.Context(), ReviewPrefix+"w1:p1"); err != nil || pi.PaneID != ReviewPrefix+"w1:p1" {
		t.Fatalf("the review pane's processes %+v (%v)", pi, err)
	}
	if !slices.Equal(review.asked, []string{"w1:p1"}) || len(yours.asked) != 0 || len(own.asked) != 0 {
		t.Fatalf("asked: review %q, yours %q, own %q; want the review session for w1:p1", review.asked, yours.asked, own.asked)
	}

	// With no server running it shows what herdr saved, as yours does.
	saved := GuardView{Own: own, Review: &Client{Socket: savedSession(t, "/copies/talkable.1")}}
	got, err = saved.Snapshot(t.Context())
	if err != nil || len(got.Agents) != 2 || got.Agents[1].PaneID != ReviewPrefix+SavedPrefix+"wAB:p6" {
		t.Fatalf("the review session with no server reads %+v (%v), want its saved agent", got.Agents, err)
	}
}
