package postreview

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
)

// appLogin is the fixer App magnum comment posts as ("fixer-app" in
// GraphQL); the judge's identity is "talkable[bot]" (bot) and a former
// reviewer's App "old-app[bot]".
const appLogin = "fixer-app[bot]"

// commentPull is pull request 5 as the fake reads it: opened by alice,
// with its title and the users asked for a review (nil: none).
type commentPull struct {
	title     string
	requested []string
	// patchFail and requestFail answer the PATCH and the review request
	// POST when set.
	patchFail, requestFail func(c execx.Cmd) (execx.Result, error)
	titles                 []string   // the titles PATCHed, in order
	requests               [][]string // the reviewers of each request POST
}

// commentWorld is a world whose posts go out as the fixer App, with pull
// request 5's title and review requests read and written through p.
func commentWorld(t *testing.T) (*world, *commentPull) {
	t.Helper()
	w := newWorld(t)
	w.postAs = "fixer-app"
	p := &commentPull{title: "Fix the total"}
	const pull = "repos/talkable/talkable/pulls/5"
	w.fake.Rules = append([]execx.Rule{
		{Prefix: []string{"gh", "api", "-X", "PATCH", pull}, Fn: func(c execx.Cmd) (execx.Result, error) {
			var req struct {
				Title string `json:"title"`
			}
			if err := json.Unmarshal(c.Stdin, &req); err != nil {
				t.Fatalf("PATCH without a JSON body on stdin: %v", err)
			}
			p.titles = append(p.titles, req.Title)
			if p.patchFail != nil {
				return p.patchFail(c)
			}
			p.title = req.Title
			return ok(map[string]any{"number": 5, "title": p.title, "state": "open", "html_url": "https://github.com/talkable/talkable/pull/5",
				"user": map[string]any{"login": "alice", "type": "User"}})
		}},
		{Prefix: []string{"gh", "api", "-X", "POST", pull + "/requested_reviewers"}, Fn: func(c execx.Cmd) (execx.Result, error) {
			var req struct {
				Reviewers []string `json:"reviewers"`
			}
			if err := json.Unmarshal(c.Stdin, &req); err != nil {
				t.Fatalf("review request without a JSON body on stdin: %v", err)
			}
			p.requests = append(p.requests, req.Reviewers)
			if p.requestFail != nil {
				return p.requestFail(c)
			}
			p.requested = append(p.requested, req.Reviewers...)
			return ok(map[string]any{"number": 5})
		}},
		{Prefix: []string{"gh", "api", pull}, Fn: func(execx.Cmd) (execx.Result, error) {
			var requested []map[string]any
			for _, l := range p.requested {
				requested = append(requested, map[string]any{"login": l, "type": "User"})
			}
			return ok(map[string]any{"number": 5, "title": p.title, "state": "open", "html_url": "https://github.com/talkable/talkable/pull/5",
				"user": map[string]any{"login": "alice", "type": "User"}, "requested_reviewers": requested})
		}},
	}, w.fake.Rules...)
	return w, p
}

// copts names pull request 5, a reply in the thread of comment replyTo (0:
// the conversation), as the fixer App.
func copts(replyTo int64) CommentOptions {
	return CommentOptions{Owner: "talkable", Repo: "talkable", Number: 5, ReplyTo: replyTo, Login: appLogin, Identity: "fixer-app",
		MagnumLogins: []string{"talkable[bot]", appLogin, "old-app[bot]"}}
}

// runComment runs magnum comment on o with text on stdin and returns what
// it reported posted.
func (w *world) runComment(o CommentOptions, text string) (CommentOutcome, []Posted) {
	var posted []Posted
	out := RunComment(context.Background(), &github.Client{Run: w.fake, Env: map[string]string{"GH_CONFIG_DIR": "/state/gh/fixer-app"}}, o, []byte(text),
		func(p Posted) { posted = append(posted, p) })
	return out, posted
}

// rev is a comment of another reviewer, a member.
func rev(id int64, body string) fakeComment {
	return fakeComment{ID: id, Author: "rev-ann", Body: body}
}

// app is a comment of the fixer App.
func app(id int64, body string) fakeComment {
	return fakeComment{ID: id, Author: "fixer-app", Bot: true, Body: body}
}

// writes counts the calls that write to GitHub.
func (w *world) writes() int {
	return w.calls("gh", "api", "-X", "POST") + w.calls("gh", "api", "-X", "PATCH")
}

// The marker of magnum comment is one of magnum's (every magnum marker
// starts the same), but neither the fixer's nor the judge's: magnum tells
// these comments from both, and a reader that asks whether magnum's side
// answered (IsOwnAnswer) takes both.
func TestCommentCarriesAMarkerOfItsOwn(t *testing.T) {
	if CommentMarker != "<!-- magnum:comment -->" || !strings.HasPrefix(CommentMarker, anyMarker) {
		t.Fatalf("marker = %q", CommentMarker)
	}
	body := "Renamed in a1b2c3d.\n\n" + CommentMarker
	if !IsComment(body) || IsFixReply(body) || !IsOwnAnswer(body) {
		t.Errorf("IsComment %v, IsFixReply %v, IsOwnAnswer %v", IsComment(body), IsFixReply(body), IsOwnAnswer(body))
	}
	if _, _, marked := ParseReplyMarker(body); marked {
		t.Error("a comment reads as the judge's reply")
	}
	if fix := "Fixed.\n\n" + FixReplyMarker; IsComment(fix) || !IsOwnAnswer(fix) {
		t.Errorf("a fix reply: IsComment %v, IsOwnAnswer %v", IsComment(fix), IsOwnAnswer(fix))
	}
	if IsOwnAnswer("Fixed in a1b2c3d.") || IsOwnAnswer(marked("Thanks.", runID, ReplyAck)) {
		t.Error("an unmarked comment or a judge's reply is an answer of magnum's side")
	}
}

// A chat session's comment goes out as the App the way the fixer's reply
// does: the text, a blank line and the comment marker, on gh's stdin and
// never in argv, as a reply to the first comment of the thread the id
// names (any of its comments) or on the conversation; each post is
// reported to the caller, which records it.
func TestCommentEndsTheBodyWithTheCommentMarker(t *testing.T) {
	w, _ := commentWorld(t)
	w.threads = []fakeThread{thread(rev(11, "Rename this?"), human(12, "Which name?"))}
	text := "Renamed to `order_total` in a1b2c3d.\n"
	out, posted := w.runComment(copts(12), text)
	if out.Status != StatusPosted || out.ExitCode() != 0 || out.Comment == nil || out.Comment.Kind != PostReply || out.Comment.Thread != 11 ||
		out.Comment.ID != 5001 || out.Comment.URL != "https://github.com/talkable/talkable/pull/5#discussion_r5001" || out.As != appLogin ||
		out.Identity != "fixer-app" || out.PR != "talkable/talkable#5" {
		t.Fatalf("reply: outcome = %+v (comment %+v)", out, out.Comment)
	}
	want := "Renamed to `order_total` in a1b2c3d.\n\n" + CommentMarker
	if len(w.replies) != 1 || w.replies[0].CommentID != 11 || w.replies[0].Body != want {
		t.Errorf("replies = %+v, want one to comment 11 with %q", w.replies, want)
	}
	if s := out.Summary(); s != "as fixer-app[bot]: posted a reply in the thread of comment 11: https://github.com/talkable/talkable/pull/5#discussion_r5001" {
		t.Errorf("summary = %q", s)
	}
	if len(posted) != 1 || !reflect.DeepEqual(posted[0], Posted{Kind: PostReply, CommentID: 5001, Thread: 11, URL: out.Comment.URL}) {
		t.Errorf("posted = %+v", posted)
	}

	conv := "> Please split this.\nhttps://github.com/talkable/talkable/pull/5#issuecomment-77\n\nSplit in a1b2c3d."
	out, posted = w.runComment(copts(0), conv)
	if out.Status != StatusPosted || out.Comment == nil || out.Comment.Kind != PostConversation || out.Comment.Thread != 0 || out.Comment.ID != 7001 {
		t.Fatalf("conversation: outcome = %+v (comment %+v)", out, out.Comment)
	}
	if len(w.issuePosts) != 1 || w.issuePosts[0] != conv+"\n\n"+CommentMarker {
		t.Errorf("conversation posts = %q", w.issuePosts)
	}
	if s := out.Summary(); s != "as fixer-app[bot]: posted a comment on the conversation: https://github.com/talkable/talkable/pull/5#issuecomment-7001" {
		t.Errorf("summary = %q", s)
	}
	if len(posted) != 1 || posted[0].Kind != PostConversation || posted[0].CommentID != 7001 {
		t.Errorf("posted = %+v", posted)
	}
	for _, post := range w.fake.CallsWithPrefix("gh", "api", "-X", "POST") {
		if !post.Mutates || slices.ContainsFunc(post.Args, func(a string) bool { return strings.Contains(a, "Renamed") || strings.Contains(a, "Split") }) {
			t.Errorf("POST = %+v: the text in argv, or not marked as a write", post)
		}
	}
}

// What fix-reply refuses, magnum comment refuses before it asks GitHub
// anything (exit 2: fix the comment): an empty comment, any magnum marker
// (the fixer's, the judge's, a run's, a status comment's, its own), a local
// path, and a comment too long for GitHub once marked. One that fills the
// limit exactly, the marker included, goes out.
func TestCommentRefusesWhatFixReplyRefuses(t *testing.T) {
	for name, tc := range map[string]struct{ text, want string }{
		"the comment marker": {"Renamed. " + CommentMarker, "the comment carries a <!-- magnum:… marker: magnum comment appends the comment marker, leave it out"},
		"the fix marker":     {"Fixed.\n\n" + FixReplyMarker, "the comment carries a <!-- magnum:… marker"},
		"a reply marker":     {marked("Thanks.", runID, ReplyAck), "the comment carries a <!-- magnum:… marker"},
		"a run marker":       {"Done.\n" + RunMarker(runID, head), "the comment carries a <!-- magnum:… marker"},
		"a status marker":    {"Done. <!-- magnum:status -->", "the comment carries a <!-- magnum:… marker"},
		"an empty comment":   {" \n\t", "the comment is empty: pass it on standard input (a pipe or a heredoc)"},
		"a local path":       {"See /Users/alice/Projects/talkable/app/a.rb:12.", `the comment names the local path "/Users/alice/Projects/talkable/app/a.rb:12"`},
		"an endless comment": {strings.Repeat("x", MaxBody), "the comment is 65561 characters (the comment marker included); GitHub takes at most 65536"},
	} {
		t.Run(name, func(t *testing.T) {
			w, _ := commentWorld(t)
			w.threads = []fakeThread{thread(rev(11, "Rename this?"))}
			for _, o := range []CommentOptions{copts(11), copts(0)} {
				out, posted := w.runComment(o, tc.text)
				if out.Status != StatusInvalid || out.ExitCode() != 2 || len(out.Problems) != 1 || !strings.HasPrefix(out.Problems[0], tc.want) || len(posted) != 0 {
					t.Errorf("reply to %d: outcome = %+v, want invalid with %q", o.ReplyTo, out, tc.want)
				}
			}
			if w.calls("gh") != 0 {
				t.Errorf("gh ran %d times for a refused comment", w.calls("gh"))
			}
		})
	}
	w, _ := commentWorld(t)
	room := MaxBody - len("\n\n"+CommentMarker)
	if out, _ := w.runComment(copts(0), strings.Repeat("x", room)); out.Status != StatusPosted {
		t.Errorf("a comment that fills the limit: outcome = %+v", out)
	}
}

// A comment id that is no review comment of this pull request is refused
// and nothing goes out; when GitHub listed only the first threads the
// words say so. A failed read is an error (exit 1).
func TestCommentRefusesACommentOfAnotherPR(t *testing.T) {
	w, _ := commentWorld(t)
	w.threads = []fakeThread{thread(rev(11, "Rename this?"))}
	out, _ := w.runComment(copts(4100000001), "Renamed.")
	if out.Status != StatusInvalid || out.ExitCode() != 2 || len(out.Problems) != 1 ||
		out.Problems[0] != "comment 4100000001 is no review comment of talkable/talkable#5: answer a thread of this pull request" {
		t.Fatalf("outcome = %+v", out)
	}

	cut, _ := commentWorld(t)
	cut.fake.Rules = append([]execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		if !strings.Contains(string(c.Stdin), "reviewThreads") {
			return cut.gh(c)
		}
		return ok(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"reviewThreads": map[string]any{"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "c"}, "nodes": []any{}}}}}})
	}}}, cut.fake.Rules...)
	if out, _ := cut.runComment(copts(11), "Renamed."); out.Status != StatusInvalid || len(out.Problems) != 1 ||
		out.Problems[0] != "comment 11 is in none of the first 1000 review threads of talkable/talkable#5" {
		t.Errorf("an incomplete list: outcome = %+v", out)
	}

	w.threadsErr = errors.New("gh: network is down")
	if out, _ := w.runComment(copts(11), "Renamed."); out.Status != StatusError || out.ExitCode() != 1 || !strings.HasPrefix(out.Message, "read the review threads: ") {
		t.Errorf("a failed read: outcome = %+v", out)
	}
	if w.writes() != 0 || cut.writes() != 0 {
		t.Errorf("posted %d times", w.writes()+cut.writes())
	}
}

// A thread magnum's review started (by the reviewer's login or a former
// one) is refused: the daemon's poll reads no comment text, so the App's
// reply there would read as the author's and start a reply round.
func TestCommentRefusesAReplyInMagnumsOwnThread(t *testing.T) {
	w, _ := commentWorld(t)
	w.threads = []fakeThread{
		thread(bot(11, "**[P2] wrong total**"), human(12, "Is it?")),
		thread(fakeComment{ID: 21, Author: "old-app", Bot: true, Body: "**[P3] a name**"}),
	}
	for id, by := range map[int64]string{12: "talkable[bot]", 21: "old-app[bot]"} {
		out, posted := w.runComment(copts(id), "It is: see the spec.")
		want := "comment " + strconv.FormatInt(id, 10) + " is in a thread magnum's review started (" + by + "): a comment of magnum's own there answers nothing; " +
			"answer it on GitHub yourself, or let the fixer answer it (magnum fix)"
		if out.Status != StatusInvalid || out.ExitCode() != 2 || len(out.Problems) != 1 || out.Problems[0] != want || len(posted) != 0 {
			t.Errorf("comment %d: outcome = %+v, want %q", id, out, want)
		}
	}
	if w.writes() != 0 {
		t.Errorf("posted %d times", w.writes())
	}
}

// A retry posts the comment once: a thread, or the conversation, that
// already holds the same comment of the App with nobody else's comment
// after it gets nothing more (exit 0), and the caller hears of it again so
// it can record a post the first run could not. The same words under
// another login (the author's own) are no such comment.
func TestCommentPostsOnceWhenTheSameCommentExists(t *testing.T) {
	w, _ := commentWorld(t)
	w.threads = []fakeThread{thread(rev(11, "Rename this?"))}
	text := "Renamed in a1b2c3d."
	first, _ := w.runComment(copts(11), text)
	again, posted := w.runComment(copts(11), text+"\n")
	if first.Status != StatusPosted || again.Status != StatusAlreadyPosted || again.ExitCode() != 0 || again.Comment == nil ||
		!again.Comment.Already || again.Comment.ID != first.Comment.ID || w.writes() != 1 {
		t.Fatalf("first = %+v, again = %+v, %d writes", first, again, w.writes())
	}
	if len(posted) != 1 || !posted[0].Already || posted[0].CommentID != first.Comment.ID {
		t.Errorf("posted = %+v, want the comment found", posted)
	}
	if s := again.Summary(); s != "as fixer-app[bot]: the reply is already in the thread of comment 11: "+first.Comment.URL {
		t.Errorf("summary = %q", s)
	}

	w.issueComments = []fakeComment{human(77, text+"\n\n"+CommentMarker)}
	first, _ = w.runComment(copts(0), text)
	again, _ = w.runComment(copts(0), text)
	if first.Status != StatusPosted || again.Status != StatusAlreadyPosted || again.Comment.ID != first.Comment.ID || len(w.issuePosts) != 1 {
		t.Fatalf("conversation: first = %+v, again = %+v, posted %q", first, again, w.issuePosts)
	}
	if s := again.Summary(); s != "as fixer-app[bot]: the comment is already on the conversation: "+first.Comment.URL {
		t.Errorf("summary = %q", s)
	}
}

// A POST whose answer is lost (GitHub made the comment, then the call
// failed) exits 1; running the same line again finds the comment and
// posts nothing, in a thread and on the conversation alike.
func TestCommentRetryAfterALostAnswerPostsNothing(t *testing.T) {
	w, _ := commentWorld(t)
	w.threads = []fakeThread{thread(rev(11, "Rename this?"))}
	w.replyPosts = []func(c execx.Cmd) (execx.Result, error){func(c execx.Cmd) (execx.Result, error) {
		var req struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(c.Stdin, &req); err != nil {
			t.Fatal(err)
		}
		w.threads[0].Comments = append(w.threads[0].Comments, app(5001, req.Body))
		return apiFail(c, 502, "Bad Gateway")
	}}
	text := "Renamed in a1b2c3d."
	if out, posted := w.runComment(copts(11), text); out.Status != StatusError || out.ExitCode() != 1 || len(posted) != 0 {
		t.Fatalf("lost answer: outcome = %+v, posted %+v", out, posted)
	}
	again, _ := w.runComment(copts(11), text)
	if again.Status != StatusAlreadyPosted || again.Comment.ID != 5001 || w.writes() != 1 {
		t.Fatalf("retry: outcome = %+v after %d writes", again, w.writes())
	}

	w.issueAnswerLost = true
	if out, _ := w.runComment(copts(0), text); out.Status != StatusError || out.ExitCode() != 1 {
		t.Fatalf("lost answer on the conversation: outcome = %+v", out)
	}
	again, _ = w.runComment(copts(0), text)
	if again.Status != StatusAlreadyPosted || again.Comment.ID != 7001 || len(w.issuePosts) != 1 {
		t.Fatalf("retry on the conversation: outcome = %+v, posted %q", again, w.issuePosts)
	}
}

// The same comment after someone else's comment answers that comment and
// goes out; the App's own comments after it keep it a retry.
func TestCommentPostsTheSameAgainAfterSomeoneElsesComment(t *testing.T) {
	text := "It stays: the caller checks it at `app/a.rb:12`."
	signed := text + "\n\n" + CommentMarker
	w, _ := commentWorld(t)
	w.threads = []fakeThread{
		thread(rev(11, "Check this?"), app(12, signed), rev(13, "The jobs skip it.")),
		thread(rev(21, "A name?"), app(22, signed), app(23, "Renamed.\n\n"+CommentMarker)),
	}
	if out, _ := w.runComment(copts(11), text); out.Status != StatusPosted || out.Comment.ID != 5001 {
		t.Errorf("after someone else's comment: outcome = %+v, want it posted", out)
	}
	if out, _ := w.runComment(copts(21), text); out.Status != StatusAlreadyPosted || out.Comment.ID != 22 {
		t.Errorf("after the App's own comment: outcome = %+v, want 22 found", out)
	}
	w.issueComments = []fakeComment{app(77, signed), rev(78, "That check is gone in jobs.")}
	if out, _ := w.runComment(copts(0), text); out.Status != StatusPosted {
		t.Errorf("on the conversation after someone else's comment: outcome = %+v", out)
	}
	if w.writes() != 2 {
		t.Errorf("%d writes, want 2", w.writes())
	}
}

// A comment is needed for the conversation and for a reply; with only a
// title or review requests it may be left out, and a comment given with
// them goes on the conversation first.
func TestCommentNeedsABodyUnlessOnlyTitleOrReviewRequests(t *testing.T) {
	title := "Fix the order total"
	for name, o := range map[string]CommentOptions{
		"the conversation": copts(0),
		"a reply":          func() CommentOptions { o := copts(11); o.Title = &title; return o }(),
	} {
		w, _ := commentWorld(t)
		w.threads = []fakeThread{thread(rev(11, "Rename this?"))}
		out, _ := w.runComment(o, "")
		if out.Status != StatusInvalid || out.ExitCode() != 2 || len(out.Problems) != 1 ||
			out.Problems[0] != "the comment is empty: pass it on standard input (a pipe or a heredoc)" || w.calls("gh") != 0 {
			t.Errorf("%s with no comment: outcome = %+v after %d gh calls", name, out, w.calls("gh"))
		}
	}
	w, p := commentWorld(t)
	o := copts(0)
	o.Title = &title
	if out, _ := w.runComment(o, " \n"); out.Status != StatusPosted || out.Comment != nil || out.Title == nil || !out.Title.Changed || p.title != title {
		t.Errorf("only a title: outcome = %+v", out)
	}
	o = copts(0)
	o.RequestReviews = []string{"rev-ann"}
	if out, _ := w.runComment(o, ""); out.Status != StatusPosted || out.Comment != nil || out.ReviewRequests == nil ||
		!slices.Equal(out.ReviewRequests.Requested, []string{"rev-ann"}) {
		t.Errorf("only a review request: outcome = %+v", out)
	}
	o.RequestReviews = []string{"bob"}
	if out, _ := w.runComment(o, "Ready for another look."); out.Status != StatusPosted || out.Comment == nil || out.Comment.Kind != PostConversation {
		t.Errorf("a review request with a comment: outcome = %+v", out)
	}
	if len(w.issuePosts) != 1 || w.issuePosts[0] != "Ready for another look.\n\n"+CommentMarker || w.writes() != 4 {
		t.Errorf("conversation posts = %q, %d writes", w.issuePosts, w.writes())
	}
}

// The parts go out in one order: the comment, then the title, then the
// review requests, each JSON on stdin. A title that is already so sends no
// PATCH, and a login already asked sends no request; someone who reviewed
// already is no longer asked on GitHub and is asked again.
func TestCommentChangesTheTitleAndRequestsReviewsInOrder(t *testing.T) {
	w, p := commentWorld(t)
	p.requested = []string{"bob"}
	title := "Fix the order total"
	o := copts(0)
	o.Title, o.RequestReviews = &title, []string{"rev-ann", "bob", "Rev-Ann"}
	out, posted := w.runComment(o, "Renamed as asked.")
	if out.Status != StatusPosted || out.Comment == nil || out.Comment.ID != 7001 || out.Title == nil || !out.Title.Changed ||
		out.ReviewRequests == nil || !slices.Equal(out.ReviewRequests.Requested, []string{"rev-ann"}) || !slices.Equal(out.ReviewRequests.Already, []string{"bob"}) {
		t.Fatalf("outcome = %+v (title %+v, requests %+v)", out, out.Title, out.ReviewRequests)
	}
	var order []string
	for _, c := range w.fake.Calls {
		if len(c.Args) > 3 && c.Args[1] == "-X" {
			order = append(order, c.Args[2]+" "+c.Args[3])
			if !c.Mutates || !slices.Contains(c.Args, "--input") {
				t.Errorf("write %q is not a Mutates call with its body on stdin", c.Args)
			}
		}
	}
	if want := []string{"POST repos/talkable/talkable/issues/5/comments", "PATCH repos/talkable/talkable/pulls/5",
		"POST repos/talkable/talkable/pulls/5/requested_reviewers"}; !slices.Equal(order, want) {
		t.Errorf("writes = %q, want %q", order, want)
	}
	if !slices.Equal(p.titles, []string{title}) || len(p.requests) != 1 || !slices.Equal(p.requests[0], []string{"rev-ann"}) {
		t.Errorf("titles %q, requests %q", p.titles, p.requests)
	}
	if kinds := func() (k []string) {
		for _, x := range posted {
			k = append(k, x.Kind)
		}
		return k
	}(); !slices.Equal(kinds, []string{PostConversation, PostTitle, PostReviewRequest}) || !slices.Equal(posted[2].Reviewers, []string{"rev-ann"}) ||
		posted[1].URL != "https://github.com/talkable/talkable/pull/5" {
		t.Errorf("posted = %+v", posted)
	}
	if s := out.Summary(); s != "as fixer-app[bot]: posted a comment on the conversation: https://github.com/talkable/talkable/pull/5#issuecomment-7001; "+
		"changed the title; requested a review from rev-ann; a review is already requested from bob" {
		t.Errorf("summary = %q", s)
	}

	o.RequestReviews = []string{"bob", "rev-ann"}
	again, posted := w.runComment(o, "")
	if again.Status != StatusAlreadyPosted || again.Title == nil || !again.Title.Already || again.Title.Changed || len(again.ReviewRequests.Requested) != 0 ||
		len(p.titles) != 1 || len(p.requests) != 1 || len(posted) != 0 {
		t.Errorf("again: outcome = %+v, titles %q, requests %q, posted %+v", again, p.titles, p.requests, posted)
	}
	p.requested = nil // rev-ann reviewed: GitHub asks no one now
	o.RequestReviews, o.Title = []string{"rev-ann"}, nil
	if out, _ := w.runComment(o, ""); out.Status != StatusPosted || len(p.requests) != 2 {
		t.Errorf("a re-request: outcome = %+v, requests %q", out, p.requests)
	}
}

// A title or a login GitHub would refuse, or that names more than a
// title, is refused before anything goes out: empty, two lines, longer
// than GitHub's 256 characters, a magnum marker, a local path; an App's
// login, a team, an "@" and the PR's author, whom GitHub never asks.
func TestCommentRefusesABadTitleOrLogin(t *testing.T) {
	for name, tc := range map[string]struct {
		title *string
		login string
		want  string
	}{
		"an empty title":     {title: new(" "), want: "the title is empty"},
		"two lines":          {title: new("Fix\nthe total"), want: "the title is more than one line"},
		"a long title":       {title: new(strings.Repeat("x", 257)), want: "the title is 257 characters; GitHub takes at most 256"},
		"a marker":           {title: new("Fix " + CommentMarker), want: "the title carries a <!-- magnum:… marker"},
		"a local path":       {title: new("Fix /Users/alice/x.rb"), want: `the title names the local path "/Users/alice/x.rb"`},
		"an App":             {login: "other-app[bot]", want: `--request-review "other-app[bot]" is no person's login`},
		"a team":             {login: "talkable/reviewers", want: `--request-review "talkable/reviewers" is no person's login`},
		"an at":              {login: "@rev-ann", want: `--request-review "@rev-ann" is no person's login`},
		"the PR's author":    {login: "Alice", want: "alice opened talkable/talkable#5: GitHub asks no pull request's author for a review"},
		"a comment and both": {title: new(""), login: "alice", want: "the title is empty"},
	} {
		t.Run(name, func(t *testing.T) {
			w, _ := commentWorld(t)
			o := copts(0)
			o.Title = tc.title
			if tc.login != "" {
				o.RequestReviews = []string{tc.login}
			}
			out, posted := w.runComment(o, "Renamed.")
			if out.Status != StatusInvalid || out.ExitCode() != 2 || !slices.ContainsFunc(out.Problems, func(p string) bool { return strings.HasPrefix(p, tc.want) }) {
				t.Errorf("outcome = %+v, want a problem %q", out, tc.want)
			}
			if w.writes() != 0 || len(posted) != 0 {
				t.Errorf("%d writes for a refused line", w.writes())
			}
		})
	}
}

// A dry run reads GitHub as the App and checks every part, says what it
// would post and as whom, and posts, changes and asks nothing; the caller
// records nothing.
func TestCommentDryRunPostsNothingAndSaysAsWhom(t *testing.T) {
	w, p := commentWorld(t)
	w.threads = []fakeThread{thread(rev(11, "Rename this?"))}
	title := "Fix the order total"
	o := copts(11)
	o.DryRun, o.Title, o.RequestReviews = true, &title, []string{"rev-ann"}
	out, posted := w.runComment(o, "Renamed.")
	if out.Status != StatusDryRun || out.ExitCode() != 0 || out.Comment == nil || !out.Comment.Planned || out.Comment.Thread != 11 ||
		out.Title == nil || !out.Title.Planned || out.ReviewRequests == nil || !out.ReviewRequests.Planned ||
		!slices.Equal(out.ReviewRequests.Requested, []string{"rev-ann"}) {
		t.Fatalf("outcome = %+v (comment %+v, title %+v, requests %+v)", out, out.Comment, out.Title, out.ReviewRequests)
	}
	if s := out.Summary(); s != "dry run as fixer-app[bot] (identity fixer-app): nothing posted; would post a reply in the thread of comment 11; "+
		"would change the title; would request a review from rev-ann" {
		t.Errorf("summary = %q", s)
	}
	if w.writes() != 0 || len(posted) != 0 || len(p.titles) != 0 {
		t.Errorf("a dry run wrote: %d writes, posted %+v", w.writes(), posted)
	}
	// A dry run refuses what the real run refuses.
	o.ReplyTo = 4100000001
	if out, _ := w.runComment(o, "Renamed."); out.Status != StatusInvalid {
		t.Errorf("a dry run of a wrong id: outcome = %+v", out)
	}
}

// A failure after a part went out says which parts did (exit 1: run the
// same line again); the retry finds the comment and goes on with the
// title.
func TestCommentAFailureMidWayReportsWhatWentOut(t *testing.T) {
	w, p := commentWorld(t)
	p.patchFail = func(c execx.Cmd) (execx.Result, error) { return apiFail(c, 502, "Bad Gateway") }
	title := "Fix the order total"
	o := copts(0)
	o.Title, o.RequestReviews = &title, []string{"rev-ann"}
	out, posted := w.runComment(o, "Renamed.")
	if out.Status != StatusError || out.ExitCode() != 1 || out.Comment == nil || out.Comment.ID != 7001 || out.Comment.Already ||
		!strings.HasPrefix(out.Message, "change the title: ") || len(p.requests) != 0 || len(posted) != 1 {
		t.Fatalf("outcome = %+v (comment %+v), requests %q, posted %+v", out, out.Comment, p.requests, posted)
	}
	if s := out.Summary(); !strings.Contains(s, "; done before it, as fixer-app[bot]: posted a comment on the conversation: https://github.com/talkable/talkable/pull/5#issuecomment-7001") {
		t.Errorf("summary = %q", s)
	}
	p.patchFail = nil
	again, _ := w.runComment(o, "Renamed.")
	if again.Status != StatusPosted || !again.Comment.Already || !again.Title.Changed || len(w.issuePosts) != 1 || len(p.requests) != 1 {
		t.Errorf("retry: outcome = %+v, posts %q, requests %q", again, w.issuePosts, p.requests)
	}
}

// GitHub must show the App as the comment's author: one it shows by anyone
// else (a login the gh config of the App did not hold) is an error that
// says to delete it, and is not reported as the App's post.
func TestCommentReadsBackTheApp(t *testing.T) {
	w, _ := commentWorld(t)
	w.answerAs = "zhuravel"
	out, posted := w.runComment(copts(0), "Renamed.")
	if out.Status != StatusError || out.ExitCode() != 1 || len(posted) != 0 ||
		out.Message != "GitHub shows comment 7001 by zhuravel, not the App fixer-app[bot]: delete it on GitHub; do not run it again" {
		t.Errorf("outcome = %+v, posted %+v", out, posted)
	}
}

// GitHub refusing a review request or the title (HTTP 422: not a
// collaborator, a title it does not take) means the line must change:
// invalid, exit 2, with GitHub's words.
func TestCommentGitHubRefusingAReviewerIsInvalid(t *testing.T) {
	w, p := commentWorld(t)
	p.requestFail = func(c execx.Cmd) (execx.Result, error) {
		return apiFail(c, 422, "Reviews may only be requested from collaborators. One or more of the users or teams you specified is not a collaborator.")
	}
	o := copts(0)
	o.RequestReviews = []string{"rev-ann"}
	out, _ := w.runComment(o, "")
	if out.Status != StatusInvalid || out.ExitCode() != 2 || len(out.Problems) != 1 ||
		!strings.HasPrefix(out.Problems[0], "GitHub refused to request a review from rev-ann: ") || !strings.Contains(out.Problems[0], "only be requested from collaborators") {
		t.Errorf("outcome = %+v", out)
	}
	p.requestFail, p.patchFail = nil, func(c execx.Cmd) (execx.Result, error) { return apiFail(c, 422, "Validation Failed") }
	title := "Fix the order total"
	o = copts(0)
	o.Title = &title
	if out, _ := w.runComment(o, ""); out.Status != StatusInvalid || out.ExitCode() != 2 || len(out.Problems) != 1 ||
		!strings.HasPrefix(out.Problems[0], "GitHub refused the title: ") {
		t.Errorf("a refused title: outcome = %+v", out)
	}
}

// The line's repository and number are refused as invalid (the caller
// typed them); a missing login is the caller's error.
func TestCommentChecksTheOptions(t *testing.T) {
	w, _ := commentWorld(t)
	for _, tc := range []struct {
		edit   func(*CommentOptions)
		status string
	}{
		{func(o *CommentOptions) { o.Repo = ".." }, StatusInvalid},
		{func(o *CommentOptions) { o.Number = 0 }, StatusInvalid},
		{func(o *CommentOptions) { o.ReplyTo = -1 }, StatusInvalid},
		{func(o *CommentOptions) { o.Login = "" }, StatusError},
	} {
		o := copts(0)
		tc.edit(&o)
		if out, _ := w.runComment(o, "Renamed."); out.Status != tc.status {
			t.Errorf("%+v: outcome = %+v, want %s", o, out, tc.status)
		}
	}
	if w.calls("gh") != 0 {
		t.Errorf("gh ran %d times for bad options", w.calls("gh"))
	}
}
