package herdr

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
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

// Your herdr server not running (no socket, or a stale one) has no pane
// processes, so it holds nothing: the guard sees its own session's panes
// alone. Its own session down is still an error: the panes its store
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
