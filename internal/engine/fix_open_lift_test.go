package engine

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
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
	ev, ok := h.fixEvent("fix.open_answered")
	if !ok {
		t.Fatalf("no lift: state %s, last_error %q", h.pr(2).State, deref(h.pr(2).LastError))
	}
	if ev.Message != "you answered what the fixer left; autofix goes on" {
		t.Errorf("the lift says %q, want your answer", ev.Message)
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
	f.PR.State, f.Attention, f.Pending = store.PRNeedsAttention, attention.KindFix, 1
	if readWanted(f) {
		t.Error("a loop's needs you with a reply is read")
	}
	f.Raised = true
	if !readWanted(f) {
		t.Error("the look's own needs you with a reply is not read")
	}
	f.Fix = &FixState{}
	if readWanted(f) {
		t.Error("read while a loop runs")
	}

	const raisedMsg = "fix: 1 item the fixer left needs you: " + prPage + "#pullrequestreview-9001"
	l := openLedger{Raised: []string{"remark:9001"}, RaisedMsg: raisedMsg}
	pr := store.PR{State: store.PRNeedsAttention, LastError: new(raisedMsg)}
	if !l.RaisedHere(pr, attention.KindFix) {
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
		if c.l.RaisedHere(c.pr, c.kind) {
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
// the attention it raised, with no step of yours, and its event says that
// the reviewer approved, not that you answered. A change request of
// theirs after the approval takes it back: the item waits for you again,
// never for a fixer.
func TestTheReviewersLaterApprovalLiftsWhatTheyAsked(t *testing.T) {
	h := raisedChangeRequest(t)
	h.advance(10 * time.Minute)
	h.revAnnReviews(9002, "APPROVED", "")
	ev, ok := h.fixEvent("fix.open_answered")
	if !ok {
		t.Fatalf("no lift after the approval: state %s, last_error %q", h.pr(2).State, deref(h.pr(2).LastError))
	}
	var data struct{ Items, Accepted int }
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	if ev.Message != "the reviewer approved what the fixer left; autofix goes on" || data.Items != 1 || data.Accepted != 1 {
		t.Errorf("the lift says %q, %+v; want the reviewer's approval of the 1 item", ev.Message, data)
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

// leavesThenFixesCI is ciFixHarnessWith's PR #2, whose re-review of the
// CI fix's push finds a P2, with rev-ann's thread: the look's first loop
// leaves it to you, the CI fix (the fixer's second run) pushes while she
// asks for changes, and each later cycle answers the findings.
func leavesThenFixesCI(t *testing.T) *harness {
	t.Helper()
	h, git := ciFixHarnessWith(t, github.CheckPassed, func(in pipeline.RoundInput) (map[string]int, string) {
		if in.TargetSHA == fixPushed {
			return map[string]int{"P2": 1}, "COMMENTED"
		}
		return map[string]int{}, "COMMENTED"
	})
	h.setThreads(extThread(4322458360, "lib/sandbox.rb", 185, h.clock.Now().Add(-time.Hour), "Why does it run twice?", "rev-ann"))
	cycleFixer(h, git,
		func(id string) (string, string) { // loop 1 leaves her question to you
			return "", fixReport(id, "nothing", "1 threads (0 fixed, 0 already fixed, 1 not a bug, 0 won't fix)", "none")
		},
		func(id string) (string, string) { // the CI fix, while she asks for changes
			h.reviewerAsks("rev-ann", 9003, "No, drop it")
			return fixPushed, fixReport(id, fixPushed[:7]+" Keep the total in cents", "0 threads", "none")
		},
		func(id string) (string, string) {
			return "", fixReport(id, "nothing", fixReplied, "none")
		})
	h.pastSettle()
	h.tick() // loop 1, on her thread: findings first, the CI after
	h.advance(time.Minute)
	h.tick() // the look raises her thread
	h.wantState(2, store.PRNeedsAttention)
	return h
}

// redCompletion polls PR #2 at fixHead with its required check Completion
// failed, as ciFixHarnessWith opened it (touch leaves its checks out).
func (h *harness) redCompletion() {
	h.t.Helper()
	h.openOwn(prSpec{head: fixHead, ci: "FAILURE", checks: []github.Check{ciCompletion(github.CheckFailed)}})
	h.tick()
}

// wantCIFixLeavesHerAlone runs the CI fix's push to its re-review and the
// loop's next cycle, and fails unless that cycle's file marks rev-ann's
// thread and her change request made during the CI fix waiting.
func wantCIFixLeavesHerAlone(t *testing.T, h *harness) {
	t.Helper()
	for range 4 {
		h.advance(time.Minute)
		h.tick()
	}
	runs := h.fixRuns(h.pr(2).ID)
	if len(runs) != 3 {
		t.Fatalf("fix runs = %d (state %s), want loop 1, the CI fix and its next cycle", len(runs), h.pr(2).State)
	}
	x := readExternalOf(t, runs[2])
	threads := map[int64]bool{}
	for _, th := range x.Threads {
		threads[th.CommentID] = th.Waits
	}
	if !threads[4322458360] {
		t.Errorf("waits in the CI fix's next cycle = %v, want her thread left alone", threads)
	}
	if waits := remarkWaits(t, runs[2]); !waits[9003] {
		t.Errorf("remarks in the CI fix's next cycle = %v, want her change request during the CI fix waiting", waits)
	}
}

// A CI fix the look starts once the reviewer approved what the fixer left
// carries the waits a later loop does: its next cycles, on the re-review's
// findings, get a file that lists her accepted thread, which they leave
// alone, and her change request made while the CI fix ran, her pushback.
func TestALaterCIFixLeavesAnAcceptedItemAndThePushbackAlone(t *testing.T) {
	h := leavesThenFixesCI(t)
	h.advance(time.Minute)
	h.revAnnReviews(9002, "APPROVED", "")
	h.wantState(2, store.PRReviewed)
	h.redCompletion()
	h.pastSettle()
	h.tick() // the CI fix, its push's re-review and its next cycle
	wantCIFixLeavesHerAlone(t, h)
}

// A CI fix by hand (`magnum fix --ci`, the screens' c) carries the look's
// waits too: over the look's raise of her thread, its next cycles leave
// that thread to you, and her change request made while the CI fix ran.
func TestACIFixByHandLeavesWhatWaitsForYouAlone(t *testing.T) {
	h := leavesThenFixesCI(t)
	id := h.fix(FixPayload{Mode: FixModeCI})
	h.handle()
	h.wantDone(id)
	wantCIFixLeavesHerAlone(t, h)
}

// An approval accepts its reviewer's items made before it: a thread, a
// review body, a thread a fixer resolved. Never one whose comment the
// approval itself holds (approve with comments: a new ask), nor another
// reviewer's, nor one whose latest word came after it: the reviewer's
// reply in their thread, a review body edited since. A change request
// after it takes it back; a review that only comments leaves it standing.
func TestAnApprovalAcceptsTheReviewersEarlierItemsOnly(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	old := extThread(100, "a.rb", 1, at, "Why?", "rev-ann")
	old.Comments[0].ReviewID = 1
	inApproval := extThread(200, "b.rb", 2, at.Add(50*time.Minute), "Nit", "rev-ann") // written while the review was pending
	inApproval.Comments[0].ReviewID = 2
	alices := extThread(300, "c.rb", 3, at, "And this?", "alice")
	resolved := extThread(400, "d.rb", 4, at, "Rename it", "rev-ann")
	resolved.Resolved, resolved.ResolvedBy = true, "talkable[bot]"
	repliedSince := extThread(500, "e.rb", 5, at, "Why twice?", "rev-ann", "rev-ann")
	repliedSince.Comments[1].CreatedAt = at.Add(2 * time.Hour) // her reply after the approval
	body := changeRequest(1, at)
	approve := github.Review{DatabaseID: 2, State: "APPROVED", SubmittedAt: at.Add(time.Hour), AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"}
	editedSince := github.Review{DatabaseID: 4, State: "COMMENTED", Body: "Also log it", URL: prPage + "#pullrequestreview-4", SubmittedAt: at,
		EditedAt: at.Add(2 * time.Hour), AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"}
	in := externalInput{Threads: []github.Thread{old, inApproval, alices, resolved, repliedSince}, Reviews: []github.Review{body, approve, editedSince},
		CommentsComplete: true, Author: "zhuravel", Own: []string{"talkable[bot]"}, Resolvers: []string{"talkable[bot]"}}
	accepted := func() map[string]bool {
		out := map[string]bool{}
		for _, it := range othersItemsOf(externalReadOf(in, fixHead), quietRules{}) {
			out[it.Key] = it.Accepted
		}
		return out
	}
	edited := "remark:4@" + strconv.FormatInt(at.Add(2*time.Hour).Unix(), 10)
	want := map[string]bool{threadKey(100, 100): true, threadKey(200, 200): false, threadKey(300, 300): false, threadKey(400, 400): true,
		threadKey(500, 501): false, "remark:1": true, edited: false}
	if got := accepted(); !maps.Equal(got, want) {
		t.Errorf("accepted = %v, want %v", got, want)
	}
	in.Reviews = append(in.Reviews, github.Review{DatabaseID: 3, State: "COMMENTED", Body: "One more thing", SubmittedAt: at.Add(2 * time.Hour),
		AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"})
	if got := accepted(); !got[threadKey(100, 100)] || !got["remark:1"] || got["remark:3"] {
		t.Errorf("after a comment review: accepted = %v, want the approval standing, and the new remark open", got)
	}
	in.Reviews[3].State = "CHANGES_REQUESTED"
	for key, ok := range accepted() {
		if ok {
			t.Errorf("%s accepted after a later change request", key)
		}
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
	storetest.Parallel(t)
	answered := func(t *testing.T) *harness {
		t.Helper()
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

// A change request a reviewer makes after one of their items waited for
// you (Waited) waits for you too: no work, a need. One they made before
// the look saw their item wait is work, as their plain comment and another
// reviewer's change request are. The look records a reviewer, with the
// time, the first time one of their person's items waits for you.
func TestAChangeRequestWaitsForYouOnceTheReviewersItemWaited(t *testing.T) {
	pushback := openItem{Key: "remark:9002", Class: classPerson, Kind: itemReview, URL: prPage + "#pullrequestreview-9002", By: "Rev-Ann", ChangesAt: openNow.Add(-time.Minute)}
	earlier := openItem{Key: "remark:9001", Class: classPerson, Kind: itemReview, URL: prPage + "#pullrequestreview-9001", By: "rev-ann", ChangesAt: openNow.Add(-2 * time.Hour)}
	comment := openItem{Key: "remark:802", Class: classPerson, Kind: itemComment, URL: prPage + "#issuecomment-802", By: "rev-ann"}
	alices := openItem{Key: "remark:9003", Class: classPerson, Kind: itemReview, URL: prPage + "#pullrequestreview-9003", By: "alice", ChangesAt: openNow.Add(-time.Minute)}
	f := idleFacts()
	f.Items = []openItem{pushback, earlier, comment, alices}
	f.Ledger.NoteWaited([]openItem{
		{Key: "thread:100@100", Class: classPerson, Kind: itemThread, By: "rev-ann"},
		{Key: "thread:200@200", Class: classBot, Kind: itemThread, By: "lint-bot[bot]"},
		{Key: "rthread:300", Class: classMagnum, Kind: itemFinding},
	}, openNow.Add(-time.Hour))
	f.Ledger.NoteWaited([]openItem{{Key: "thread:100@101", Class: classPerson, Kind: itemThread, By: "rev-ann"}}, openNow)
	if want := map[string]time.Time{"rev-ann": openNow.Add(-time.Hour)}; !maps.Equal(f.Ledger.Waited, want) {
		t.Errorf("waited = %v, want %v", f.Ledger.Waited, want)
	}
	v, work, needs := decideOpen(f)
	if got := openKeys(work); len(needs) != 1 || needs[0].Key != pushback.Key || !slices.Equal(got, openKeys([]openItem{earlier, comment, alices})) {
		t.Errorf("work %v, needs %v; want rev-ann's later change request waiting, her earlier one, her comment and alice's change request as work", work, needs)
	}
	wantVerdict(t, v, OpenFixing, "1 item waits for you: 1 review body of a person")
}

// A loop the look starts carries a pushback key per reviewer of Waited
// beside the keys of what waits (loopWaits), and each cycle's file marks
// that reviewer's change requests made after the key's time waiting: not
// one made before, not their comment review, not another reviewer's.
func TestAPushbackKeyMarksTheReviewersLaterChangeRequestsWaiting(t *testing.T) {
	since := time.Date(2026, 11, 10, 9, 0, 0, 123, time.UTC)
	l := openLedger{Waited: map[string]time.Time{"rev-ann": since}}
	handed := personThread(4100)
	keys := l.LoopWaits(nil, []openItem{handed})
	if want := []string{handed.Key, "changes:rev-ann@" + strconv.FormatInt(since.UnixNano(), 10)}; !slices.Equal(keys, want) {
		t.Errorf("loop waits = %v, want %v", keys, want)
	}
	before, after, comment, alices := changeRequest(1, since.Add(-time.Second)), changeRequest(2, since.Add(time.Nanosecond)), changeRequest(3, since.Add(time.Minute)),
		changeRequest(4, since.Add(time.Minute))
	comment.State, alices.AuthorLogin = "COMMENTED", "alice"
	x, _ := externalOf(externalInput{Reviews: []github.Review{before, after, comment, alices}, CommentsComplete: true, Author: "zhuravel"})
	waits := map[int64]bool{}
	for _, r := range x.waiting(keys).Conversation {
		waits[r.ID] = r.Waits
	}
	if want := map[int64]bool{1: false, 2: true, 3: false, 4: false}; !maps.Equal(waits, want) {
		t.Errorf("waits = %v, want %v", waits, want)
	}
	if got := pushbacksOf([]string{"changes:", "changes:@1", "changes:rev-ann@x", "remark:9"}); got != nil {
		t.Errorf("pushbacks of malformed keys = %v, want none", got)
	}
}

// reviewerAsks adds login's change request review id with body on PR #2
// while a fixer works: GitHub lists it, with no poll, made a second after
// now (the fake clock stands still while the fixer works).
func (h *harness) reviewerAsks(login string, id int64, body string) {
	r := changeRequest(id, h.clock.Now().Add(time.Second))
	r.AuthorLogin, r.Body = login, body
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	if h.gh.allReviews == nil {
		h.gh.allReviews = map[int][]github.Review{}
	}
	h.gh.allReviews[2] = append(h.gh.allReviews[2], r)
}

// remarkWaits is the conversation of a fix run's external file: each
// remark's id and whether it is marked waiting.
func remarkWaits(t *testing.T, run store.Run) map[int64]bool {
	t.Helper()
	out := map[int64]bool{}
	for _, r := range readExternalOf(t, run).Conversation {
		out[r.ID] = r.Waits
	}
	return out
}

// A change request the reviewer made while a loop worked on her thread,
// before the look ever saw that thread wait for you, is a request of its
// own, not pushback: the look that sees the thread wait calls it work, and
// so does every later look while the start waits (here `magnum pause`),
// with nothing new on GitHub, so it reaches the next loop's fixer, and is
// never raised as an item the fixer left.
func TestAChangeRequestMadeBeforeTheReviewersItemWaitedIsWork(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.setThreads(extThread(4322458360, "lib/sandbox.rb", 185, time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local), "Why does it run twice?", "rev-ann"))
	cycleFixer(h, git,
		func(id string) (string, string) { // loop 1, while she asks for more
			h.reviewerAsks("rev-ann", 9005, "Also rename the flag to keep_stderr")
			return "", fixReport(id, "nothing", "1 threads (0 fixed, 0 already fixed, 1 not a bug, 0 won't fix)", "none")
		},
		func(id string) (string, string) {
			return "", fixReport(id, "nothing", "1 review body answered", "none")
		})
	h.tick() // reviewed
	h.pastSettle()
	h.tick() // loop 1 leaves the thread to you
	h.e.setKV(h.ctx, store.KVDaemonPaused, "1")
	h.advance(time.Minute)
	h.touch(fixHead) // the look sees the thread wait, and the change request as work; the start waits
	if _, ok := h.ledgerOf(2).Waited["rev-ann"]; !ok {
		t.Fatalf("waited = %v, want rev-ann's thread seen waiting", h.ledgerOf(2).Waited)
	}
	h.advance(time.Minute)
	h.tick() // nothing new on GitHub
	h.e.delKV(h.ctx, store.KVDaemonPaused)
	h.advance(time.Minute)
	h.tick()
	pr := h.pr(2)
	runs := h.fixRuns(pr.ID)
	if len(runs) != 2 {
		t.Fatalf("fix runs = %d (state %s, last_error %q): the earlier change request never reached a fixer", len(runs), pr.State, deref(pr.LastError))
	}
	if waits := remarkWaits(t, runs[1]); waits[9005] {
		t.Errorf("the second loop's file marks the earlier change request waiting: %v", waits)
	}
}

// Pushback counts from when the loop handed the reviewer's item, not from
// the first read that shows it wait: here your conversation comment, a
// reply magnum has not re-decided, holds the look's read from the loop's
// end (no raise holds the PR yet), and the reviewer asks for changes
// meanwhile. The first read after the reply's round sorts her change
// request as pushback beside her thread: both wait for you and are
// raised, and no loop starts on it.
func TestAChangeRequestAfterTheLoopHandedHerItemWaitsForYouBeforeAnyReadSawIt(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"), onlyTheAppPosts)
	h.setThreads(extThread(4322458360, "lib/sandbox.rb", 185, time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local), "Why does it run twice?", "rev-ann"))
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", "1 threads (0 fixed, 0 already fixed, 1 not a bug, 0 won't fix)", "none")
	})
	h.tick() // reviewed
	emptyRepliedRounds(h)
	h.pastSettle()
	h.tick() // loop 1 leaves her thread to you
	h.advance(time.Minute)
	yourComment(h, 801, "@rev-ann It runs twice on purpose") // the look hands her thread; its read waits for the reply
	if l := h.ledgerOf(2); len(l.Handed) == 0 || len(l.Waited) > 0 || len(l.Raised) > 0 {
		t.Fatalf("ledger handed %v, waited %v, raised %v; want her thread handed, unread", l.Handed, l.Waited, l.Raised)
	}
	h.advance(time.Minute)
	h.revAnnReviews(9002, "CHANGES_REQUESTED", "No, drop it")
	for range 3 { // the reply's round, the look's read, the settle
		h.advance(31 * time.Minute)
		h.tick()
	}
	pr := h.pr(2)
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Errorf("fix runs = %d: her pushback went to a fixer", n)
	}
	pr = h.wantState(2, store.PRNeedsAttention)
	if msg := deref(pr.LastError); !strings.Contains(msg, prPage+"#pullrequestreview-9002") || !strings.Contains(msg, prPage+"#discussion_r4322458360") {
		t.Errorf("last_error = %q, want her thread and her change request", msg)
	}
}

// A change request a reviewer makes while a loop runs beside her item that
// waits for you is pushback all the same: the loop's waits name her
// (FixState.Waits, pushbackKey), so the next cycle's file, read again from
// GitHub, marks it waiting and its fixer leaves it to you. Another
// reviewer's change request in that cycle is the fixer's to answer.
func TestAChangeRequestDuringALoopWaitsForYouInItsNextCycle(t *testing.T) {
	h, git := openHarness(t, func(in pipeline.RoundInput) (map[string]int, string) { // the re-review of the loop's push finds a P2
		if in.TargetSHA == fixPushed {
			return map[string]int{"P2": 1}, "COMMENTED"
		}
		return map[string]int{}, "COMMENTED"
	})
	person := extThread(4322458360, "lib/sandbox.rb", 185, h.clock.Now().Add(-time.Hour), "Why does it run twice?", "rev-ann")
	h.setThreads(person)
	cycleFixer(h, git,
		func(id string) (string, string) { // loop 1 leaves her question to you
			return "", fixReport(id, "nothing", "0 threads", "- 4322458360 lib/sandbox.rb:185: not fixed. only the author can say why")
		},
		func(id string) (string, string) { // loop 2's first cycle, while both reviewers ask for changes
			h.reviewerAsks("rev-ann", 9002, "No, drop it")
			h.reviewerAsks("alice", 9003, "Rename the spec")
			return fixPushed, fixReport(id, fixPushed[:7]+" Fix the lint", "1 threads (1 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
		},
		func(id string) (string, string) {
			return "", fixReport(id, "nothing", "1 review body answered", "none")
		})
	h.tick() // reviewed
	h.pastSettle()
	h.tick() // loop 1
	pr := h.wantState(2, store.PRNeedsAttention)
	id := h.fix(FixPayload{Done: true, Seen: deref(pr.LastError)})
	h.handle()
	h.wantDone(id)
	h.setThreads(person, extThread(4322458500, "spec/a_spec.rb", 12, h.clock.Now(), "Repeated describe block", "lint-bot"))
	h.advance(time.Minute)
	h.touch(fixHead)
	h.pastSettle()
	h.tick() // loop 2 on the lint thread pushes, its re-review finds a P2, and its second cycle runs
	runs := h.fixRuns(pr.ID)
	if len(runs) != 3 {
		t.Fatalf("fix runs = %d, want loop 2's second cycle", len(runs))
	}
	if waits := remarkWaits(t, runs[2]); !waits[9002] || waits[9003] {
		t.Errorf("waits in cycle 2's file = %v, want rev-ann's change request alone", waits)
	}
}

// The lift's event says who closed what the fixer left: you, the
// reviewer's later approval, or both, one reviewer or more.
func TestTheLiftSaysWhoClosedWhatTheFixerLeft(t *testing.T) {
	for _, c := range []struct {
		n         int
		approvers []string
		want      string
	}{
		{2, nil, "you answered what the fixer left; autofix goes on"},
		{1, []string{"rev-ann"}, "the reviewer approved what the fixer left; autofix goes on"},
		{2, []string{"rev-ann", "rev-ann"}, "the reviewer approved what the fixer left; autofix goes on"},
		{2, []string{"rev-ann", "alice"}, "the reviewers approved what the fixer left; autofix goes on"},
		{3, []string{"rev-ann"}, "you answered what the fixer left, and the reviewer approved the rest; autofix goes on"},
	} {
		if got := liftWords(c.n, c.approvers); got != c.want {
			t.Errorf("liftWords(%d, %v) = %q, want %q", c.n, c.approvers, got, c.want)
		}
	}
}
