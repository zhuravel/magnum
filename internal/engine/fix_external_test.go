package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/store"
)

const prPage = "https://github.com/talkable/talkable/pull/2"

// extThread is an unresolved review thread of PR #2 on path:line whose
// comments are by the given authors (GraphQL logins; "talkable" is the
// App's and one ending in "-bot" another App's, Bots; the others are
// members of the repository), the first with id and body, each one minute
// after the last from at.
func extThread(id int64, path string, line int, at time.Time, body string, authors ...string) github.Thread {
	t := github.Thread{ID: "PRRT_" + path, Path: path, Line: line, OriginalLine: line}
	for i, a := range authors {
		c := github.ThreadComment{ID: id + int64(i), AuthorLogin: a, AuthorType: "User", AuthorAssociation: "MEMBER", Body: "a reply",
			URL: prPage + "#discussion_r" + itoa(id+int64(i)), CreatedAt: at.Add(time.Duration(i) * time.Minute)}
		if a == "talkable" || strings.HasSuffix(a, "-bot") {
			c.AuthorType, c.AuthorAssociation = "Bot", "NONE"
		}
		if i == 0 {
			c.Body = body
		}
		t.Comments = append(t.Comments, c)
	}
	return t
}

// readExternalOf reads the external file beside the fix run's report.
func readExternalOf(t *testing.T, run store.Run) externalItems {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(deref(run.ReportPath)), externalFile))
	if err != nil {
		t.Fatalf("no external file beside the fixer's report: %v", err)
	}
	var x externalItems
	if err := json.Unmarshal(b, &x); err != nil {
		t.Fatal(err)
	}
	return x
}

// On talkable#12054, the operator's own PR, another reviewer's tool left a
// MINOR follow-up an hour before `magnum fix` ran,
// and the fixer, told only of magnum's review, never saw it. The fix job
// now writes external-threads.json beside the report, which the fix prompt
// names: the unresolved threads none of magnum's logins started (people and
// bots alike), each answered when its last comment is the author's, and
// the review bodies and conversation comments of others since the author's
// last comment of their own, an item a later fixer comment links answered.
// magnum's own threads, its review bodies and its status comment are not
// in it, nor a resolved thread, a body-less approval or what came before
// the author's last word. The event counts them and quotes none.
func TestTheFixJobListsTheOtherReviewersOpenThreadsAndComments(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	base := h.clock.Now().Add(-3 * time.Hour)
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	h.gh.mu.Lock()
	h.gh.threads = map[int][]github.Thread{2: {
		extThread(4322458360, "lib/sandbox.rb", 185, at(10), "MINOR: `run` drops stderr", "rev-ann"),
		extThread(4322458400, "app/b.rb", 9, at(20), "Rename this", "bob-rev", "zhuravel"),
		extThread(4322458500, "app/c.rb", 3, at(30), "**[P2] magnum's own**", "talkable"),
		extThread(4322458600, "app/d.rb", 4, at(40), "Is this needed?", "alice", "zhuravel", "alice"),
		extThread(4322458700, "app/e.rb", 5, at(50), "Nit: spacing", "review-bot", "zhuravel", "rev-ann"),
	}}
	// The fixer answered the bot's nit in an earlier run, and a person came
	// back to the thread after it.
	h.gh.threads[2][4].Comments[1].Body = "Not a bug: the linter wants it.\n\n" + postreview.FixReplyMarker
	resolved := extThread(4322458800, "app/f.rb", 6, at(55), "Done already", "rev-ann")
	resolved.Resolved = true
	h.gh.threads[2] = append(h.gh.threads[2], resolved)
	h.gh.allReviews = map[int][]github.Review{2: {
		{DatabaseID: 9001, State: "COMMENTED", Body: "Overall: add a spec for the sandbox", URL: prPage + "#pullrequestreview-9001",
			SubmittedAt: at(60), AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"},
		{DatabaseID: 9002, State: "CHANGES_REQUESTED", Body: "**Blocking: 1 problem**\n\n<!-- magnum:run=r1 head=a1a1a1a -->", URL: prPage + "#pullrequestreview-9002",
			SubmittedAt: at(80), AuthorLogin: "talkable", AuthorType: "Bot"},
		{DatabaseID: 9003, State: "CHANGES_REQUESTED", Body: "Needs a note on the migration", URL: prPage + "#pullrequestreview-9003",
			SubmittedAt: at(90), AuthorLogin: "bob-rev", AuthorType: "User", AuthorAssociation: "COLLABORATOR"},
		{DatabaseID: 9004, State: "APPROVED", URL: prPage + "#pullrequestreview-9004", SubmittedAt: at(95), AuthorLogin: "alice", AuthorType: "User", AuthorAssociation: "OWNER"},
		{DatabaseID: 9005, State: "COMMENTED", Body: "Looks fine to me", URL: prPage + "#pullrequestreview-9005",
			SubmittedAt: at(97), AuthorLogin: "alice", AuthorType: "User", AuthorAssociation: "OWNER"},
	}}
	h.gh.prComments = map[int][]github.PRComment{2: {
		{ID: 701, URL: prPage + "#issuecomment-701", Body: "Thanks all, addressing it now", CreatedAt: at(70), AuthorLogin: "zhuravel", AuthorType: "User"},
		{ID: 702, URL: prPage + "#issuecomment-702", Body: "<!-- magnum:status -->\nreviewed", CreatedAt: at(85), AuthorLogin: "talkable", AuthorType: "Bot"},
		{ID: 703, URL: prPage + "#issuecomment-703", Body: "Can we keep the old flag?", CreatedAt: at(100), AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"},
		{ID: 704, URL: prPage + "#issuecomment-704", Body: "> Looks fine to me\n" + prPage + "#pullrequestreview-9005\n\nAnswer: thanks.\n\n" +
			postreview.FixReplyMarker, CreatedAt: at(110), AuthorLogin: "zhuravel", AuthorType: "User"},
		{ID: 705, URL: prPage + "#issuecomment-705", Body: "> x\n" + prPage + "#issuecomment-70\n\n" + postreview.FixReplyMarker,
			CreatedAt: at(111), AuthorLogin: "zhuravel", AuthorType: "User"},
	}}
	h.gh.mu.Unlock()
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
	})
	h.fix(FixPayload{})
	h.handle()

	runs := h.fixRuns(pr.ID)
	if len(runs) != 1 {
		t.Fatalf("fix runs = %+v", runs)
	}
	x := readExternalOf(t, runs[0])
	if x.Author != "zhuravel" || !slices.Contains(x.FixerLogins, "talkable[bot]") || slices.Contains(x.FixerLogins, "rev-ann") {
		t.Errorf("author = %q, fixer logins %q: want the PR author and magnum's logins, by which the fixer's look before a reply tells an answer", x.Author, x.FixerLogins)
	}
	var threads []string
	for _, th := range x.Threads {
		threads = append(threads, itoa(th.CommentID)+" "+th.Path+":"+itoa(int64(th.Line))+" answered="+map[bool]string{true: "yes", false: "no"}[th.Answered])
	}
	if want := []string{
		"4322458360 lib/sandbox.rb:185 answered=no",
		"4322458400 app/b.rb:9 answered=yes",
		"4322458600 app/d.rb:4 answered=no",
		"4322458700 app/e.rb:5 answered=no",
	}; !slices.Equal(threads, want) {
		t.Errorf("threads = %q\nwant      %q", threads, want)
	}
	if first := x.Threads[0]; first.URL != prPage+"#discussion_r4322458360" || len(first.Comments) != 1 ||
		first.Comments[0] != (externalComment{ID: 4322458360, URL: prPage + "#discussion_r4322458360", Author: "rev-ann", Body: "MINOR: `run` drops stderr",
			CreatedAt: at(10).UTC().Format(time.RFC3339)}) {
		t.Errorf("the 12054 thread = %+v", first)
	}
	if bot := x.Threads[3].Comments; len(bot) != 3 || bot[0].Author != "review-bot[bot]" || !bot[1].Fix || bot[2].Fix {
		t.Errorf("the bot's thread with the fixer's earlier answer = %+v", bot)
	}
	var conv []string
	for _, r := range x.Conversation {
		conv = append(conv, r.Kind+" "+itoa(r.ID)+" "+r.Author+" answered="+map[bool]string{true: "yes", false: "no"}[r.Answered])
	}
	if want := []string{
		"review 9003 bob-rev answered=no",
		"review 9005 alice answered=yes",
		"comment 703 rev-ann answered=no",
	}; !slices.Equal(conv, want) {
		t.Errorf("conversation = %q\nwant           %q", conv, want)
	}
	if r := x.Conversation[0]; r.State != "CHANGES_REQUESTED" || r.Body != "Needs a note on the migration" || r.URL != prPage+"#pullrequestreview-9003" {
		t.Errorf("the changes request = %+v", r)
	}

	external := filepath.Join(filepath.Dir(deref(runs[0].ReportPath)), "external-threads.json")
	if !slices.ContainsFunc(h.ag.all(), func(c string) bool {
		return strings.HasPrefix(c, "fix_prompt:claude-fix:"+runs[0].ID+"|") && strings.HasSuffix(c, "|"+external)
	}) {
		t.Errorf("the fix prompt does not name %s: %v", external, h.ag.all())
	}
	ev, ok := h.fixEvent("fix.external")
	if !ok || ev.Level != "info" || ev.Message != "external-threads.json: 4 open threads of other reviewers (3 unanswered), "+
		"3 review bodies and comments since the author's last (2 unanswered)" {
		t.Errorf("fix.external = %+v, %v", ev, ok)
	}
	for _, words := range []string{"stderr", "migration", "old flag"} {
		if strings.Contains(ev.Message, words) || strings.Contains(string(ev.Data), words) {
			t.Errorf("fix.external carries the PR's text %q", words)
		}
	}
}

// member is a comment of a review thread by a member of the repository
// (login, User), m minutes after 07:00 UTC.
func member(id int64, login, body string, m int) github.ThreadComment {
	return github.ThreadComment{ID: id, AuthorLogin: login, AuthorType: "User", AuthorAssociation: "MEMBER", Body: body,
		URL: prPage + "#discussion_r" + itoa(id), CreatedAt: time.Date(2026, 10, 20, 7, 0, 0, 0, time.UTC).Add(time.Duration(m) * time.Minute)}
}

// A body longer than the threads file's excerpt is cut and marked, a
// follow-up's as a first comment's, and each comment keeps its id and link,
// so the fixer reads the right one whole with gh; the fixer's marker in
// another reviewer's comment answers nothing.
func TestTheExternalFileCutsLongBodiesAndKeepsEachCommentsID(t *testing.T) {
	long := strings.Repeat("x", externalBodyMax+50)
	x, _ := externalOf(externalInput{Threads: []github.Thread{{Path: "a.rb", Line: 1, Comments: []github.ThreadComment{
		member(1, "rev-ann", long, 0), member(2, "bob-rev", "And this: "+long, 1)}}}, CommentsComplete: true, Author: "zhuravel", Own: []string{"talkable[bot]"}})
	for i, c := range x.Threads[0].Comments {
		if !c.Truncated || len([]rune(c.Body)) > externalBodyMax || c.ID != int64(i+1) || c.URL != prPage+"#discussion_r"+itoa(int64(i+1)) {
			t.Errorf("comment %d = id %d, url %q, %d runes, truncated %v", i, c.ID, c.URL, len([]rune(c.Body)), c.Truncated)
		}
	}
	spoof, _ := externalOf(externalInput{Threads: []github.Thread{{Path: "a.rb", Line: 1, Comments: []github.ThreadComment{
		member(1, "rev-ann", "first", 0), member(2, "bob-rev", "me too "+postreview.FixReplyMarker, 1)}}}, CommentsComplete: true,
		Author: "zhuravel", Own: []string{"talkable[bot]"}})
	if spoof.Threads[0].Answered {
		t.Error("a reviewer's comment with the fixer's marker answers the thread")
	}
}

// ReviewThreads reads the first 50 comments of a thread and the newest
// ones as its Tail: whether a thread is answered is its latest comment's
// call, so a 51st comment that is the author's answer answers it, and a
// reviewer's 51st after the author's 50th opens it again.
func TestAThreadIsAnsweredByItsLatestCommentPastTheFirstFifty(t *testing.T) {
	long := func(last github.ThreadComment, fiftieth string) github.Thread {
		th := github.Thread{Path: "a.rb", Line: 1, CommentCount: 51}
		for i := range 50 {
			who := "rev-ann"
			if i == 49 {
				who = fiftieth
			}
			th.Comments = append(th.Comments, member(int64(i+1), who, "text", i))
		}
		th.Tail = []github.ThreadComment{last}
		return th
	}
	answered := long(member(51, "zhuravel", "Done in abc1234", 60), "rev-ann")
	reopened := long(member(51, "rev-ann", "Still broken", 60), "zhuravel")
	x, _ := externalOf(externalInput{Threads: []github.Thread{answered, reopened}, CommentsComplete: true, Author: "zhuravel", Own: []string{"talkable[bot]"}})
	if len(x.Threads) != 2 || !x.Threads[0].Answered || x.Threads[1].Answered {
		t.Fatalf("threads = %+v, want the first answered by its 51st comment and the second opened again by it", x.Threads)
	}
	if n := len(x.Threads[0].Comments); n != 51 || x.Threads[0].Comments[50].ID != 51 {
		t.Errorf("the long thread lists %d comments", n)
	}
	gap := long(member(60, "rev-ann", "x", 70), "rev-ann")
	gap.CommentCount = 60
	if x, _ := externalOf(externalInput{Threads: []github.Thread{gap}, CommentsComplete: true, Author: "zhuravel"}); x.Threads[0].Omitted != 9 {
		t.Errorf("omitted = %d, want the 9 comments between the first 50 and the newest", x.Threads[0].Omitted)
	}
}

// A thread the PR author started is the author's own, not another
// reviewer's, though a collaborator replied in it last: the fixer answers
// other reviewers, and the author's own notes are the author's.
func TestAThreadThePRAuthorStartedIsNotAnExternalItem(t *testing.T) {
	collaborator := member(2, "bob-rev", "Why not keep it?", 1)
	collaborator.AuthorAssociation = "COLLABORATOR"
	x, _ := externalOf(externalInput{Threads: []github.Thread{
		{Path: "a.rb", Line: 1, Comments: []github.ThreadComment{member(1, "zhuravel", "Note to self", 0), collaborator}},
		{Path: "b.rb", Line: 2, Comments: []github.ThreadComment{member(3, "rev-ann", "Rename it", 0)}},
	}, CommentsComplete: true, Author: "zhuravel", Own: []string{"talkable[bot]"}})
	if len(x.Threads) != 1 || x.Threads[0].Path != "b.rb" {
		t.Errorf("threads = %+v, want the reviewer's alone", x.Threads)
	}
}

// The fixer's issue comment answers the item its target line names, the
// line under its quote: a link to another item in its quote or its reply
// answers nothing, and neither does a link to "#issuecomment-70" for
// "#issuecomment-703".
func TestAFixerReplyAnswersOnlyTheItemItsTargetLineNames(t *testing.T) {
	at := time.Date(2026, 10, 20, 7, 0, 0, 0, time.UTC)
	a, b, c := prPage+"#issuecomment-701", prPage+"#pullrequestreview-9001", prPage+"#issuecomment-703"
	x, _ := externalOf(externalInput{
		Reviews: []github.Review{{DatabaseID: 9001, State: "COMMENTED", Body: "Log stderr too", URL: b, SubmittedAt: at, AuthorLogin: "bob-rev",
			AuthorType: "User", AuthorAssociation: "MEMBER"}},
		Comments: []github.PRComment{
			{ID: 701, URL: a, Body: "Can we keep the old flag? Same as " + b, CreatedAt: at.Add(time.Minute), AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"},
			{ID: 703, URL: c, Body: "And the docs?", CreatedAt: at.Add(2 * time.Minute), AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"},
			{ID: 704, Body: "> Can we keep the old flag? Same as " + b + "\n" + a + "\n\nAnswer: no, " + b + " covers it.\n\n" + postreview.FixReplyMarker,
				CreatedAt: at.Add(3 * time.Minute), AuthorLogin: "zhuravel", AuthorType: "User"},
			{ID: 705, Body: "> x\n" + prPage + "#issuecomment-70\n\nAnswer: x.\n\n" + postreview.FixReplyMarker,
				CreatedAt: at.Add(4 * time.Minute), AuthorLogin: "zhuravel", AuthorType: "User"},
		},
		CommentsComplete: true, Author: "zhuravel", Own: []string{"talkable[bot]"}})
	var got []string
	for _, r := range x.Conversation {
		got = append(got, itoa(r.ID)+" answered="+map[bool]string{true: "yes", false: "no"}[r.Answered])
	}
	if want := []string{"9001 answered=no", "701 answered=yes", "703 answered=no"}; !slices.Equal(got, want) {
		t.Errorf("conversation = %q, want %q", got, want)
	}
	if replyTarget("> quote\n>\n\n  "+a+"  \nAnswer") != a || replyTarget("> only a quote") != "" {
		t.Error("replyTarget does not take the first line under the quote")
	}
}

// When GitHub's conversation comments stop short of the author's last own
// word (more than the comment read's page limit after it), only what came
// after the oldest comment listed is an external item: an older review
// body, from before the author's word, is not brought back.
func TestTheConversationStopsAtTheOldestCommentWhenTheAuthorsWordIsBeyondIt(t *testing.T) {
	at := time.Date(2026, 10, 20, 7, 0, 0, 0, time.UTC)
	old := github.Review{DatabaseID: 9001, State: "COMMENTED", Body: "Old request, answered", URL: prPage + "#pullrequestreview-9001", SubmittedAt: at,
		AuthorLogin: "bob-rev", AuthorType: "User", AuthorAssociation: "MEMBER"}
	listed := []github.PRComment{{ID: 702, URL: prPage + "#issuecomment-702", Body: "A new question", CreatedAt: at.Add(2 * time.Hour),
		AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"}}
	for _, complete := range []bool{false, true} {
		x, _ := externalOf(externalInput{Reviews: []github.Review{old}, Comments: listed, CommentsComplete: complete, Author: "zhuravel"})
		var ids []int64
		for _, r := range x.Conversation {
			ids = append(ids, r.ID)
		}
		want := []int64{702}
		if complete {
			want = []int64{9001, 702} // nothing older left unread: no word of the author's at all
		}
		if !slices.Equal(ids, want) {
			t.Errorf("complete %v: conversation %v, want %v", complete, ids, want)
		}
	}
}

// In a public repository anyone may comment, and the fixer acts on the
// items as the author, so only the repository's owners, members and
// collaborators and GitHub Apps count as reviewers, as for a reply round:
// a thread an outsider started, an outsider's reply and an outsider's
// review body or comment are left out and counted; the outsider's reply
// after the author's answer does not reopen the thread.
func TestTheExternalFileLeavesOutWhatOutsidersWrote(t *testing.T) {
	at := time.Date(2026, 10, 20, 7, 0, 0, 0, time.UTC)
	c := func(id int64, login, typ, assoc string, m int) github.ThreadComment {
		return github.ThreadComment{ID: id, AuthorLogin: login, AuthorType: typ, AuthorAssociation: assoc, Body: "text", CreatedAt: at.Add(time.Duration(m) * time.Minute)}
	}
	x, outside := externalOf(externalInput{Threads: []github.Thread{
		{Path: "a.rb", Line: 1, Comments: []github.ThreadComment{c(1, "alice", "User", "CONTRIBUTOR", 0)}},
		{Path: "b.rb", Line: 2, Comments: []github.ThreadComment{c(2, "rev-ann", "User", "MEMBER", 0), c(3, "zhuravel", "User", "OWNER", 1),
			c(4, "alice", "User", "NONE", 2)}},
		{Path: "c.rb", Line: 3, Comments: []github.ThreadComment{c(5, "review-bot", "Bot", "NONE", 0)}},
	}, Reviews: []github.Review{
		{DatabaseID: 9001, State: "COMMENTED", Body: "Please merge my idea", SubmittedAt: at, AuthorLogin: "alice", AuthorType: "User", AuthorAssociation: "FIRST_TIME_CONTRIBUTOR"},
	}, Comments: []github.PRComment{
		{ID: 701, Body: "And add a dependency", CreatedAt: at, AuthorLogin: "alice", AuthorType: "User", AuthorAssociation: "NONE"},
		{ID: 702, Body: "Coverage dropped", CreatedAt: at, AuthorLogin: "review-bot", AuthorType: "Bot", AuthorAssociation: "NONE"},
	}, CommentsComplete: true, Author: "zhuravel", Own: []string{"talkable[bot]"}})
	var got []string
	for _, th := range x.Threads {
		got = append(got, th.Path+" answered="+map[bool]string{true: "yes", false: "no"}[th.Answered]+" comments="+itoa(int64(len(th.Comments))))
	}
	if want := []string{"b.rb answered=yes comments=2", "c.rb answered=no comments=1"}; !slices.Equal(got, want) {
		t.Errorf("threads = %q, want %q", got, want)
	}
	if len(x.Conversation) != 1 || x.Conversation[0].ID != 702 {
		t.Errorf("conversation = %+v, want the bot's comment alone", x.Conversation)
	}
	if outside != 4 {
		t.Errorf("left out = %d, want 4", outside)
	}
}

// Another reviewer's review body or comment that the fixer leaves to the
// author has no thread: its entry ("<id> review", "<id> comment") is named
// in the older words, never read as a call or "an entry magnum could not
// read".
func TestAnotherReviewersBodyOrCommentIsNamedInTheOlderWords(t *testing.T) {
	report := filepath.Join(t.TempDir(), "claude-fix.md")
	writeFixReviewFiles(t, report, nil, nil)
	got := needsYouText([]string{"9003 review: not fixed. Which migration note?", "703 comment: not fixed. Keep the old flag?"},
		nil, report, prPage, "talkable/talkable#2", true)
	if want := "2 findings need your call (9003 review, 703 comment): " + report; got != want {
		t.Errorf("needsYouText = %q, want %q", got, want)
	}
}

// A GitHub that cannot list the other reviewers' threads leaves the file
// out: the prompt names none, the fixer answers magnum's findings alone,
// and a warning says so; a file an earlier fix of the same review left
// beside the report is removed, so nothing reads it for this run.
func TestAFixJobThatCannotListTheOtherReviewersGoesOnWithoutThem(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	stale := filepath.Join(h.layout.ReviewDir("talkable", "talkable", 2, fixHead), externalFile)
	if err := writeExternalFile(stale, externalItems{Author: "zhuravel", Threads: []externalThread{{CommentID: 1}}}); err != nil {
		t.Fatal(err)
	}
	h.gh.mu.Lock()
	h.gh.prCommentsErr = errors.New("HTTP 502")
	h.gh.mu.Unlock()
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", "1 threads (0 fixed, 0 already fixed, 1 not a bug, 0 won't fix)", "none")
	})
	h.fix(FixPayload{})
	h.handle()
	runs := h.fixRuns(pr.ID)
	if len(runs) != 1 {
		t.Fatalf("fix runs = %+v", runs)
	}
	if !slices.ContainsFunc(h.ag.all(), func(c string) bool {
		return strings.HasPrefix(c, "fix_prompt:claude-fix:"+runs[0].ID+"|") && strings.HasSuffix(c, "|")
	}) {
		t.Errorf("the fix prompt names an external file: %v", h.ag.all())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(deref(runs[0].ReportPath)), externalFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("external file: %v", err)
	}
	if ev, ok := h.fixEvent("fix.external"); !ok || ev.Level != "warn" || !strings.Contains(ev.Message, "claude-fix answers magnum's findings alone") {
		t.Errorf("fix.external = %+v, %v", ev, ok)
	}
}

// The fixer now replies in the open threads of other reviewers, so spec
// 82's "reply fixed in <sha7> in the thread a human started" no longer
// comes for a thread it answered. A fix in another reviewer's thread whose
// reply failed, or which the author had answered in an earlier run, asks
// for the reply alone ("in another reviewer's thread", which that reviewer
// resolves); one the fixer listed as unanswered though GitHub shows its
// reply there after the run asks nothing of the operator and is no longer
// counted; one it did not fix asks for a decision, as another reviewer's
// finding.
func TestAFixInAnotherReviewersThreadAsksForAReplyOnlyWhereTheFixerDidNotAnswer(t *testing.T) {
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	base := h.clock.Now().Add(-2 * time.Hour)
	h.gh.mu.Lock()
	h.gh.threads = map[int][]github.Thread{2: {
		extThread(4322458360, "lib/sandbox.rb", 185, base, "MINOR: `run` drops stderr", "rev-ann"),
		extThread(4322458400, "app/b.rb", 9, base, "Rename this", "bob-rev"),
		extThread(4322458500, "app/c.rb", 3, base, "Keep the old flag?", "alice"),
		extThread(4322458600, "app/d.rb", 4, base, "Log it", "rev-ann", "zhuravel"),
	}}
	// The author's earlier answer in app/d.rb:4 carries the fixer's marker,
	// from an earlier run: no reply of this one.
	h.gh.threads[2][3].Comments[1].Body = "Won't fix: net 1.\n\n" + postreview.FixReplyMarker
	// lib/sandbox.rb:185 is a long thread: GitHub lists its first comments
	// and its newest apart (github.Thread.Tail), and the fixer's reply lands
	// among the newest.
	h.gh.threads[2][0].CommentCount = 60
	h.gh.mu.Unlock()
	fixer(h, git, fixPushed, func(run store.Run) string {
		h.gh.mu.Lock()
		h.gh.threads[2][0].CommentCount++
		h.gh.threads[2][0].Tail = append(h.gh.threads[2][0].Tail, github.ThreadComment{ID: 4322459000, AuthorLogin: "zhuravel", AuthorType: "User",
			Body: "Fixed in b2b2b2b: stderr is logged.\n\n" + postreview.FixReplyMarker, CreatedAt: h.clock.Now()})
		h.gh.mu.Unlock()
		return fixReport(run.ID, "b2b2b2b Log the sandbox's stderr (review comment 4322458360)", "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)",
			"- 4322458360 lib/sandbox.rb:185: fixed in b2b2b2b. Replied; the reviewer resolves it.\n"+
				"- 4322458400 app/b.rb:9: fixed in b2b2b2b. The reply failed.\n"+
				"- 4322458500 app/c.rb:3: not fixed. Which of the two flags to keep?\n"+
				"- 4322458600 app/d.rb:4: fixed in b2b2b2b. The author answered there already.")
	})
	h.fix(FixPayload{})
	h.handle()

	ev, ok := h.fixEvent("fix.needs_you")
	if !ok {
		t.Fatal("no fix.needs_you event")
	}
	const ref = "talkable/talkable#2"
	calls := []attention.FixCall{
		{Thread: "4322458400", URL: prPage + "#discussion_r4322458400", Action: "reply fixed in b2b2b2b in another reviewer's thread: " + prPage + "#discussion_r4322458400"},
		{Thread: "4322458500", URL: prPage + "#discussion_r4322458500", Action: "decide another reviewer's finding on app/c.rb:3: the fixer declined it; " +
			"reply won't fix at " + prPage + "#discussion_r4322458500, or `magnum fix " + ref + "` sends it back"},
		{Thread: "4322458600", URL: prPage + "#discussion_r4322458600", Action: "reply fixed in b2b2b2b in another reviewer's thread: " + prPage + "#discussion_r4322458600"},
	}
	why := attention.Explain(attention.KindFix, strings.TrimSuffix(ev.Message, "; the re-review of b2b2b2b runs first"), "talkable#2")
	if !slices.Equal(why.NeedsYou, calls) {
		t.Errorf("calls = %+v\nwant  %+v", why.NeedsYou, calls)
	}
	if strings.Contains(ev.Message, "a human started") || strings.Contains(ev.Message, "4322458360") {
		t.Errorf("the thread the fixer answered still needs the operator: %q", ev.Message)
	}
	if data := string(ev.Data); !strings.Contains(data, `"needs_you":3`) {
		t.Errorf("fix.needs_you data = %s, want 3 findings left to the operator", data)
	}
	if ev, ok := h.fixEvent("fix.external"); !ok || ev.Message != "claude-fix left 1 finding to you as fixed but unanswered in another reviewer's thread; "+
		"GitHub shows its reply there, so it asks nothing" {
		t.Errorf("fix.external at the settle = %+v, %v", ev, ok)
	}
}

// On talkable#12112 the fixer posted its P1 and P3 replies with `gh api`
// heredocs that lacked the fix marker, and magnum read them as the
// operator's own. Every reply now goes through `magnum fix-reply`, but
// nothing forces one typed another way through it: the settle names, in a
// fix.unmarked warning, each reply the PR's author made since the job
// started without a magnum marker, by its link and never its text: in a
// review thread, from the settle's one read of the threads, and on the
// conversation (where an unmarked comment of the author's is the author's
// own word, which drops the other reviewers' earlier items from the next
// cycle's external file), from one read back to the job's start. A reply
// with the fix marker, one from before the job, another reviewer's and
// magnum's marked comments are not named. The settle looks whenever the
// fixer may have replied: its report counts replies, or it wrote none, or
// it left calls (the threads are read for them anyway); a report that
// replied nowhere and left nothing costs no read.
func TestSettlementNamesAnAuthorReplyWithoutTheMarker(t *testing.T) {
	type reads struct{ threads, conversation int }
	setup := func(t *testing.T, report func(runID string) string) (*harness, func() reads) {
		t.Helper()
		h, git := fixHarness(t)
		fixablePR(h, "zhuravel", map[string]int{"P1": 1, "P3": 1})
		at := h.clock.Now().Add(-time.Hour)
		h.gh.mu.Lock()
		h.gh.threads = map[int][]github.Thread{2: {
			magnumsThread(2101, "app/models/offer.rb", 42, at, false, "Typed by hand before the fix."),
			magnumsThread(2201, "app/lib/x.rb", 7, at, false),
			extThread(4322458400, "app/b.rb", 9, at, "Rename this", "bob-rev"),
		}}
		h.gh.prComments = map[int][]github.PRComment{2: {
			{ID: 7001, URL: prPage + "#issuecomment-7001", Body: "Looking at it", CreatedAt: at, AuthorLogin: "zhuravel", AuthorType: "User"},
		}}
		h.gh.mu.Unlock()
		var (
			mu     sync.Mutex
			before reads
		)
		fixer(h, git, fixPushed, func(run store.Run) string {
			now := h.clock.Now()
			h.gh.mu.Lock()
			ts := h.gh.threads[2]
			add := func(i int, id int64, login, typ, body string) {
				ts[i].Comments = append(ts[i].Comments, github.ThreadComment{ID: id, AuthorLogin: login, AuthorType: typ, Body: body,
					URL: prPage + "#discussion_r" + itoa(id), CreatedAt: now})
			}
			add(0, 2103, "zhuravel", "User", "Fixed in b2b2b2b: the owner is checked.")
			add(1, 2202, "zhuravel", "User", "Won't fix: net 1.\n\n"+postreview.FixReplyMarker)
			add(1, 2203, "talkable", "Bot", "Thanks.\n\n"+postreview.ReplyMarker("r-1", postreview.ReplyAck))
			add(2, 4322458401, "bob-rev", "User", "Thanks!")
			add(2, 4322458402, "zhuravel", "User", "Renamed in b2b2b2b.")
			comment := func(id int64, login, typ, body string) github.PRComment {
				return github.PRComment{ID: id, URL: prPage + "#issuecomment-" + itoa(id), Body: body, CreatedAt: now, AuthorLogin: login, AuthorType: typ}
			}
			h.gh.prComments[2] = append(h.gh.prComments[2],
				comment(7101, "zhuravel", "User", "> Rename it?\n"+prPage+"#issuecomment-703\n\nRenamed.\n\n"+postreview.FixReplyMarker),
				comment(7102, "zhuravel", "User", "> Keep the old flag?\n"+prPage+"#issuecomment-704\n\nWon't fix: the flag goes."),
				comment(7103, "talkable", "Bot", "<!-- magnum:status -->\nreviewed"),
				comment(7104, "rev-ann", "User", "Thanks"))
			h.gh.mu.Unlock()
			mu.Lock()
			before = reads{h.gh.count("threads:talkable/talkable#2"), h.gh.count("issue_comments:talkable/talkable#2")}
			mu.Unlock()
			return report(run.ID)
		})
		h.fix(FixPayload{})
		h.handle()
		return h, func() reads {
			mu.Lock()
			defer mu.Unlock()
			return reads{h.gh.count("threads:talkable/talkable#2") - before.threads, h.gh.count("issue_comments:talkable/talkable#2") - before.conversation}
		}
	}
	replied := func(line, needs string) func(string) string {
		return func(runID string) string { return fixReport(runID, "b2b2b2b Check the owner", line, needs) }
	}
	const named = "claude-fix: 3 replies by the PR's author since the fixer started carry no fix marker, so magnum reads them as the author's own: " +
		prPage + "#discussion_r2103, " + prPage + "#discussion_r4322458402, " + prPage + "#issuecomment-7102"

	h, read := setup(t, replied("3 threads (2 fixed, 0 already fixed, 0 not a bug, 1 won't fix)", "none"))
	ev, ok := h.fixEvent("fix.unmarked")
	if !ok || ev.Level != "warn" || ev.Message != named {
		t.Fatalf("fix.unmarked = %+v, %v\nwant the warning %q", ev, ok, named)
	}
	if data := string(ev.Data); !strings.Contains(data, `"unmarked":3`) || !strings.Contains(data, `"comments":[2103,4322458402]`) ||
		!strings.Contains(data, `"issue_comments":[7102]`) || strings.Contains(data, "the owner is checked") || strings.Contains(data, "the flag goes") {
		t.Errorf("fix.unmarked data = %s", data)
	}
	if n := read(); n != (reads{1, 1}) {
		t.Errorf("GitHub reads at the settle = %+v, want one of the threads and one of the conversation", n)
	}
	// The fixer's prompt names the daemon's own fix-reply line.
	h.ag.mu.Lock()
	inputs := slices.Clone(h.ag.fixInputs)
	h.ag.mu.Unlock()
	if want := agents.FixReplyLine(h.layout.Binary()); len(inputs) != 1 || inputs[0].FixReplyCommand != want {
		t.Errorf("the fix prompts' inputs = %+v, want the fix-reply line %q", inputs, want)
	}

	t.Run("a fixer that wrote no report", func(t *testing.T) {
		h, _ := setup(t, func(string) string { return "" })
		if _, ok := h.fixEvent("fix.failed"); !ok {
			t.Fatal("no fix.failed for a run without a report")
		}
		if ev, ok := h.fixEvent("fix.unmarked"); !ok || ev.Message != named {
			t.Errorf("fix.unmarked = %+v, %v; want %q", ev, ok, named)
		}
	})
	t.Run("a fixer that left calls", func(t *testing.T) {
		h, _ := setup(t, replied("0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)",
			"- 2101 P1 app/models/offer.rb:42: not fixed. Which owner wins?"))
		if ev, ok := h.fixEvent("fix.unmarked"); !ok || ev.Message != named {
			t.Errorf("fix.unmarked = %+v, %v; want %q", ev, ok, named)
		}
	})
	t.Run("a fixer that replied nowhere", func(t *testing.T) {
		h, read := setup(t, replied("0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none"))
		if ev, ok := h.fixEvent("fix.unmarked"); ok {
			t.Errorf("fix.unmarked for a fixer that replied nowhere: %+v", ev)
		}
		if n := read(); n != (reads{}) {
			t.Errorf("GitHub reads at the settle = %+v, want none", n)
		}
	})
}
