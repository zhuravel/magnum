package engine

import (
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
