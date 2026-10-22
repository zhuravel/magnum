package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// parkedJudge seeds talkable#5 with its judge parked: `magnum open` restores
// it. A daemon (pid 99) answers, and the restored judge's pane comes up at
// the second poll.
func (h *actHarness) parkedJudge() store.PR {
	h.t.Helper()
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	s := h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	if err := h.st.TransitionSession(h.ctx, s.ID, nil, store.SessionParked, func(u *store.SessionUpdate) { u.Set("session_id", "u1") }); err != nil {
		h.t.Fatal(err)
	}
	h.pid = 99
	h.onSleep = func(h *actHarness) {
		if live, err := h.st.LiveSessionByPRRole(h.ctx, pr.ID, store.RoleJudge); err != nil || store.Deref(live.HerdrPaneID) == "" {
			h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_9", "w_9")
		}
	}
	return pr
}

const openRestoreQ = "Restore talkable#5 in a slot to read it? It holds the PR until 30 minutes after your last input. [y/N]"

// `magnum open` on a terminal asks y/N before it has the daemon restore a
// parked PR, naming the hold; only y sends the request.
func TestOpenAsksOnATerminalBeforeItRestores(t *testing.T) {
	h := newActHarness(t)
	h.parkedJudge()
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	for _, in := range []string{"n\n", "\n", ""} {
		h.stdin(in)
		h.errb.Reset()
		if code := h.cmd("open", "5", "--no-reveal"); code != 1 {
			t.Fatalf("exit %d after %q: %s", code, in, h.errb.String())
		}
		actContains(t, h.errb.String(), openRestoreQ, "nothing restored")
		if reqs := h.requests(); len(reqs) != 0 {
			t.Fatalf("restored without a y: %+v", reqs)
		}
	}
	h.stdin("y\n")
	h.errb.Reset()
	if code := h.cmd("open", "5", "--no-reveal"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if reqs := h.requests(); len(reqs) != 1 || reqs[0].Kind != engine.ReqOpen {
		t.Fatalf("requests %+v", reqs)
	}
	actContains(t, h.errb.String(), "held for you until 30 minutes after your last input (request 1)")
}

// --yes skips the question; --json and a script (no terminal) never ask, as
// the herdr plugin and scripts call open.
func TestOpenRestoresWithoutAskingWithYesJSONOrNoTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		tty  bool
		args []string
	}{
		{"--yes", true, []string{"--yes"}},
		{"--json", true, []string{"--json"}},
		{"no terminal", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newActHarness(t)
			h.parkedJudge()
			h.d.StdinTTY, h.d.StdoutTTY = tc.tty, tc.tty
			if code := h.cmd("open", append([]string{"5", "--no-reveal"}, tc.args...)...); code != 0 {
				t.Fatalf("exit %d: %s", code, h.errb.String())
			}
			if strings.Contains(h.out.String()+h.errb.String(), "[y/N]") {
				t.Fatalf("asked: %s%s", h.out.String(), h.errb.String())
			}
			if reqs := h.requests(); len(reqs) != 1 || reqs[0].Kind != engine.ReqOpen {
				t.Fatalf("requests %+v", reqs)
			}
		})
	}
}

// A focus of a live pane asks nothing on a terminal: nothing is restored.
func TestOpenOfALivePaneAsksNothing(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.session(pr.ID, store.RoleJudge, store.SessionLive, "mg-talkable-5-judge", "p_1", "w_1")
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	if code := h.cmd("open", "5", "--no-reveal"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if strings.Contains(h.out.String()+h.errb.String(), "[y/N]") || len(h.requests()) != 0 {
		t.Fatalf("a focus asked or queued: %s%s %+v", h.out.String(), h.errb.String(), h.requests())
	}
}

// With open_hold = "0" the question says the pin lasts until `magnum unpin`.
func TestOpenAsksForALastingPinWhenOpenHoldIsZero(t *testing.T) {
	h := newActHarness(t)
	h.parkedJudge()
	h.d.Cfg.Daemon.OpenHold.Duration = 0
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.stdin("n\n")
	if code := h.cmd("open", "5", "--no-reveal"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "Restore talkable#5 in a slot to read it? It holds the PR until you unpin it. [y/N]")
}

// The board's rows say whether o restores (the judge parked, none live) and
// for how long, as open decides it.
func TestOpenWouldRestoreOnlyAParkedRoleWithoutALiveOne(t *testing.T) {
	parked := store.Session{Role: store.RoleJudge, State: store.SessionParked}
	live := store.Session{Role: store.RoleJudge, State: store.SessionLive}
	closed := store.Session{Role: store.RoleJudge, State: store.SessionClosed}
	other := store.Session{Role: store.RoleClaude, State: store.SessionParked}
	for _, tc := range []struct {
		name string
		all  []store.Session
		want bool
	}{
		{"parked", []store.Session{parked}, true},
		{"parked, then closed", []store.Session{parked, closed}, true},
		{"live", []store.Session{parked, live}, false},
		{"closed only", []store.Session{closed}, false},
		{"another role parked", []store.Session{other}, false},
		{"none", nil, false},
	} {
		if got := openWouldRestore(tc.all, store.RoleJudge); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The board's rows carry what o needs: whether it restores the PR (its
// judge parked and none live: o asks first) and for how long a restore
// holds it, and, for a PR an open holds, when the hold ends; the plain
// table says "open-hold→03:21" in place of "pinned".
func TestBoardRowsCarryTheOpenFacts(t *testing.T) {
	_, st, d, now := statusFixture(t)
	ctx := context.Background()
	repo, err := st.RepoByFullName(ctx, "talkable/talkable")
	if err != nil {
		t.Fatal(err)
	}
	prOf := func(n int) store.PR {
		pr, err := st.PRByRepoNumber(ctx, repo.ID, n)
		if err != nil {
			t.Fatal(err)
		}
		return pr
	}
	parked, held := prOf(11930), prOf(11931)
	if _, err := st.CreateSession(ctx, store.Session{PRID: parked.ID, Role: store.RoleJudge, State: store.SessionParked, SessionID: new("u-1")}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdatePR(ctx, held.ID, func(u *store.PRUpdate) { u.Set("pinned", true) }); err != nil {
		t.Fatal(err)
	}
	until := now.Add(20 * time.Minute)
	b, _ := json.Marshal(engine.OpenHold{Until: until, At: now.Add(-10 * time.Minute)})
	if err := st.SetKV(ctx, engine.KVPROpenHold(held.ID), string(b)); err != nil {
		t.Fatal(err)
	}

	rows, err := prsSource(st, d.Config, store.BoardFilter{}, nil, d.Layout)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byNum := map[int]tui.PRBoardRow{}
	for _, r := range rows {
		byNum[r.Number] = r
	}
	if r := byNum[11930]; !r.OpenRestores || r.OpenHold != 30*time.Minute || !r.HeldUntil.IsZero() {
		t.Errorf("the parked row: OpenRestores %v, OpenHold %v, HeldUntil %v", r.OpenRestores, r.OpenHold, r.HeldUntil)
	}
	if r := byNum[11920]; r.OpenRestores { // its judge is live
		t.Errorf("a row with a live judge restores")
	}
	r := byNum[11931]
	if !r.HeldUntil.Equal(until) || r.OpenRestores {
		t.Errorf("the held row: HeldUntil %v, OpenRestores %v", r.HeldUntil, r.OpenRestores)
	}
	if s := prsStateCell(r, now); !strings.Contains(s, ",open-hold→"+inspClock(now, until)) || strings.Contains(s, "pinned") {
		t.Errorf("STATE of the held row = %q", s)
	}
}
