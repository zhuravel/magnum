package tui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// actPR is an open, reviewed PR in a slot of its own, as a board lists it;
// each case below changes what it is about.
func actPR(edit func(r *PRBoardRow)) PRBoardRow {
	r := PRBoardRow{Ref: "talkable/talkable#5", Owner: "talkable", Repo: "talkable", Number: 5, Title: "Coupon export",
		State: "reviewed", GHState: "OPEN", ActivityAt: boardNow, HeadSHA: "abcdef1234", Slot: "~/Projects/talkable.review3",
		LastReview: &ReviewInfo{Login: "zhuravel", Event: "COMMENTED", SubmittedAt: ago(time.Hour), CommitSHA: "abcdef1234"},
		Findings:   &FindingsInfo{Counts: [4]int{0, 1, 0, 0}, Verdict: "blocking", Posted: "COMMENT", SHA: "abcdef1234"}}
	if edit != nil {
		edit(&r)
	}
	return r
}

// Each refusal says, before anything is asked, why the action cannot run on
// the row, naming the key that lifts it where there is one.
func TestActionRefusalSaysWhyBeforeAsking(t *testing.T) {
	for _, c := range []struct {
		name string
		act  rowAct
		edit func(r *PRBoardRow)
		want string
	}{
		{"release a pinned PR", actRelease, func(r *PRBoardRow) { r.Pinned = true }, "talkable#5 is pinned: unpin first (u)"},
		{"release with no slot", actRelease, func(r *PRBoardRow) { r.Slot = "" }, "talkable#5 holds no slot: nothing to release"},
		{"approve after the head moved", actApprove, func(r *PRBoardRow) { r.HeadSHA = "9999999aaa" },
			"talkable#5: the head moved since magnum reviewed abcdef1: review again first (r)"},
		{"request changes after the head moved", actRequestChanges, func(r *PRBoardRow) { r.HeadSHA = "9999999aaa" },
			"talkable#5: the head moved since magnum reviewed abcdef1: review again first (r)"},
		{"approve unreviewed", actApprove, func(r *PRBoardRow) { r.Findings = nil }, "magnum has not reviewed this PR: nothing to approve on"},
		{"approve merged", actApprove, func(r *PRBoardRow) { r.GHState, r.State = "MERGED", "closed" }, "talkable#5 is merged: nothing to approve on"},
		{"review while a round runs", actReview, func(r *PRBoardRow) { r.State = "reviewing" }, "talkable#5: round in progress (reviewing); K kills it"},
		{"fresh review while claiming", actFresh, func(r *PRBoardRow) { r.State = "claiming" }, "talkable#5: round in progress (claiming); K kills it"},
		{"simplify while verifying", actSimplify, func(r *PRBoardRow) { r.State = "verifying" }, "talkable#5: round in progress (verifying); K kills it"},
		{"review closed unmerged", actReview, func(r *PRBoardRow) { r.GHState, r.State = "CLOSED", "closed" },
			"talkable#5 was closed without merging: only open or merged PRs are reviewed"},
		{"review a merged reviewed head", actReview, func(r *PRBoardRow) { r.GHState, r.State = "MERGED", "closed" },
			"talkable#5: its merged head abcdef1 was already reviewed"},
		{"ignore merged", actIgnore, func(r *PRBoardRow) { r.GHState, r.State = "MERGED", "released" }, "talkable#5 is merged: nothing to ignore"},
		{"ignore an ignored PR", actIgnore, func(r *PRBoardRow) { r.State, r.Muted = "ignored", true }, "talkable#5 is already ignored (U stops ignoring it)"},
		{"kill with nothing running", actAbort, nil, "no review of talkable#5 is running or queued (reviewed)"},
		{"kill on baseline", actAbort, func(r *PRBoardRow) { r.State = "baseline" }, "no review of talkable#5 is running or queued (not reviewed)"},
		{"unmute merged", actUnmute, func(r *PRBoardRow) { r.GHState, r.State, r.Muted = "MERGED", "released", true }, "talkable#5 is merged: nothing to unmute"},
		{"unmute closed", actUnmute, func(r *PRBoardRow) { r.GHState, r.State, r.Muted = "CLOSED", "released", true }, "talkable#5 is closed: nothing to unmute"},
		{"unmute an unmuted PR", actUnmute, nil, "talkable#5 is not muted"},
		{"mute a muted PR", actMute, func(r *PRBoardRow) { r.Muted = true }, "talkable#5 is already muted (U unmutes it)"},
		{"mute merged", actMute, func(r *PRBoardRow) { r.GHState, r.State = "MERGED", "released" }, "talkable#5 is merged: nothing to mute"},
		{"pin a pinned PR", actPin, func(r *PRBoardRow) { r.Pinned = true }, "talkable#5 is already pinned"},
		{"unpin an unpinned PR", actUnpin, nil, "talkable#5 is not pinned"},
		{"tracker without an issue", actTracker, nil, "the title names no issue: [board] trackers lists the issue keys and their URLs"},
	} {
		r := boardActRow(actPR(c.edit), "talkable#5", boardNow)
		if got := actionRefusal(c.act, r); got != c.want {
			t.Errorf("%s: refusal %q, want %q", c.name, got, c.want)
		}
	}

	// What can run is not refused: the kill of a round running, paused or
	// waiting in line, U on an ignored PR and on a merged one whose flag a
	// mute dismissed, A on the reviewed head, x on an unpinned slot.
	for _, c := range []struct {
		act  rowAct
		edit func(r *PRBoardRow)
	}{
		{actAbort, func(r *PRBoardRow) { r.State = "reviewing" }},
		{actAbort, func(r *PRBoardRow) { r.State = "paused" }},
		{actAbort, func(r *PRBoardRow) { r.State = "queued" }},
		{actAbort, func(r *PRBoardRow) { r.State = "rereview_pending" }},
		{actUnmute, func(r *PRBoardRow) { r.State, r.Muted = "ignored", true }},
		{actUnmute, func(r *PRBoardRow) { r.GHState, r.State, r.Muted, r.FlagDismissed = "MERGED", "released", true, true }},
		{actIgnore, func(r *PRBoardRow) { r.GHState, r.State = "MERGED", "queued" }}, // its post-merge review waits
		{actApprove, nil},
		{actRelease, nil},
		{actReview, func(r *PRBoardRow) { r.GHState, r.State, r.HeadSHA = "MERGED", "closed", "fedcba9876" }}, // post-merge
	} {
		r := actPR(c.edit)
		if got := actionRefusal(c.act, boardActRow(r, "", boardNow)); got != "" {
			t.Errorf("%s on %s (%s): refused %q", rowActDefs[c.act].key, r.State, r.GHState, got)
		}
	}

	// Nothing selected, or a row naming no PR, comes first.
	if got := actionRefusal(actReview, actRow{}); got != "nothing selected" {
		t.Errorf("no row: %q", got)
	}
	if got := actionRefusal(actOpen, actRow{ok: true, slot: "review2", label: "slot review2", none: "slot review2 holds no PR"}); got != "slot review2 holds no PR" {
		t.Errorf("empty slot open: %q", got)
	}
	if got := actionRefusal(actRelease, actRow{ok: true, slot: "review2", label: "slot review2", pinned: true, pinKnown: true}); got != "slot review2 is pinned: unpin first (u)" {
		t.Errorf("pinned empty slot release: %q", got)
	}
}

// A key never promises what the daemon will not do: x refuses a row whose
// round runs or is paused and one in its close grace (the release skips
// them), D a merged or closed row; A and C name the findings still open, I
// leaves out the slot of a pinned PR (the daemon keeps it).
func TestBoardKeysPromiseWhatYDoes(t *testing.T) {
	for _, c := range []struct {
		name string
		act  rowAct
		edit func(r *PRBoardRow)
		want string
	}{
		{"release while a round runs", actRelease, func(r *PRBoardRow) { r.State = "reviewing" },
			"talkable#5: round in progress (reviewing): no slot is released under a round; K kills it"},
		{"release a paused round", actRelease, func(r *PRBoardRow) { r.State = "paused" },
			"talkable#5: its paused round keeps the slot; K kills it"},
		{"release in the close grace", actRelease, func(r *PRBoardRow) {
			r.GHState, r.State, r.ReleaseAfter = "MERGED", "closed", boardNow.Add(8*time.Minute)
		}, "talkable#5 is in its close grace: magnum releases its slot in 8m"},
		{"withdraw on merged", actUnapprove, func(r *PRBoardRow) { r.GHState, r.State = "MERGED", "closed" },
			"talkable#5 is merged: no approval to withdraw"},
		{"withdraw on closed", actUnapprove, func(r *PRBoardRow) { r.GHState, r.State = "CLOSED", "released" },
			"talkable#5 is closed: no approval to withdraw"},
	} {
		r := boardActRow(actPR(c.edit), "talkable#5", boardNow)
		if got := actionRefusal(c.act, r); got != c.want {
			t.Errorf("%s: refusal %q, want %q", c.name, got, c.want)
		}
	}
	// A closed PR past its grace is released.
	past := boardActRow(actPR(func(r *PRBoardRow) {
		r.GHState, r.State, r.ReleaseAfter = "MERGED", "closed", boardNow.Add(-time.Minute)
	}), "talkable#5", boardNow)
	if got := actionRefusal(actRelease, past); got != "" {
		t.Errorf("release past the grace: refused %q", got)
	}

	for _, c := range []struct {
		name string
		act  rowAct
		edit func(r *PRBoardRow)
		want string
	}{
		{"approve with earlier findings open", actApprove, func(r *PRBoardRow) {
			r.Findings = &FindingsInfo{Open: 3, Verdict: "blocking", SHA: "abcdef1234"}
		}, "Approve talkable#5 at abcdef1? magnum found no new findings; 3 earlier findings still open"},
		{"request changes with new and open findings", actRequestChanges, func(r *PRBoardRow) {
			r.Findings = &FindingsInfo{Counts: [4]int{0, 1, 0, 0}, Open: 1, Verdict: "blocking", SHA: "abcdef1234"}
		}, "Request changes on talkable#5 at abcdef1? magnum found 1 P1; 1 earlier finding still open"},
		{"approve a clean review", actApprove, func(r *PRBoardRow) {
			r.Findings = &FindingsInfo{Verdict: "clean", SHA: "abcdef1234"}
		}, "Approve talkable#5 at abcdef1? magnum found no findings"},
		{"ignore a pinned PR", actIgnore, func(r *PRBoardRow) { r.Pinned = true }, "Ignore talkable#5: mute it?"},
		{"ignore a pinned running PR", actIgnore, func(r *PRBoardRow) { r.Pinned, r.State = true, "reviewing" },
			"Ignore talkable#5: kill its review and mute it?"},
	} {
		r := boardActRow(actPR(c.edit), "talkable#5", boardNow)
		if why := actionRefusal(c.act, r); why != "" {
			t.Errorf("%s: refused %q", c.name, why)
			continue
		}
		if got := actionQuestion(c.act, r); got != c.want {
			t.Errorf("%s: asked %q, want %q", c.name, got, c.want)
		}
	}
}

// The picker names its own keys in a refusal, and leaves out one it lacks.
func TestActionRefusalNamesThePickersKeys(t *testing.T) {
	pinned := PickEntry{Ref: "talkable/talkable#1", State: "reviewed,pinned", Pinned: true, GHState: "OPEN"}
	if got := actionRefusal(actRelease, pickActRow(&pinned, "", boardNow)); got != "talkable/talkable#1 is pinned: unpin first (ctrl+p)" {
		t.Errorf("release: %q", got)
	}
	running := PickEntry{Ref: "talkable/talkable#2", State: "reviewing", GHState: "OPEN"}
	if got := actionRefusal(actReview, pickActRow(&running, "", boardNow)); got != "talkable/talkable#2: round in progress (reviewing)" {
		t.Errorf("review: %q (the picker has no kill key)", got)
	}
}

// The questions say what y will do on the row.
func TestActionQuestionsSayWhatYWillDo(t *testing.T) {
	for _, c := range []struct {
		name string
		act  rowAct
		edit func(r *PRBoardRow)
		want string
	}{
		{"kill a running review", actAbort, func(r *PRBoardRow) { r.State = "reviewing" }, "Kill the running review of talkable#5?"},
		{"kill a paused review", actAbort, func(r *PRBoardRow) { r.State = "paused" }, "Kill the paused review of talkable#5?"},
		{"drop a queued review", actAbort, func(r *PRBoardRow) { r.State = "queued" }, "Drop the queued review of talkable#5 before it starts?"},
		{"drop a queued post-merge review", actAbort, func(r *PRBoardRow) { r.State, r.GHState = "rereview_pending", "MERGED" },
			"Drop the queued post-merge review of talkable#5 before it starts?"},
		{"ignore a running PR in a slot", actIgnore, func(r *PRBoardRow) { r.State = "reviewing" }, "Ignore talkable#5: kill its review, mute it and free its slot?"},
		{"ignore a queued PR without a slot", actIgnore, func(r *PRBoardRow) { r.State, r.Slot = "queued", "" }, "Ignore talkable#5: drop its queued review and mute it?"},
		{"ignore a reviewed PR in a slot", actIgnore, nil, "Ignore talkable#5: mute it and free its slot?"},
		{"ignore a closed PR", actIgnore, func(r *PRBoardRow) { r.GHState, r.State = "CLOSED", "released" },
			"Ignore talkable#5: mute it (closed: it stays ignored if reopened)?"},
		{"unmute an ignored PR", actUnmute, func(r *PRBoardRow) { r.State, r.Muted = "ignored", true },
			"Unmute talkable#5: stop ignoring it and review it on its next push?"},
		{"unmute a merged PR whose flag was dismissed", actUnmute, func(r *PRBoardRow) { r.GHState, r.State, r.Muted, r.FlagDismissed = "MERGED", "released", true, true },
			"Restore the merged-unreviewed flag on talkable#5?"},
		{"unmute a muted open PR", actUnmute, func(r *PRBoardRow) { r.Muted = true }, "Unmute talkable#5: resume automatic reviews of it?"},
		{"approve the reviewed head", actApprove, nil, "Approve talkable#5 at abcdef1? magnum found 1 P1"},
		{"review a pinned PR", actReview, func(r *PRBoardRow) { r.Pinned = true },
			"Review talkable#5 now (no new commits since head abcdef1 was reviewed 1h ago by zhuravel, pinned: the review unpins it)?"},
		{"release", actRelease, nil, "Release talkable#5: hand back its slot now, sessions parked and worktree reset?"},
	} {
		r := boardActRow(actPR(c.edit), "talkable#5", boardNow)
		if why := actionRefusal(c.act, r); why != "" {
			t.Errorf("%s: refused %q", c.name, why)
			continue
		}
		if got := actionQuestion(c.act, r); got != c.want {
			t.Errorf("%s: asked %q, want %q", c.name, got, c.want)
		}
	}
	for _, a := range []rowAct{actOpen, actBrowser, actPin, actUnpin} {
		if q := actionQuestion(a, boardActRow(actPR(nil), "", boardNow)); q != "" {
			t.Errorf("%s asks %q; it runs at once", rowActDefs[a].key, q)
		}
	}
}

// A refused key on the board flashes why at once: no question, no action, and
// a y after it does nothing either.
func TestPRBoardRefusesAtTheKeypress(t *testing.T) {
	for _, c := range []struct {
		key  string
		edit func(r *PRBoardRow)
		want string
	}{
		{"x", func(r *PRBoardRow) { r.Pinned = true }, "talkable#5 is pinned: unpin first (u)"},
		{"x", func(r *PRBoardRow) { r.Slot = "" }, "talkable#5 holds no slot: nothing to release"},
		{"A", func(r *PRBoardRow) { r.HeadSHA = "9999999aaa" }, "review again first (r)"},
		{"C", func(r *PRBoardRow) { r.HeadSHA = "9999999aaa" }, "review again first (r)"},
		{"r", func(r *PRBoardRow) { r.State = "reviewing" }, "talkable#5: round in progress (reviewing); K kills it"},
		{"R", func(r *PRBoardRow) { r.State = "reviewing" }, "round in progress"},
		{"i", func(r *PRBoardRow) { r.State = "verifying" }, "round in progress"},
		{"I", func(r *PRBoardRow) { r.GHState, r.State = "MERGED", "released" }, "talkable#5 is merged: nothing to ignore"},
		{"K", nil, "no review of talkable#5 is running or queued"},
		{"U", func(r *PRBoardRow) { r.GHState, r.State, r.Muted = "MERGED", "released", true }, "talkable#5 is merged: nothing to unmute"},
	} {
		m, _, act := newBoard(t, 200, 24, PRBoardOptions{})
		m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{actPR(c.edit)}})
		m, cmd := send(t, m, keyMsg(c.key))
		if m.confirm != nil || m.busy != "" || hasMsg[actionDoneMsg](execCmd(t, cmd)) {
			t.Errorf("%s asked (%+v) or ran (%q)", c.key, m.confirm, m.busy)
		}
		if !strings.Contains(m.flash, c.want) || !m.flashErr {
			t.Errorf("%s flashed %q (error %v), want %q", c.key, m.flash, m.flashErr, c.want)
		}
		mustNotContain(t, viewOf(m), "y/N")
		if m, _ = send(t, m, keyMsg("y")); len(act.calls) != 0 || m.busy != "" {
			t.Errorf("%s then y ran %v", c.key, act.calls)
		}
	}
}

// K on a PR whose review waits in line asks to drop it, and y sends the
// abort, which takes the queued review back.
func TestPRBoardKillDropsAQueuedReview(t *testing.T) {
	m, _, act := newBoard(t, 200, 24, PRBoardOptions{})
	m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{actPR(func(r *PRBoardRow) { r.State = "queued" })}}, keyMsg("K"))
	if m.confirm == nil || m.confirm.question != "Drop the queued review of talkable#5 before it starts?" {
		t.Fatalf("K asked %+v", m.confirm)
	}
	if _, _ = boardAct(t, m, "y"); act.last() != "abort talkable/talkable#5" {
		t.Fatalf("y called %q", act.last())
	}
}

// The dashboard refuses what the board refuses: r on a PR closed without
// merging or merged with its head reviewed, x where nothing holds a slot or
// the slot is pinned, and U on an ignored PR asks to stop ignoring it.
func TestDashboardRefusesAtTheKeypress(t *testing.T) {
	data := dashData()
	data.Queue[0].GHState, data.Queue[0].State = "CLOSED", "closed" // talkable#7
	data.Queue = append(data.Queue,
		PRRow{Ref: "talkable#9", State: "closed", GHState: "MERGED", Review: &ReviewFacts{HeadSHA: "abcdef1234", ReviewedSHA: "abcdef1234"}},
		PRRow{Ref: "talkable#10", State: "queued", GHState: "OPEN"})
	data.Slots = append(data.Slots, SlotRow{Name: "review3", PRRef: "talkable#11", PRState: "ignored", State: "held", SlotState: "held", PRGHState: "OPEN"},
		SlotRow{Name: "review4", PRRef: "talkable#12", PRState: "reviewed", State: "held", Pinned: true, SlotState: "held [pinned]", PRGHState: "OPEN"})
	at := func(m dashboardModel, key string) int {
		for i, r := range m.rows {
			if r.key() == key {
				return i
			}
		}
		t.Fatalf("no row %s", key)
		return -1
	}
	for _, c := range []struct {
		row, key, want string
	}{
		{"pr:talkable#7", "r", "talkable#7 was closed without merging: only open or merged PRs are reviewed"},
		{"pr:talkable#9", "R", "talkable#9: its merged head abcdef1 was already reviewed"},
		{"pr:talkable#10", "x", "talkable#10 holds no slot: nothing to release"},
		{"slot:review4", "x", "talkable#12 is pinned: unpin first (u)"},
		{"slot:review1", "r", "talkable#1: round in progress (reviewing); K kills it"},
		{"pr:talkable#10", "I", ""}, // asks
	} {
		m, _, act := newDash(t, 220, 60)
		m, _ = send(t, m, dashDataMsg{data: data})
		m.moveTo(at(m, c.row))
		m, _ = send(t, m, keyMsg(c.key))
		if c.want == "" {
			if m.confirm == nil {
				t.Errorf("%s on %s did not ask (flash %q)", c.key, c.row, m.flash)
			}
			continue
		}
		if m.confirm != nil || !strings.Contains(m.flash, c.want) {
			t.Errorf("%s on %s: asked %+v, flashed %q, want %q", c.key, c.row, m.confirm, m.flash, c.want)
		}
		if m, _ = send(t, m, keyMsg("y")); len(act.calls) != 0 {
			t.Errorf("%s on %s then y ran %v", c.key, c.row, act.calls)
		}
	}

	m, _, _ := newDash(t, 220, 60)
	m, _ = send(t, m, dashDataMsg{data: data})
	m.moveTo(at(m, "slot:review3"))
	m, _ = send(t, m, keyMsg("U"))
	if want := "Unmute talkable#11: stop ignoring it and review it on its next push?"; m.confirm == nil || m.confirm.question != want {
		t.Errorf("U on an ignored PR asked %+v, want %q", m.confirm, want)
	}
	items := menuState(m.menuItems())
	if !items["unmute (stop ignoring)"] || items["ignore"] {
		t.Errorf("menu of an ignored PR: %v", items)
	}
}

// The menus, the card's actions and the key hints come from the one table:
// every action key appears once, a menu entry is enabled exactly when the
// predicate lets the action run, and the card lists exactly those.
func TestMenusCardAndHintsComeFromTheTable(t *testing.T) {
	for _, acts := range [][]rowAct{boardActs, dashActs} {
		seen := map[string]bool{}
		for _, a := range acts {
			k := rowActDefs[a].key
			if seen[k] {
				t.Errorf("key %s twice", k)
			}
			seen[k] = true
			if got, ok := rowActFor(acts, k); !ok || got != a {
				t.Errorf("key %s runs %v, want %v", k, got, a)
			}
		}
	}

	rows := []PRBoardRow{actPR(nil), actPR(func(r *PRBoardRow) { r.Pinned = true }), actPR(func(r *PRBoardRow) { r.State = "reviewing" }),
		actPR(func(r *PRBoardRow) { r.GHState, r.State, r.Muted, r.FlagDismissed = "MERGED", "released", true, true }),
		actPR(func(r *PRBoardRow) { r.State, r.Muted = "ignored", true }), actPR(func(r *PRBoardRow) { r.HeadSHA = "9999999aaa" })}
	for _, pr := range rows {
		m, _, _ := newBoard(t, 200, 40, PRBoardOptions{})
		m, _ = send(t, m, prbDataMsg{rows: []PRBoardRow{pr}})
		r := m.actRow()
		items := m.menuItems()
		var wantCard []string
		for i, a := range boardActs {
			it := items[i]
			if it.key != rowActDefs[a].key || it.label != actionLabel(a, r) {
				t.Errorf("menu item %d is %q %q, want %q %q", i, it.key, it.label, rowActDefs[a].key, actionLabel(a, r))
			}
			if it.ok != (actionRefusal(a, r) == "") {
				t.Errorf("%s/%s: menu %s enabled %v, refusal %q", pr.State, pr.GHState, it.key, it.ok, actionRefusal(a, r))
			}
			if it.ok {
				wantCard = append(wantCard, it.key+" "+it.label)
			}
		}
		card := viewOf(func() prBoardModel { c, _ := send(t, m, keyMsg("enter")); return c }())
		_, acts, _ := strings.Cut(card, "ACTIONS")
		acts, _, _ = strings.Cut(acts, "esc back") // the section, not the key hints under the card
		acts = strings.Join(strings.Fields(acts), " ") + " "
		for _, w := range wantCard {
			if !strings.Contains(acts, w) {
				t.Errorf("%s/%s: the card lacks %q:\n%s", pr.State, pr.GHState, w, acts)
			}
		}
		for i, a := range boardActs {
			if !items[i].ok && strings.Contains(acts, rowActDefs[a].key+" "+actionLabel(a, r)+" ") {
				t.Errorf("%s/%s: the card offers the refused %q", pr.State, pr.GHState, items[i].label)
			}
		}
	}

	if got := actionHints(actReview, actPin, actUnpin, actRelease, actMute, actUnmute); !slices.Equal(got,
		[]hint{{"r", "review"}, {"p/u", "pin"}, {"x", "release"}, {"M/U", "mute"}}) {
		t.Errorf("hints %v", got)
	}
	m, _, _ := newBoard(t, 220, 24, PRBoardOptions{})
	mustContain(t, viewOf(m), "r review · R fresh · i simplify · o open · b browser · t tracker · p/u pin · x release")
	d, _, _ := newDash(t, 220, 50)
	mustContain(t, viewOf(d), "enter open · r review · p/u pin · x release · a attention · b browser")
}

// ctrl+r and F5 refresh the picker's list (they reviewed again before),
// keeping the filter; without a source they say so.
func TestPickerRefreshKeys(t *testing.T) {
	calls := 0
	reload := func(context.Context) ([]PickEntry, error) {
		calls++
		return append(pickEntries(), PickEntry{Ref: "talkable/talkable#99", Title: "Coupon import", State: "queued", Age: "1m"}), nil
	}
	for _, k := range []tea.KeyPressMsg{keyMsg("ctrl+r"), {Code: tea.KeyF5}} {
		m := newPickerModel(pickEntries(), PickerOptions{Query: "coupon", Reload: reload})
		m, _ = send(t, m, tea.WindowSizeMsg{Width: 120, Height: 24})
		m, cmd := send(t, m, k)
		if m.done || m.confirm != nil {
			t.Fatalf("%s finished or asked", k)
		}
		m, _ = send(t, m, execCmd(t, cmd)...)
		if got := visibleRefs(m); !slices.Equal(slices.Sorted(slices.Values(got)), []string{"talkable/talkable#12", "talkable/talkable#99"}) {
			t.Errorf("%s: after the refresh the filter shows %v", k, got)
		}
		mustContain(t, viewOf(m), "refreshed: 5 PRs", "^r refresh")
	}
	if calls != 2 {
		t.Errorf("reloaded %d times", calls)
	}

	failing := newPickerModel(pickEntries(), PickerOptions{Reload: func(context.Context) ([]PickEntry, error) { return nil, errors.New("registry locked") }})
	failing, cmd := send(t, failing, keyMsg("ctrl+r"))
	failing, _ = send(t, failing, execCmd(t, cmd)...)
	mustContain(t, viewOf(failing), "refresh failed: registry locked")
	if len(failing.entries) != len(pickEntries()) {
		t.Errorf("a failed refresh changed the list: %d entries", len(failing.entries))
	}

	plain := newPicker(t, "", 120, 24)
	plain, _ = send(t, plain, keyMsg("ctrl+r"))
	mustContain(t, viewOf(plain), "refresh is not available here")
	mustNotContain(t, viewOf(plain), "^r")
}
