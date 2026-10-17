package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// fixPushed3 is the head the fixer pushes in its third cycle.
const fixPushed3 = "d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4"

// fixerPane is what h's fake fixer pane shows as its context: figure percent
// used (ok false: no figure), compacted to after by a /compact (-1: the
// compaction changes nothing).
type fixerPane struct {
	mu     sync.Mutex
	figure int
	ok     bool
	after  int
}

// fixerContext makes h's fake fixer pane show figure (ok false: none) and a
// /compact take it to after (-1: to nothing new).
func fixerContext(h *harness, figure int, ok bool, after int) *fixerPane {
	p := &fixerPane{figure: figure, ok: ok, after: after}
	h.ag.mu.Lock()
	defer h.ag.mu.Unlock()
	h.ag.contextUsed = func(store.Session) (int, bool) {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.figure, p.ok
	}
	h.ag.onCompact = func(store.Session) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.after >= 0 {
			p.figure = p.after
		}
	}
	return p
}

// fixEvents are PR #2's events of kind, oldest first.
func (h *harness) fixEvents(kind string) []store.Event {
	h.t.Helper()
	return kindOf(subjectEvents(h.t, h, "pr:talkable/talkable#2"), kind)
}

// callIndex is the index of h's first fake agents call with prefix after
// from (-1: none).
func (h *harness) callIndex(prefix string, from int) int {
	calls := h.ag.all()
	for i := max(from, 0); i < len(calls); i++ {
		if strings.HasPrefix(calls[i], prefix) {
			return i
		}
	}
	return -1
}

// toCycle2 runs a loop whose fixer pushes in cycles 1 and 2 to the start of
// cycle 2: the re-review of the first push finds a P2, so cycle 2 resumes
// the fixer of cycle 1, whose session is still live in its pane.
func toCycle2(t *testing.T, h *harness, git *fixGit) store.PR {
	t.Helper()
	pr := loopToReReview(t, h, git, findings(map[string]int{"P2": 1}, "COMMENTED"), fixPushed2, fixPushed3)
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
	if st, ok := h.fixStateOf(2); !ok || st.Cycle != 2 {
		t.Fatalf("fix state after the re-review = %+v, %v", st, ok)
	}
	return pr
}

// Before a cycle that resumes the fixer, a context at or above
// fix_compact_at (70) is compacted first: the pane shows 82%, the cycle
// sends /compact, waits for it, reads 31%, records fix.compacted from 82%
// to 31% and the loop's row says so, and the fix prompt follows in the same
// session.
func TestTheFixerCompactsBeforeACycleWhenItsContextIsHigh(t *testing.T) {
	h, git := fixHarness(t)
	fixerContext(h, 82, true, 31)
	pr := toCycle2(t, h, git)

	compact := h.callIndex(fmt.Sprintf("compact:%d:claude-fix", pr.ID), 0)
	runs := h.fixRuns(pr.ID)
	if compact < 0 || len(runs) != 2 {
		t.Fatalf("no /compact before cycle 2 (fix runs %d): %v", len(runs), h.ag.all())
	}
	if submit := h.callIndex("submit:claude-fix:fix:"+runs[1].ID, compact); submit < 0 {
		t.Errorf("cycle 2's prompt does not follow the compaction: %v", h.ag.all())
	}
	if h.ag.count(fmt.Sprintf("start:%d:claude-fix:", pr.ID)) != 1 || h.ag.count("quit:") != 0 {
		t.Errorf("the compacted fixer was started anew: %v", h.ag.all())
	}
	evs := h.fixEvents("fix.compacted")
	if len(evs) != 1 || evs[0].Level != "info" || !strings.Contains(evs[0].Message, "82% to 31%") || !strings.Contains(evs[0].Message, "cycle 2") {
		t.Fatalf("fix.compacted = %+v", evs)
	}
	if d := eventData(t, evs[0]); d["from"] != float64(82) || d["to"] != float64(31) || d["at"] != float64(70) || d["role"] != "claude-fix" || d["cycle"] != float64(2) {
		t.Errorf("fix.compacted data = %v", d)
	}
	if len(h.fixEvents("fix.fresh_session")) != 0 || len(h.fixEvents("fix.context_unknown")) != 0 {
		t.Error("a compaction that worked also started the fixer fresh or warned")
	}
	if st, _ := h.fixStateOf(2); st.Context != "compacted 82% → 31%" {
		t.Errorf("the loop's row says %q of the cycle's context", st.Context)
	}
}

// A fixer still at or above fix_compact_at after its /compact is parked and
// started fresh for the cycle: fix.fresh_session says why, the cycle's run
// is the fresh session's, and its prompt, which carries the findings, is
// that session's first turn.
func TestAFixerStillFullAfterCompactionStartsFresh(t *testing.T) {
	h, git := fixHarness(t)
	fixerContext(h, 82, true, -1)
	pr := toCycle2(t, h, git)

	compact := h.callIndex(fmt.Sprintf("compact:%d:claude-fix", pr.ID), 0)
	quit := h.callIndex(fmt.Sprintf("quit:%d:claude-fix", pr.ID), compact)
	start := h.callIndex(fmt.Sprintf("start:%d:claude-fix:", pr.ID), quit)
	runs := h.fixRuns(pr.ID)
	if compact < 0 || quit < 0 || start < 0 || len(runs) != 2 {
		t.Fatalf("compact %d, quit %d, fresh start %d, fix runs %d: %v", compact, quit, start, len(runs), h.ag.all())
	}
	if h.callIndex("submit:claude-fix:fix:"+runs[1].ID, start) < 0 {
		t.Errorf("cycle 2's prompt does not go to the fresh session: %v", h.ag.all())
	}
	live, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, "claude-fix")
	if err != nil {
		t.Fatal(err)
	}
	if runs[1].SessionID == nil || *runs[1].SessionID != live.ID || (runs[0].SessionID != nil && *runs[0].SessionID == live.ID) {
		t.Errorf("cycle 2's run is session %v, the live one %d, cycle 1's %v", runs[1].SessionID, live.ID, runs[0].SessionID)
	}
	evs := h.fixEvents("fix.fresh_session")
	if len(evs) != 1 || evs[0].Level != "info" || !strings.Contains(evs[0].Message, "82% after /compact") {
		t.Fatalf("fix.fresh_session = %+v", evs)
	}
	if d := eventData(t, evs[0]); d["from"] != float64(82) || d["to"] != float64(82) || d["role"] != "claude-fix" {
		t.Errorf("fix.fresh_session data = %v", d)
	}
	if len(h.fixEvents("fix.compacted")) != 0 {
		t.Error("a compaction that left 82% is recorded as compacted")
	}
	if st, _ := h.fixStateOf(2); st.Context != "fresh session (82% after /compact)" {
		t.Errorf("the loop's row says %q of the cycle's context", st.Context)
	}
}

// A fixer below fix_compact_at is resumed as it is: its figure is read,
// nothing is sent, nothing recorded. With fix_compact_at = 0 nothing is even
// read, whatever the pane shows.
func TestAFixerBelowTheKeyIsNotCompacted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		at     int
		figure int
		reads  int
	}{
		{"41% under 70", 70, 41, 1},
		{"69% under 70", 70, 69, 1},
		{"95% with the key off", 0, 95, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, git := fixHarness(t, func(h *harness) { h.cfg.Pipeline.FixCompactAt = tc.at })
			fixerContext(h, tc.figure, true, 10)
			pr := toCycle2(t, h, git)
			if n := h.ag.count("context:"); n != tc.reads {
				t.Errorf("context reads = %d, want %d", n, tc.reads)
			}
			if h.ag.count("compact:") != 0 || h.ag.count("quit:") != 0 || h.ag.count(fmt.Sprintf("start:%d:claude-fix:", pr.ID)) != 1 {
				t.Errorf("the fixer was compacted or started anew: %v", h.ag.all())
			}
			for _, kind := range []string{"fix.compacted", "fix.fresh_session", "fix.context_unknown"} {
				if evs := h.fixEvents(kind); len(evs) != 0 {
					t.Errorf("%s = %+v", kind, evs)
				}
			}
			if st, _ := h.fixStateOf(2); st.Context != "" {
				t.Errorf("the loop's row says %q", st.Context)
			}
		})
	}
}

// A cycle that starts the fixer fresh (no session to resume) never compacts
// it nor reads its figure, however full the pane says it is.
func TestAFreshFixerIsNeverCompacted(t *testing.T) {
	h, git := fixHarness(t)
	fixerContext(h, 95, true, 10)
	pr := loopToReReview(t, h, git, findings(map[string]int{"P2": 1}, "COMMENTED"))
	if h.ag.count("context:") != 0 || h.ag.count("compact:") != 0 || h.ag.count("quit:") != 0 {
		t.Errorf("cycle 1's fresh fixer was read or compacted: %v", h.ag.all())
	}
	if n := h.ag.count(fmt.Sprintf("start:%d:claude-fix:", pr.ID)); n != 1 {
		t.Errorf("fixer starts = %d, want 1", n)
	}
	if st, _ := h.fixStateOf(2); st.Context != "" {
		t.Errorf("the loop's row says %q", st.Context)
	}
	if evs := h.fixEvents("fix.compacted"); len(evs) != 0 {
		t.Errorf("fix.compacted = %+v", evs)
	}
}

// A pane that shows no figure although fix_compact_at is on leaves the
// resumed fixer as it is at every cycle (no /compact, no fresh session: a
// figure missing right after a resume would otherwise cost a fresh fixer
// each cycle), and warns fix.context_unknown once per PR, then stays silent.
func TestNoContextFigureWarnsOncePerPR(t *testing.T) {
	h, git := fixHarness(t)
	fixerContext(h, 0, false, -1)
	pr := toCycle2(t, h, git)
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: fixPushed2, author: "zhuravel"}) // re-review 2: cycle 3
	if st, ok := h.fixStateOf(2); !ok || st.Cycle != 3 {
		t.Fatalf("fix state after the second re-review = %+v, %v", st, ok)
	}

	if n := h.ag.count("context:"); n != 2 {
		t.Errorf("context reads = %d, want one per resumed cycle (2)", n)
	}
	if n := h.ag.count("compact:"); n != 0 {
		t.Errorf("/compact sent %d times to a pane that shows no figure", n)
	}
	if h.ag.count("quit:") != 0 || h.ag.count(fmt.Sprintf("start:%d:claude-fix:", pr.ID)) != 1 {
		t.Errorf("the fixer was parked or started anew: %v", h.ag.all())
	}
	runs := h.fixRuns(pr.ID)
	if len(runs) != 3 || runs[0].SessionID == nil || runs[2].SessionID == nil || *runs[2].SessionID != *runs[0].SessionID {
		t.Fatalf("fix runs %d: cycle 3 does not go on in cycle 1's session", len(runs))
	}
	warns := h.fixEvents("fix.context_unknown")
	if len(warns) != 1 || warns[0].Level != "warn" || !strings.Contains(warns[0].Message, "context_pattern") ||
		!strings.Contains(warns[0].Message, "resumes it as it is") {
		t.Fatalf("fix.context_unknown = %+v, want one warning", warns)
	}
	for _, kind := range []string{"fix.fresh_session", "fix.compacted"} {
		if evs := h.fixEvents(kind); len(evs) != 0 {
			t.Errorf("%s = %+v", kind, evs)
		}
	}
	if st, _ := h.fixStateOf(2); st.Context != "" {
		t.Errorf("the loop's row says %q", st.Context)
	}
}

// A /compact that did not finish (timed out, or the agent was busy) starts
// the fixer fresh for the cycle, as a figure still at or above the key does.
func TestAFailedCompactionStartsTheFixerFresh(t *testing.T) {
	h, git := fixHarness(t)
	fixerContext(h, 82, true, -1)
	h.ag.mu.Lock()
	h.ag.compactErr = fmt.Errorf("agents: compact claude-fix: not idle again after 10m0s: %w", agents.ErrTimeout)
	h.ag.mu.Unlock()
	pr := toCycle2(t, h, git)

	compact := h.callIndex(fmt.Sprintf("compact:%d:claude-fix", pr.ID), 0)
	quit := h.callIndex(fmt.Sprintf("quit:%d:claude-fix", pr.ID), compact)
	if compact < 0 || quit < 0 || h.callIndex(fmt.Sprintf("start:%d:claude-fix:", pr.ID), quit) < 0 {
		t.Fatalf("a failed compaction did not start the fixer fresh: %v", h.ag.all())
	}
	evs := h.fixEvents("fix.fresh_session")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "/compact did not finish") {
		t.Fatalf("fix.fresh_session = %+v", evs)
	}
	if st, _ := h.fixStateOf(2); st.Context != "fresh session (/compact failed)" {
		t.Errorf("the loop's row says %q", st.Context)
	}
}

// A compaction that finished, after which the pane shows no figure, keeps
// the session: fix.compacted names the figure before it, the row says
// "compacted from 82%", and the missing figure warns once.
func TestAFigureMissingAfterCompactionKeepsTheSession(t *testing.T) {
	h, git := fixHarness(t)
	p := fixerContext(h, 82, true, -1)
	h.ag.mu.Lock()
	h.ag.onCompact = func(store.Session) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.ok = false
	}
	h.ag.mu.Unlock()
	pr := toCycle2(t, h, git)

	if h.ag.count("compact:") != 1 || h.ag.count("quit:") != 0 || h.ag.count(fmt.Sprintf("start:%d:claude-fix:", pr.ID)) != 1 {
		t.Errorf("compacted once and kept the session? %v", h.ag.all())
	}
	evs := h.fixEvents("fix.compacted")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "was at 82%") || !strings.Contains(evs[0].Message, "no figure after /compact") {
		t.Fatalf("fix.compacted = %+v", evs)
	}
	if d := eventData(t, evs[0]); d["from"] != float64(82) || d["to"] != nil {
		t.Errorf("fix.compacted data = %v", d)
	}
	if n := len(h.fixEvents("fix.context_unknown")); n != 1 {
		t.Errorf("fix.context_unknown %d times, want once", n)
	}
	if n := len(h.fixEvents("fix.fresh_session")); n != 0 {
		t.Errorf("fix.fresh_session %d times", n)
	}
	if st, _ := h.fixStateOf(2); st.Context != "compacted from 82%" {
		t.Errorf("the loop's row says %q", st.Context)
	}
}

// fixPushed4 is the head the fixer pushes in its fourth cycle.
const fixPushed4 = "e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5"

// clockSleep makes the engine's sleeps move h's clock by what they wait, at
// once.
func clockSleep(h *harness) {
	h.d.Sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		h.clock.Add(d)
		return nil
	}
}

// paneReads is h's fake fixer pane, which answers its n-th context read
// (from 0) with show(n), recording the fake clock's time at each read.
type paneReads struct {
	mu   sync.Mutex
	at   []time.Time
	show func(n int) (int, bool)
}

// logReads makes h's fake fixer pane answer with show and record its reads.
func logReads(h *harness, show func(n int) (int, bool)) *paneReads {
	p := &paneReads{show: show}
	h.ag.mu.Lock()
	defer h.ag.mu.Unlock()
	h.ag.contextUsed = func(store.Session) (int, bool) {
		p.mu.Lock()
		defer p.mu.Unlock()
		n := len(p.at)
		p.at = append(p.at, h.clock.Now())
		return p.show(n)
	}
	return p
}

// times are the fake clock's times of the pane's reads, oldest first.
func (p *paneReads) times() []time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.at)
}

// answer makes the pane answer later reads with show.
func (p *paneReads) answer(show func(n int) (int, bool)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.show = show
}

// noFigure is a pane that never shows a context figure.
func noFigure(int) (int, bool) { return 0, false }

// toResumedCycle2 is toCycle2 whose fixer an idle park parked after cycle 1
// with its conversation kept: cycle 2 resumes it (ensureAgent), and its pane
// has not drawn the status line yet. Its fixer pushes fixPushed4 in cycle 4.
func toResumedCycle2(t *testing.T, h *harness, git *fixGit) store.PR {
	t.Helper()
	pr := loopToReReview(t, h, git, findings(map[string]int{"P2": 1}, "COMMENTED"), fixPushed2, fixPushed3, fixPushed4)
	s, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, "claude-fix")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.TransitionSession(h.ctx, s.ID, []string{store.SessionLive}, store.SessionParked, nil); err != nil {
		t.Fatal(err)
	}
	h.ag.mu.Lock()
	h.ag.resumeIDs = map[agents.Role]string{"claude-fix": "fix-conversation"}
	h.ag.mu.Unlock()
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
	if st, ok := h.fixStateOf(2); !ok || st.Cycle != 2 {
		t.Fatalf("fix state after the re-review = %+v, %v", st, ok)
	}
	if n := h.ag.count(fmt.Sprintf("start:%d:claude-fix:fix-conversation", pr.ID)); n != 1 {
		t.Fatalf("cycle 2 resumed the parked fixer %d times: %v", n, h.ag.all())
	}
	return pr
}

// A fixer resumed from a park has not drawn its status line when the cycle
// first reads it (live, the warning came 0.2 s after the resume), so the
// cycle reads again every 2 s until a
// figure shows: here after three blank reads, 82%, which it compacts as it
// would have at once, without a warning.
func TestTheContextFigureIsAwaitedAfterAResume(t *testing.T) {
	h, git := fixHarness(t, clockSleep)
	p := logReads(h, func(n int) (int, bool) {
		switch {
		case n < 3:
			return 0, false
		case n == 3:
			return 82, true
		}
		return 31, true // after the /compact
	})
	pr := toResumedCycle2(t, h, git)

	at := p.times()
	if len(at) != 5 {
		t.Fatalf("context reads = %d, want four until the figure and one after the /compact", len(at))
	}
	for i := 1; i < 4; i++ {
		if gap := at[i].Sub(at[i-1]); gap != 2*time.Second {
			t.Errorf("read %d came %s after the one before, want 2s", i+1, gap)
		}
	}
	if h.ag.count(fmt.Sprintf("compact:%d:claude-fix", pr.ID)) != 1 || h.ag.count("quit:") != 0 {
		t.Errorf("the resumed fixer was not compacted once in its conversation: %v", h.ag.all())
	}
	evs := h.fixEvents("fix.compacted")
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "82% to 31%") {
		t.Fatalf("fix.compacted = %+v", evs)
	}
	if d := eventData(t, evs[0]); d["from"] != float64(82) || d["to"] != float64(31) {
		t.Errorf("fix.compacted data = %v", d)
	}
	if evs := h.fixEvents("fix.context_unknown"); len(evs) != 0 {
		t.Errorf("fix.context_unknown = %+v, want none: the figure came within the wait", evs)
	}
	if st, _ := h.fixStateOf(2); st.Context != "compacted 82% → 31%" {
		t.Errorf("the loop's row says %q of the cycle's context", st.Context)
	}
}

// A resumed fixer whose pane shows no figure within the wait (60 s of reads
// 2 s apart) warns fix.context_unknown once, naming the wait, and goes on in
// its conversation. A later cycle that reads a figure (30%, under the key)
// forgets the warning and records nothing, so the next cycle without a
// figure warns again.
func TestNoFigureWithinTheWaitWarnsOnceAndKeepsTheSession(t *testing.T) {
	h, git := fixHarness(t, clockSleep, func(h *harness) { h.cfg.Pipeline.FixCycles = 4 })
	p := logReads(h, noFigure)
	pr := toResumedCycle2(t, h, git)

	at := p.times()
	if len(at) != 31 {
		t.Fatalf("context reads = %d, want 31 (60s of reads 2s apart)", len(at))
	}
	if waited := at[len(at)-1].Sub(at[0]); waited != time.Minute {
		t.Errorf("the reads span %s, want 1m", waited)
	}
	if h.ag.count("compact:") != 0 || h.ag.count("quit:") != 0 || h.ag.count(fmt.Sprintf("start:%d:claude-fix:", pr.ID)) != 2 {
		t.Errorf("the fixer was compacted, parked or started anew: %v", h.ag.all())
	}
	runs := h.fixRuns(pr.ID)
	live, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, "claude-fix")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[1].SessionID == nil || *runs[1].SessionID != live.ID {
		t.Fatalf("fix runs %d: cycle 2 does not go on in the resumed session %d", len(runs), live.ID)
	}
	warns := h.fixEvents("fix.context_unknown")
	if len(warns) != 1 || warns[0].Level != "warn" || !strings.Contains(warns[0].Message, "within 60s of its resume") ||
		!strings.Contains(warns[0].Message, "resumes it as it is") {
		t.Fatalf("fix.context_unknown = %+v, want one warning naming the wait", warns)
	}
	if _, ok, _ := h.st.GetKV(h.ctx, kvPRFixContextUnknown(pr.ID)); !ok {
		t.Fatal("the warning is not remembered for the PR")
	}

	p.answer(func(int) (int, bool) { return 30, true })
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: fixPushed2, author: "zhuravel"}) // re-review 2: cycle 3
	if st, ok := h.fixStateOf(2); !ok || st.Cycle != 3 {
		t.Fatalf("fix state after the second re-review = %+v, %v", st, ok)
	}
	if n := len(p.times()); n != 32 {
		t.Errorf("context reads = %d, want one more for cycle 3's live fixer", n)
	}
	if _, ok, _ := h.st.GetKV(h.ctx, kvPRFixContextUnknown(pr.ID)); ok {
		t.Error("a figure read in cycle 3 left the warning remembered")
	}
	for _, kind := range []string{"fix.compacted", "fix.fresh_session"} {
		if evs := h.fixEvents(kind); len(evs) != 0 {
			t.Errorf("%s = %+v", kind, evs)
		}
	}
	if n := len(h.fixEvents("fix.context_unknown")); n != 1 {
		t.Errorf("fix.context_unknown %d times after cycle 3, want still 1", n)
	}

	p.answer(noFigure)
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: fixPushed3, author: "zhuravel"}) // re-review 3: cycle 4
	if st, ok := h.fixStateOf(2); !ok || st.Cycle != 4 {
		t.Fatalf("fix state after the third re-review = %+v, %v", st, ok)
	}
	warns = h.fixEvents("fix.context_unknown")
	if len(warns) != 2 || strings.Contains(warns[1].Message, "within") {
		t.Errorf("fix.context_unknown = %+v, want a second warning for cycle 4's live fixer, as today", warns)
	}
}

// A fixer whose session was live before the cycle has drawn its status
// line long ago: a pane without a figure is read once, with no wait, and
// warns as it did before the wait existed.
func TestALiveFixerIsReadOnce(t *testing.T) {
	h, git := fixHarness(t, clockSleep)
	p := logReads(h, noFigure)
	pr := toCycle2(t, h, git)

	if n := len(p.times()); n != 1 {
		t.Errorf("context reads = %d, want 1", n)
	}
	if h.ag.count("compact:") != 0 || h.ag.count("quit:") != 0 || h.ag.count(fmt.Sprintf("start:%d:claude-fix:", pr.ID)) != 1 {
		t.Errorf("the live fixer was compacted, parked or started anew: %v", h.ag.all())
	}
	warns := h.fixEvents("fix.context_unknown")
	if len(warns) != 1 || !strings.Contains(warns[0].Message, "resumes it as it is") || strings.Contains(warns[0].Message, "within") {
		t.Fatalf("fix.context_unknown = %+v, want today's warning", warns)
	}
}
