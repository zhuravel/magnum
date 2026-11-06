package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// The move of magnum's panes to the herdr session the config names
// ([herdr] review_session): the registry's herdr ids belong to the session
// store.KVHerdrPanes records, and only an idle daemon takes them elsewhere.

var (
	yourPanes    = herdr.Session{Name: "default", Socket: "/run/herdr/herdr.sock"}
	reviewsPanes = herdr.Session{Name: "reviews", Socket: "/run/herdr/sessions/reviews/herdr.sock"}
)

// withPanesInYours makes the registry's herdr ids those of your session
// (Deps.PaneSession, as FromApp sets it from the App's), which the config
// names too: no move is due until moveToReviews. The idle park is off, so
// only the move parks.
func withPanesInYours(h *harness) {
	h.cfg.Herdr.Socket = yourPanes.Socket
	h.d.PaneSession = yourPanes
	h.cfg.Daemon.ParkIdleAfter.Duration = 0
}

// moveToReviews names session reviews for magnum's panes, as a daemon that
// started on [herdr] review_session = "reviews" reads it.
func (h *harness) moveToReviews() {
	h.cfg.Herdr.ReviewSession, h.cfg.Herdr.ReviewSocket = reviewsPanes.Name, reviewsPanes.Socket
}

// panesRecord is the session the registry records its herdr ids as of
// (store.KVHerdrPanes); false for none.
func (h *harness) panesRecord() (herdr.Session, bool) {
	h.t.Helper()
	v, ok := kvValue(h, store.KVHerdrPanes)
	if !ok {
		return herdr.Session{}, false
	}
	var s herdr.Session
	if err := json.Unmarshal([]byte(v), &s); err != nil {
		h.t.Fatalf("kv %s = %q: %v", store.KVHerdrPanes, v, err)
	}
	return s, true
}

// moveTick runs one tick, lets what it started finish, and returns the
// tick's error (ErrRestartForPanes once the move is done).
func (h *harness) moveTick() error {
	h.t.Helper()
	err := h.e.Tick(h.ctx)
	h.settle()
	return err
}

// herdrIDPRs are the PRs a session of which still records a herdr id.
func (h *harness) herdrIDPRs() []int64 {
	h.t.Helper()
	ids, err := h.st.PRsWithHerdrIDs(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return ids
}

// liveCount counts the starting and live sessions.
func (h *harness) liveCount() int {
	h.t.Helper()
	live, err := h.st.LiveSessions(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return len(live)
}

// closedPRWithPanes registers talkable/talkable#n as merged a day ago, with
// a lost session whose pane the old herdr session may still show.
func (h *harness) closedPRWithPanes(n int) store.PR {
	h.t.Helper()
	closed := h.clock.Now().Add(-24 * time.Hour)
	up, err := h.st.UpsertPRFromGitHub(h.ctx, store.GitHubPR{
		RepoID: h.repo().ID, NodeID: "PR_closed_" + strconv.Itoa(n), Number: n,
		URL: "https://github.com/talkable/talkable/pull/" + strconv.Itoa(n), HeadSHA: "c" + strconv.Itoa(n),
		AuthorLogin: new("alice"), AuthorType: new("User"), GHState: store.GHMerged, MergedAt: &closed, ClosedAt: &closed,
		InitialState: store.PRReviewed, Identity: "talkable-app",
	})
	if err != nil {
		h.t.Fatal(err)
	}
	ws := "w" + strconv.Itoa(n)
	if _, err := h.st.CreateSession(h.ctx, store.Session{PRID: up.PR.ID, Role: store.RoleClaude, SessionID: new("conv-" + ws),
		HerdrWorkspaceID: &ws, HerdrPaneID: new(ws + ":p1"), State: store.SessionLost}); err != nil {
		h.t.Fatal(err)
	}
	return up.PR
}

// moveAgents is fakeAgents whose Park refuses the PRs refused names, with
// their errors, as agents.Manager.Park refuses while an agent works or a
// pane of a workspace it would close is busy. For each park it lets
// through, it records whether the PR's sessions still recorded their herdr
// ids and the registry still named your session then: a move parks through
// the old server before it forgets them.
type moveAgents struct {
	*fakeAgents
	mu      sync.Mutex
	refused map[int64]error
	before  map[int64]bool
}

func withMoveAgents(out **moveAgents) func(*harness) {
	return func(h *harness) {
		*out = &moveAgents{fakeAgents: h.ag, refused: map[int64]error{}, before: map[int64]bool{}}
		h.d.Agents = *out
	}
}

func (m *moveAgents) refuse(prID int64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refused[prID] = err
}

func (m *moveAgents) Park(ctx context.Context, pr store.PR) error {
	m.mu.Lock()
	err := m.refused[pr.ID]
	m.mu.Unlock()
	if err != nil {
		m.record(fmt.Sprintf("park_refused:%d", pr.ID))
		return err
	}
	ids, _ := m.st.PRsWithHerdrIDs(ctx)
	rec, _, _ := m.st.GetKV(ctx, store.KVHerdrPanes)
	m.mu.Lock()
	m.before[pr.ID] = slices.Contains(ids, pr.ID) && strings.Contains(rec, yourPanes.Socket)
	m.mu.Unlock()
	return m.fakeAgents.Park(ctx, pr)
}

// since forgets the parks seen so far: the move's are those after it.
func (m *moveAgents) since() {
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.before)
}

func (m *moveAgents) parkedBefore(prID int64) (seen, before bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	before, seen = m.before[prID]
	return seen, before
}

// A daemon records, at its start, the herdr session the registry's herdr
// ids belong to when nothing does yet: the one its App opens the panes in,
// your session, where every build before the record opened them. A config
// that names another session leaves the record to the move; a renamed
// session of yours on the same server only renames it, since the reveals
// use the name. The daemon's wiring hands the engine the App's session; a
// tagged App's engine (a replay, the factory's local review), on a scratch
// registry, gets none, and so records and moves nothing.
func TestTheStartRecordsWhereTheRegistrysPanesAre(t *testing.T) {
	h := newHarness(t, withPanesInYours)
	if _, ok := h.panesRecord(); ok {
		t.Fatal("a record before any start")
	}
	h.startup()
	if got, ok := h.panesRecord(); !ok || got != yourPanes {
		t.Fatalf("the start recorded %+v, %v; want your session %+v", got, ok, yourPanes)
	}

	h.moveToReviews()
	h.startup()
	if got, _ := h.panesRecord(); got != yourPanes {
		t.Fatalf("a start on a config naming session reviews recorded %+v before any move", got)
	}

	h.cfg.Herdr.ReviewSession, h.cfg.Herdr.ReviewSocket = "", ""
	h.cfg.Terminal.Session = "work"
	h.startup()
	if got, _ := h.panesRecord(); got != (herdr.Session{Name: "work", Socket: yourPanes.Socket}) {
		t.Fatalf("your session renamed on the same server: the record is %+v, want it named work", got)
	}

	e, a, _ := appEngine(t, nil, app.Options{DryRun: true}, nil)
	if e.d.PaneSession != a.PaneSession || e.d.PaneSession.Socket == "" {
		t.Fatalf("FromApp's PaneSession = %+v, want the App's %+v", e.d.PaneSession, a.PaneSession)
	}
	if e, _, _ := appEngine(t, nil, app.Options{DryRun: true, AgentTag: "eval-1"}, nil); e.d.PaneSession != (herdr.Session{}) {
		t.Fatalf("a tagged engine's PaneSession = %+v, want none", e.d.PaneSession)
	}
}

// With the registry's panes in the session the config names (your session,
// as before the key, or session reviews after its move, whatever the
// socket's spelling), nothing moves: rounds start, nothing parks, the
// daemon does not restart. A `magnum daemon --once` never moves them
// either: it has no launchd to start it again.
func TestNoMoveWhenThePanesAreWhereTheConfigSays(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*harness)
	}{
		{"your session", func(*harness) {}},
		{"session reviews", func(h *harness) {
			h.moveToReviews()
			h.cfg.Herdr.ReviewSocket = "/run/herdr/sessions/../sessions/reviews/herdr.sock"
			h.d.PaneSession = reviewsPanes
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withPanesInYours, tc.mod)
			pr := h.reviewedPR(2, "b1")
			parks := h.ag.count("park")
			h.push(2, "b2")
			h.reviewTo(2, "b2") // every tick fails the test on an error, ErrRestartForPanes included
			if n := h.ag.count("park") - parks; n != 0 || !slices.Contains(h.herdrIDPRs(), pr.ID) {
				t.Fatalf("%d parks, PRs with herdr ids %v: the panes moved", n, h.herdrIDPRs())
			}
			if why := h.e.holdReason(h.ctx); why != "" {
				t.Fatalf("hold %q", why)
			}
		})
	}

	t.Run("--once", func(t *testing.T) {
		h := newHarness(t, withPanesInYours)
		pr := h.reviewedPR(2, "b1")
		parks := h.ag.count("park")
		h.moveToReviews()
		if err := h.e.Run(h.ctx, Options{Once: true, NoSignals: true}); err != nil {
			t.Fatalf("--once = %v", err)
		}
		if got, _ := h.panesRecord(); got != yourPanes || !slices.Contains(h.herdrIDPRs(), pr.ID) || h.ag.count("park") != parks {
			t.Fatalf("--once moved the panes: record %+v, PRs with herdr ids %v, calls %v", got, h.herdrIDPRs(), h.ag.all())
		}
	})
}

// While a move waits, the daemon holds what would open panes in the old
// session, as a drain does: no round starts though a slot is free (the
// PR's wait says why), a `magnum retro` is queued for after the restart
// and does not start meanwhile, and `magnum open` of a PR with nothing
// live asks to try again in a minute. Once the move can park every PR, it
// goes on and restarts; no round or retro started meanwhile. Without the
// move, the same PR takes the free slot and the retro starts at once.
func TestAMoveHoldsNewRoundsAndRetros(t *testing.T) {
	for _, move := range []bool{false, true} {
		t.Run(map[bool]string{false: "no move", true: "a move"}[move], func(t *testing.T) {
			var ma *moveAgents
			h := newHarness(t, withPanesInYours, withMoveAgents(&ma), func(h *harness) { h.cfg.Pools[0].Max = 2 })
			pool := h.cfg.Pools[0]
			if _, err := h.st.CreateSlot(h.ctx, store.Slot{Name: pool.Slot(2), RepoFullName: pool.Repo, Kind: store.SlotKindPool,
				Path: pool.Path(2), MainClone: pool.MainClone, PlaceholderBranch: new("review2"), DBSlug: new("review2"),
				State: store.SlotFree}); err != nil {
				t.Fatal(err)
			}
			held := h.reviewedPR(5, "e1")
			ma.refuse(held.ID, fmt.Errorf("agents: park pr 5: codex-judge is working: %w", agents.ErrBusy))
			if move {
				h.moveToReviews()
			}
			rounds := len(h.rd.all())
			h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 5, head: "e1"}, prSpec{n: 2, head: "b1"})
			if err := h.moveTick(); err != nil {
				t.Fatalf("tick: %v", err)
			}
			h.advance(5 * time.Minute)
			if err := h.moveTick(); err != nil {
				t.Fatalf("tick: %v", err)
			}
			if !move {
				h.wantState(2, store.PRReviewed) // the free slot is #2's
				if req := h.requestRetro(RetroPayload{}); req.State != store.RequestDone || !strings.Contains(deref(req.Result), "retro started") {
					t.Fatalf("a retro asked with no move = %s %q, want it started", req.State, deref(req.Result))
				}
				return
			}
			h.wantState(2, store.PRQueued)
			if w, ok := h.waitOf(2); !ok || w.Reason != WaitDraining ||
				!strings.Contains(w.Detail, "moving magnum's panes from herdr session default to reviews") {
				t.Fatalf("#2 waits %+v, %v; want the move's restart", w, ok)
			}

			req := h.requestRetro(RetroPayload{})
			if req.State != store.RequestDone || !strings.Contains(deref(req.Result), drainStarts) {
				t.Fatalf("a retro asked during the move = %s %q, want it queued for after the restart", req.State, deref(req.Result))
			}
			open := h.enqueue(ReqOpen, OpenPayload{PRTarget: PRTarget{Ref: "2"}})
			for range 2 {
				h.advance(5 * time.Minute)
				if err := h.moveTick(); err != nil {
					t.Fatalf("tick: %v", err)
				}
			}
			if r := h.request(open); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "again in a minute") ||
				!strings.Contains(deref(r.Result), "moving magnum's panes from herdr session default to reviews") {
				t.Fatalf("magnum open during the move = %s %q", r.State, deref(r.Result))
			}

			ma.refuse(held.ID, nil)
			if err := h.moveTick(); !errors.Is(err, ErrRestartForPanes) {
				t.Fatalf("tick once every PR parks = %v, want ErrRestartForPanes", err)
			}
			if n := len(h.rd.all()); n != rounds {
				t.Fatalf("%d rounds started during the move", n-rounds)
			}
			h.wantState(2, store.PRQueued)
			if evs := append(h.events("retro.start"), h.events("retro.done")...); len(evs) != 0 {
				t.Fatalf("a retro ran during the move: %+v", evs)
			}
		})
	}
}

// A move never parks a PR whose round is in flight: it waits for the
// daemon's first tick with nothing in flight, then parks that PR too.
func TestAMoveWaitsForTheRoundsInFlight(t *testing.T) {
	h := newHarness(t, withPanesInYours)
	pr := h.runningRound(2, "h2", false)
	parks := h.ag.count("park")
	h.moveToReviews()
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatalf("tick with a round in flight: %v", err)
	}
	if got, _ := h.panesRecord(); h.ag.count("park") != parks || got != yourPanes || !slices.Contains(h.herdrIDPRs(), pr.ID) {
		t.Fatalf("the move went on with a round in flight: calls %v, record %+v", h.ag.all(), got)
	}

	h.rd.mu.Lock()
	close(h.rd.gate)
	h.rd.gate = nil
	h.rd.mu.Unlock()
	h.settle()
	h.wantState(2, store.PRReviewed)
	calls := len(h.ag.all())
	if err := h.moveTick(); !errors.Is(err, ErrRestartForPanes) {
		t.Fatalf("tick after the round = %v, want ErrRestartForPanes", err)
	}
	if !slices.Contains(h.ag.all()[calls:], "park:"+itoa(pr.ID)) {
		t.Fatalf("the PR's sessions were not parked: %v", h.ag.all())
	}
}

// At the first idle tick the move parks every PR whose sessions record a
// herdr id, through the old session: a live one, a pinned one (its pins
// and slot stay; its conversations resume on the next open) and a closed
// one whose retired pane lingers, then closes their retired panes there,
// forgets every id (no row stays live: each resumes by its conversation),
// records session reviews, toasts once in your session and returns
// ErrRestartForPanes, so launchd starts the daemon there.
func TestAMoveParksEveryPRsSessionsThenRestarts(t *testing.T) {
	var ma *moveAgents
	h := newHarness(t, withPanesInYours, withMoveAgents(&ma))
	live := h.reviewedPR(2, "b1")
	if err := h.st.UpdatePR(h.ctx, live.ID, func(u *store.PRUpdate) { u.Set("pinned", true) }); err != nil {
		t.Fatal(err)
	}
	old := h.closedPRWithPanes(7)
	slotCalls, calls := len(h.sl.all()), len(h.ag.all())
	ma.since()
	h.moveToReviews()

	if err := h.moveTick(); !errors.Is(err, ErrRestartForPanes) {
		t.Fatalf("tick = %v, want ErrRestartForPanes", err)
	}
	for _, pr := range []store.PR{live, old} {
		if seen, before := ma.parkedBefore(pr.ID); !seen || !before {
			t.Errorf("PR %d parked %v, before its ids were forgotten %v: %v", pr.Number, seen, before, h.ag.all()[calls:])
		}
		if !slices.Contains(h.ag.all()[calls:], "close_panes:"+itoa(pr.ID)) {
			t.Errorf("PR %d's retired panes were not closed: %v", pr.Number, h.ag.all())
		}
	}
	if ids, n := h.herdrIDPRs(), h.liveCount(); len(ids) != 0 || n != 0 {
		t.Fatalf("after the move: PRs with herdr ids %v, %d live sessions", ids, n)
	}
	if got, ok := h.panesRecord(); !ok || got != reviewsPanes {
		t.Fatalf("record = %+v, %v; want session reviews", got, ok)
	}
	if !h.pr(2).Pinned || len(h.sl.all()) != slotCalls {
		t.Fatalf("the pinned PR: pinned %v, slot calls %v", h.pr(2).Pinned, h.sl.all()[slotCalls:])
	}
	if begin, done := h.events("herdr.panes_move_begin"), h.events("herdr.panes_moved"); len(begin) != 1 || len(done) != 1 ||
		!strings.Contains(done[0].Message, "herdr session reviews") {
		t.Fatalf("events begin %+v, moved %+v", begin, done)
	}
	if got := h.nh.titles("herdr session reviews"); len(got) != 1 {
		t.Fatalf("toasts in your session %v, want one", got)
	}
	if _, waits := ReadPanesMove(h.ctx, h.st); waits {
		t.Fatal("a done move is still recorded as waiting")
	}
}

// An old session with no server running (herdr.NoServer: its socket is
// gone, or stale and refusing) still has magnum's panes: herdr restores a
// session's workspaces when its server starts again and resumes their
// agents in their conversations ([session] resume_agents_on_restore). Ids
// forgotten then would leave those agents to nobody, and a round resuming
// one of the conversations in the new session would run it twice. So the
// move parks and forgets nothing while that server is not running, or goes
// away during the parks: it waits (kv herdr.panes_move, one event, a toast
// after 30 minutes) and names how to start the session; a named one the
// daemon opens itself (Deps.OpenHerdr, at most twice an hour), yours it
// never opens. Once the server answers, the move parks what it restored and
// goes on. With no herdr id recorded nothing can be restored, and the move
// does not wait for the server.
func TestAMoveFromASessionWithNoServerWaitsForItsServer(t *testing.T) {
	noServer := func(s herdr.Session) error {
		return fmt.Errorf("herdr session.snapshot: %w: dial unix %s: %w", herdr.ErrUnavailable, s.Socket, syscall.ECONNREFUSED)
	}
	for _, tc := range []struct {
		name        string
		named       bool // the panes move back from session reviews to yours
		duringParks bool // the snapshot answers, then a park finds no server
		start       string
	}{
		{"your session", false, false, "`herdr`"},
		{"a named session", true, false, "`herdr --session reviews`"},
		{"gone during the parks", false, true, "`herdr`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ma *moveAgents
			var opened atomic.Int32
			h := newHarness(t, withPanesInYours, withMoveAgents(&ma), func(h *harness) {
				if tc.named {
					h.d.PaneSession = reviewsPanes
					h.moveToReviews()
					h.d.OpenHerdr = func(context.Context) error { opened.Add(1); return nil }
				}
			})
			pr := h.reviewedPR(2, "b1")
			from, to := yourPanes, reviewsPanes
			if tc.named {
				from, to = reviewsPanes, yourPanes
				h.cfg.Herdr.ReviewSession, h.cfg.Herdr.ReviewSocket = "", ""
			} else {
				h.moveToReviews()
			}
			if tc.duringParks {
				ma.refuse(pr.ID, fmt.Errorf("agents: park pr 2: %w", noServer(from)))
			} else {
				h.hd.mu.Lock()
				h.hd.err = noServer(from)
				h.hd.mu.Unlock()
			}
			calls := len(h.ag.all())

			if err := h.moveTick(); err != nil {
				t.Fatalf("tick = %v, want the move to wait for herdr session %s", err, from.Name)
			}
			if moved := h.ag.all()[calls:]; slices.ContainsFunc(moved, func(c string) bool {
				return strings.HasPrefix(c, "park:") || strings.HasPrefix(c, "close_panes")
			}) {
				t.Fatalf("parked or closed with no server: %v", moved)
			}
			if got, _ := h.panesRecord(); got != from || !slices.Contains(h.herdrIDPRs(), pr.ID) {
				t.Fatalf("the move went on: record %+v, PRs with ids %v", got, h.herdrIDPRs())
			}
			mv, ok := ReadPanesMove(h.ctx, h.st)
			if !ok || mv.From != from || mv.To != to || mv.Subject != "" ||
				!strings.Contains(mv.Reason, "no herdr server runs in session "+from.Name) || !strings.Contains(mv.Reason, tc.start) {
				t.Fatalf("the wait = %+v, %v", mv, ok)
			}
			h.advance(10 * time.Minute)
			if err := h.moveTick(); err != nil {
				t.Fatal(err)
			}
			h.advance(21 * time.Minute)
			if err := h.moveTick(); err != nil {
				t.Fatal(err)
			}
			if evs := h.events("herdr.panes_move_waits"); len(evs) != 1 {
				t.Fatalf("waits events %+v", evs)
			}
			if got := h.nh.titles("wait to move to herdr session " + to.Name); len(got) != 1 {
				t.Fatalf("toasts after 31 minutes %v, want one", got)
			}
			if want := map[bool]int32{false: 0, true: 2}[tc.named]; opened.Load() != want {
				t.Fatalf("herdr session %s opened %d times, want %d", from.Name, opened.Load(), want)
			}

			h.hd.mu.Lock()
			h.hd.err = nil
			h.hd.mu.Unlock()
			ma.refuse(pr.ID, nil)
			calls = len(h.ag.all())
			if err := h.moveTick(); !errors.Is(err, ErrRestartForPanes) {
				t.Fatalf("tick once the server answers = %v, want ErrRestartForPanes", err)
			}
			if got, _ := h.panesRecord(); got != to || !slices.Contains(h.ag.all()[calls:], "park:"+itoa(pr.ID)) {
				t.Fatalf("record %+v, calls %v", got, h.ag.all()[calls:])
			}
		})
	}

	t.Run("no herdr id recorded", func(t *testing.T) {
		h := newHarness(t, withPanesInYours)
		h.reviewedPR(2, "b1")
		if _, err := h.st.ForgetHerdrIDs(h.ctx); err != nil {
			t.Fatal(err)
		}
		h.hd.mu.Lock()
		h.hd.err = noServer(yourPanes)
		h.hd.mu.Unlock()
		h.moveToReviews()
		if err := h.moveTick(); !errors.Is(err, ErrRestartForPanes) {
			t.Fatalf("tick = %v, want ErrRestartForPanes", err)
		}
		if got, _ := h.panesRecord(); got != reviewsPanes {
			t.Fatalf("record %+v", got)
		}
	})
}

// When the old session's server starts again, herdr restores its
// workspaces under new ids and resumes their agents in their conversations,
// an agent without the name magnum gave it among them; the daemon's start
// found no server and so rebound nothing, and observe finds no agent by that
// name, so the row is lost. The move rebinds each PR's sessions to the
// panes that hold their conversations (agents.Manager.Recover) before it
// parks them: the restored agent is parked and its workspace closed in the
// old session, never left to resume its conversation beside the new
// session's round.
func TestAMoveParksAnAgentTheOldServerRestoredByItsConversation(t *testing.T) {
	eh := newEvalHerdr()
	h := newHarness(t, withPanesInYours, func(hs *harness) {
		hs.d.Herdr = eh
		run := &execx.Fake{Rules: []execx.Rule{
			{Prefix: []string{"codex", "login", "status"}, Result: execx.Result{Stdout: []byte("Logged in using ChatGPT\n")}},
			{Prefix: []string{"claude", "auth", "status"}, Result: execx.Result{Stdout: []byte(`{"loggedIn": true}`)}},
		}}
		cli := t.TempDir()
		hs.d.Agents = agents.New(agents.Deps{Herdr: eh, Store: hs.st, Runner: run, Config: hs.cfg, Layout: hs.layout,
			Clock: hs.clock.Now, CodexConfig: cli + "/codex.toml", ClaudeConfig: cli + "/claude.json",
			ClaudeSettings: cli + "/settings.json", ClaudeDir: cli, CodexHome: cli,
			Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }})
	})
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick()
	h.closedPRWithPanes(7) // its session conv-w7 recorded in workspace w7, pane w7:p1
	ws := eh.addWorkspace("talkable#7", "/slots/talkable.7")
	conv := &herdr.AgentSession{Agent: "claude", Kind: "id", Source: "herdr:claude", Value: "conv-w7"}
	eh.mu.Lock()
	for i := range eh.panes {
		if eh.panes[i].WorkspaceID == ws {
			eh.panes[i].Agent, eh.panes[i].AgentSession = "claude", conv
		}
	}
	eh.agents = append(eh.agents, herdr.AgentInfo{PaneID: ws + ":p1", WorkspaceID: ws, TabID: ws + ":t1", Agent: "claude",
		AgentStatus: herdr.StatusIdle, AgentSession: conv})
	eh.mu.Unlock()
	h.moveToReviews()

	if err := h.moveTick(); !errors.Is(err, ErrRestartForPanes) {
		t.Fatalf("tick = %v, want ErrRestartForPanes", err)
	}
	if eh.has(ws) {
		t.Fatalf("workspace %s, where herdr restored conversation conv-w7, stays in the old session (closed %v)", ws, eh.closedIDs())
	}
	if got, _ := h.panesRecord(); got != reviewsPanes {
		t.Fatalf("record %+v, want session reviews", got)
	}
}

// A move whose restart is refused (what launchd would start fails its
// check: a binary gone, or rebuilt so that it refuses the config, or a
// config on disk that no longer loads) holds new rounds as any wait of the
// move does, so it says so as they do: kv herdr.panes_move names why for
// `magnum status` and doctor, one herdr.panes_move_waits event per reason,
// and a toast in your session once it has waited 30 minutes, also from a
// tick that only remembers the refusal. Nothing is parked or forgotten:
// the daemon could not start again in the new session.
func TestAMoveWhoseRestartIsRefusedWaitsAndSaysWhy(t *testing.T) {
	h := newHarness(t, withPanesInYours)
	h.e.supervised = true
	pr := h.reviewedPR(2, "b1")
	bin := filepath.Join(t.TempDir(), "magnum")
	if err := os.WriteFile(bin, []byte("v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.e.build = CurrentBuild("v1", bin)
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	parks := h.ag.count("park")
	h.moveToReviews()

	for _, step := range []time.Duration{0, 25 * time.Minute, 6 * time.Minute} { // the last within restartRefuseRetry
		h.advance(step)
		if err := h.moveTick(); err != nil {
			t.Fatalf("tick = %v, want the move to wait", err)
		}
	}
	if got, _ := h.panesRecord(); h.ag.count("park") != parks || got != yourPanes || !slices.Contains(h.herdrIDPRs(), pr.ID) {
		t.Fatalf("the move went on: calls %v, record %+v", h.ag.all(), got)
	}
	mv, ok := ReadPanesMove(h.ctx, h.st)
	if !ok || mv.From != yourPanes || mv.To != reviewsPanes || mv.Subject != "" ||
		!strings.Contains(mv.Reason, "fails its check") || !strings.Contains(mv.Reason, "missing or cannot be read") {
		t.Fatalf("the wait = %+v, %v", mv, ok)
	}
	if evs := h.events("herdr.panes_move_waits"); len(evs) != 1 || evs[0].Level != "warn" ||
		!strings.Contains(evs[0].Message, "missing or cannot be read") {
		t.Fatalf("waits events %+v", evs)
	}
	if got := h.nh.titles("wait to move to herdr session reviews"); len(got) != 1 {
		t.Fatalf("toasts after 31 minutes %v, want one", got)
	}
}

// An old session that does not answer (a timeout, or a server that holds
// its socket but accepts no connections) may have panes at work: the move
// parks nothing, forgets nothing, says why it waits, and goes on once the
// server answers.
func TestAMoveFromAServerThatTimesOutWaits(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"timeout", fmt.Errorf("herdr session.snapshot: %w: context deadline exceeded", herdr.ErrTimeout)},
		{"no accept", fmt.Errorf("herdr session.snapshot: %w: a server holds the socket but accepts no connections (dial unix: connection refused)",
			herdr.ErrUnavailable)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withPanesInYours)
			pr := h.reviewedPR(2, "b1")
			parks := h.ag.count("park")
			h.hd.mu.Lock()
			h.hd.err = tc.err
			h.hd.mu.Unlock()
			h.moveToReviews()
			if err := h.moveTick(); err != nil {
				t.Fatalf("tick = %v, want the move to wait", err)
			}
			if got, _ := h.panesRecord(); h.ag.count("park") != parks || got != yourPanes || !slices.Contains(h.herdrIDPRs(), pr.ID) {
				t.Fatalf("the move went on: calls %v, record %+v, PRs with ids %v", h.ag.all(), got, h.herdrIDPRs())
			}
			mv, ok := ReadPanesMove(h.ctx, h.st)
			if !ok || mv.From != yourPanes || mv.To != reviewsPanes || mv.Subject != "" ||
				!strings.Contains(mv.Reason, "herdr session default does not answer") {
				t.Fatalf("the wait = %+v, %v", mv, ok)
			}
			if evs := h.events("herdr.panes_move_waits"); len(evs) != 1 || evs[0].Level != "warn" {
				t.Fatalf("waits events %+v", evs)
			}

			h.hd.mu.Lock()
			h.hd.err = nil
			h.hd.mu.Unlock()
			calls := len(h.ag.all())
			if err := h.moveTick(); !errors.Is(err, ErrRestartForPanes) {
				t.Fatalf("tick once the server answers = %v, want ErrRestartForPanes", err)
			}
			if !slices.Contains(h.ag.all()[calls:], "park:"+itoa(pr.ID)) {
				t.Fatalf("not parked: %v", h.ag.all())
			}
		})
	}
}

// A PR whose park is refused (an agent at work, a busy pane of someone's
// in a magnum workspace) holds the move: the ids stay, `magnum status`
// can name the PR and why (KVHerdrPanesMove), one event says so per
// reason, and a toast in your session does once the move has waited 30
// minutes. Every idle tick tries again; once the park goes through, the
// move ends and the wait's record goes.
func TestAMoveThatCannotParkAPRWaitsAndSaysWhy(t *testing.T) {
	var ma *moveAgents
	h := newHarness(t, withPanesInYours, withMoveAgents(&ma))
	pr := h.reviewedPR(2, "b1")
	ma.refuse(pr.ID, fmt.Errorf("agents: park pr 2: pane w1:p9 runs make: %w", agents.ErrBusy))
	h.moveToReviews()

	if err := h.moveTick(); err != nil {
		t.Fatalf("tick = %v, want the move to wait", err)
	}
	if got, _ := h.panesRecord(); got != yourPanes || !slices.Contains(h.herdrIDPRs(), pr.ID) {
		t.Fatalf("the move went on: record %+v, PRs with ids %v", got, h.herdrIDPRs())
	}
	mv, ok := ReadPanesMove(h.ctx, h.st)
	if !ok || mv.Subject != "pr:talkable/talkable#2" || !strings.Contains(mv.Reason, "pane w1:p9 runs make") ||
		mv.From != yourPanes || mv.To != reviewsPanes {
		t.Fatalf("the wait = %+v, %v", mv, ok)
	}
	waits := func() []store.Event { return h.events("herdr.panes_move_waits") }
	if evs := waits(); len(evs) != 1 || !strings.Contains(evs[0].Message, "talkable/talkable#2") ||
		!strings.Contains(evs[0].Message, "pane w1:p9 runs make") {
		t.Fatalf("waits events %+v", evs)
	}
	toasts := func() []string { return h.nh.titles("herdr session reviews") }

	h.advance(10 * time.Minute)
	if err := h.moveTick(); err != nil {
		t.Fatal(err)
	}
	if n, got := len(waits()), toasts(); n != 1 || len(got) != 0 {
		t.Fatalf("after 10 minutes: %d events, toasts %v", n, got)
	}
	h.advance(21 * time.Minute)
	if err := h.moveTick(); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Minute)
	if err := h.moveTick(); err != nil {
		t.Fatal(err)
	}
	if got := toasts(); len(got) != 1 {
		t.Fatalf("after 32 minutes: toasts %v, want one", got)
	}
	if n := ma.count("park_refused:"); n != 4 {
		t.Fatalf("%d tries, want one per idle tick", n)
	}

	ma.refuse(pr.ID, nil)
	if err := h.moveTick(); !errors.Is(err, ErrRestartForPanes) {
		t.Fatalf("tick once the park goes through = %v, want ErrRestartForPanes", err)
	}
	if _, waiting := ReadPanesMove(h.ctx, h.st); waiting {
		t.Fatal("the wait outlived the move")
	}
}

// A move cut short (the daemon stopped or crashed mid-way) leaves the
// record of the old session, which the next start's App opens its panes
// in again: that start moves them anew, the steps done already doing
// nothing twice, whether it stopped during the parks or after it forgot
// the ids.
func TestAMoveCutShortGoesOnAtTheNextStart(t *testing.T) {
	t.Run("during the parks", func(t *testing.T) {
		h := newHarness(t, withPanesInYours)
		pr := h.reviewedPR(2, "b1")
		h.closedPRWithPanes(7)
		h.moveToReviews()
		h.ag.mu.Lock()
		h.ag.parkGate, h.ag.parkStarted = make(chan struct{}), make(chan struct{}, 1)
		h.ag.mu.Unlock()
		ctx, cancel := context.WithCancel(h.ctx)
		done := make(chan error, 1)
		go func() { done <- h.e.Tick(ctx) }()
		select {
		case <-h.ag.parkStarted:
		case <-time.After(10 * time.Second):
			t.Fatal("the move never parked")
		}
		cancel()
		if err := <-done; errors.Is(err, ErrRestartForPanes) {
			t.Fatal("a stopped move restarted for the panes")
		}
		if got, _ := h.panesRecord(); got != yourPanes || len(h.herdrIDPRs()) != 2 {
			t.Fatalf("a move cut short changed the record (%+v) or forgot ids (PRs %v)", got, h.herdrIDPRs())
		}

		h.ag.mu.Lock()
		h.ag.parkGate, h.ag.parkStarted = nil, nil
		h.ag.mu.Unlock()
		h.e = New(h.d)
		h.startup()
		calls := len(h.ag.all())
		if err := h.moveTick(); !errors.Is(err, ErrRestartForPanes) {
			t.Fatalf("the next start's tick = %v, want ErrRestartForPanes", err)
		}
		if got, _ := h.panesRecord(); got != reviewsPanes || len(h.herdrIDPRs()) != 0 || !slices.Contains(h.ag.all()[calls:], "park:"+itoa(pr.ID)) {
			t.Fatalf("record %+v, PRs with ids %v, calls %v", got, h.herdrIDPRs(), h.ag.all())
		}
	})

	t.Run("after the ids were forgotten", func(t *testing.T) {
		h := newHarness(t, withPanesInYours)
		h.reviewedPR(2, "b1")
		h.moveToReviews()
		if _, err := h.st.ForgetHerdrIDs(h.ctx); err != nil {
			t.Fatal(err)
		}
		h.e = New(h.d)
		h.startup()
		parks := h.ag.count("park")
		if err := h.moveTick(); !errors.Is(err, ErrRestartForPanes) {
			t.Fatalf("the next start's tick = %v, want ErrRestartForPanes", err)
		}
		if got, _ := h.panesRecord(); got != reviewsPanes || h.ag.count("park") != parks {
			t.Fatalf("record %+v, calls %v", got, h.ag.all())
		}
	})
}

// The daemon's loop exits with ErrRestartForPanes once the move is done
// (the exit is non-zero, so launchd starts it again, in the new session).
func TestTheRunLoopExitsForAMoveOfThePanes(t *testing.T) {
	h := newHarness(t, withPanesInYours)
	pr := h.reviewedPR(2, "b1")
	calls := len(h.ag.all())
	h.moveToReviews()
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	done := h.goRun(ctx, Options{NoSignals: true})
	for deadline := time.Now().Add(20 * time.Second); ; {
		select {
		case err := <-done:
			if !errors.Is(err, ErrRestartForPanes) {
				t.Fatalf("Run = %v, want ErrRestartForPanes", err)
			}
			if got, _ := h.panesRecord(); got != reviewsPanes || !slices.Contains(h.ag.all()[calls:], "park:"+itoa(pr.ID)) {
				t.Fatalf("record %+v, calls %v", got, h.ag.all())
			}
			return
		case <-time.After(20 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("Run never exited for the move")
			}
			h.e.Kick()
		}
	}
}
