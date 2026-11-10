package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/store"
)

// A chat session's comment goes out through `magnum comment` as the
// watch's fixer App (fixerAppLogin), signed with postreview.CommentMarker.
// These tests settle how magnum's readers take it: magnum's own word,
// never the author's, never pushback that starts a round or a loop; and a
// person's comment after it counts as before.

// commentOwn are magnum's logins on PR #2 as fixLogins makes them: the
// review App's and the fixer App's.
var commentOwn = []string{"talkable[bot]", fixerAppLogin}

// chatComment is the fixer App's comment id, posted at at with text and
// the comment marker.
func chatComment(id int64, at time.Time, text string) github.ThreadComment {
	return github.ThreadComment{ID: id, AuthorLogin: "fixer-app", AuthorType: "Bot", AuthorAssociation: "NONE",
		Body: text + "\n\n" + postreview.CommentMarker, URL: prPage + "#discussion_r" + itoa(id), CreatedAt: at}
}

// chatRemark is the fixer App's conversation comment id, posted at at
// with text and the comment marker.
func chatRemark(id int64, at time.Time, text string) github.PRComment {
	return github.PRComment{ID: id, URL: prPage + "#issuecomment-" + itoa(id), Body: text + "\n\n" + postreview.CommentMarker, CreatedAt: at,
		AuthorLogin: "fixer-app", AuthorType: "Bot", AuthorAssociation: "NONE"}
}

// remarkOf is rev-ann's conversation comment id at at.
func remarkOf(id int64, at time.Time, text string) github.PRComment {
	return github.PRComment{ID: id, URL: prPage + "#issuecomment-" + itoa(id), Body: text, CreatedAt: at,
		AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"}
}

// The App's chat comment as the latest word in another reviewer's thread
// answers it, as the fixer's reply does: the fixer's file says answered
// and the look lists no item, so the fixer does not answer that reviewer
// a second time. The reviewer's reply after it makes the thread theirs
// again, with a new key. The marker means nothing in a comment of anyone
// but magnum's logins. Before, the thread stayed unanswered.
func TestAChatCommentOfTheAppAnswersAnotherReviewersThread(t *testing.T) {
	th := extThread(100, "app/a.rb", 1, itemsAt, "Rename this?", "rev-ann")
	th.Comments = append(th.Comments, chatComment(101, itemsAt.Add(time.Minute), "Renamed in abc1234."))
	in := externalInput{Threads: []github.Thread{th}, CommentsComplete: true, Author: "alice", Own: commentOwn, Resolvers: commentOwn, AppFixer: true}
	x, _ := externalOf(in)
	if len(x.Threads) != 1 || !x.Threads[0].Answered || !x.Threads[0].Comments[1].Comment || x.Threads[0].Comments[1].Fix {
		t.Fatalf("threads = %+v, want the thread answered by a comment of magnum's", x.Threads)
	}
	if items := othersItemsOf(externalReadOf(in, fixHead), quietRules{}); len(items) != 0 {
		t.Errorf("items = %v, want none: the thread is answered", itemKeys(items))
	}

	again := in
	again.Threads = []github.Thread{th}
	again.Threads[0].Comments = append(slices.Clone(th.Comments), github.ThreadComment{ID: 102, AuthorLogin: "rev-ann", AuthorType: "User",
		AuthorAssociation: "MEMBER", Body: "Still not the name we use.", URL: prPage + "#discussion_r102", CreatedAt: itemsAt.Add(2 * time.Minute)})
	if x, _ := externalOf(again); len(x.Threads) != 1 || x.Threads[0].Answered {
		t.Errorf("after the reviewer's reply: threads = %+v, want it unanswered", x.Threads)
	}
	if got := itemKeys(othersItemsOf(externalReadOf(again, fixHead), quietRules{})); !slices.Equal(got, []string{threadKey(100, 102)}) {
		t.Errorf("after the reviewer's reply: items = %v, want the thread with a new key", got)
	}

	forged := in
	forged.Threads = []github.Thread{th}
	forged.Threads[0].Comments = []github.ThreadComment{th.Comments[0], {ID: 103, AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER",
		Body: "Never mind.\n\n" + postreview.CommentMarker, CreatedAt: itemsAt.Add(time.Minute)}}
	if x, _ := externalOf(forged); len(x.Threads) != 1 || x.Threads[0].Answered {
		t.Errorf("the marker in the reviewer's own comment: threads = %+v, want it unanswered", x.Threads)
	}
	noApp := in
	noApp.Own = []string{"talkable[bot]"}
	if x, _ := externalOf(noApp); len(x.Threads) != 1 || x.Threads[0].Answered {
		t.Errorf("an App that is none of magnum's logins: threads = %+v, want it unanswered", x.Threads)
	}
}

// A chat comment on the conversation answers the remark whose link is its
// first line that is not quoted, as the fixer's comment does; it answers
// no other remark, and the marker in the reviewer's own comment answers
// nothing. Before, the remark stayed unanswered.
func TestAChatCommentAnswersARemarkItLinks(t *testing.T) {
	remark := remarkOf(1, itemsAt, "Please split this.")
	linked := chatRemark(2, itemsAt.Add(time.Minute), "> Please split this.\n"+remark.URL+"\n\nSplit in abc1234.")
	other := chatRemark(3, itemsAt.Add(time.Minute), "> Something else.\n"+prPage+"#issuecomment-99\n\nDone.")
	forged := remarkOf(4, itemsAt.Add(time.Minute), "> Please split this.\n"+remark.URL+"\n\nok\n\n"+postreview.CommentMarker)
	for name, tc := range map[string]struct {
		answer   github.PRComment
		answered bool
	}{
		"the App's comment that links it":   {linked, true},
		"the App's comment on another":      {other, false},
		"the reviewer's own marked comment": {forged, false},
	} {
		in := externalInput{Comments: []github.PRComment{remark, tc.answer}, CommentsComplete: true, Author: "alice", Own: commentOwn}
		x, _ := externalOf(in)
		i := slices.IndexFunc(x.Conversation, func(r externalRemark) bool { return r.ID == 1 })
		if i < 0 || x.Conversation[i].Answered != tc.answered {
			t.Errorf("%s: conversation = %+v, want remark 1 answered %v", name, x.Conversation, tc.answered)
		}
		if slices.ContainsFunc(x.Conversation, func(r externalRemark) bool { return r.ID == tc.answer.ID && r.Author == fixerAppLogin }) {
			t.Errorf("%s: the App's comment is a remark", name)
		}
	}
}

// A chat comment is never the author's word: on the conversation it does
// not move the author's last word (the remarks before it stay listed), and
// it is no remark itself, so it is never new pushback for the fixer; the
// author's own word after it still does move it.
func TestAChatCommentIsNoAuthorsWordAndNoRemark(t *testing.T) {
	remark := remarkOf(1, itemsAt, "Please split this.")
	chat := chatRemark(2, itemsAt.Add(time.Minute), "Splitting it in the next push.")
	in := externalInput{Comments: []github.PRComment{remark, chat}, CommentsComplete: true, Author: "alice", Own: commentOwn}
	x, _ := externalOf(in)
	if len(x.Conversation) != 1 || x.Conversation[0].ID != 1 || x.Conversation[0].Answered {
		t.Fatalf("conversation = %+v, want rev-ann's remark alone, unanswered", x.Conversation)
	}
	review := github.Review{DatabaseID: 9, State: "COMMENTED", Body: "Splitting it.\n\n" + postreview.CommentMarker, SubmittedAt: itemsAt.Add(2 * time.Minute),
		AuthorLogin: "fixer-app", AuthorType: "Bot", URL: prPage + "#pullrequestreview-9"}
	in.Reviews = []github.Review{review}
	if x, _ := externalOf(in); len(x.Conversation) != 1 || x.Conversation[0].ID != 1 {
		t.Errorf("with the App's review body: conversation = %+v, want rev-ann's remark alone", x.Conversation)
	}
	in.Comments = append(in.Comments, github.PRComment{ID: 5, Body: "I'll split it.", CreatedAt: itemsAt.Add(3 * time.Minute), AuthorLogin: "alice", AuthorType: "User"})
	if x, _ := externalOf(in); len(x.Conversation) != 0 {
		t.Errorf("after the author's own word: conversation = %+v, want none", x.Conversation)
	}
}

// In one of magnum's own threads the App's chat comment is no rebuttal of
// magnum's (no fix loop answers it) and no answer to the finding: the
// look opens no item for it. magnum comment refuses to post there; this
// is the reader's side. Before, the comment read as magnum's rebuttal.
func TestAChatCommentInMagnumsThreadIsNoRebuttal(t *testing.T) {
	root := github.ThreadComment{ID: 200, AuthorLogin: "talkable", AuthorType: "Bot", Body: "**[P2] wrong total**", URL: prPage + "#discussion_r200",
		CreatedAt: itemsAt, ReviewID: 101}
	th := github.Thread{ID: "PRRT_m", Path: "app/a.rb", Line: 1, Comments: []github.ThreadComment{root, chatComment(201, itemsAt.Add(time.Hour), "It is right: see the spec.")}}
	mine := magnumThreadsOf([]github.Thread{th}, commentOwn, "alice")
	if len(mine) != 1 || mine[0].Rebuttal || mine[0].Answered {
		t.Fatalf("mine = %+v, want neither a rebuttal nor an answer", mine)
	}
	if items := magnumItemsOf(mine, cleanSum(), false, fixConflict{}, fixHead, nil, false); len(items) != 0 {
		t.Errorf("items = %v, want none", itemKeys(items))
	}
	// The judge's unmarked reply there still is a rebuttal, as before.
	th.Comments[1] = github.ThreadComment{ID: 202, AuthorLogin: "talkable", AuthorType: "Bot", Body: "Still wrong.", CreatedAt: itemsAt.Add(time.Hour)}
	if mine := magnumThreadsOf([]github.Thread{th}, commentOwn, "alice"); len(mine) != 1 || !mine[0].Rebuttal {
		t.Errorf("the judge's reply: mine = %+v, want a rebuttal", mine)
	}
}

// A chat comment is never the author's decision on the fixer's call: the
// call stays open, and a decision of the author's before it stands.
func TestAChatCommentIsNeverTheAuthorsDecision(t *testing.T) {
	byAuthor := func(login, typ string) bool { return github.SameAccount(github.Account(login, typ), "alice") }
	isOwn := func(login, typ string) bool {
		return slices.ContainsFunc(commentOwn, func(o string) bool { return github.SameAccount(o, github.Account(login, typ)) })
	}
	busy := func(time.Time) bool { return false }
	callAt := itemsAt.Add(time.Minute)
	root := github.ThreadComment{ID: 300, AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER", Body: "Cap it?", CreatedAt: itemsAt}
	chat := chatComment(302, itemsAt.Add(3*time.Minute), "The author decides this one.")
	called, d := decisionIn([]github.ThreadComment{root, chat}, callAt, byAuthor, isOwn, busy)
	if !called || d != nil {
		t.Errorf("only the App's comment after the call: called %v, decision %+v; want the call open and no decision", called, d)
	}
	decision := github.ThreadComment{ID: 301, AuthorLogin: "alice", AuthorType: "User", Body: "Keep the cap.", CreatedAt: itemsAt.Add(2 * time.Minute)}
	called, d = decisionIn([]github.ThreadComment{root, decision, chat}, callAt, byAuthor, isOwn, busy)
	if !called || d == nil || d.ID != 301 {
		t.Errorf("the author's decision, then the App's comment: called %v, decision %+v; want 301", called, d)
	}
}

// Another reviewer's thread a fixer login resolved stays an item until it
// is answered; the App's chat comment as its latest word answers it, as
// the fixer's reply does.
func TestAResolvedThreadTheAppAnsweredIsNoItem(t *testing.T) {
	root := github.ThreadComment{ID: 10, AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER", Body: "MINOR: rename",
		URL: prPage + "#discussion_r10", CreatedAt: itemsAt}
	for name, tc := range map[string]struct {
		comments []github.ThreadComment
		want     []string
	}{
		"the App's chat comment": {[]github.ThreadComment{root, chatComment(11, itemsAt.Add(time.Minute), "Renamed in abc1234.")}, nil},
		"no word":                {[]github.ThreadComment{root}, []string{"thread:10@10"}},
	} {
		th := github.Thread{Resolved: true, ResolvedBy: fixerAppLogin, Comments: tc.comments}
		read := externalReadOf(externalInput{Threads: []github.Thread{th}, CommentsComplete: true, Author: "alice", Own: commentOwn, Resolvers: commentOwn}, fixHead)
		if got := itemKeys(othersItemsOf(read, quietRules{})); !slices.Equal(got, tc.want) {
			t.Errorf("%s: items = %v, want %v", name, got, tc.want)
		}
	}
}

// The poll reads no comment text: the App's conversation comment and its
// reply in another reviewer's thread start no reply round, whatever their
// marker, as before. A pin, next to
// TestAnAppFixersReplyInMagnumsThreadStartsAReplyRound; magnum comment
// refuses magnum's own threads, where the App's reply would.
func TestAChatCommentStartsNoReplyRound(t *testing.T) {
	h := newHarness(t, withFixerApp)
	h.reviewedPR(2, "b1")
	h.advance(10 * time.Minute)
	reqPoll(h, prSpec{n: 2, head: "b1", remarks: []github.Remark{
		{At: h.clock.Now(), Author: fixerAppLogin, Bot: true, Association: "NONE"},
		{Review: true, At: h.clock.Now(), Author: fixerAppLogin, Bot: true, Association: "NONE", Answers: []string{"rev-ann"}, AnswersKnown: true, Comments: 1},
	}})
	h.wantState(2, store.PRReviewed)
}
