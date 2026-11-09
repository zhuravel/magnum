package tui

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// The board's PR keys on the factory screen (R29): r and R review, z
// snoozes or lifts the snooze, M and U mute and unmute, on home's row and
// on the card's, whenever the row has a PR in magnum's registry. Each asks
// the board's y/N and refuses where the board's does, from the facts
// magnum's registry has of the PR (FactoryPR.Facts).

// facts gives the PR of the row keyed key the facts f, as the screen's
// source reads them from magnum's registry, and reads the factory again.
func (h *factoryHarness) facts(key string, f FactoryPRFacts) {
	h.t.Helper()
	found := false
	h.src.change(func(d *FactoryData) {
		for i, r := range d.Rows {
			if r.Key == key && r.PR != nil {
				pr := *r.PR
				pr.Facts = &f
				d.Rows[i].PR, found = &pr, true
			}
		}
	})
	if !found {
		h.t.Fatalf("no row %s with a PR", key)
	}
	h.press("ctrl+r")
}

// wantPRCalls checks the board's row actions the screen ran, in order.
func (h *factoryHarness) wantPRCalls(want ...string) {
	h.t.Helper()
	if got := h.act.prCalls(); !slices.Equal(got, want) {
		h.t.Errorf("the screen ran %q, want %q", got, want)
	}
}

const (
	factoryHead     = "b2c3d4e5f6a7b8c9d0e1"
	factoryReviewed = "a1b2c3d4e5f6a7b8c9d0"
)

// r, R, z and M on your PR's row ask the board's question for it, with
// what magnum's registry says of the PR (the commits since its review),
// and only y runs the board's call through magnum's own request paths
// (FactoryActions.Review, Snooze, Mute). Before, the factory screen had
// none of these keys: your PRs moved there from the board with less
// control than they had.
func TestTheBoardsPRKeysAskAndRunOnAPRRow(t *testing.T) {
	h := testYours(t, 160, 40)
	h.facts("#4602", FactoryPRFacts{State: "reviewed", GHState: "OPEN", Head: factoryHead, Reviewed: factoryReviewed})
	h.press("g")
	h.selectRow("#4602")
	h.press("r")
	mustContain(t, h.view(), "Review #4602 now (new commits since the last review of a1b2c3d, head b2c3d4e)? y/N")
	h.press("n")
	if h.act.calls() != 0 {
		t.Fatal("n ran the review")
	}
	h.press("r", "y", "R")
	mustContain(t, h.view(), "Fresh review of #4602 in new agent sessions (new commits since the last review of a1b2c3d, head b2c3d4e)? y/N")
	h.press("y", "z")
	mustContain(t, h.view(), "Snooze #4602 for 2h: no automatic review starts on it until 16:09 (r and review requests still run)? y/N")
	h.press("y", "M")
	mustContain(t, h.view(), "Mute #4602: stop automatic reviews of it? y/N")
	h.press("y")
	h.wantPRCalls("review example/talkable#4602", "fresh example/talkable#4602", "snooze 2h0m0s example/talkable#4602",
		"mute example/talkable#4602")
}

// z on a snoozed PR lifts its snooze and U unmutes a muted one, as the
// board's z and U do; M on a muted PR refuses and names U, before any
// question.
func TestZLiftsTheSnoozeAndUUnmutes(t *testing.T) {
	h := testYours(t, 160, 40)
	h.facts("#4602", FactoryPRFacts{State: "reviewed", GHState: "OPEN", Muted: true, SnoozedUntil: factoryNow.Add(90 * time.Minute)})
	h.press("g")
	h.selectRow("#4602")
	h.press("z")
	mustContain(t, h.view(), "Lift the snooze of #4602 (until 15:39): automatic reviews start again? y/N")
	h.press("y", "M")
	v := h.view()
	mustContain(t, v, "#4602 is already muted (U unmutes it)")
	mustNotContain(t, v, "y/N")
	h.press("U")
	mustContain(t, h.view(), "Unmute #4602: resume automatic reviews of it? y/N")
	h.press("y")
	h.wantPRCalls("unsnooze example/talkable#4602", "unmute example/talkable#4602")
}

// The keys refuse, with the board's reason and before any question, where
// the board's refuse: a row without a PR in magnum's registry, a merged PR
// whose head magnum reviewed, a PR that is not muted, a PR Codex flagged.
func TestThePRKeysRefuseWhereTheBoardsDo(t *testing.T) {
	h := testYours(t, 160, 40)
	h.press("g")
	h.selectRow("PR-12345")
	for _, k := range []string{"r", "R", "z", "M", "U"} {
		h.press(k)
		mustContain(t, h.view(), "r, R, z, M and U act on a PR in magnum's registry: PR-12345 has none there")
	}

	h.facts("#4602", FactoryPRFacts{State: "released", GHState: "MERGED", Head: factoryHead, Reviewed: factoryHead})
	h.selectRow("#4602")
	for k, why := range map[string]string{
		"r": "#4602: its merged head b2c3d4e was already reviewed", "R": "#4602: its merged head b2c3d4e was already reviewed",
		"z": "#4602 is merged: nothing to snooze", "M": "#4602 is merged: nothing to mute", "U": "#4602 is merged: nothing to unmute",
	} {
		h.press(k)
		v := h.view()
		mustContain(t, v, why)
		mustNotContain(t, v, "y/N")
	}

	h.facts("#4603", FactoryPRFacts{State: "reviewed", GHState: "OPEN"})
	h.press("g")
	h.selectRow("#4603")
	h.press("U")
	mustContain(t, h.view(), "#4603 is not muted")

	h.facts("#4611", FactoryPRFacts{State: "reviewed", GHState: "OPEN", CodexFlag: "Codex flagged · never reviewed again"})
	h.press("g")
	h.selectRow("#4611")
	h.press("r")
	mustContain(t, h.view(), "#4611: Codex flagged · never reviewed again (`magnum codex-flag clear` lifts it)")
	if h.act.calls() != 0 {
		t.Errorf("a refused key ran %d calls: %q", h.act.calls(), h.act.prCalls())
	}
}

// r on a PR whose reviewed head has replies waiting for the judge has the
// judge alone re-decide them, as the board's r does (ReviewOpts.Replies,
// the board's question and menu words); R is still a fresh full review.
// Before, the factory screen never knew of the replies: its r ran an
// ordinary forced review of every role on the same head, where the
// board's r runs the judge-only reply round.
func TestRReDecidesTheRepliesWaitingAsTheBoardsDoes(t *testing.T) {
	h := testYours(t, 160, 40)
	h.facts("#4602", FactoryPRFacts{State: "reviewed", GHState: "OPEN", Head: factoryHead, Reviewed: factoryHead, Replies: 1})
	h.press("g")
	h.selectRow("#4602")
	i := slices.IndexFunc(h.m.menuItems(), func(it menuItem) bool { return it.key == "r" })
	if i < 0 || h.m.menuItems()[i].label != "re-decide replies" {
		t.Errorf("the menu's r item: %+v, want re-decide replies", h.m.menuItems())
	}
	h.press("r")
	mustContain(t, h.view(), "Judge re-decides 1 reply on #4602 now (judge only; no new commits since head b2c3d4e was reviewed)? y/N")
	h.press("y", "R")
	mustContain(t, h.view(), "Fresh review of #4602 in new agent sessions")
	h.press("y")
	h.wantPRCalls("replies example/talkable#4602", "fresh example/talkable#4602")
}

// r on a PR with a round in flight refuses as the board's r does, and
// names K only where K stops what runs: K on this screen stops a fix loop
// (and the re-review it forced), never a review alone. Before, the refusal
// said "K kills it" on a PR magnum only reviewed, where K says no fix loop
// runs and kills nothing.
func TestARoundInProgressNamesKOnlyWhereKStopsIt(t *testing.T) {
	h := testYours(t, 160, 40)
	h.facts("#4602", FactoryPRFacts{State: "reviewing", GHState: "OPEN"})
	h.facts("#4612", FactoryPRFacts{State: "reviewing", GHState: "OPEN"}) // magnum fixes #4612
	h.press("g")
	h.selectRow("#4602")
	h.press("r")
	v := h.view()
	mustContain(t, v, "#4602: round in progress (reviewing)")
	mustNotContain(t, v, "K kills it", "y/N")
	h.press("K")
	mustContain(t, h.view(), "no fix loop runs on #4602")

	h.selectRow("#4612")
	h.press("R")
	mustContain(t, h.view(), "#4612: round in progress (reviewing); K kills it")
	h.press("K")
	mustContain(t, h.view(), "Stop what magnum runs on #4612 (its fix loop and the re-review the loop forced)? y/N")
	h.press("n")
	if h.act.calls() != 0 {
		t.Errorf("the refusals ran %d calls", h.act.calls())
	}
}

// r, R, z, M and U that went through on a card of the walk (a) move it on
// to the next card, as f, F and K do (followPR): a PR the operator acted on
// has had its turn. An answer n keeps the card.
func TestAPRKeyThatWentThroughMovesTheWalkOn(t *testing.T) {
	h := testYours(t, 140, 40)
	h.src.change(func(d *FactoryData) { // PR-12345's questions answered: the walk starts at #4602
		d.Rows = d.Rows[1:]
		delete(d.Cards, 2)
	})
	h.press("ctrl+r", "a")
	if h.m.view != fvTask || h.m.task != -72 {
		t.Fatalf("a opened view %v of %d, want #4602's card", h.m.view, h.m.task)
	}
	h.press("M", "n")
	if h.m.task != -72 {
		t.Fatalf("n on M left #4602's card for %d", h.m.task)
	}
	h.press("M", "y")
	if h.m.view != fvTask || h.m.task != -73 {
		t.Errorf("M on #4602 left the walk on view %v of %d, want #4603's card", h.m.view, h.m.task)
	}
	h.wantPRCalls("mute example/talkable#4602")
}

// r is the review of your PR on its row and on its card, each asking the
// board's y/N, and the card's hint bar offers it where it acts: the move of
// a PR whose review magnum owes ("r reviews it") names a key the card shows.
// A merged PR whose head magnum reviewed refuses it, and the bar leaves it
// out.
func TestROnYourPRsRowAndCardReviewsAndTheCardOffersIt(t *testing.T) {
	h := testYours(t, 160, 40)
	h.src.change(func(d *FactoryData) { *d = yoursCardData() })
	h.press("ctrl+r")
	h.facts("#4602", FactoryPRFacts{State: "reviewed", GHState: "OPEN", Head: factoryHead, Reviewed: factoryReviewed})
	h.press("g")
	h.selectRow("#4602")
	h.press("r")
	mustContain(t, h.view(), "Review #4602 now (new commits since the last review of a1b2c3d, head b2c3d4e)? y/N")
	h.press("y", "enter")
	if h.m.view != fvTask || h.m.task != -72 {
		t.Fatalf("enter opened view %v of %d, want #4602's card", h.m.view, h.m.task)
	}
	mustContain(t, h.hintLine(), "r review", "f fix", "o magnum's pane")
	h.press("r")
	mustContain(t, h.view(), "Review #4602 now (new commits since the last review of a1b2c3d, head b2c3d4e)? y/N")
	h.press("y")
	h.wantPRCalls("review example/talkable#4602", "review example/talkable#4602")

	h.press("esc")
	h.facts("#4602", FactoryPRFacts{State: "released", GHState: "MERGED", Head: factoryHead, Reviewed: factoryHead})
	h.press("enter")
	mustNotContain(t, h.hintLine(), "r review")
	mustContain(t, h.hintLine(), "f fix")
}

// On a task card that offers a retry, r stays the retry and its hints say
// "r retry"; R is the fresh review there as anywhere, and z's question
// names no key for the review. On a card that offers none, r is the review
// of the task's PR.
func TestROnATaskCardRetriesWhereItCanElseReviews(t *testing.T) {
	h := testPublished(t, 140, 40)
	h.src.change(func(d *FactoryData) {
		c := d.Cards[1]
		c.Task.Retry = true
		d.Cards[1] = c
	})
	h.press("ctrl+r")
	h.open("PR-12320")
	mustContain(t, h.view(), "r retry")
	h.press("r")
	mustContain(t, h.view(), "Retry PR-12320: it goes on at its stage, which checks again what stopped it? y/N")
	h.press("n", "z") // r retries here: z names no key for the review
	mustContain(t, h.view(), "(a review and review requests still run)? y/N")
	h.press("n", "R")
	mustContain(t, h.view(), "Fresh review of PR-12320's #4530 in new agent sessions? y/N")
	h.press("y", "esc")
	h.open("PR-12324")
	h.press("r")
	mustContain(t, h.view(), "Review PR-12324's #4534 now? y/N")
	h.press("y")
	h.wantPRCalls("fresh example/talkable#4530", "review example/talkable#4534")
	if len(h.act.retries) != 0 {
		t.Errorf("retries %v, want none", h.act.retries)
	}
}

// On a task's PR the question also says what y does to the factory's own
// loop, in magnum's engine's terms: a mute stops autofix's new loops but
// not a loop at work (its re-reviews are forced, and a mute holds only the
// rounds nobody forced); a snooze lets a loop at work finish its cycle but
// start no next one (every fix start refuses a snoozed PR), so a re-review
// that still finds work ends the loop and leaves the PR needing you; a
// review's findings may go to autofix, unless fix is off, and while a fix
// loop works magnum refuses the review (requestReview). Before, r there
// promised autofix the findings of a review magnum refuses, and z said the
// loop just stops.
func TestThePRKeysSayWhatTheyDoToTheFactorysLoop(t *testing.T) {
	h := testPublished(t, 200, 40)
	h.facts("PR-12321", FactoryPRFacts{State: "reviewed", GHState: "OPEN", Autofix: true})
	h.press("g")
	h.selectRow("PR-12321") // magnum fixes #4531
	h.press("M")
	if got, want := h.m.bar.question(), "Mute PR-12321's #4531: stop automatic reviews of it; the fix loop at work on the factory's PR "+
		"goes on, its re-reviews too, but autofix starts no other until U?"; got != want {
		t.Errorf("M asks %q, want %q", got, want)
	}
	h.press("n", "z")
	if got, want := h.m.bar.question(), "Snooze PR-12321's #4531 for 2h: no automatic review starts on it until 16:09 (r and review "+
		"requests still run); the fix loop at work on the factory's PR runs no next cycle: if its re-review still finds work, the loop "+
		"ends and the PR needs you, and autofix starts no other until then?"; got != want {
		t.Errorf("z asks %q, want %q", got, want)
	}
	for _, k := range []string{"r", "R"} {
		h.press("n", k)
		if got := h.m.bar.question(); !strings.HasSuffix(got, "; magnum refuses a review while its fix loop works on the factory's PR (K stops the loop)?") ||
			strings.Contains(got, "autofix") {
			t.Errorf("%s under magnum's fix loop asks %q, want that magnum refuses it", k, got)
		}
	}
	h.press("n")

	h.facts("PR-12320", FactoryPRFacts{State: "reviewed", GHState: "OPEN", Head: factoryHead, Reviewed: factoryHead, Autofix: true})
	h.press("g")
	h.selectRow("PR-12320")
	h.press("r")
	if got, want := h.m.bar.question(), "Review PR-12320's #4530 now (no new commits since head b2c3d4e was reviewed); "+
		"autofix may take what it finds on the factory's PR?"; got != want {
		t.Errorf("r asks %q, want %q", got, want)
	}
	h.press("n", "M")
	if got, want := h.m.bar.question(), "Mute PR-12320's #4530: stop automatic reviews of it; autofix starts no fix loop on the factory's PR until U?"; got != want {
		t.Errorf("M asks %q, want %q", got, want)
	}
	h.press("n")
	h.wantPRCalls()
}

// lineOf is the line of view that holds text.
func lineOf(t *testing.T, view, text string) string {
	t.Helper()
	for l := range strings.SplitSeq(view, "\n") {
		if strings.Contains(l, text) {
			return l
		}
	}
	t.Fatalf("no line holds %q:\n%s", text, view)
	return ""
}

// After M or z the row says so: a muted PR's row says "muted" and a
// snoozed one's "snoozed → <when it ends>" before what it waits on, and
// the hints offer U and z's lift, on home and on the PR's card. A row whose
// words say it is muted already (LEFT ALONE's "#4606 is muted: U unmutes
// it") gets no second mark. Before, nothing on the factory
// screen said a PR was snoozed, and a mute showed only once magnum's look
// left the PR alone.
func TestAMutedOrSnoozedPRsRowSaysSo(t *testing.T) {
	h := testYours(t, 200, 40)
	h.facts("#4602", FactoryPRFacts{State: "reviewed", GHState: "OPEN", Muted: true, SnoozedUntil: factoryNow.Add(2 * time.Hour)})
	h.src.change(func(d *FactoryData) {
		for i, r := range d.Rows {
			if r.Key == "#4606" {
				why := "#4606 is muted: U unmutes it"
				d.Rows[i].WaitsOn = []WaitsOn{{Who: WaitsOnYou, What: why, Short: "#4606 is muted", OnYou: true}}
				d.Rows[i].PR = &FactoryPR{Ref: "example/talkable#4606", Off: why, Facts: &FactoryPRFacts{State: "reviewed", GHState: "OPEN", Muted: true}}
			}
		}
	})
	h.press("ctrl+r", "g")
	h.selectRow("#4602")
	v := h.view()
	mustContain(t, lineOf(t, v, "Coupon rounding"), "muted · snoozed → 16:09 · you: 1 item needs you")
	mustContain(t, v, "U unmute", "z lift snooze")
	if l := lineOf(t, v, "Old sync job cleanup"); strings.Count(l, "muted") != 1 {
		t.Errorf("the LEFT ALONE row of a muted PR says it twice: %q", l)
	}
	h.press("enter")
	mustContain(t, h.view(), "U unmute", "z lift snooze")
	h.press("esc", "g")
	h.selectRow("#4603")
	mustNotContain(t, lineOf(t, h.view(), "? more"), "U unmute", "z lift snooze") // the hints: LEFT ALONE's words say "U unmutes it"
}

// The menu of a row or a card with a PR lists r, R, z, M and U in the
// board's words, dimmed where the board's predicate refuses them (M on a
// muted PR) and on a row without a PR; an item asks its key's y/N. On a
// card that offers a retry, r is the retry's item alone.
func TestTheMenuListsThePRKeys(t *testing.T) {
	h := testYours(t, 160, 40)
	h.facts("#4602", FactoryPRFacts{State: "reviewed", GHState: "OPEN", Muted: true})
	h.press("g")
	h.selectRow("#4602")
	items := h.m.menuItems()
	for k, want := range map[string]struct {
		label string
		ok    bool
	}{"r": {"review", true}, "R": {"fresh review", true}, "z": {"snooze 2h", true}, "M": {"mute", false}, "U": {"unmute", true}} {
		i := slices.IndexFunc(items, func(it menuItem) bool { return it.key == k })
		if i < 0 || items[i].label != want.label || items[i].ok != want.ok {
			t.Errorf("menu item %s: %+v, want %q (live %v)", k, items, want.label, want.ok)
		}
	}
	h.m.menu = ctxMenu{open: true, rowKey: h.m.menuOwner()}
	h.press("U")
	mustContain(t, h.view(), "Unmute #4602: resume automatic reviews of it? y/N")
	h.press("n")

	h.selectRow("PR-12371") // a task's row without a PR
	for _, it := range h.m.menuItems() {
		if slices.Contains([]string{"r", "R", "z", "M", "U"}, it.key) && it.ok {
			t.Errorf("the menu of a row without a PR offers %+v", it)
		}
	}

	p := testPublished(t, 140, 40)
	p.src.change(func(d *FactoryData) {
		c := d.Cards[1]
		c.Task.Retry = true
		d.Cards[1] = c
	})
	p.press("ctrl+r")
	p.open("PR-12320")
	var rs []string
	for _, it := range p.m.menuItems() {
		if it.key == "r" {
			rs = append(rs, it.label)
		}
	}
	if !slices.Equal(rs, []string{"retry"}) {
		t.Errorf("the r items of a card with a retry: %q, want the retry alone", rs)
	}
}
