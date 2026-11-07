package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// The herdr sessions an App talks to: the one of magnum's panes, whose ids
// the registry records, and yours ([herdr] socket), where the toasts show
// and whose agents the slot guard and the inventory read too.

// herdrServer is a herdr API socket of a test's own, answering the calls
// the App's layers make: a snapshot it is given, a toast, sidebar tokens.
// It records the method of every call.
type herdrServer struct {
	socket string
	snap   herdr.Snapshot

	mu    sync.Mutex
	calls []string
}

func newHerdrServer(t *testing.T, snap herdr.Snapshot) *herdrServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "ap") //nolint:usetesting // t.TempDir() can pass the 104 bytes of a socket path on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s := &herdrServer{socket: filepath.Join(dir, "h.sock"), snap: snap}
	ln, err := net.Listen("unix", s.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.answer(conn)
		}
	}()
	return s
}

func (s *herdrServer) answer(conn net.Conn) {
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req struct{ ID, Method string }
	if json.Unmarshal(line, &req) != nil {
		return
	}
	s.mu.Lock()
	s.calls = append(s.calls, req.Method)
	s.mu.Unlock()
	var result any = map[string]any{}
	switch req.Method {
	case "session.snapshot":
		result = map[string]any{"snapshot": s.snap}
	case "notification.show":
		result = herdr.NotificationResult{Shown: true}
	}
	b, _ := json.Marshal(map[string]any{"id": req.ID, "result": result})
	conn.Write(append(b, '\n'))
}

// methods lists the methods called so far.
func (s *herdrServer) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// herdrConfig is testConfig with your herdr session on yours and, for a
// review socket, magnum's review panes in session reviews on it.
func herdrConfig(yours, review string) string {
	keys := fmt.Sprintf("socket = %q", yours)
	if review != "" {
		keys += fmt.Sprintf("\nreview_session = \"reviews\"\nreview_socket = %q", review)
	}
	return strings.Replace(testConfig, `socket = "/nonexistent/herdr.sock"`, keys, 1)
}

// recordPanes returns a seed for testAppSeeded that records, as the daemon
// does, the herdr session the registry's ids belong to.
func recordPanes(s herdr.Session) func(t *testing.T, layout paths.Layout) {
	return func(t *testing.T, layout paths.Layout) {
		t.Helper()
		st := storetest.OpenFile(t, layout.DB())
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetKV(context.Background(), store.KVHerdrPanes, string(b)); err != nil {
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// A registry that names no session for its ids is one every earlier build
// kept in your session: the App opens its panes there, whatever
// review_session says, until the daemon has moved them.
func TestAnUnrecordedRegistryKeepsThePanesOnYourSocket(t *testing.T) {
	yours, reviews := newHerdrServer(t, herdr.Snapshot{}), newHerdrServer(t, herdr.Snapshot{})
	a, _ := testAppWith(t, herdrConfig(yours.socket, reviews.socket), Options{})
	if want := a.Config.YourHerdr(); a.PaneSession != want || a.Herdr.Socket != yours.socket {
		t.Fatalf("panes in %+v on %s, want your session %+v", a.PaneSession, a.Herdr.Socket, want)
	}
	if a.Yours != nil {
		t.Fatalf("a second client of your session on %s", a.Yours.Socket)
	}
	if _, err := a.Herdr.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := reviews.methods(); len(got) != 0 {
		t.Fatalf("the review session was called (%v) before any pane moved there", got)
	}
}

// The untagged App, on the real registry, opens its panes where the
// registry's ids came from (store.KVHerdrPanes), not where the config says
// they go: each herdr server numbers its own ids, and only the daemon's move
// takes the panes from one session to another.
func TestTheAppOpensPanesWhereTheRegistrysSessionsAre(t *testing.T) {
	yours, reviews := newHerdrServer(t, herdr.Snapshot{}), newHerdrServer(t, herdr.Snapshot{})
	recorded := herdr.Session{Name: "reviews", Socket: reviews.socket}
	// review_session is "" again (a rollback), and the move back waits.
	a, _ := testAppSeeded(t, herdrConfig(yours.socket, ""), Options{}, recordPanes(recorded))
	if a.PaneSession != recorded || a.Herdr.Socket != reviews.socket {
		t.Fatalf("panes in %+v on %s, want the recorded %+v", a.PaneSession, a.Herdr.Socket, recorded)
	}
	if a.Yours == nil || a.Yours.Socket != yours.socket {
		t.Fatalf("your session's client = %+v, want one on %s", a.Yours, yours.socket)
	}

	// The same holds under --dry-run, which reads a copy of the registry.
	d, _ := testAppSeeded(t, herdrConfig(yours.socket, ""), Options{DryRun: true}, recordPanes(recorded))
	if d.PaneSession != recorded {
		t.Fatalf("a dry run's panes in %+v, want the recorded %+v", d.PaneSession, recorded)
	}
}

// A registry whose record names no session is refused: the App cannot tell
// whose panes its ids are, and the wrong server's would be someone else's.
func TestARecordOfNoHerdrSessionIsRefused(t *testing.T) {
	home := t.TempDir()
	layout := paths.Layout{Home: home}
	st := storetest.OpenFile(t, layout.DB())
	if err := st.SetKV(context.Background(), store.KVHerdrPanes, "reviews"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := newTestApp(t, layout, Options{})
	if err == nil {
		a.Close()
		t.Fatalf("an App on a registry whose %s holds no session: panes on %s", store.KVHerdrPanes, a.Herdr.Socket)
	}
	if !strings.Contains(err.Error(), store.KVHerdrPanes) {
		t.Fatalf("the error does not name the record: %v", err)
	}
}

// A tagged App (an eval replay, the factory's local review) runs on a
// scratch registry of its own, so its panes open in the configured review
// session directly.
func TestATaggedAppOpensItsPanesInTheConfiguredReviewSession(t *testing.T) {
	yours, reviews := newHerdrServer(t, herdr.Snapshot{}), newHerdrServer(t, herdr.Snapshot{})
	a, _ := testAppSeeded(t, herdrConfig(yours.socket, reviews.socket), Options{AgentTag: "eval"},
		recordPanes(herdr.Session{Name: "default", Socket: yours.socket}))
	if want := (herdr.Session{Name: "reviews", Socket: reviews.socket}); a.PaneSession != want || a.Herdr.Socket != reviews.socket {
		t.Fatalf("a tagged App's panes in %+v on %s, want %+v", a.PaneSession, a.Herdr.Socket, want)
	}
}

// Toasts show in your session, the one you look at, the agents' too; the
// sidebar tokens label magnum's workspaces, which are in the panes' session.
func TestTheAppToastsInYourSessionAndLabelsWorkspacesInThePaneSession(t *testing.T) {
	yours, reviews := newHerdrServer(t, herdr.Snapshot{}), newHerdrServer(t, herdr.Snapshot{})
	a, _ := testAppSeeded(t, herdrConfig(yours.socket, reviews.socket), Options{},
		recordPanes(herdr.Session{Name: "reviews", Socket: reviews.socket}))
	ctx := context.Background()
	if shown, err := a.Notify.Toast(ctx, "", "talkable#2: COMMENTED", "", 0); err != nil || !shown {
		t.Fatalf("toast: %v, %v", shown, err)
	}
	if err := a.Sidebar.Sidebar(ctx, "w1", map[string]string{"magnum": "reviewing abc1234"}); err != nil {
		t.Fatalf("sidebar: %v", err)
	}
	a.toastAsync(ctx, "agent:1", "codex-judge needs you", "")
	deadline := time.Now().Add(10 * time.Second)
	for len(yours.methods()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got, want := yours.methods(), []string{"notification.show", "notification.show"}; !slices.Equal(got, want) {
		t.Errorf("your session got %v, want the App's and the agents' toasts %v", got, want)
	}
	if got, want := reviews.methods(), []string{"workspace.report_metadata"}; !slices.Equal(got, want) {
		t.Errorf("the panes' session got %v, want the sidebar tokens %v", got, want)
	}
}

// The slot guard and the inventory read both sessions: magnum's panes,
// whose ids the registry records, and yours, its ids prefixed, so your agent
// in a slot holds it even when its pane has the id of magnum's own.
func TestTheSlotGuardAndInventoryReadThePaneSessionAndYours(t *testing.T) {
	const slot = "/nonexistent/talkable.review1"
	reviews := newHerdrServer(t, herdr.Snapshot{
		Workspaces: []herdr.Workspace{{ID: "w1"}},
		Panes:      []herdr.Pane{{ID: "w1:p1", WorkspaceID: "w1", Agent: "codex", Cwd: slot}},
		Agents: []herdr.AgentInfo{{Name: "mg-talkable-1-codex-judge", Agent: "codex", PaneID: "w1:p1", WorkspaceID: "w1",
			Cwd: slot, AgentStatus: herdr.StatusIdle}},
	})
	yours := newHerdrServer(t, herdr.Snapshot{
		Workspaces: []herdr.Workspace{{ID: "w1"}},
		Panes:      []herdr.Pane{{ID: "w1:p1", WorkspaceID: "w1", Agent: "claude", Cwd: slot}},
		Agents: []herdr.AgentInfo{{Name: "alice", Agent: "claude", PaneID: "w1:p1", WorkspaceID: "w1", Cwd: slot,
			AgentStatus: herdr.StatusWorking}},
	})
	a, _ := testAppSeeded(t, herdrConfig(yours.socket, reviews.socket), Options{},
		recordPanes(herdr.Session{Name: "reviews", Socket: reviews.socket}))
	ctx := context.Background()
	repo, err := a.Store.UpsertRepo(ctx, store.Repo{NodeID: "R", Owner: "talkable", Name: "talkable", Mode: store.RepoModePool})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Store.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "P", Number: 1, URL: "u",
		HeadSHA: "abc", InitialState: store.PRReviewed, Identity: "talkable-app"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.CreateSession(ctx, store.Session{PRID: res.PR.ID, Role: store.RoleJudge,
		AgentName: new("mg-talkable-1-codex-judge"), HerdrWorkspaceID: new("w1"), HerdrPaneID: new("w1:p1"),
		Cwd: new(slot), State: store.SessionLive}); err != nil {
		t.Fatal(err)
	}

	err = a.Slots.GuardLive(ctx, store.Slot{Name: "review1", Path: slot})
	var hold slots.ErrHold
	if !errors.As(err, &hold) || hold.Reason != slots.HoldForeignAgent || !strings.Contains(hold.Detail, herdr.YourPrefix+"w1:p1") {
		t.Fatalf("the slot guard = %v, want your agent in pane %sw1:p1 to hold the slot", err, herdr.YourPrefix)
	}

	snap, err := a.Inventory.Herdr.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var panes []string
	for _, ag := range snap.Agents {
		panes = append(panes, ag.PaneID)
	}
	if want := []string{"w1:p1", herdr.YourPrefix + "w1:p1"}; !slices.Equal(panes, want) {
		t.Fatalf("the inventory sees agents in panes %v, want magnum's and yours %v", panes, want)
	}
}

// herdr restores your stopped session's panes when its server starts, and
// resumes their agents in their conversations: with no server of yours
// running, an agent herdr saved in a slot (session.json beside your socket)
// holds the slot as a live one does, so a slot's switch or a worktree's
// removal never takes the tree from under it.
func TestAnAgentHerdrSavedInYourStoppedSessionHoldsItsSlot(t *testing.T) {
	const slot = "/nonexistent/talkable.review1"
	reviews := newHerdrServer(t, herdr.Snapshot{})
	dir, err := os.MkdirTemp("", "hy") //nolint:usetesting // a dial of a longer path fails EINVAL, not ENOENT
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	saved := `{"version": 3, "workspaces": [{"id": "w2", "tabs": [{"panes": {"4": {"cwd": "` + slot + `",
		"agent_session": {"agent": "claude", "kind": "id", "source": "herdr:claude", "value": "conv-1"}}}}]}]}`
	if err := os.WriteFile(filepath.Join(dir, herdr.SavedFile), []byte(saved), 0o600); err != nil {
		t.Fatal(err)
	}
	a, _ := testAppSeeded(t, herdrConfig(filepath.Join(dir, "herdr.sock"), reviews.socket), Options{},
		recordPanes(herdr.Session{Name: "reviews", Socket: reviews.socket}))

	err = a.Slots.GuardLive(context.Background(), store.Slot{Name: "review1", Path: slot})
	var hold slots.ErrHold
	if !errors.As(err, &hold) || hold.Reason != slots.HoldForeignAgent || !strings.Contains(hold.Detail, herdr.YourPrefix+herdr.SavedPrefix) {
		t.Fatalf("the slot guard = %v, want the agent herdr saved in your session to hold the slot", err)
	}
}

// While the panes are in your session, one client reads it: the guard does
// not list each of its panes twice (once as yours, which would hold every
// slot for magnum's own agents), and the toasts and the sidebar share it.
func TestOneSessionIsOneClient(t *testing.T) {
	yours := newHerdrServer(t, herdr.Snapshot{
		Panes:  []herdr.Pane{{ID: "w1:p1", WorkspaceID: "w1", Agent: "codex"}},
		Agents: []herdr.AgentInfo{{Name: "mg-talkable-1-codex-judge", Agent: "codex", PaneID: "w1:p1", WorkspaceID: "w1"}},
	})
	for name, seed := range map[string]func(*testing.T, paths.Layout){
		"no record":         nil,
		"your session kept": recordPanes(herdr.Session{Name: "default", Socket: yours.socket}),
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := testAppSeeded(t, herdrConfig(yours.socket, ""), Options{}, seed)
			if a.Yours != nil || a.Guard.Yours != nil {
				t.Fatalf("a second client of the one session: %+v, guard %+v", a.Yours, a.Guard.Yours)
			}
			if a.Notify.Herdr != a.Herdr || a.Sidebar.Herdr != a.Herdr {
				t.Fatalf("toasts on %v and sidebar on %v, want the one client %v", a.Notify.Herdr, a.Sidebar.Herdr, a.Herdr)
			}
			snap, err := a.Guard.Snapshot(context.Background())
			if err != nil || len(snap.Agents) != 1 || snap.Agents[0].PaneID != "w1:p1" {
				t.Fatalf("the guard's snapshot = %+v, %v; want the one agent once, as the registry records it", snap.Agents, err)
			}
		})
	}
}
