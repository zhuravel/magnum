package postreview

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// rep renders one entry of a replies file.
func rep(commentID int64, kind, body string) string {
	b, err := json.Marshal(Reply{CommentID: commentID, Kind: kind, Body: body})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// repliesFile renders a replies file.
func repliesFile(replies ...string) string {
	return `{"replies":[` + strings.Join(replies, ",") + `]}`
}

// runReplies runs the tool on file as the judge's identity.
func (w *world) runReplies(o Options, file string) Outcome {
	return RunReplies(context.Background(), w.deps(), o, []byte(file))
}

// marked is a reply's text as it stands on GitHub: the text, a blank line
// and the marker of run.
func marked(text, run, kind string) string { return text + "\n\n" + ReplyMarker(run, kind) }

// withID is c with another id.
func withID(c fakeComment, id int64) fakeComment {
	c.ID = id
	return c
}

// The marker is one hidden line a body can end with: the run id and the
// kind read back whatever text comes before it, another run's marker reads
// as that run's, and a body without one (or with only the review's run
// marker) has none.
func TestReplyMarkerRoundTrip(t *testing.T) {
	m := ReplyMarker(runID, ReplyRebuttal)
	if m != "<!-- magnum:reply run=r-20261006T120000-7 kind=rebuttal -->" {
		t.Fatalf("marker = %q", m)
	}
	if run, kind, ok := ParseReplyMarker("Still wrong: see `x.rb:12`.\n\n" + m); !ok || run != runID || kind != ReplyRebuttal {
		t.Errorf("parsed = %q %q %v", run, kind, ok)
	}
	if run, kind, ok := ParseReplyMarker(marked("Thanks.", "r-20261001T090000-1", ReplyAck)); !ok || run != "r-20261001T090000-1" || kind != ReplyAck {
		t.Errorf("another run's marker = %q %q %v", run, kind, ok)
	}
	for _, body := range []string{"", "No marker here.", RunMarker(runID, head), "<!-- magnum:reply run= kind=ack -->", "<!-- magnum:reply run=r-1 -->"} {
		if run, kind, ok := ParseReplyMarker(body); ok {
			t.Errorf("ParseReplyMarker(%q) = %q %q, want none", body, run, kind)
		}
	}
}

// A replies file with a fault stops before GitHub is read: every problem
// names its reply, nothing is posted, and the judge is told to fix its file
// (exit 2).
func TestRunRepliesRejectsAFaultyFileBeforeReadingGitHub(t *testing.T) {
	long := strings.Repeat("x", MaxBody)
	cases := []struct {
		name, file, problem string
	}{
		{"unknown field", `{"replies":[{"comment_id":1,"kind":"ack","body":"ok","resolve":true}]}`, "unknown field"},
		{"unknown top-level field", `{"replies":[],"event":"COMMENT"}`, "unknown field"},
		{"not an object", `[]`, "not a replies object"},
		{"two values", `{"replies":[]} {"replies":[]}`, "more than one JSON value"},
		{"no replies key", `{}`, "replies is missing"},
		{"null replies", `{"replies":null}`, "replies is missing"},
		{"unknown kind", repliesFile(rep(1, "agree", "ok")), `replies[0]: kind "agree"`},
		{"no kind", repliesFile(rep(1, "", "ok")), `replies[0]: kind ""`},
		{"duplicate comment id", repliesFile(rep(1, "ack", "ok"), rep(2, "ack", "ok"), rep(1, "answer", "again")), "replies[2]: comment_id 1 repeats replies[0]"},
		{"comment id 0", repliesFile(rep(0, "ack", "ok")), "replies[0]: comment_id 0"},
		{"negative comment id", repliesFile(rep(-4, "ack", "ok")), "replies[0]: comment_id -4"},
		{"blank body", repliesFile(rep(1, "ack", " \n\t")), "replies[0]: body is empty"},
		{"body with a run marker", repliesFile(rep(1, "ack", "ok "+RunMarker(runID, head))), "replies[0]: body carries a <!-- magnum:"},
		{"body with a reply marker", repliesFile(rep(1, "ack", "ok\n"+ReplyMarker(runID, "ack"))), "replies[0]: body carries a <!-- magnum:"},
		{"body with a footer marker", repliesFile(rep(1, "ack", FooterMarker)), "replies[0]: body carries a <!-- magnum:"},
		{"body too long with the marker", repliesFile(rep(1, "ack", long)), "replies[0]: body is"},
		{"body with a local path", repliesFile(rep(1, "rebuttal", "It still fails: /private/tmp/magnum7/run.log")),
			`replies[0]: body names the local path "/private/tmp/magnum7/run.log"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			out := w.runReplies(opts(), tc.file)
			if out.Status != StatusInvalid || out.ExitCode() != 2 {
				t.Fatalf("outcome = %+v", out)
			}
			if !slices.ContainsFunc(out.Problems, func(p string) bool { return strings.Contains(p, tc.problem) }) {
				t.Errorf("problems = %q, want one with %q", out.Problems, tc.problem)
			}
			if w.calls("gh") != 0 || len(w.replies) != 0 {
				t.Errorf("gh ran for a faulty file: %d calls", w.calls("gh"))
			}
			if !strings.HasPrefix(out.Summary(), "the file has ") {
				t.Errorf("summary = %q", out.Summary())
			}
		})
	}
	// A body that fills the limit exactly, marker included, is fine.
	w := newWorld(t)
	w.threads = []fakeThread{thread(bot(1, "**[P2] x**"))}
	room := MaxBody - len("\n\n"+ReplyMarker(runID, ReplyAck))
	if out := w.runReplies(opts(), repliesFile(rep(1, "ack", strings.Repeat("x", room)))); out.Status != StatusReplied {
		t.Errorf("a body filling the limit: %+v", out)
	}
}

// A reply goes out as the reviewer on the thread's first comment: its text,
// a blank line and the run's reply marker (trailing blank lines of the
// judge's text dropped), as JSON on gh's stdin with no reply text in argv.
func TestRunRepliesPostsTheTextThenTheMarker(t *testing.T) {
	w := newWorld(t)
	w.threads = []fakeThread{
		thread(bot(11, "**[P2] wrong total**"), human(12, "It is intended: see the spec.")),
		thread(bot(21, "**[P3] naming**"), human(22, "Why?")),
	}
	out := w.runReplies(opts(), repliesFile(
		rep(21, ReplyAnswer, "Because `x` is a count.\n\n  \n"),
		rep(11, ReplyRebuttal, "The spec only covers tax; `$(rm -rf)` stays text."),
	))
	if out.Status != StatusReplied || out.ExitCode() != 0 || out.Message != "" {
		t.Fatalf("outcome = %+v", out)
	}
	want := []PostedReply{
		{CommentID: 21, ID: 5001, URL: "https://github.com/talkable/talkable/pull/5#discussion_r5001", Kind: ReplyAnswer},
		{CommentID: 11, ID: 5002, URL: "https://github.com/talkable/talkable/pull/5#discussion_r5002", Kind: ReplyRebuttal},
	}
	if !slices.Equal(out.Replies, want) {
		t.Errorf("replies = %+v", out.Replies)
	}
	wantSent := []sentReply{
		{21, "Because `x` is a count.\n\n" + ReplyMarker(runID, ReplyAnswer)},
		{11, "The spec only covers tax; `$(rm -rf)` stays text.\n\n" + ReplyMarker(runID, ReplyRebuttal)},
	}
	if !slices.Equal(w.replies, wantSent) {
		t.Errorf("sent = %+v", w.replies)
	}
	posts := w.fake.CallsWithPrefix("gh", "api", "-X", "POST")
	if len(posts) != 2 {
		t.Fatalf("%d POSTs", len(posts))
	}
	for _, p := range posts {
		if !p.Mutates || p.Env["GH_CONFIG_DIR"] != "/state/gh/talkable-app" || !slices.Contains(p.Args, "--input") {
			t.Errorf("POST = %+v", p)
		}
		for _, a := range p.Args {
			if strings.Contains(a, "count") || strings.Contains(a, "rm -rf") || strings.Contains(a, "magnum:reply") {
				t.Errorf("reply text in argv: %q", p.Args)
			}
		}
	}
	if got, wantSummary := out.Summary(), "posted 2 replies (1 rebuttal, 1 answer)"; got != wantSummary {
		t.Errorf("summary = %q, want %q", got, wantSummary)
	}
	if w.calls("git") != 0 {
		t.Error("git ran for replies")
	}
}

// An empty list is a valid file: nothing to read, nothing to post.
func TestRunRepliesAnEmptyListPostsNothing(t *testing.T) {
	w := newWorld(t)
	out := w.runReplies(opts(), repliesFile())
	if out.Status != StatusReplied || out.ExitCode() != 0 || len(out.Replies) != 0 || out.Summary() != "no replies to post" {
		t.Fatalf("outcome = %+v (%s)", out, out.Summary())
	}
	if w.calls("gh") != 0 {
		t.Errorf("gh ran for an empty list: %d calls", w.calls("gh"))
	}
}

// A reply answers the first comment of a thread of the reviewer: a comment
// nothing starts, a reply inside a thread, a human's thread and a User with
// the reviewer's name (an App is "talkable[bot]", another account than the
// user "talkable") are all refused, each named, and nothing is posted.
func TestRunRepliesRefusesACommentThatDoesNotStartAnOwnThread(t *testing.T) {
	w := newWorld(t)
	w.threads = []fakeThread{
		thread(bot(11, "**[P2] mine**"), human(12, "reply")),
		thread(human(31, "A human's own thread")),
		thread(fakeComment{ID: 41, Author: "talkable", Body: "the user talkable, not the App"}),
		thread(bot(51, "**[P2] fine**")),
	}
	out := w.runReplies(opts(), repliesFile(
		rep(51, ReplyAck, "ok"), // fine: the others are not
		rep(999, ReplyAck, "no such comment"),
		rep(12, ReplyAck, "a reply, not a thread's first comment"),
		rep(31, ReplyAck, "a human's thread"),
		rep(41, ReplyAck, "another account"),
	))
	if out.Status != StatusInvalid || out.ExitCode() != 2 {
		t.Fatalf("outcome = %+v", out)
	}
	want := []string{
		"replies[1]: comment 999 does not start a thread of talkable[bot]",
		"replies[2]: comment 12 does not start a thread of talkable[bot]",
		"replies[3]: comment 31 does not start a thread of talkable[bot]",
		"replies[4]: comment 41 does not start a thread of talkable[bot]",
	}
	if !slices.Equal(out.Problems, want) {
		t.Errorf("problems = %q", out.Problems)
	}
	if w.postCount() != 0 || len(w.replies) != 0 {
		t.Errorf("%d POSTs after a refused file", w.postCount())
	}
}

// A thread started under a former identity (the PR's reviews moved to
// another login) is the reviewer's: the reply is accepted with it listed,
// refused without.
func TestRunRepliesAcceptsAFormerLoginsThread(t *testing.T) {
	file := repliesFile(rep(11, ReplyAck, "ok"))
	threads := []fakeThread{thread(fakeComment{ID: 11, Author: "rev-ann", Body: "**[P2] old**"}, human(12, "fixed"))}

	w := newWorld(t)
	w.threads = threads
	if out := w.runReplies(opts(), file); out.Status != StatusInvalid {
		t.Fatalf("without the former login: %+v", out)
	}

	o := opts()
	o.FormerLogins = []string{"rev-ann"}
	w = newWorld(t)
	w.threads = threads
	out := w.runReplies(o, file)
	if out.Status != StatusReplied || len(w.replies) != 1 || w.replies[0].CommentID != 11 {
		t.Fatalf("with the former login: %+v, sent %v", out, w.replies)
	}
}

// After two of its rebuttals in a thread magnum stops arguing there: a
// rebuttal carrying the marker counts, and so does an own reply with no
// marker (the rebuttals magnum posted before the marker existed); an ack, an
// answer and the author's replies do not. The refusal covers every kind of
// new reply and posts nothing.
func TestRunRepliesStopsAfterTwoRebuttals(t *testing.T) {
	other := "r-20261001T090000-1"
	legacy := bot(0, "Still wrong, see `x.rb:12`.") // no marker: an earlier rebuttal
	cases := []struct {
		name     string
		replies  []fakeComment // after the thread's first comment
		refused  bool
		newReply string
	}{
		{"two marked rebuttals", []fakeComment{
			bot(2, marked("No.", other, ReplyRebuttal)), human(3, "Yes."), bot(4, marked("No, still.", "r-20261002T090000-2", ReplyRebuttal))}, true, ReplyRebuttal},
		{"a marked rebuttal and an unmarked own reply", []fakeComment{
			bot(2, marked("No.", other, ReplyRebuttal)), human(3, "Yes."), withID(legacy, 4)}, true, ReplyRebuttal},
		{"two unmarked own replies", []fakeComment{withID(legacy, 2), human(3, "Yes."), withID(legacy, 4)}, true, ReplyRebuttal},
		{"an ack is refused too", []fakeComment{withID(legacy, 2), withID(legacy, 4)}, true, ReplyAck},
		{"an answer is refused too", []fakeComment{withID(legacy, 2), withID(legacy, 4)}, true, ReplyAnswer},
		{"one rebuttal", []fakeComment{bot(2, marked("No.", other, ReplyRebuttal)), human(3, "Yes.")}, false, ReplyRebuttal},
		{"one unmarked own reply", []fakeComment{withID(legacy, 2), human(3, "Yes.")}, false, ReplyRebuttal},
		{"acks and answers do not count", []fakeComment{
			bot(2, marked("Fixed.", other, ReplyAck)), bot(3, marked("Because.", other, ReplyAnswer)), bot(4, marked("Fine.", other, ReplyAck)), human(5, "?")}, false, ReplyRebuttal},
		{"one rebuttal with acks", []fakeComment{
			bot(2, marked("No.", other, ReplyRebuttal)), bot(3, marked("Fixed.", other, ReplyAck)), bot(4, marked("Fixed.", other, ReplyAck))}, false, ReplyRebuttal},
		{"the author's unmarked replies do not count", []fakeComment{human(2, "No."), human(3, "No!"), human(4, "No!!")}, false, ReplyRebuttal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.threads = []fakeThread{thread(append([]fakeComment{bot(1, "**[P2] x**")}, tc.replies...)...)}
			out := w.runReplies(opts(), repliesFile(rep(1, tc.newReply, "Reply.")))
			if tc.refused {
				want := []string{"replies[0]: you already rebutted twice in this thread; magnum stops arguing there and asks the operator"}
				if out.Status != StatusInvalid || out.ExitCode() != 2 || !slices.Equal(out.Problems, want) || len(w.replies) != 0 {
					t.Fatalf("outcome = %+v, sent %v", out, w.replies)
				}
				return
			}
			if out.Status != StatusReplied || len(w.replies) != 1 {
				t.Fatalf("outcome = %+v, sent %v", out, w.replies)
			}
		})
	}
}

// The rebuttal count is per thread, and a refusal in one thread keeps the
// whole file from posting, so nothing goes out half-checked.
func TestRunRepliesRefusesTheWholeFileWhenOneThreadIsClosed(t *testing.T) {
	w := newWorld(t)
	w.threads = []fakeThread{
		thread(bot(1, "**[P2] a**"), bot(2, "No."), bot(3, "No!")),
		thread(bot(11, "**[P2] b**"), human(12, "why?")),
	}
	out := w.runReplies(opts(), repliesFile(rep(11, ReplyAnswer, "Because."), rep(1, ReplyRebuttal, "No!!")))
	if out.Status != StatusInvalid || len(out.Problems) != 1 || !strings.HasPrefix(out.Problems[0], "replies[1]: you already rebutted twice") || len(w.replies) != 0 {
		t.Fatalf("outcome = %+v, sent %v", out, w.replies)
	}
}

// A thread that already holds an own reply carrying this run's marker gets
// nothing more, whatever kind the file now says: the reply found is reported
// (its id, url and kind as it stands), so running the tool again after a
// failure or a lost answer never doubles a reply. Another run's marker, and
// a human's copy of the marker, do not count.
func TestRunRepliesDoesNotPostAgainWhatThisRunPosted(t *testing.T) {
	w := newWorld(t)
	w.threads = []fakeThread{
		thread(bot(11, "**[P2] a**"), human(12, "why?"), bot(13, marked("Because.", runID, ReplyAnswer))),
		thread(bot(21, "**[P2] b**"), human(22, "no"), bot(23, marked("Earlier round.", "r-20261001T090000-1", ReplyAck))),
		thread(bot(31, "**[P2] c**"), human(32, marked("quoted", runID, ReplyAck))),
	}
	out := w.runReplies(opts(), repliesFile(rep(11, ReplyRebuttal, "Because."), rep(21, ReplyAck, "ok"), rep(31, ReplyAck, "ok")))
	if out.Status != StatusReplied || out.ExitCode() != 0 {
		t.Fatalf("outcome = %+v", out)
	}
	want := []PostedReply{
		{CommentID: 11, ID: 13, URL: "https://github.com/talkable/talkable/pull/5#discussion_r13", Kind: ReplyAnswer, Already: true},
		{CommentID: 21, ID: 5001, URL: "https://github.com/talkable/talkable/pull/5#discussion_r5001", Kind: ReplyAck},
		{CommentID: 31, ID: 5002, URL: "https://github.com/talkable/talkable/pull/5#discussion_r5002", Kind: ReplyAck},
	}
	if !slices.Equal(out.Replies, want) {
		t.Errorf("replies = %+v", out.Replies)
	}
	if got, wantSummary := out.Summary(), "posted 2 replies (2 acks); 1 already posted"; got != wantSummary {
		t.Errorf("summary = %q, want %q", got, wantSummary)
	}
	if len(w.replies) != 2 || w.replies[0].CommentID != 21 || w.replies[1].CommentID != 31 {
		t.Errorf("sent = %v", w.replies)
	}
}

// Running the tool twice posts once: the second run finds the first's
// replies on GitHub and reports them already posted.
func TestRunRepliesTwiceRepliesOnce(t *testing.T) {
	w := newWorld(t)
	w.threads = []fakeThread{thread(bot(11, "**[P2] a**"), human(12, "why?"))}
	file := repliesFile(rep(11, ReplyAnswer, "Because."))
	first := w.runReplies(opts(), file)
	second := w.runReplies(opts(), file)
	if first.Status != StatusReplied || len(w.replies) != 1 {
		t.Fatalf("first = %+v, sent %v", first, w.replies)
	}
	if second.Status != StatusReplied || len(second.Replies) != 1 || !second.Replies[0].Already || second.Replies[0].ID != first.Replies[0].ID {
		t.Fatalf("second = %+v", second)
	}
	if want := "1 reply is already posted; nothing posted"; second.Summary() != want {
		t.Errorf("summary = %q, want %q", second.Summary(), want)
	}
}

// A reply this run already posted is reported, not refused, even when it
// was the thread's second rebuttal: only a new reply meets the stop.
func TestRunRepliesAlreadyPostedWinsOverTheRebuttalStop(t *testing.T) {
	w := newWorld(t)
	w.threads = []fakeThread{thread(bot(1, "**[P2] a**"), bot(2, "No."), bot(3, marked("No, still.", runID, ReplyRebuttal)))}
	out := w.runReplies(opts(), repliesFile(rep(1, ReplyRebuttal, "No, still.")))
	if out.Status != StatusReplied || len(out.Replies) != 1 || !out.Replies[0].Already || len(w.replies) != 0 {
		t.Fatalf("outcome = %+v, sent %v", out, w.replies)
	}
}

// A dry run checks everything and posts nothing: the plan lists each reply
// (without an id), and a reply already posted by the run is listed as that.
func TestRunRepliesDryRunPostsNothingAndListsThePlan(t *testing.T) {
	w := newWorld(t)
	w.threads = []fakeThread{
		thread(bot(11, "**[P2] a**"), human(12, "why?")),
		thread(bot(21, "**[P2] b**"), human(22, "fixed"), bot(23, marked("Thanks.", runID, ReplyAck))),
		thread(bot(31, "**[P2] c**"), human(32, "no")),
	}
	o := opts()
	o.DryRun = true
	out := w.runReplies(o, repliesFile(rep(11, ReplyAnswer, "Because."), rep(21, ReplyAck, "ok"), rep(31, ReplyRebuttal, "Still wrong.")))
	if out.Status != StatusDryRun || out.ExitCode() != 0 || out.PlannedReview != nil {
		t.Fatalf("outcome = %+v", out)
	}
	want := []PostedReply{
		{CommentID: 11, Kind: ReplyAnswer},
		{CommentID: 21, ID: 23, URL: "https://github.com/talkable/talkable/pull/5#discussion_r23", Kind: ReplyAck, Already: true},
		{CommentID: 31, Kind: ReplyRebuttal},
	}
	if !slices.Equal(out.Replies, want) {
		t.Errorf("replies = %+v", out.Replies)
	}
	if w.postCount() != 0 || len(w.replies) != 0 {
		t.Errorf("a dry run posted: %d POSTs", w.postCount())
	}
	if got, wantSummary := out.Summary(), "dry run: nothing posted; 2 replies planned (1 rebuttal, 1 answer); 1 already posted"; got != wantSummary {
		t.Errorf("summary = %q, want %q", got, wantSummary)
	}
	// A dry run still refuses what a real run would.
	w.threads = []fakeThread{thread(human(1, "not magnum's"))}
	if out := w.runReplies(o, repliesFile(rep(1, ReplyAck, "ok"))); out.Status != StatusInvalid {
		t.Errorf("dry run on a human's thread: %+v", out)
	}
}

// A failure stops the run at that reply: the replies posted before it are
// reported, none after it is tried, and running the tool again continues
// (the posted ones are found already posted).
func TestRunRepliesAFailureMidWayReportsWhatWasPosted(t *testing.T) {
	w := newWorld(t)
	w.threads = []fakeThread{
		thread(bot(11, "**[P2] a**"), human(12, "?")),
		thread(bot(21, "**[P2] b**"), human(22, "?")),
		thread(bot(31, "**[P2] c**"), human(32, "?")),
	}
	w.replyPosts = []func(execx.Cmd) (execx.Result, error){
		nil, // the first POST succeeds
		func(c execx.Cmd) (execx.Result, error) { return apiFail(c, 500, "Server Error") },
	}
	file := repliesFile(rep(11, ReplyAck, "ok"), rep(21, ReplyAnswer, "Because."), rep(31, ReplyAck, "ok"))

	out := w.runReplies(opts(), file)
	if out.Status != StatusError || out.ExitCode() != 1 || !strings.HasPrefix(out.Message, "post the reply to comment 21: ") {
		t.Fatalf("outcome = %+v", out)
	}
	if len(out.Replies) != 1 || out.Replies[0].CommentID != 11 || out.Replies[0].ID == 0 || out.Replies[0].Already {
		t.Errorf("replies = %+v", out.Replies)
	}
	if len(w.replies) != 2 {
		t.Errorf("%d reply POSTs, want 2: the third must not be tried", len(w.replies))
	}
	// Run again: the first is found posted, the others go out.
	w.replyPosts = nil
	again := w.runReplies(opts(), file)
	if again.Status != StatusReplied || len(again.Replies) != 3 || !again.Replies[0].Already || again.Replies[1].Already || again.Replies[2].Already {
		t.Fatalf("again = %+v", again)
	}
	if len(w.replies) != 4 {
		t.Errorf("%d reply POSTs in all, want 4", len(w.replies))
	}
}

// Reading the threads can fail: nothing is posted and the status says what
// failed.
func TestRunRepliesAThreadListFailureIsAnError(t *testing.T) {
	w := newWorld(t)
	w.threadsErr = errors.New("gh: network is down")
	out := w.runReplies(opts(), repliesFile(rep(11, ReplyAck, "ok")))
	if out.Status != StatusError || out.ExitCode() != 1 || !strings.HasPrefix(out.Message, "read the review threads: ") || w.postCount() != 0 {
		t.Fatalf("outcome = %+v", out)
	}
}

// The options are checked first, and a local base (a review's blind replay)
// has no meaning for replies.
func TestRunRepliesChecksTheOptions(t *testing.T) {
	w := newWorld(t)
	file := repliesFile(rep(11, ReplyAck, "ok"))
	bad := opts()
	bad.HeadSHA = "d4e5f6a"
	if out := w.runReplies(bad, file); out.Status != StatusError || !strings.Contains(out.Message, "not a full commit id") {
		t.Errorf("short head: %+v", out)
	}
	local := opts()
	local.DryRun, local.LocalBase = true, base
	if out := w.runReplies(local, file); out.Status != StatusError || out.Message != "a local base is for a review's dry run only" {
		t.Errorf("local base: %+v", out)
	}
	local.DryRun = false
	if out := w.runReplies(local, file); out.Status != StatusError || out.ExitCode() != 1 {
		t.Errorf("local base without a dry run: %+v", out)
	}
	if w.calls("gh") != 0 {
		t.Errorf("gh ran for bad options: %d calls", w.calls("gh"))
	}
}
