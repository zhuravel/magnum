package engine

import (
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// A colleague's change request on an own PR waited for the author, handed
// to an earlier loop, while a lint bot's thread on the merge's code sat
// beside it, never handed: rule 8 said needs you before rule 9 looked at the
// work, so the small fix waited on the person. Work never handed now starts
// a loop while the items that wait are all a person's: they stay listed as
// needs in the verdict and its words. Anything else handed and still open
// (magnum's finding skipped without a word, a bot's thread, a conflict)
// still needs you first, and so do a person's items with no work beside
// them.
func TestTheLookStartsOnNeverHandedWorkWhileAPersonsItemsWait(t *testing.T) {
	person := personThread(4100)
	lint := openItem{Key: "thread:4200@4200", Class: classBot, Kind: itemThread, URL: prPage + "#discussion_r4200"}
	f := idleFacts()
	f.items = []openItem{person, lint}
	f.ledger.Handed = map[string]time.Time{person.Key: openNow}
	v, work, needs := decideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop on 1 thread of a bot; 1 item waits for you: 1 thread of a person")
	if len(work) != 1 || work[0].Key != lint.Key || len(needs) != 1 || needs[0].Key != person.Key || len(v.Needs) != 1 || v.Needs[0].URL != person.URL {
		t.Errorf("work %v, needs %v, verdict needs %+v; want the bot's thread as work and the person's listed", work, needs, v.Needs)
	}
	if v.leftovers {
		t.Error("the person's item is raised while the loop starts")
	}
	skipped := openItem{Key: "rthread:5000", Class: classMagnum, Kind: itemFinding, URL: prPage + "#discussion_r5000"}
	f.items = []openItem{person, skipped, lint}
	v, work, _ = decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "2 items need you")
	if len(work) != 0 {
		t.Errorf("work = %v beside magnum's skipped finding", work)
	}
	f.items = []openItem{person}
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "1 item needs you: 1 thread of a person")
}

// A person's handed items wait beside work only for the loop the work
// starts. While the work itself waits (a pause, quiet hours, the settle,
// the hands off after your push) the verdict still names the person's
// items, which the look raises once the loop ran; raising them now would
// hold the PR in needs attention, and no loop would start on the work. At
// the daily cap no loop starts today: rule 8 then raises them, as before.
func TestAPersonsWaitingItemsStayNamedWhileTheWorkCannotStart(t *testing.T) {
	person := personThread(4100)
	lint := openItem{Key: "thread:4200@4200", Class: classBot, Kind: itemThread, URL: prPage + "#discussion_r4200"}
	facts := func() openFacts {
		f := idleFacts()
		f.items = []openItem{person, lint}
		f.ledger.Handed = map[string]time.Time{person.Key: openNow}
		return f
	}
	f := facts()
	f.pause = "fixes paused"
	v, _, needs := decideOpen(f)
	wantVerdict(t, v, OpenWaits, "fixes paused: 1 thread of a bot waits for `magnum resume`; 1 item waits for you: 1 thread of a person")
	if v.leftovers || len(needs) != 1 || len(v.Needs) != 1 {
		t.Errorf("paused: leftovers %v, needs %v, verdict needs %+v; want the person's item listed, not raised", v.leftovers, needs, v.Needs)
	}
	f = facts()
	act := openNow.Add(-time.Minute)
	f.pr.HeadChangedAt, f.pr.ActivityAt = openNow.Add(-10*time.Minute), &act
	v, _, _ = decideOpen(f)
	wantVerdict(t, v, OpenWaits, "settling: 1 thread of a bot waits until "+waitClock(act.Add(3*time.Minute), openNow)+
		"; 1 item waits for you: 1 thread of a person")
	if v.leftovers {
		t.Error("settling: the person's item is raised")
	}
	f = facts()
	f.ledger.Starts = []time.Time{openNow.Add(-5 * time.Hour), openNow.Add(-3 * time.Hour), openNow.Add(-time.Hour)}
	v, work, _ := decideOpen(f)
	wantVerdict(t, v, OpenNeedsYou, "1 item needs you: 1 thread of a person")
	if !v.leftovers || len(work) != 0 {
		t.Errorf("at the cap: leftovers %v, work %v; want the person's item raised as rule 8 does", v.leftovers, work)
	}
}

// The loop the look starts while a person's handed items wait is limited to
// the work never handed: its fixer's file marks each waiting item `waits`
// (FixState.Waits, the items' keys), which the skill leaves alone, so no
// push or reply answers a colleague's open change request before the
// author did; the item it never had is not marked.
func TestTheLoopOverAPersonsWaitingItemLeavesItAlone(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	person := extThread(4322458360, "lib/sandbox.rb", 185, h.clock.Now().Add(-time.Hour), "Why does it run twice?", "rev-ann")
	h.setThreads(person)
	cycleFixer(h, git,
		func(id string) (string, string) {
			return "", fixReport(id, "nothing", "0 threads", "- 4322458360 lib/sandbox.rb:185: not fixed. only the author can say why")
		},
		func(id string) (string, string) {
			return "", fixReport(id, "nothing", "1 threads (0 fixed, 0 already fixed, 1 not a bug, 0 won't fix)", "none")
		})
	h.tick() // reviewed
	h.pastSettle()
	h.tick() // loop 1 leaves the colleague's question to you
	pr := h.wantState(2, store.PRNeedsAttention)
	id := h.fix(FixPayload{Done: true, Seen: deref(pr.LastError)})
	h.handle()
	h.wantDone(id)
	lint := extThread(4322458500, "spec/a_spec.rb", 12, h.clock.Now(), "Repeated describe block", "lint-bot")
	h.setThreads(person, lint)
	h.advance(time.Minute)
	h.touch(fixHead)
	h.pastSettle()
	h.tick()
	runs := h.fixRuns(pr.ID)
	if len(runs) != 2 {
		t.Fatalf("fix runs = %d, want a second loop on the lint thread while the colleague's question waits", len(runs))
	}
	if st := h.ledgerOf(2); len(st.Starts) != 2 {
		t.Errorf("ledger starts = %v", st.Starts)
	}
	x := readExternalOf(t, runs[1])
	waits := map[int64]bool{}
	for _, th := range x.Threads {
		waits[th.CommentID] = th.Waits
	}
	if !waits[4322458360] || waits[4322458500] {
		t.Errorf("waits in the second loop's file = %v, want the colleague's thread alone", waits)
	}
}
