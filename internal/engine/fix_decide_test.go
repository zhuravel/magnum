package engine

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

// decideThread is rev-ann's thread on PR #2 that the fixer leaves to you.
const decideThread = 4322458360

// decideCall is the call the fixer's "not fixed." entry in decideThread
// makes.
const decideCall = "decide another reviewer's finding on lib/sandbox.rb:185: the fixer declined it; reply won't fix at " +
	prPage + "#discussion_r4322458360, or `magnum fix talkable/talkable#2` sends it back"

// noReplies is a report's "## Replied" line of a fixer that replied nowhere.
const noReplies = "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)"

// authorSays is th with a comment of the PR author's own (no marker), body,
// at at: a reply the operator typed on GitHub.
func authorSays(th github.Thread, id int64, body string, at time.Time) github.Thread {
	th.Comments = append(slices.Clone(th.Comments), github.ThreadComment{ID: id, AuthorLogin: "zhuravel", AuthorType: "User",
		AuthorAssociation: "OWNER", Body: body, URL: prPage + "#discussion_r" + itoa(id), CreatedAt: at})
	return th
}

// fixerSays is th with a fix-marked reply of the fixer's, posted as the
// author (a gh fixer), at at.
func fixerSays(th github.Thread, id int64, body string, at time.Time) github.Thread {
	return authorSays(th, id, body+"\n\n"+postreview.FixReplyMarker, at)
}

// leftDecision is the first fixer's "## Needs you" in fixLeftADecision:
// rev-ann's thread, declined as a product choice.
const leftDecision = "- 4322458360 lib/sandbox.rb:185: not fixed. keeping stderr is a product choice"

// leftDescription is a "## Needs you" entry beside it that asks something
// else of you: the description, which the attention names in the older
// words.
const leftDescription = "- 5808905525 description: not fixed. the title is the product's call"

// fixLeftADecision runs one loop on PR #2, whose review found nothing while
// rev-ann's thread waited: the fixer reads keeping stderr as a product
// choice and leaves it to you, with no reply, as on a live own PR on
// 2026-11-07. The PR needs attention with the call to decide it. next is
// the fixer of each later run (nil: one that does nothing).
func fixLeftADecision(t *testing.T, next func(h *harness, run store.Run) string, mods ...func(*harness)) (*harness, *fixGit) {
	t.Helper()
	return fixLeft(t, leftDecision, next, mods...)
}

// fixLeft is fixLeftADecision whose first fixer leaves needs (its report's
// "## Needs you", rev-ann's thread among them).
func fixLeft(t *testing.T, needs string, next func(h *harness, run store.Run) string, mods ...func(*harness)) (*harness, *fixGit) {
	t.Helper()
	h, git := fixHarness(t, mods...)
	h.gh.clock = h.clock.Now // a reply --decide posts is made now
	fixablePRResult(h, "zhuravel", map[string]any{"event": "COMMENTED", "findings": map[string]int{}})
	h.setThreads(h.colleagueThread(decideThread))
	var mu sync.Mutex
	runs := 0
	fixerBy(h, git, func(run store.Run) (string, string) {
		mu.Lock()
		runs++
		first := runs == 1
		mu.Unlock()
		if first {
			return "", fixReport(run.ID, "nothing", noReplies, needs)
		}
		if next == nil {
			return "", fixReport(run.ID, "nothing", noReplies, "none")
		}
		return "", next(h, run)
	})
	h.fix(FixPayload{})
	h.handle()
	pr := h.wantState(2, store.PRNeedsAttention)
	if msg := deref(pr.LastError); !strings.Contains(msg, decideCall) {
		t.Fatalf("last_error = %q, want the call to decide rev-ann's thread", msg)
	}
	h.advance(10 * time.Minute)
	return h, git
}

// decide makes the author's reply in decideThread on GitHub now: the
// decision.
func (h *harness) decide(body string) github.Thread {
	h.t.Helper()
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	th := authorSays(h.gh.threads[2][0], 4322458999, body, h.clock.Now())
	h.gh.threads[2] = []github.Thread{th}
	return th
}

// appSays is th with a comment of magnum's App, with no marker, at at.
func appSays(th github.Thread, id int64, body string, at time.Time) github.Thread {
	th.Comments = append(slices.Clone(th.Comments), github.ThreadComment{ID: id, AuthorLogin: "talkable", AuthorType: "Bot",
		Body: body, URL: prPage + "#discussion_r" + itoa(id), CreatedAt: at})
	return th
}

// The fixer's call in another reviewer's thread asks you to decide; your
// reply there, the thread's last comment, made after the call while the
// call is open, is that decision: the external file marks the thread
// decision (with the reply's link) and not answered, so the fixer builds it.
// Only your own word counts: a reply of yours made before the call, one in
// a thread the fixer made no call in, the fixer's own fix-marked reply (it
// posts as you), another reviewer's reply after yours and a reply after the
// fixer already answered since the call are no decision. Nor is a reply of
// yours made while a fix run of the PR was at work (Busy): a gh fixer posts
// as you, and an unmarked reply of its own cannot be told from yours, so
// your decision before it stands; nor an unmarked word of magnum's logins.
func TestTheAuthorsReplyAfterTheFixersCallIsADecision(t *testing.T) {
	call := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	root := func(id int64) github.Thread {
		return extThread(id, "app/a.rb", 3, call.Add(-time.Hour), "Should this keep stderr?", "rev-ann")
	}
	threads := []github.Thread{
		authorSays(root(100), 101, "Keep it, and fix the drop.", call.Add(time.Minute)),                                            // the decision
		authorSays(root(200), 201, "Keep it.", call.Add(-time.Minute)),                                                             // before the call
		authorSays(root(300), 301, "Keep it.", call.Add(time.Minute)),                                                              // no call there
		fixerSays(root(400), 401, "Won't fix: as decided.", call.Add(time.Minute)),                                                 // the fixer's own
		authorSays(root(500), 501, "Keep it.", call.Add(time.Minute)),                                                              // then rev-ann again
		fixerSays(authorSays(root(600), 601, "x", call.Add(-time.Hour)), 602, "Answer: ok", call.Add(30*time.Second)),              // the fixer answered since the call
		authorSays(authorSays(root(700), 701, "Fix the drop.", call.Add(time.Minute)), 702, "Won't fix.", call.Add(5*time.Minute)), // then an unmarked reply during a run
		authorSays(root(800), 801, "Won't fix.", call.Add(5*time.Minute)),                                                          // only a reply during a run
		appSays(authorSays(root(900), 901, "Fix the drop.", call.Add(time.Minute)), 902, "Won't fix.", call.Add(2*time.Minute)),    // then magnum's App, unmarked
	}
	threads[4].Comments = append(threads[4].Comments, github.ThreadComment{ID: 502, AuthorLogin: "rev-ann", AuthorType: "User",
		AuthorAssociation: "MEMBER", Body: "Why?", CreatedAt: call.Add(2 * time.Minute)})
	threads[5] = authorSays(threads[5], 603, "Keep it.", call.Add(time.Minute))
	calls := map[int64]time.Time{100: call, 200: call, 400: call, 500: call, 600: call, 700: call, 800: call, 900: call}
	busy := func(at time.Time) bool {
		return at.After(call.Add(4*time.Minute)) && at.Before(call.Add(6*time.Minute))
	} // a fix run at work
	in := externalInput{Threads: threads, Author: "zhuravel", Own: []string{"talkable[bot]"}, Calls: calls, Busy: busy}
	x, _ := externalOf(in)
	type got struct {
		answered, decision, called bool
		url                        string
	}
	want := map[int64]got{
		100: {decision: true, called: true, url: prPage + "#discussion_r101"},
		200: {answered: true, called: true},
		300: {answered: true},
		400: {answered: true},
		500: {called: true},
		600: {answered: true},
		700: {decision: true, called: true, url: prPage + "#discussion_r701"},
		800: {answered: true, called: true},
		900: {decision: true, called: true, url: prPage + "#discussion_r901"},
	}
	// A fixer App never posts as you: your replies during its runs are yours.
	in.AppFixer = true
	app, _ := externalOf(in)
	for _, th := range app.Threads {
		switch th.CommentID {
		case 700:
			if !th.Decision || th.DecisionURL != prPage+"#discussion_r702" {
				t.Errorf("under a fixer App, thread 700: decision %v at %q, want your latest reply", th.Decision, th.DecisionURL)
			}
		case 800:
			if !th.Decision || th.DecisionURL != prPage+"#discussion_r801" {
				t.Errorf("under a fixer App, thread 800: decision %v at %q, want your reply", th.Decision, th.DecisionURL)
			}
		}
	}
	for _, th := range x.Threads {
		g := got{answered: th.Answered, decision: th.Decision, called: th.called, url: th.DecisionURL}
		if w, ok := want[th.CommentID]; !ok || g != w {
			t.Errorf("thread %d: %+v, want %+v", th.CommentID, g, w)
		}
		delete(want, th.CommentID)
	}
	if len(want) > 0 {
		t.Errorf("threads missing from the file: %v", want)
	}
	if threads, _ := x.unanswered(); threads != 4 {
		t.Errorf("unanswered threads = %d, want the three decisions and rev-ann's question", threads)
	}
	b, err := json.Marshal(x.Threads[0])
	if err != nil || !strings.Contains(string(b), `"decision":true,"decision_url":"`+prPage+`#discussion_r101"`) || strings.Contains(string(b), "called") {
		t.Errorf("the file's thread = %s, want decision and decision_url, and nothing of the call", b)
	}
}

// Live on an own PR: the fixer left rev-ann's thread under Needs you, you
// decided in that thread, and `magnum fix` ran, but your reply, the
// thread's last comment, made it "answered", so the fixer left it and never
// built your decision. Now the call, still open in the PR's attention, makes
// your later reply a decision: the next loop starts on it (the review found
// nothing else) and hands it to the fixer as one.
func TestMagnumFixHandsTheFixerYourDecision(t *testing.T) {
	h, _ := fixLeftADecision(t, nil)
	h.decide("Keep stderr: fix the drop.")
	id := h.fix(FixPayload{})
	h.handle()
	if res := h.wantDone(id); !strings.Contains(res, "answers the other reviewers' 1 unanswered thread or comment") {
		t.Fatalf("answer = %q, want a loop on your decision", res)
	}
	runs := h.fixRuns(h.pr(2).ID)
	if len(runs) != 2 {
		t.Fatalf("fix runs = %d, want the second loop's", len(runs))
	}
	x := readExternalOf(t, runs[1])
	if len(x.Threads) != 1 || !x.Threads[0].Decision || x.Threads[0].Answered || x.Threads[0].DecisionURL != prPage+"#discussion_r4322458999" {
		t.Errorf("external threads = %+v, want rev-ann's thread as your decision", x.Threads)
	}
}

// A reply of yours with no open call of the fixer's is still an answer: a
// thread you answered before any loop asked is left alone, and so is one
// whose call `magnum fix --done` lifted.
func TestYourReplyWithNoOpenCallIsNoDecision(t *testing.T) {
	h, _ := fixLeftADecision(t, nil)
	pr := h.pr(2)
	h.decide("Keep stderr: fix the drop.")
	done := h.fix(FixPayload{Done: true, Seen: deref(pr.LastError)})
	h.handle()
	h.wantDone(done)
	id := h.fix(FixPayload{})
	h.handle()
	if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "nothing to fix") {
		t.Errorf("request = %s %q, want nothing to fix: the thread is answered", r.State, deref(r.Result))
	}
}

// The fixer builds your decision: it replies in the thread (citing it), and
// GitHub shows that reply. Its loop pushed nothing and left nothing, and
// every call of the attention it started over was a decision it built, so
// that attention is lifted: the PR is back to reviewed, with a
// fix.decisions event.
func TestALoopThatBuiltEveryDecisionLiftsTheAttention(t *testing.T) {
	h, _ := fixLeftADecision(t, func(h *harness, run store.Run) string {
		h.gh.mu.Lock()
		h.gh.threads[2][0] = fixerSays(h.gh.threads[2][0], 4322459000, "Won't fix: stderr stays dropped (decided at "+prPage+"#discussion_r4322458999).",
			h.clock.Now())
		h.gh.mu.Unlock()
		return fixReport(run.ID, "nothing", "1 threads (0 fixed, 0 already fixed, 0 not a bug, 1 won't fix)", "none")
	})
	h.decide("Drop it: reply won't fix.")
	h.fix(FixPayload{})
	h.handle()
	cur := h.wantState(2, store.PRReviewed)
	if cur.LastError != nil {
		t.Errorf("last_error = %q after the lift", deref(cur.LastError))
	}
	if _, ok := h.e.getKV(h.ctx, KVPRAttention(cur.ID)); ok {
		t.Error("the attention's kind outlived the lift")
	}
	if ev, ok := h.fixEvent("fix.decisions"); !ok || !strings.Contains(ev.Message, "built your decision in 1 thread") {
		t.Errorf("fix.decisions = %q, %v", ev.Message, ok)
	}
}

// A fixer that leaves something else under Needs you, and neither builds
// your decision nor leaves it there, has the new attention, which replaces
// the one you decided on, name the decision's call beside its own entry.
func TestADecisionTheFixerDidNotBuildGoesBackBesideWhatItLeft(t *testing.T) {
	h, _ := fixLeftADecision(t, func(_ *harness, run store.Run) string {
		return fixReport(run.ID, "nothing", noReplies, leftDescription)
	})
	h.decide("Keep stderr: fix the drop.")
	h.fix(FixPayload{})
	h.handle()
	cur := h.wantState(2, store.PRNeedsAttention)
	if msg := deref(cur.LastError); !strings.Contains(msg, decideCall) || !strings.Contains(msg, "5808905525 description") {
		t.Errorf("last_error = %q, want the call again beside the description", msg)
	}
}

// A person's decision never closes on the fixer's word alone: a fixer that
// neither builds it (no reply of its own on GitHub since it started) nor
// leaves it under Needs you has it put back there by magnum, so the PR
// needs you for it again, with the call; your earlier reply, older than
// this call, is no decision any more.
func TestADecisionTheFixerDidNotBuildGoesBackToNeedsYou(t *testing.T) {
	h, _ := fixLeftADecision(t, func(_ *harness, run store.Run) string {
		return fixReport(run.ID, "nothing", "1 threads (0 fixed, 0 already fixed, 0 not a bug, 1 won't fix)", "none")
	})
	h.decide("Keep stderr: fix the drop.")
	h.fix(FixPayload{})
	h.handle()
	cur := h.wantState(2, store.PRNeedsAttention)
	if msg := deref(cur.LastError); !strings.Contains(msg, decideCall) {
		t.Errorf("last_error = %q, want the call again", msg)
	}
	if ev, ok := h.fixEvent("fix.decisions"); !ok || !strings.Contains(ev.Message, "1 decision goes back to you") {
		t.Errorf("fix.decisions = %q, %v", ev.Message, ok)
	}
	if calls := h.e.fixerCalls(h.ctx, cur); len(calls) != 1 {
		t.Fatalf("open calls = %v, want rev-ann's thread", calls)
	}
	read, err := h.e.readExternal(h.ctx, &fixJob{repo: h.repo(), watch: h.cfg.Watches[0], pr: cur, login: "talkable[bot]"})
	if err != nil {
		t.Fatal(err)
	}
	if th := read.items.Threads; len(th) != 1 || th[0].Decision || !th[0].Answered {
		t.Errorf("threads = %+v, want your older reply read as an answer to the old call", th)
	}
}

// readDecision is PR #2's other reviewers' items as a loop would read
// them now: rev-ann's thread.
func (h *harness) readDecision() externalThread {
	h.t.Helper()
	read, err := h.e.readExternal(h.ctx, &fixJob{repo: h.repo(), watch: h.cfg.Watches[0], pr: h.pr(2), login: "talkable[bot]"})
	if err != nil {
		h.t.Fatal(err)
	}
	if len(read.items.Threads) != 1 {
		h.t.Fatalf("threads = %+v, want rev-ann's", read.items.Threads)
	}
	return read.items.Threads[0]
}

// A fix run that fails over your decisions keeps them: the failure's
// attention names their calls after its own words, so the next `magnum fix`
// still builds them. The calls keep the time they were made: a failed run
// that rewrote the report of the run that made them (one report per head),
// as a failed push does, does not make your decision older than the call.
// A call GitHub shows the failed run built (a fix-marked reply of the
// fixer's after it) is done and goes. An attention raised before magnum
// recorded when calls are made has them dated when a loop starts over it.
func TestAFailedRunKeepsYourDecisions(t *testing.T) {
	failedPush := func(_ *harness, run store.Run) string {
		return fixReport(run.ID, "push failed: rejected", noReplies, leftDecision)
	}
	for _, tc := range []struct {
		name   string
		next   func(h *harness, run store.Run) string
		before bool // the attention was raised before magnum recorded when calls are made
		built  bool
	}{
		{name: "no report", next: func(*harness, store.Run) string { return "" }},
		{name: "a failed push", next: failedPush},
		{name: "a failed push over an attention raised before the record", next: failedPush, before: true},
		{name: "a failed push after it built your decision", built: true, next: func(h *harness, run store.Run) string {
			h.advance(time.Minute)
			h.gh.mu.Lock()
			h.gh.threads[2][0] = fixerSays(h.gh.threads[2][0], 4322459000, "Fixed in 1a2b3c4: the drop is gone (decided at "+prPage+"#discussion_r4322458999).",
				h.clock.Now())
			h.gh.mu.Unlock()
			return fixReport(run.ID, "push failed: rejected", "1 threads (1 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := fixLeftADecision(t, tc.next)
			if tc.before {
				h.e.delKV(h.ctx, kvPRFixCalls(h.pr(2).ID))
			}
			h.decide("Keep stderr: fix the drop.")
			h.fix(FixPayload{})
			h.handle()
			msg := deref(h.wantState(2, store.PRNeedsAttention).LastError)
			if !strings.HasPrefix(msg, "fix run failed: ") || strings.Contains(msg, decideCall) == tc.built {
				t.Fatalf("last_error = %q, want the failure, and the call unless the run built it", msg)
			}
			if tc.built {
				return
			}
			if th := h.readDecision(); !th.Decision || th.DecisionURL != prPage+"#discussion_r4322458999" {
				t.Errorf("thread = %+v, want your decision still to build", th)
			}
		})
	}
}

// A gh fixer posts as you, and a reply it posts without the fix marker (with
// `gh api`, as on talkable#12112) reads as yours. One that a run posts while
// it is at work is no decision, even when the run fails with no report and
// leaves the report of the run that made the call on disk: your decision
// before it is still the one the next fixer builds, and the call stays.
func TestAnUnmarkedReplyOfAFixRunIsNoDecision(t *testing.T) {
	h, _ := fixLeftADecision(t, func(h *harness, _ store.Run) string {
		h.advance(time.Minute) // the fixer works a while
		h.gh.mu.Lock()
		h.gh.threads[2][0] = authorSays(h.gh.threads[2][0], 4322459111, "Won't fix: stderr stays dropped.", h.clock.Now())
		h.gh.mu.Unlock()
		return "" // no report: the run fails
	})
	h.decide("Keep stderr: fix the drop.")
	h.fix(FixPayload{})
	h.handle()
	if msg := deref(h.wantState(2, store.PRNeedsAttention).LastError); !strings.Contains(msg, decideCall) {
		t.Fatalf("last_error = %q, want the call kept", msg)
	}
	h.advance(10 * time.Minute)
	if th := h.readDecision(); !th.Decision || th.DecisionURL != prPage+"#discussion_r4322458999" {
		t.Errorf("thread = %+v, want your decision, not the fixer's unmarked reply", th)
	}
}

// `magnum fix --decide <thread>="<text>"` posts your decision in the
// thread as you (the gh identity the fix role uses), then starts the loop
// in one step, whose fixer gets it as a decision. A dry run posts nothing,
// and the reply, made as the run starts, is never named as an unmarked
// reply of the fixer's.
func TestFixDecidePostsYourReplyThenStartsTheLoop(t *testing.T) {
	h, _ := fixLeftADecision(t, func(h *harness, run store.Run) string { // it builds your decision, so the settle looks for unmarked replies
		h.gh.mu.Lock()
		h.gh.threads[2][0] = fixerSays(h.gh.threads[2][0], 4322459000, "Fixed in 1a2b3c4: the drop is gone.", h.clock.Now())
		h.gh.mu.Unlock()
		return fixReport(run.ID, "nothing", "1 threads (1 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
	})
	decide := []FixDecision{{Thread: decideThread, Text: "Keep stderr: fix the drop."}}
	dry := h.fix(FixPayload{Decide: decide, DryRun: true})
	h.handle()
	if res := h.wantDone(dry); !strings.HasPrefix(res, "dry run: would reply in 1 thread as zhuravel, then start fixing talkable/talkable#2") {
		t.Errorf("dry run answer = %q", res)
	}
	if n := h.gh.count("reply:"); n != 0 {
		t.Fatalf("the dry run posted %d replies", n)
	}
	id := h.fix(FixPayload{Decide: decide})
	h.handle()
	res := h.wantDone(id)
	if !strings.HasPrefix(res, "fixing talkable/talkable#2: ") || !strings.Contains(res, "replied in 1 thread as zhuravel: "+prPage+"#discussion_r") {
		t.Errorf("answer = %q", res)
	}
	h.gh.mu.Lock()
	got := slices.Clone(h.gh.calls)
	h.gh.mu.Unlock()
	if !slices.Contains(got, "reply:talkable/talkable#2/4322458360:Keep stderr: fix the drop.") {
		t.Errorf("GitHub calls = %q, want your reply in rev-ann's thread", got)
	}
	runs := h.fixRuns(h.pr(2).ID)
	if len(runs) != 2 {
		t.Fatalf("fix runs = %d, want the loop --decide started", len(runs))
	}
	if x := readExternalOf(t, runs[1]); len(x.Threads) != 1 || !x.Threads[0].Decision {
		t.Errorf("external threads = %+v, want your decision", x.Threads)
	}
	if ev, ok := h.fixEvent("fix.decided"); !ok || strings.Contains(ev.Message, "Keep stderr") {
		t.Errorf("fix.decided = %q, %v: want it, without your words", ev.Message, ok)
	}
	// Your reply went out the moment the run started: it is your word, not
	// an unmarked reply of the fixer's.
	if ev, ok := h.fixEvent("fix.unmarked"); ok {
		t.Errorf("fix.unmarked = %q: your decision read as the fixer's reply", ev.Message)
	}
}

// --decide answers a call the fixer made, and only then: a thread it made
// no call in, an empty or marked reply, a PR that is not yours and a loop
// that runs are refused, and nothing is posted.
func TestFixDecideRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		decide []FixDecision
		mod    func(h *harness)
		want   string
	}{
		{name: "no call there", decide: []FixDecision{{Thread: 4322450001, Text: "Keep it."}},
			want: "PR #2: the fixer left you no call in thread 4322450001 (`magnum status talkable/talkable#2` lists what it left); nothing was posted"},
		{name: "empty", decide: []FixDecision{{Thread: decideThread, Text: "  "}}, want: "magnum fix --decide: the reply in thread 4322458360 is empty"},
		{name: "marked", decide: []FixDecision{{Thread: decideThread, Text: "ok <!-- magnum:fix -->"}},
			want: "magnum fix --decide: the reply in thread 4322458360 carries a magnum marker"},
		{name: "a loop runs", decide: []FixDecision{{Thread: decideThread, Text: "Keep it."}}, mod: func(h *harness) {
			if err := h.e.setFixState(h.ctx, h.pr(2).ID, FixState{Cycle: 1, Cap: 3, Phase: FixPhaseReviewing, Head: fixHead}); err != nil {
				h.t.Fatal(err)
			}
		}, want: "fixing, cycle 1 of 3; `magnum abort` stops it"},
		{name: "with --done", decide: []FixDecision{{Thread: decideThread, Text: "Keep it."}}, want: "magnum fix: --decide goes alone"},
		{name: "the fixer answered since its call", decide: []FixDecision{{Thread: decideThread, Text: "Keep it."}}, mod: func(h *harness) {
			h.gh.mu.Lock()
			h.gh.threads[2][0] = fixerSays(h.gh.threads[2][0], 4322459000, "Won't fix: a product choice.", h.clock.Now())
			h.gh.mu.Unlock()
		}, want: "PR #2: the fixer left you no call in thread 4322458360"},
		{name: "not as you", decide: []FixDecision{{Thread: decideThread, Text: "Keep it."}}, mod: func(h *harness) {
			h.e.d.Identities["zhuravel"] = otherLogin{Source: h.e.d.Identities["zhuravel"], login: "alice"}
		}, want: "no gh identity of zhuravel, the PR's author, to post your reply as"},
		{name: "no free slot", decide: []FixDecision{{Thread: decideThread, Text: "Keep it."}}, mod: func(h *harness) {
			rel := h.enqueue(ReqRelease, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
			h.handle()
			if r := h.request(rel); r.State != store.RequestDone {
				h.t.Fatalf("release: %s %q", r.State, deref(r.Result))
			}
			takeOnlySlot(h)
		}, want: "no free talkable/talkable slot right now; run `magnum fix talkable/talkable#2` again shortly; nothing was posted"},
		{name: "a person's agent in the pinned slot", decide: []FixDecision{{Thread: decideThread, Text: "Keep it."}}, mod: func(h *harness) {
			pin := h.enqueue(ReqPin, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
			h.handle()
			if r := h.request(pin); r.State != store.RequestDone {
				h.t.Fatalf("pin: %s %q", r.State, deref(r.Result))
			}
			h.sl.mu.Lock()
			h.sl.guardErr = slots.ErrHold{Reason: slots.HoldForeignAgent, Detail: `pane p9: claude agent "alice" is idle in /tmp/review1`}
			h.sl.mu.Unlock()
		}, want: `but slot review1 is held: foreign_agent: pane p9: claude agent "alice" is idle in /tmp/review1; nothing was posted`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := fixLeftADecision(t, nil)
			if tc.mod != nil {
				tc.mod(h)
			}
			p := FixPayload{Decide: tc.decide}
			if tc.name == "with --done" {
				p.Done = true
			}
			id := h.fix(p)
			h.handle()
			if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), tc.want) {
				t.Errorf("request = %s %q, want %q", r.State, deref(r.Result), tc.want)
			}
			if n := h.gh.count("reply:"); n != 0 {
				t.Errorf("a refused --decide posted %d replies", n)
			}
		})
	}
	t.Run("not yours", func(t *testing.T) {
		h, _ := fixHarness(t)
		fixablePRResult(h, "alice", map[string]any{"event": "COMMENTED", "findings": map[string]int{}})
		id := h.fix(FixPayload{Decide: []FixDecision{{Thread: decideThread, Text: "Keep it."}}})
		h.handle()
		if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "PR #2 is alice's, not yours") {
			t.Errorf("request = %s %q", r.State, deref(r.Result))
		}
	})
}

// otherLogin is an identity whose login is not the one configured.
type otherLogin struct {
	identity.Source
	login string
}

func (o otherLogin) Login() string { return o.login }

// Autofix starts no loop over an attention whose call you did not decide:
// the other reviewers' items the start read show no decision of yours, so
// the attention still asks you.
func TestAutofixStartsNoLoopOverACallYouDidNotDecide(t *testing.T) {
	h, _ := fixLeftADecision(t, nil)
	if _, err := h.e.startFix(h.ctx, h.pr(2), autofixBy, FixState{}, nil); err == nil ||
		!strings.Contains(err.Error(), "needs attention: autofix starts no loop over it") {
		t.Errorf("autofix over an undecided call: %v, want a refusal", err)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("autofix started a loop over a call you did not decide")
	}
}

// A loop that built your decision lifts only an attention of decision calls
// alone: one that also asks something else of you (here the description,
// in the older words) stays for the rest.
func TestALoopThatBuiltYourDecisionKeepsAnAttentionThatAsksMore(t *testing.T) {
	h, _ := fixLeft(t, leftDecision+"\n"+leftDescription, func(h *harness, run store.Run) string {
		h.gh.mu.Lock()
		h.gh.threads[2][0] = fixerSays(h.gh.threads[2][0], 4322459000, "Won't fix: stderr stays dropped (decided at "+prPage+"#discussion_r4322458999).",
			h.clock.Now())
		h.gh.mu.Unlock()
		return fixReport(run.ID, "nothing", "1 threads (0 fixed, 0 already fixed, 0 not a bug, 1 won't fix)", "none")
	})
	before := deref(h.pr(2).LastError)
	if !strings.Contains(before, "1 more finding needs your call (5808905525 description)") {
		t.Fatalf("last_error = %q, want the description beside the call", before)
	}
	h.decide("Drop it: reply won't fix.")
	h.fix(FixPayload{})
	h.handle()
	if msg := deref(h.wantState(2, store.PRNeedsAttention).LastError); msg != before {
		t.Errorf("last_error = %q, want the attention as it was", msg)
	}
	if ev, ok := h.fixEvent("fix.decisions"); !ok || !strings.Contains(ev.Message, "built your decision in 1 thread") {
		t.Errorf("fix.decisions = %q, %v", ev.Message, ok)
	}
	if ev, ok := h.fixEvent("fix.decisions_built"); ok {
		t.Errorf("fix.decisions_built = %q over an attention that asks more", ev.Message)
	}
}

// Under autofix = "open" your decision is new work for the look: the
// thread's key changes with your reply, and an attention whose calls you
// all decided stops nothing, so the loop starts by itself once the PR
// settled, and its fixer gets the decision.
func TestTheLookStartsALoopOnYourDecisionAfterTheSettle(t *testing.T) {
	h, git := openHarness(t, findings(map[string]int{}, "COMMENTED"))
	h.setThreads(h.colleagueThread(decideThread))
	var mu sync.Mutex
	runs := 0
	fixerBy(h, git, func(run store.Run) (string, string) {
		mu.Lock()
		runs++
		first := runs == 1
		mu.Unlock()
		if first {
			return "", fixReport(run.ID, "nothing", noReplies, "- 4322458360 lib/sandbox.rb:185: not fixed. keeping stderr is a product choice")
		}
		return "", fixReport(run.ID, "nothing", noReplies, "none")
	})
	h.tick() // reviewed
	h.pastSettle()
	h.tick() // the look starts the first loop, which leaves you the call
	pr := h.wantState(2, store.PRNeedsAttention)
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Fatalf("fix runs = %d, want the first loop's", n)
	}
	h.advance(5 * time.Minute)
	h.tick()
	h.wantOpen(OpenNeedsYou, "needs your attention (fix)")

	h.decide("Keep stderr: fix the drop.")
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: "zhuravel", activity: h.clock.Now()}) // your reply is activity
	h.tick()
	h.wantOpen(OpenWaits, "settling")
	if n := len(h.fixRuns(pr.ID)); n != 1 {
		t.Fatalf("fix runs = %d before the settle", n)
	}
	h.pastSettle()
	h.tick()
	all := h.fixRuns(pr.ID)
	if len(all) != 2 || h.fixStartBy() != autofixBy {
		t.Fatalf("fix runs = %d by %q, want autofix's on your decision", len(all), h.fixStartBy())
	}
	if x := readExternalOf(t, all[1]); len(x.Threads) != 1 || !x.Threads[0].Decision {
		t.Errorf("external threads = %+v, want your decision", x.Threads)
	}
}

// Only an attention whose calls are all decisions you made lets the look
// start: one that also asks something else (here a description entry) still
// needs you.
func TestAnAttentionWithMoreThanDecisionsStillNeedsYou(t *testing.T) {
	msg := "fix: " + attention.FixCallsText([]attention.FixCall{{Thread: "4322458360", URL: prPage + "#discussion_r4322458360", Action: decideCall}},
		"1 more finding needs your call (5808905525 description)", "/state/claude-fix.md")
	if callsDecided(msg, func(int64) bool { return true }) {
		t.Error("an attention with a description entry beside the decision counts as decided")
	}
	msg = "fix: " + attention.FixCallsText([]attention.FixCall{{Thread: "4322458360", URL: prPage + "#discussion_r4322458360", Action: decideCall}}, "",
		"/state/claude-fix.md")
	if !callsDecided(msg, func(id int64) bool { return id == decideThread }) {
		t.Error("an attention of one decided call does not count as decided")
	}
	if callsDecided(msg, func(int64) bool { return false }) {
		t.Error("an attention whose call you did not decide counts as decided")
	}
}

// magnumsCallThread is magnum's thread on PR #2 whose finding's Fix ends
// "This is the author's call.": the fixer leaves it to you.
const magnumsCallThread = 4302866759

// magnumsCall is the call the fixer's "not fixed." entry in magnum's
// thread makes.
const magnumsCall = "decide magnum's P2 on app/models/offer.rb:42: the fixer declined it; reply won't fix at " +
	prPage + "#discussion_r4302866759, or `magnum fix talkable/talkable#2` sends it back"

// fixLeftMagnumsCall runs one loop on PR #2, whose review posted a P2 that
// is the author's call in magnum's thread: the fixer leaves it to you, as
// on an own PR on 2026-11-07 (it asked which of two options to take). The
// PR needs attention with the call to decide it. next is the fixer of each
// later run.
func fixLeftMagnumsCall(t *testing.T, next func(h *harness, run store.Run) string) *harness {
	t.Helper()
	h, git := fixHarness(t)
	h.gh.clock = h.clock.Now // a reply --decide posts is made now
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.setThreads(magnumsThread(magnumsCallThread, "app/models/offer.rb", 42, h.clock.Now().Add(-time.Hour), false))
	var mu sync.Mutex
	runs := 0
	fixerBy(h, git, func(run store.Run) (string, string) {
		mu.Lock()
		runs++
		first := runs == 1
		mu.Unlock()
		if first {
			writeFixReviewFiles(h.t, deref(run.ReportPath), []int64{magnumsCallThread}, nil)
			return "", fixReport(run.ID, "nothing", noReplies,
				"- 4302866759 P2 app/models/offer.rb:42: not fixed. This is the author's call: option A keeps the cap, option B lifts it")
		}
		return "", next(h, run)
	})
	h.fix(FixPayload{})
	h.handle()
	if msg := deref(h.wantState(2, store.PRNeedsAttention).LastError); !strings.Contains(msg, magnumsCall) {
		t.Fatalf("last_error = %q, want the call to decide magnum's finding", msg)
	}
	h.advance(10 * time.Minute)
	return h
}

// The fixer left you the author's call of magnum's own finding, and
// `magnum fix --decide` refused your choice: it took only threads other
// reviewers started, so the way out was a reply on GitHub, a poll and
// `magnum fix --done`, and nothing built the option you picked. It now
// takes magnum's thread too, found through the fixer's calls: it posts your
// reply there as you and starts the loop, whose external file hands the
// fixer your decision (decisions, never one of the other reviewers'
// threads). The fixer builds it, and magnum still checks on GitHub that its
// reply is there: one there lifts the attention, and none puts the call
// back to you.
func TestFixDecideTakesMagnumsOwnAuthorsCall(t *testing.T) {
	decide := []FixDecision{{Thread: magnumsCallThread, Text: "Keep the cap: reply won't fix."}}
	t.Run("built", func(t *testing.T) {
		h := fixLeftMagnumsCall(t, func(h *harness, run store.Run) string {
			x, _ := readExternalFile(filepath.Dir(deref(run.ReportPath)))
			if len(x.Decisions) == 1 {
				h.gh.mu.Lock()
				h.gh.threads[2][0] = fixerSays(h.gh.threads[2][0], 4302866900, "Won't fix: the cap stays (decided at "+x.Decisions[0].DecisionURL+").",
					h.clock.Now())
				h.gh.mu.Unlock()
			}
			return fixReport(run.ID, "nothing", "1 threads (0 fixed, 0 already fixed, 0 not a bug, 1 won't fix)", "none")
		})
		id := h.fix(FixPayload{Decide: decide})
		h.handle()
		if res := h.wantDone(id); !strings.Contains(res, "replied in 1 thread as zhuravel: "+prPage+"#discussion_r") {
			t.Errorf("answer = %q, want your reply posted and the loop started", res)
		}
		h.gh.mu.Lock()
		got := slices.Clone(h.gh.calls)
		h.gh.mu.Unlock()
		if !slices.Contains(got, "reply:talkable/talkable#2/4302866759:Keep the cap: reply won't fix.") {
			t.Errorf("GitHub calls = %q, want your reply in magnum's thread", got)
		}
		runs := h.fixRuns(h.pr(2).ID)
		if len(runs) != 2 {
			t.Fatalf("fix runs = %d, want the loop --decide started", len(runs))
		}
		x := readExternalOf(t, runs[1])
		if len(x.Threads) != 0 {
			t.Errorf("external threads = %+v, want none: magnum's thread is no other reviewer's", x.Threads)
		}
		if len(x.Decisions) != 1 || x.Decisions[0].CommentID != magnumsCallThread || !x.Decisions[0].Decision ||
			!strings.HasPrefix(x.Decisions[0].DecisionURL, prPage+"#discussion_r") {
			t.Errorf("decisions = %+v, want your decision in magnum's thread", x.Decisions)
		}
		if cur := h.wantState(2, store.PRReviewed); cur.LastError != nil {
			t.Errorf("last_error = %q after the fixer built your decision", deref(cur.LastError))
		}
		if ev, ok := h.fixEvent("fix.decisions"); !ok || !strings.Contains(ev.Message, "built your decision in 1 thread") {
			t.Errorf("fix.decisions = %q, %v", ev.Message, ok)
		}
		if ev, ok := h.fixEvent("fix.unmarked"); ok {
			t.Errorf("fix.unmarked = %q: your decision read as the fixer's reply", ev.Message)
		}
	})
	t.Run("not built", func(t *testing.T) {
		h := fixLeftMagnumsCall(t, func(_ *harness, run store.Run) string {
			return fixReport(run.ID, "nothing", "1 threads (0 fixed, 0 already fixed, 0 not a bug, 1 won't fix)", "none")
		})
		id := h.fix(FixPayload{Decide: decide})
		h.handle()
		h.wantDone(id)
		cur := h.wantState(2, store.PRNeedsAttention)
		if msg := deref(cur.LastError); !strings.Contains(msg, "decide magnum's finding on app/models/offer.rb:42: ") {
			t.Errorf("last_error = %q, want the call back to you", msg)
		}
		if ev, ok := h.fixEvent("fix.decisions"); !ok || !strings.Contains(ev.Message, "1 decision goes back to you") {
			t.Errorf("fix.decisions = %q, %v", ev.Message, ok)
		}
	})
}
