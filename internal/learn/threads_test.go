package learn

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/store"
)

// ownBody is a finding as magnum posts it: the priority and title in bold
// on the first line, the explanation below.
const ownBody = "**[P2] Coupon lookup ignores the site**\n\nThe finder takes the code from the request and never scopes it."

// ownThread is a review thread magnum's App started on sha at line, an
// hour after t0, in review reviewID, answered by replies (login, body) in
// order, each a minute after the last.
func ownThread(id int64, path string, line int, sha string, reviewID int64, replies ...[2]string) github.Thread {
	th := thread(id, path, line, sha, ownBody)
	root := &th.Comments[0]
	root.AuthorLogin, root.AuthorType, root.ReviewID = "talkable", "Bot", reviewID
	for i, r := range replies {
		typ := "User"
		if r[0] == "talkable" || r[0] == "lint-bot" {
			typ = "Bot"
		}
		th.Comments = append(th.Comments, github.ThreadComment{ID: id*100 + int64(i), AuthorLogin: r[0], AuthorType: typ, Body: r[1],
			CreatedAt: root.CreatedAt.Add(time.Duration(i+1) * time.Minute), ReviewID: reviewID*100 + int64(i),
			URL: "https://github.com/talkable/widgets/pull/7#discussion_r" + strconv.FormatInt(id*100+int64(i), 10)})
	}
	return th
}

// ownInput is input with review 501 one magnum posted, by run r-1.
func ownInput(threads []github.Thread) Input {
	in := input(threads, nil)
	in.ReviewRuns = map[int64]string{501: "r-1"}
	return in
}

func threadIDs(ts []OwnThread) []string {
	out := []string{}
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

// TestOwnThreadsKeepOnlyMagnumsRootsWithOthersReplies: a thread is magnum's
// when its root comment is one of Own's logins in a review magnum posted
// (ReviewRuns), and it is kept only when someone other than magnum
// replied: the author or another reviewer, not a ghost, not a bot unless
// IncludeBots. The replies come in order, magnum's App's marked OwnAuthor,
// and Build carries the threads beside the candidates, never as
// candidates.
func TestOwnThreadsKeepOnlyMagnumsRootsWithOthersReplies(t *testing.T) {
	user := ownThread(7, "app/a.rb", 3, shaA, 501, [2]string{"rev-ann", "Is this reachable?"})
	user.Comments[0].AuthorLogin, user.Comments[0].AuthorType = "ZHURAVEL", "User" // magnum's user identity, any case
	threads := []github.Thread{
		ownThread(1, "app/a.rb", 10, shaA, 501, [2]string{"alice", "Fixed, thanks."}),
		ownThread(2, "app/a.rb", 20, shaA, 501, [2]string{"talkable", "Still open at the new head."}), // only magnum replied
		ownThread(3, "app/a.rb", 30, shaA, 501),                                                       // nobody replied
		thread(4, "app/a.rb", 40, shaA, longish),                                                      // rev-ann's own thread
		ownThread(5, "app/a.rb", 50, shaA, 501, [2]string{"lint-bot", "Rule X fires here."}),          // only a bot replied
		ownThread(6, "app/a.rb", 60, shaA, 501, [2]string{"", "A ghost's reply."}),
		user,
		ownThread(8, "app/a.rb", 80, shaA, 501, [2]string{"rev-ann", "Not a problem: the caller checks it."},
			[2]string{"talkable", "The caller does not run on the import path."}, [2]string{"alice", "Won't fix."}),
	}
	in := ownInput(threads)
	got := OwnThreads(in)
	if ids := threadIDs(got); !slices.Equal(ids, []string{"o1", "o7", "o8"}) {
		t.Fatalf("own threads = %v, want [o1 o7 o8]", ids)
	}
	want := []ThreadReply{{Author: "rev-ann", Body: "Not a problem: the caller checks it."},
		{Author: OwnAuthor, Body: "The caller does not run on the import path."}, {Author: "alice", Body: "Won't fix."}}
	if o8 := got[2]; !reflect.DeepEqual(o8.Replies, want) || o8.CommentID != 8 || o8.URL != threads[7].Comments[0].URL ||
		o8.Path != "app/a.rb" || o8.Line != 80 || o8.ReviewID != 501 {
		t.Fatalf("o8 = %+v", o8)
	}

	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if ids := threadIDs(res.Threads); !slices.Equal(ids, []string{"o1", "o7", "o8"}) {
		t.Fatalf("Build's threads = %v", ids)
	}
	if ids := ids(res.Candidates); !slices.Equal(ids, []string{"t4"}) {
		t.Fatalf("Build's candidates = %v, want only rev-ann's own thread", ids)
	}

	in.IncludeBots = true
	if ids := threadIDs(OwnThreads(in)); !slices.Equal(ids, []string{"o1", "o5", "o7", "o8"}) {
		t.Fatalf("with include_bots: %v", ids)
	}
}

// TestAThreadTheOperatorStartedByHandIsNotMagnums: a configured gh
// identity is the operator's own login, so a root comment by it is
// magnum's only in a review magnum posted (ReviewRuns). One the operator
// wrote in a review of his own, or one whose review is unknown, is no own
// thread (and, by one of magnum's logins, no candidate either), whoever
// answered it.
func TestAThreadTheOperatorStartedByHandIsNotMagnums(t *testing.T) {
	byHand := ownThread(1, "app/a.rb", 10, shaA, 777, [2]string{"alice", "No, the caller handles it."})
	byHand.Comments[0].AuthorLogin, byHand.Comments[0].AuthorType = "zhuravel", "User"
	byHand.Comments[0].Body = "Could you rename this variable?"
	posted := ownThread(2, "app/a.rb", 20, shaA, 501, [2]string{"alice", "No, the caller handles it."})
	posted.Comments[0].AuthorLogin, posted.Comments[0].AuthorType = "zhuravel", "User" // magnum posting as the operator
	unknown := ownThread(3, "app/a.rb", 30, shaA, 0, [2]string{"alice", "No, the caller handles it."})
	in := ownInput([]github.Thread{byHand, posted, unknown})
	if ids := threadIDs(OwnThreads(in)); !slices.Equal(ids, []string{"o2"}) {
		t.Fatalf("own threads = %v, want only o2, magnum's review's", ids)
	}
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := threadIDs(res.Threads); !slices.Equal(got, []string{"o2"}) || len(res.Candidates) != 0 {
		t.Fatalf("Build: threads %v, candidates %v", got, ids(res.Candidates))
	}
}

// TestOnlyAMarkedOrAppReplyIsMagnums: a reply is magnum's (OwnAuthor) when
// it carries magnum's reply marker or comes from one of magnum's Apps. The
// operator's own login replying by hand is a person who answered: the
// thread is kept with his reply under his login.
func TestOnlyAMarkedOrAppReplyIsMagnums(t *testing.T) {
	marked := "The caller does not run on the import path.\n\n<!-- magnum:reply run=r-1 kind=rebuttal -->"
	threads := []github.Thread{
		ownThread(1, "app/a.rb", 10, shaA, 501, [2]string{"zhuravel", "Agreed with the author, drop it."}),
		ownThread(2, "app/a.rb", 20, shaA, 501, [2]string{"zhuravel", marked}),
		ownThread(3, "app/a.rb", 30, shaA, 501, [2]string{"zhuravel", marked}, [2]string{"alice", "Won't fix."}),
		ownThread(4, "app/a.rb", 40, shaA, 501, [2]string{"talkable", "Still open at the new head."}),
	}
	got := OwnThreads(ownInput(threads))
	if ids := threadIDs(got); !slices.Equal(ids, []string{"o1", "o3"}) {
		t.Fatalf("own threads = %v, want [o1 o3]", ids)
	}
	if want := []ThreadReply{{Author: "zhuravel", Body: "Agreed with the author, drop it."}}; !reflect.DeepEqual(got[0].Replies, want) {
		t.Fatalf("o1's replies = %+v", got[0].Replies)
	}
	if want := []ThreadReply{{Author: OwnAuthor, Body: marked}, {Author: "alice", Body: "Won't fix."}}; !reflect.DeepEqual(got[1].Replies, want) {
		t.Fatalf("o3's replies = %+v", got[1].Replies)
	}
}

// fixerSays is a reply the fixer posts: its text and the fixer's marker.
func fixerSays(text string) string { return text + "\n\n" + postreview.FixReplyMarker }

// TestAFixerReplyIsNotTheAuthors: the fixer posts as the PR's author (the
// operator, whose login a gh identity shares), so a reply by the author or
// one of magnum's logins that carries the fixer's marker is the fixer's
// (FixerAuthor), not the author's. It still answers the finding, so the
// thread goes to the classifier, marked Fixer when no person answered
// beside it; a person's reply next to it makes it a person's thread, and
// the marker in a stranger's reply makes no fixer of them.
func TestAFixerReplyIsNotTheAuthors(t *testing.T) {
	declined := fixerSays("Won't fix: the caller checks the site at `app/a.rb:12`.")
	threads := []github.Thread{
		ownThread(1, "app/a.rb", 10, shaA, 501, [2]string{"alice", declined}),
		ownThread(2, "app/a.rb", 20, shaA, 501, [2]string{"zhuravel", declined}),
		ownThread(3, "app/a.rb", 30, shaA, 501, [2]string{"alice", declined}, [2]string{"rev-ann", "The caller does not run on import."}),
		ownThread(4, "app/a.rb", 40, shaA, 501, [2]string{"rev-ann", declined}),
		ownThread(5, "app/a.rb", 50, shaA, 501, [2]string{"alice", "Won't fix: the caller checks the site."}),
	}
	got := OwnThreads(ownInput(threads))
	if ids := threadIDs(got); !slices.Equal(ids, []string{"o1", "o2", "o3", "o4", "o5"}) {
		t.Fatalf("own threads = %v, want all five: the fixer answers a finding too", ids)
	}
	for i, want := range []struct {
		fixer   bool
		authors []string
	}{
		{true, []string{FixerAuthor}},
		{true, []string{FixerAuthor}},
		{false, []string{FixerAuthor, "rev-ann"}},
		{false, []string{"rev-ann"}},
		{false, []string{"alice"}},
	} {
		var authors []string
		for _, r := range got[i].Replies {
			authors = append(authors, r.Author)
		}
		if got[i].Fixer != want.fixer || !slices.Equal(authors, want.authors) {
			t.Errorf("%s: fixer %v, authors %v; want %v, %v", got[i].ID, got[i].Fixer, authors, want.fixer, want.authors)
		}
	}
	if got[0].Replies[0].Body != declined {
		t.Errorf("o1's reply = %q, want the fixer's text as posted", got[0].Replies[0].Body)
	}
}

// TestAFixersAnswerTeachesNothing: the classifier gives a thread only the
// fixer answered its outcome and verdict, as for any thread, but the
// fixer's choice is no author's verdict on magnum, so no lesson, scope or
// match is kept (no learned case, no repository miss can come of it) and
// none is needed; a person's thread still needs its lesson.
func TestAFixersAnswerTeachesNothing(t *testing.T) {
	threads := OwnThreads(ownInput([]github.Thread{
		ownThread(1, "app/a.rb", 10, shaA, 501, [2]string{"alice", fixerSays("Won't fix: net 1, the caller checks the site.")}),
		ownThread(2, "app/a.rb", 20, shaA, 501, [2]string{"alice", fixerSays("Not a bug: the caller checks the site.")}),
		ownThread(3, "app/a.rb", 30, shaA, 501, [2]string{"alice", "Not a bug: the caller checks the site."}),
	}))
	lesson := `"lesson":"When the caller checks the site, do not report the finder.","scope":"repo","match":["site"]`
	out := `{"items":[],"threads":[` +
		`{"id":"o1","outcome":"declined","verdict":"missing_context",` + lesson + `},` +
		`{"id":"o2","outcome":"declined","verdict":"wrong"},` +
		`{"id":"o3","outcome":"declined","verdict":"wrong",` + lesson + `}]}`
	_, got, err := ParseOutput([]byte(out), nil, nil, threads)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]ThreadItem{
		"o1": {ID: "o1", Outcome: store.DeclineDeclined, Verdict: store.VerdictMissingContext},
		"o2": {ID: "o2", Outcome: store.DeclineDeclined, Verdict: store.VerdictWrong},
		"o3": {ID: "o3", Outcome: store.DeclineDeclined, Verdict: store.VerdictWrong,
			Lesson: "When the caller checks the site, do not report the finder.", Scope: store.MissScopeRepo, Match: []string{"site"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("thread items = %+v\nwant %+v", got, want)
	}
	if _, _, err := ParseOutput([]byte(`{"items":[],"threads":[{"id":"o1","outcome":"declined"},`+
		`{"id":"o2","outcome":"fixed"},{"id":"o3","outcome":"fixed"}]}`), nil, nil, threads); err == nil ||
		!strings.Contains(err.Error(), "o1") || !strings.Contains(err.Error(), "needs a verdict") {
		t.Fatalf("a fixer's decline without a verdict: err = %v, want o1 refused", err)
	}
	if _, _, err := ParseOutput([]byte(`{"items":[],"threads":[{"id":"o1","outcome":"fixed"},`+
		`{"id":"o2","outcome":"fixed"},{"id":"o3","outcome":"declined","verdict":"wrong"}]}`), nil, nil, threads); err == nil ||
		!strings.Contains(err.Error(), "o3") || !strings.Contains(err.Error(), "needs a lesson") {
		t.Fatalf("a person's wrong verdict without a lesson: err = %v, want o3 refused", err)
	}
}

// TestOwnThreadsBindTheReviewedCommitAndTheFinding: a thread's reviewed
// commit is its root's when magnum reviewed that commit, else the newest
// reviewed before the root; its finding is the posted one of the same
// review's run (ReviewRuns) on the same path within NearLines, never a
// rejected one or another review's; its title is the root's first line
// without the bold and the priority tag, and its priority the first P0 to
// P3 in it.
func TestOwnThreadsBindTheReviewedCommitAndTheFinding(t *testing.T) {
	reply := [2]string{"alice", "We never pass a foreign code here."}
	onC := ownThread(1, "app/models/coupon.rb", 42, shaC, 502, reply)
	onB := ownThread(2, "app/models/coupon.rb", 42, shaB, 503, reply) // a commit magnum did not review, a run without findings
	onB.Comments[0].CreatedAt = t0.Add(2 * time.Hour)
	far := ownThread(3, "app/models/coupon.rb", 60, shaA, 501, reply)
	plain := ownThread(4, "app/models/gift.rb", 5, shaA, 501, reply)
	plain.Comments[0].Body = "\n  Gift cards skip the balance check  \nmore"
	in := input([]github.Thread{onC, onB, far, plain}, nil)
	in.Reviewed = []Reviewed{{SHA: shaA, At: t0}, {SHA: shaC, At: t0.Add(3 * time.Hour)}}
	in.ReviewRuns = map[int64]string{501: "r-1", 502: "r-2", 503: "r-3"}
	in.Findings = []store.Finding{
		{RunID: "r-1", FindingID: "F1", Path: "app/models/coupon.rb", Line: 42, Verdict: store.FindingPosted},   // review 501's
		{RunID: "r-2", FindingID: "F1", Path: "app/models/coupon.rb", Line: 44, Verdict: store.FindingPosted},   // review 502's
		{RunID: "r-2", FindingID: "F2", Path: "app/models/coupon.rb", Line: 42, Verdict: store.FindingRejected}, // rejected
		{RunID: "r-1", FindingID: "F3", Path: "app/models/coupon.rb", Line: 56, Verdict: store.FindingPosted},   // 4 lines from 60
	}
	got := OwnThreads(in)
	if len(got) != 4 {
		t.Fatalf("own threads = %v", threadIDs(got))
	}
	for i, want := range []struct{ sha, ref, title, priority string }{
		{shaC, "r-2/F1", "Coupon lookup ignores the site", "P2"},
		{shaA, "", "Coupon lookup ignores the site", "P2"},
		{shaA, "", "Coupon lookup ignores the site", "P2"},
		{shaA, "", "Gift cards skip the balance check", ""},
	} {
		o := got[i]
		if o.ReviewedSHA != want.sha || o.FindingRef != want.ref || o.Title != want.title || o.Priority != want.priority {
			t.Errorf("%s: reviewed %s, finding %q, title %q, priority %q; want %s, %q, %q, %q", o.ID, short(o.ReviewedSHA),
				o.FindingRef, o.Title, o.Priority, short(want.sha), want.ref, want.title, want.priority)
		}
	}
	// The same review's finding within NearLines binds.
	near := ownThread(5, "app/models/coupon.rb", 45, shaA, 501, reply)
	in.Threads = []github.Thread{near}
	if got := OwnThreads(in); len(got) != 1 || got[0].FindingRef != "r-1/F1" {
		t.Fatalf("a thread 3 lines from its review's finding: %+v", got)
	}
}

// TestAFixedReplyNeedsNoLesson: an author who only quotes the finding and
// says it is fixed answers with outcome fixed: no verdict, lesson, scope or
// match is needed, and any the classifier adds is dropped.
func TestAFixedReplyNeedsNoLesson(t *testing.T) {
	th := ownThread(9, "app/a.rb", 10, shaA, 501, [2]string{"alice", "> **[P2] Coupon lookup ignores the site**\n\nfixed in abc1234"})
	threads := OwnThreads(ownInput([]github.Thread{th}))
	if len(threads) != 1 || threads[0].ID != "o9" {
		t.Fatalf("own threads = %+v", threads)
	}
	for _, out := range []string{
		`{"items":[],"threads":[{"id":"o9","outcome":"fixed"}]}`,
		`{"items":[],"threads":[{"id":"o9","outcome":"fixed","verdict":"wrong","lesson":"When x, check y.","scope":"general","match":["x"]}]}`,
	} {
		items, got, err := ParseOutput([]byte(out), nil, nil, threads)
		if err != nil {
			t.Fatalf("%s: %v", out, err)
		}
		if len(items) != 0 || !reflect.DeepEqual(got, map[string]ThreadItem{"o9": {ID: "o9", Outcome: store.DeclineFixed}}) {
			t.Fatalf("%s: items %+v, threads %+v", out, items, got)
		}
	}
}

// TestFindingTitleReadsASimplificationThread: a simplification comment's
// first line is the heading alone ("**Simplification** (keeps behavior:
// apply it, or reply with the risk)" since 2026-10-29, and the stored rows
// and posted threads carry the earlier "**Simplification** (optional, no
// reply needed)" and "**Simplification (optional)**"), and what the
// proposal removes is the sentence below it, so the title is that sentence
// and not the heading with half its bold left on; a simplification has no
// priority, and a heading with no sentence under it stays "Simplification".
func TestFindingTitleReadsASimplificationThread(t *testing.T) {
	const (
		suggestion = "```suggestion\nreturn Foo(x)\n```"
		heading    = "**Simplification** (keeps behavior: apply it, or reply with the risk)"
	)
	for name, c := range map[string]struct{ body, title string }{
		"the current heading": {heading + "\n\nThe helper duplicates `Foo`; call it instead.\n\n" + suggestion,
			"Simplification: The helper duplicates `Foo`; call it instead."},
		"the heading before 2026-10-29": {"**Simplification** (optional, no reply needed)\n\nThe helper duplicates `Foo`; call it instead.\n\n" + suggestion,
			"Simplification: The helper duplicates `Foo`; call it instead."},
		"the older heading": {"**Simplification (optional)**\n\nThe flag is read once and never changes.\n" + suggestion,
			"Simplification: The flag is read once and never changes."},
		"the first sentence of a paragraph": {heading + "\n\nDrops the `a. b` copy. The rest is the same\nacross two lines.\n\n" + suggestion,
			"Simplification: Drops the `a. b` copy."},
		"a paragraph over two lines": {heading + "\n\nRemoves the second pass\nover the rows.\n\n" + suggestion,
			"Simplification: Removes the second pass over the rows."},
		"emphasis inside the sentence": {heading + "\n\nThe **second** loop is _redundant_.\n\n" + suggestion,
			"Simplification: The second loop is redundant."},
		"no sentence below the heading":          {heading + "\n\n" + suggestion, "Simplification"},
		"nothing below the heading":              {heading, "Simplification"},
		"nothing below an older heading":         {"**Simplification** (optional, no reply needed)", "Simplification"},
		"a title that only starts with the word": {"**Simplification of the retry loop**\n\nSomething.", "Simplification of the retry loop"},
		"a title that is already a sentence":     {"**Simplification: drop the copy**\n\nSomething.", "Simplification: drop the copy"},
	} {
		if title, priority := findingTitle(c.body); title != c.title || priority != "" {
			t.Errorf("%s: title %q priority %q; want %q and none", name, title, priority, c.title)
		}
	}
	long := heading + "\n\n" + strings.Repeat("word ", OwnTitleMax)
	if title, _ := findingTitle(long); utf8.RuneCountInString(title) != OwnTitleMax || !strings.HasPrefix(title, "Simplification: word ") || !strings.HasSuffix(title, "…") {
		t.Errorf("a sentence of %d runes gives %q", len(long), title)
	}
}

// TestFindingTitleStripsInnerMarkup: the bold and italic markers go from
// the whole first line, not only its ends, with the "[P2]" tag in front;
// what is no emphasis stays: an underscore inside a word, and anything in
// a code span.
func TestFindingTitleStripsInnerMarkup(t *testing.T) {
	for body, want := range map[string][2]string{
		"**[P2] A **bold** word**":                                    {"A bold word", "P2"},
		"__[P1] An _italic_ and *starred* word__":                     {"An italic and starred word", "P1"},
		"**[P3]** Trailing emphasis*":                                 {"Trailing emphasis", "P3"},
		"**[P2] `snake_case` stays, so does snake_case, and `*Foo`**": {"`snake_case` stays, so does snake_case, and `*Foo`", "P2"},
		"  \n**[P0] Second line is not read**\nmore":                  {"Second line is not read", "P0"},
		"****": {"", ""},
	} {
		if title, priority := findingTitle(body); title != want[0] || priority != want[1] {
			t.Errorf("%q: title %q priority %q; want %q and %q", body, title, priority, want[0], want[1])
		}
	}
}
