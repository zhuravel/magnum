package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/app"
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
// session, as a drain does: no round starts (the PR's wait says why), a
// `magnum retro` is queued for after the restart, and `magnum open` of a
// PR with nothing live asks to try again in a minute. Once the move can
// park every PR, it goes on and restarts; no round started meanwhile.
func TestAMoveHoldsNewRoundsAndRetros(t *testing.T) {
	var ma *moveAgents
	h := newHarness(t, withPanesInYours, withMoveAgents(&ma))
	held := h.reviewedPR(5, "e1")
	ma.refuse(held.ID, fmt.Errorf("agents: park pr 5: codex-judge is working: %w", agents.ErrBusy))
	h.moveToReviews()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 5, head: "e1"}, prSpec{n: 2, head: "b1"})
	if err := h.moveTick(); err != nil {
		t.Fatalf("tick: %v", err)
	}
	h.advance(5 * time.Minute)
	if err := h.moveTick(); err != nil {
		t.Fatalf("tick: %v", err)
	}
	h.wantState(2, store.PRQueued)
	rounds := len(h.rd.all())
	if w, ok := h.waitOf(2); !ok || w.Reason != WaitDraining ||
		!strings.Contains(w.Detail, "moving magnum's panes from herdr session default to reviews") {
		t.Fatalf("#2 waits %+v, %v; want the move's restart", w, ok)
	}

	req := h.requestRetro(RetroPayload{})
	if req.State != store.RequestDone || !strings.Contains(deref(req.Result), drainStarts) {
		t.Fatalf("a retro asked during the move = %s %q, want it queued for after the restart", req.State, deref(req.Result))
	}
	open := h.enqueue(ReqOpen, OpenPayload{PRTarget: PRTarget{Ref: "2"}})
	if err := h.moveTick(); err != nil {
		t.Fatalf("tick: %v", err)
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
// gone, or stale and refusing) has no panes: the move forgets the ids
// without parking anything, the live rows lost, and restarts.
func TestAMoveFromAServerThatIsGoneForgetsTheIdsWithoutParking(t *testing.T) {
	h := newHarness(t, withPanesInYours)
	pr := h.reviewedPR(2, "b1")
	calls := len(h.ag.all())
	h.hd.mu.Lock()
	h.hd.err = fmt.Errorf("herdr session.snapshot: %w: dial unix %s: %w", herdr.ErrUnavailable, yourPanes.Socket, syscall.ECONNREFUSED)
	h.hd.mu.Unlock()
	h.moveToReviews()

	if err := h.moveTick(); !errors.Is(err, ErrRestartForPanes) {
		t.Fatalf("tick = %v, want ErrRestartForPanes", err)
	}
	if moved := h.ag.all()[calls:]; slices.ContainsFunc(moved, func(c string) bool {
		return strings.HasPrefix(c, "park") || strings.HasPrefix(c, "close_panes")
	}) {
		t.Fatalf("a gone server's PRs were parked or closed: %v", moved)
	}
	if ids, n := h.herdrIDPRs(), h.liveCount(); len(ids) != 0 || n != 0 {
		t.Fatalf("PRs with herdr ids %v, %d live sessions", ids, n)
	}
	ss, err := h.st.SessionsByPR(h.ctx, pr.ID)
	if err != nil || len(ss) == 0 || slices.ContainsFunc(ss, func(s store.Session) bool { return s.State != store.SessionLost }) {
		t.Fatalf("PR 2's sessions = %+v, %v; want them lost", ss, err)
	}
	if got, _ := h.panesRecord(); got != reviewsPanes {
		t.Fatalf("record = %+v", got)
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
