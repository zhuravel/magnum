package engine

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// changeRequest is rev-ann's change request on PR #2, at at.
func changeRequest(id int64, at time.Time) github.Review {
	return github.Review{DatabaseID: id, State: "CHANGES_REQUESTED", Body: "Drop the flag for every model", URL: prPage + "#pullrequestreview-" + itoa(id),
		SubmittedAt: at, AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"}
}

// onlyTheAppPosts leaves the harness the talkable watch alone, which posts
// as its App: the operator's own comments are no longer magnum's (the
// zhuravel watch posts as their login), so the author's comment is a reply
// to magnum's review, as on the live PR.
func onlyTheAppPosts(h *harness) { h.cfg.Watches = h.cfg.Watches[:1] }

// raisedChangeRequest runs, on openHarness's PR #2 (a clean review of
// magnum's), one loop handed rev-ann's change request, whose fixer leaves it
// to the author, and the look that raises it.
func raisedChangeRequest(t *testing.T) *harness {
	t.Helper()
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"), onlyTheAppPosts)
	h.gh.mu.Lock()
	h.gh.allReviews = map[int][]github.Review{2: {changeRequest(9001, h.clock.Now().Add(-time.Hour))}}
	h.gh.mu.Unlock()
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", "0 threads", "none") })
	h.tick() // reviewed
	h.pastSettle()
	h.tick() // the loop leaves the change request to you
	h.advance(time.Minute)
	h.tick() // the look raises it
	pr := h.wantState(2, store.PRNeedsAttention)
	if l := h.ledgerOf(2); l.RaisedMsg == "" || deref(pr.LastError) != l.RaisedMsg {
		t.Fatalf("last_error %q, the look's message %q: want the look's own raise", deref(pr.LastError), l.RaisedMsg)
	}
	return h
}

// yourComment posts the operator's conversation comment on PR #2 now, and
// polls it: GitHub lists it among the comments and as the author's remark,
// a reply magnum's judge has not re-decided.
func yourComment(h *harness, id int64, body string) {
	h.t.Helper()
	at := h.clock.Now()
	h.gh.mu.Lock()
	if h.gh.prComments == nil {
		h.gh.prComments = map[int][]github.PRComment{}
	}
	h.gh.prComments[2] = append(h.gh.prComments[2], github.PRComment{ID: id, URL: prPage + "#issuecomment-" + itoa(id), Body: body, CreatedAt: at,
		AuthorLogin: "zhuravel", AuthorType: "User"})
	h.gh.mu.Unlock()
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: "zhuravel", remarks: []github.Remark{{At: at, Author: "zhuravel"}}})
	h.tick()
}

// The live case (2026-11-09): the look raised a colleague's change request
// that a loop left to the author, and the author answered it with a
// conversation comment. The comment is a reply to magnum's review too
// (repliesOf), so the PR held a reply the judge had not re-decided: the
// look's read waited for that reply (readWanted), and a PR in needs
// attention gets no reply round (considerReplies), so the look never read
// the answer, never lifted, and the reply was never re-decided, for 20
// hours, until a `magnum fix` by hand. The look now reads a PR held by its
// own raise despite the reply, lifts the attention on your answer, and the
// reply gets its round.
func TestYourConversationReplyLiftsTheLooksOwnNeedsYou(t *testing.T) {
	h := raisedChangeRequest(t)
	h.advance(10 * time.Minute)
	yourComment(h, 801, "@rev-ann The other models keep it, see the follow-up PR")
	if n := len(h.pr(2).PendingReplies()); n != 1 {
		t.Fatalf("pending replies = %d, want your comment", n)
	}
	emptyRepliedRounds(h)
	for range 3 { // the 30-minute safety read included
		h.advance(31 * time.Minute)
		h.tick()
	}
	if _, ok := h.fixEvent("fix.open_answered"); !ok {
		t.Fatalf("no lift: state %s, last_error %q", h.pr(2).State, deref(h.pr(2).LastError))
	}
	pr := h.wantState(2, store.PRReviewed)
	if n := len(pr.PendingReplies()); n != 0 {
		t.Errorf("pending replies = %d after the lift, want your comment re-decided", n)
	}
	ins := h.rd.all()
	if in := ins[len(ins)-1]; in.Replies != 1 || !in.SameHead {
		t.Errorf("last round: replies %d, same head %v; want the reply round", in.Replies, in.SameHead)
	}
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Errorf("fix runs = %d: a loop ran beside the reply's round", n)
	}
}

// A loop's own Needs you (here a push that failed) keeps its rules when a
// reply of yours is pending: the look's read waits for the reply's round,
// as before, and the attention is yours to act on (f, a push, r), never the
// look's to lift, whatever you answered.
func TestALoopsNeedsYouWithAReplyKeepsItsRules(t *testing.T) {
	th := extThread(4322458360, "lib/sandbox.rb", 185, time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local), "MINOR: `run` drops stderr", "rev-ann")
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"), onlyTheAppPosts)
	h.setThreads(th)
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "push failed: remote rejected", "0 threads", "none")
	})
	h.tick()
	h.pastSettle()
	h.tick() // the loop fails on its push
	msg := deref(h.wantState(2, store.PRNeedsAttention).LastError)
	h.advance(time.Minute)
	h.tick()
	read := h.ledgerOf(2).Read.At
	h.setThreads(extThread(4322458360, "lib/sandbox.rb", 185, time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local), "MINOR: `run` drops stderr", "rev-ann", "zhuravel"))
	h.advance(time.Minute)
	yourComment(h, 801, "@rev-ann It keeps stderr on purpose")
	for range 2 {
		h.advance(31 * time.Minute)
		h.tick()
	}
	if got := deref(h.wantState(2, store.PRNeedsAttention).LastError); got != msg {
		t.Errorf("last_error = %q, want the loop's %q", got, msg)
	}
	if at := h.ledgerOf(2).Read.At; !at.Equal(read) {
		t.Errorf("the look read GitHub at %v under a loop's Needs you with a reply pending (last read %v)", at, read)
	}
	if _, ok := h.fixEvent("fix.open_answered"); ok {
		t.Error("the look lifted a loop's Needs you")
	}
}

// The look's read waits for a reply's round, as before, on a PR a loop's or
// a round's Needs you holds: only the look's own raise, which no reply
// round can take, is read despite the reply.
func TestOnlyTheLooksOwnRaiseIsReadDespiteAReply(t *testing.T) {
	f := idleFacts()
	f.pr.State, f.attention, f.pending = store.PRNeedsAttention, attention.KindFix, 1
	if readWanted(f) {
		t.Error("a loop's needs you with a reply is read")
	}
	f.raised = true
	if !readWanted(f) {
		t.Error("the look's own needs you with a reply is not read")
	}
	f.fix = &FixState{}
	if readWanted(f) {
		t.Error("read while a loop runs")
	}

	const raisedMsg = "fix: 1 item the fixer left needs you: " + prPage + "#pullrequestreview-9001"
	l := openLedger{Raised: []string{"remark:9001"}, RaisedMsg: raisedMsg}
	pr := store.PR{State: store.PRNeedsAttention, LastError: new(raisedMsg)}
	if !l.raisedHere(pr, attention.KindFix) {
		t.Error("the look's own raise is not its own")
	}
	for name, c := range map[string]struct {
		pr   store.PR
		kind string
		l    openLedger
	}{
		"a loop's words":    {store.PR{State: store.PRNeedsAttention, LastError: new("fix: claude-fix's push failed: remote rejected")}, attention.KindFix, l},
		"a round's kind":    {pr, "attention", l},
		"reviewed":          {store.PR{State: store.PRReviewed, LastError: new(raisedMsg)}, attention.KindFix, l},
		"nothing raised":    {pr, attention.KindFix, openLedger{RaisedMsg: raisedMsg}},
		"no message of its": {pr, attention.KindFix, openLedger{Raised: l.Raised}},
	} {
		if c.l.raisedHere(c.pr, c.kind) {
			t.Errorf("%s: the look takes the attention for its own", name)
		}
	}
}

// revAnnReviews adds rev-ann's review in state with body on PR #2 now, and
// polls the PR (GitHub moved its updatedAt).
func (h *harness) revAnnReviews(id int64, state, body string) {
	h.t.Helper()
	r := changeRequest(id, h.clock.Now())
	r.State, r.Body = state, body
	h.gh.mu.Lock()
	if h.gh.allReviews == nil {
		h.gh.allReviews = map[int][]github.Review{}
	}
	h.gh.allReviews[2] = append(h.gh.allReviews[2], r)
	h.gh.mu.Unlock()
	h.touch(fixHead)
}

// The reviewer whose change request waited for you approves the PR later,
// and you never answered it: the approval accepts it, and the look lifts
// the attention it raised, with no step of yours. A change request of
// theirs after the approval takes it back: the item waits for you again,
// never for a fixer.
func TestTheReviewersLaterApprovalLiftsWhatTheyAsked(t *testing.T) {
	h := raisedChangeRequest(t)
	h.advance(10 * time.Minute)
	h.revAnnReviews(9002, "APPROVED", "")
	if _, ok := h.fixEvent("fix.open_answered"); !ok {
		t.Fatalf("no lift after the approval: state %s, last_error %q", h.pr(2).State, deref(h.pr(2).LastError))
	}
	h.wantState(2, store.PRReviewed)
	h.wantOpen(OpenIdle, "nothing open")
	h.advance(10 * time.Minute)
	h.revAnnReviews(9003, "CHANGES_REQUESTED", "")
	pr := h.wantState(2, store.PRNeedsAttention)
	if !strings.Contains(deref(pr.LastError), prPage+"#pullrequestreview-9001") {
		t.Errorf("last_error = %q, want the change request that waits for you again", deref(pr.LastError))
	}
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Errorf("fix runs = %d: the item that waits for you went to a fixer", n)
	}
}

// An item its reviewer accepted waits for no one, but a loop that starts
// on other work later still leaves it alone: an earlier fixer left it to
// you, and the next one, which lists it unanswered in its file, must not
// answer it, nor name it under Needs you again.
func TestALaterLoopLeavesAnAcceptedItemAlone(t *testing.T) {
	person := extThread(4322458360, "lib/sandbox.rb", 185, time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local), "Why does it run twice?", "rev-ann")
	h := loopLeaves(t, person)
	pr := h.wantState(2, store.PRNeedsAttention)
	h.advance(time.Minute)
	h.revAnnReviews(9002, "APPROVED", "")
	h.wantState(2, store.PRReviewed)
	lint := extThread(4322458500, "spec/a_spec.rb", 12, h.clock.Now(), "Repeated describe block", "lint-bot")
	h.setThreads(person, lint)
	h.advance(time.Minute)
	h.touch(fixHead)
	h.pastSettle()
	h.tick()
	runs := h.fixRuns(pr.ID)
	if len(runs) != 2 {
		t.Fatalf("fix runs = %d, want a second loop on the lint thread", len(runs))
	}
	waits := map[int64]bool{}
	for _, th := range readExternalOf(t, runs[1]).Threads {
		waits[th.CommentID] = th.Waits
	}
	if !waits[4322458360] || waits[4322458500] {
		t.Errorf("waits in the second loop's file = %v, want the accepted thread alone", waits)
	}
}

// An approval accepts its reviewer's items made before it: a thread, a
// review body, a thread a fixer resolved. Never one whose comment the
// approval itself holds (approve with comments: a new ask), nor another
// reviewer's. A change request after it takes it back; a review that only
// comments leaves it standing.
func TestAnApprovalAcceptsTheReviewersEarlierItemsOnly(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	old := extThread(100, "a.rb", 1, at, "Why?", "rev-ann")
	old.Comments[0].ReviewID = 1
	inApproval := extThread(200, "b.rb", 2, at.Add(50*time.Minute), "Nit", "rev-ann") // written while the review was pending
	inApproval.Comments[0].ReviewID = 2
	alices := extThread(300, "c.rb", 3, at, "And this?", "alice")
	resolved := extThread(400, "d.rb", 4, at, "Rename it", "rev-ann")
	resolved.Resolved, resolved.ResolvedBy = true, "talkable[bot]"
	body := changeRequest(1, at)
	approve := github.Review{DatabaseID: 2, State: "APPROVED", SubmittedAt: at.Add(time.Hour), AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"}
	in := externalInput{Threads: []github.Thread{old, inApproval, alices, resolved}, Reviews: []github.Review{body, approve}, CommentsComplete: true,
		Author: "zhuravel", Own: []string{"talkable[bot]"}, Resolvers: []string{"talkable[bot]"}}
	accepted := func() map[string]bool {
		out := map[string]bool{}
		for _, it := range othersItemsOf(externalReadOf(in, fixHead), quietRules{}) {
			out[it.Key] = it.Accepted
		}
		return out
	}
	want := map[string]bool{threadKey(100, 100): true, threadKey(200, 200): false, threadKey(300, 300): false, threadKey(400, 400): true, "remark:1": true}
	if got := accepted(); !maps.Equal(got, want) {
		t.Errorf("accepted = %v, want %v", got, want)
	}
	in.Reviews = append(in.Reviews, github.Review{DatabaseID: 3, State: "COMMENTED", Body: "One more thing", SubmittedAt: at.Add(2 * time.Hour),
		AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"})
	if got := accepted(); !got[threadKey(100, 100)] || !got["remark:1"] || got["remark:3"] {
		t.Errorf("after a comment review: accepted = %v, want the approval standing, and the new remark open", got)
	}
	in.Reviews[2].State = "CHANGES_REQUESTED"
	for key, ok := range accepted() {
		if ok {
			t.Errorf("%s accepted after a later change request", key)
		}
	}
}

// An accepted item waits for no one where it would wait for you (handed to
// a loop, a thread a fixer resolved): no work and no need. One never handed
// is work as before: a reviewer's comment and a separate approval still ask
// for the fix.
func TestAnAcceptedItemWaitsForNoOneButWorkStaysWork(t *testing.T) {
	handed, fresh, resolved := personThread(4100), personThread(4200), personThread(4300)
	handed.Accepted, fresh.Accepted = true, true
	resolved.Resolved, resolved.Accepted = true, true
	f := idleFacts()
	f.items = []openItem{handed, fresh, resolved}
	f.ledger.Handed = map[string]time.Time{handed.Key: openNow}
	v, work, needs := decideOpen(f)
	wantVerdict(t, v, OpenFixing, "starts a fix loop on 1 thread of a person")
	if len(work) != 1 || work[0].Key != fresh.Key || len(needs) != 0 {
		t.Errorf("work %v, needs %v; want the never-handed thread as work alone", work, needs)
	}
	if got := waitsOf(acceptedWaits(f.items, f.ledger)); !slices.Equal(got, []string{handed.Key}) {
		t.Errorf("waits = %v, want the handed accepted thread", got)
	}
}

// revAnnComments adds rev-ann's conversation comment on PR #2 now, and
// polls the PR.
func (h *harness) revAnnComments(id int64, body string) {
	h.t.Helper()
	h.gh.mu.Lock()
	if h.gh.prComments == nil {
		h.gh.prComments = map[int][]github.PRComment{}
	}
	h.gh.prComments[2] = append(h.gh.prComments[2], github.PRComment{ID: id, URL: prPage + "#issuecomment-" + itoa(id), Body: body,
		CreatedAt: h.clock.Now(), AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"})
	h.gh.mu.Unlock()
	h.touch(fixHead)
}

// You answered the change request that waited for you, which lifted the
// attention; then the reviewer wrote again. A plain comment of theirs is an
// item of its own, which a loop works on. A change request instead is
// pushback: it waits for you, as the item it answers did, never for a
// fixer, and the look raises it.
func TestTheReviewersPlainReplyIsItsOwnItemAndAChangeRequestWaitsForYou(t *testing.T) {
	answered := func(t *testing.T) *harness {
		h := raisedChangeRequest(t)
		emptyRepliedRounds(h)
		h.advance(10 * time.Minute)
		yourComment(h, 801, "@rev-ann It is kept on purpose")
		h.advance(10 * time.Minute)
		h.tick() // the reply round
		h.wantState(2, store.PRReviewed)
		h.advance(10 * time.Minute)
		return h
	}
	t.Run("a plain comment", func(t *testing.T) {
		h := answered(t)
		h.revAnnComments(802, "OK, then document it")
		h.pastSettle()
		h.tick()
		if n := len(h.fixRuns(h.pr(2).ID)); n != 2 {
			t.Errorf("fix runs = %d, want a loop on the reviewer's new comment", n)
		}
	})
	t.Run("a change request", func(t *testing.T) {
		h := answered(t)
		h.revAnnReviews(9002, "CHANGES_REQUESTED", "No, drop it")
		h.pastSettle()
		h.tick()
		if n := len(h.fixRuns(h.pr(2).ID)); n != 1 {
			t.Errorf("fix runs = %d: the reviewer's pushback went to a fixer", n)
		}
		pr := h.wantState(2, store.PRNeedsAttention)
		if !strings.Contains(deref(pr.LastError), prPage+"#pullrequestreview-9002") {
			t.Errorf("last_error = %q, want the pushback's link", deref(pr.LastError))
		}
	})
}

// A change request of a reviewer one of whose items waited for you
// (Waited) waits for you too: no work, a need. Their plain comment, and
// another reviewer's change request, are work as before. The look records
// a reviewer the first time one of their person's items waits for you.
func TestAChangeRequestWaitsForYouOnceTheReviewersItemWaited(t *testing.T) {
	pushback := openItem{Key: "remark:9002", Class: classPerson, Kind: itemReview, URL: prPage + "#pullrequestreview-9002", By: "Rev-Ann", Changes: true}
	comment := openItem{Key: "remark:802", Class: classPerson, Kind: itemComment, URL: prPage + "#issuecomment-802", By: "rev-ann"}
	alices := openItem{Key: "remark:9003", Class: classPerson, Kind: itemReview, URL: prPage + "#pullrequestreview-9003", By: "alice", Changes: true}
	f := idleFacts()
	f.items = []openItem{pushback, comment, alices}
	f.ledger.noteWaited([]openItem{
		{Key: "thread:100@100", Class: classPerson, Kind: itemThread, By: "rev-ann"},
		{Key: "thread:200@200", Class: classBot, Kind: itemThread, By: "lint-bot[bot]"},
		{Key: "rthread:300", Class: classMagnum, Kind: itemFinding},
	}, openNow.Add(-time.Hour))
	f.ledger.noteWaited([]openItem{{Key: "thread:100@101", Class: classPerson, Kind: itemThread, By: "rev-ann"}}, openNow)
	if want := map[string]time.Time{"rev-ann": openNow.Add(-time.Hour)}; !maps.Equal(f.ledger.Waited, want) {
		t.Errorf("waited = %v, want %v", f.ledger.Waited, want)
	}
	v, work, needs := decideOpen(f)
	if len(needs) != 1 || needs[0].Key != pushback.Key || len(work) != 2 || work[0].Key != comment.Key || work[1].Key != alices.Key {
		t.Errorf("work %v, needs %v; want rev-ann's change request waiting, her comment and alice's change request as work", work, needs)
	}
	wantVerdict(t, v, OpenFixing, "1 item waits for you: 1 review body of a person")
}
