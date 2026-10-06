package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/store"
)

// replyGitHub is the fake GitHub with review threads the judge can reply
// in (guarded: the judge's behavior writes while the round reads).
type replyGitHub struct {
	*fakeGitHub
	mu      sync.Mutex
	threads []github.Thread
	err     error
}

func (g *replyGitHub) ReviewThreads(ctx context.Context, owner, repo string, number int) ([]github.Thread, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]github.Thread, len(g.threads))
	for i, t := range g.threads {
		t.Comments = slices.Clone(t.Comments)
		out[i] = t
	}
	return out, g.err
}

// reply appends a reply by login to the thread whose first comment is root.
func (g *replyGitHub) reply(root, id int64, login, typ, body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.threads {
		if c := g.threads[i].Comments; len(c) > 0 && c[0].ID == root {
			g.threads[i].Comments = append(c, github.ThreadComment{ID: id, AuthorLogin: login, AuthorType: typ, Body: body,
				URL: "https://github.com/talkable/talkable/pull/11920#discussion_r" + strings.Repeat("9", 3), CreatedAt: t0})
		}
	}
}

// replyInput is a reply round: the judge alone on the head its review 900
// covered, two replies since.
func (e *env) replyInput() RoundInput {
	in := e.input(KindRereview)
	in.Round, in.SameHead, in.Replies, in.Roles = 2, true, 2, []config.Role{e.judgeRole()}
	in.Previous = &PreviousReview{ID: 900, Event: "CHANGES_REQUESTED", SHA: target, SubmittedAt: t0.Add(-3 * time.Hour), Login: "talkable[bot]"}
	in.Since = t0.Add(-3 * time.Hour)
	return in
}

// judgeReplies scripts a reply round's judge: it posts replies (comment id
// and kind each) in its threads with the run's reply marker, as
// post-review does, and writes the replied result listing claimed.
func judgeReplies(t *testing.T, gh *replyGitHub, posts [][2]any, claimed int) behavior {
	return func(f *fakeAgents, run store.Run, text string) error {
		id := markerRunID(t, text)
		var listed []map[string]any
		for i, p := range posts {
			root, kind := p[0].(int64), p[1].(string)
			gh.reply(root, int64(800+i), "talkable", "Bot", "One sentence.\n\n"+postreview.ReplyMarker(id, kind))
		}
		for i := range claimed {
			listed = append(listed, map[string]any{"comment_id": 101, "id": 800 + i, "kind": postreview.ReplyRebuttal})
		}
		if listed == nil {
			listed = []map[string]any{}
		}
		if err := writeJudgeJSON(store.Deref(run.ReportPath), map[string]any{"status": "replied", "run_id": id, "replies": listed}); err != nil {
			return err
		}
		return f.end(run.ID)
	}
}

// The live case (2026-10-06): an author declined a finding, and the verdict
// waited for a push. A reply round whose verdict stays posts no review: the
// judge answers in its threads, the round ends replied, verified by the
// replies with its run's marker on GitHub, its runs verified without a
// review, no findings recorded and no review touched.
func TestAReplyRoundThatAnswersInItsThreadsEndsRepliedWithoutAReview(t *testing.T) {
	e := newEnv(t)
	fa := captureJudge(e)
	gh := &replyGitHub{fakeGitHub: e.gh, threads: prThreads()}
	e.r.GitHub = gh
	e.ag.behaviors[agents.RoleJudge] = []behavior{judgeReplies(t, gh, [][2]any{{int64(101), postreview.ReplyRebuttal}, {int64(105), postreview.ReplyAck}}, 2)}

	res, err := e.r.RunRound(e.ctx, e.replyInput())
	if err != nil || res.Outcome != OutcomeReplied {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if res.ReviewID != 0 || res.Event != "" || res.Findings != nil || len(res.Replies) != 2 {
		t.Fatalf("result: review %d event %q findings %v replies %+v", res.ReviewID, res.Event, res.Findings, res.Replies)
	}
	if r := res.Replies[0]; r.CommentID != 101 || r.ID != 800 || r.Kind != postreview.ReplyRebuttal {
		t.Fatalf("first reply = %+v", r)
	}
	if res.JudgePromptedAt.IsZero() || !res.ThreadsRead {
		t.Fatalf("prompted at %v, threads read %v", res.JudgePromptedAt, res.ThreadsRead)
	}
	jd := fa.data(t)
	if jd.Replies != 2 || !jd.SameHead {
		t.Fatalf("judge data: replies %d same head %v", jd.Replies, jd.SameHead)
	}
	prompt := e.ag.submitsFor(agents.RoleJudge)[0].Text
	mustContain(t, "judge prompt", prompt, "2 replies came on your review since you last read the threads.",
		"post no review: answer in the threads", "post_replies: ", " --replies ")
	judge := e.runOf(agents.RoleJudge, KindRereview)
	if judge.State != store.RunVerified || store.Deref(judge.Outcome) != OutcomeReplied || judge.ReviewID != nil || judge.ResultJSON == nil {
		t.Fatalf("judge run: state %s outcome %v review %v result %v", judge.State, judge.Outcome, judge.ReviewID, judge.ResultJSON)
	}
	if fs, err := e.st.FindingsByPR(e.ctx, e.pr.ID); err != nil || len(fs) != 0 {
		t.Fatalf("findings recorded for a replied round: %+v, %v", fs, err)
	}
	if len(e.gh.updates) != 0 || len(e.gh.dismissed) != 0 {
		t.Fatalf("a replied round touched a review: updates %+v, dismissals %+v", e.gh.updates, e.gh.dismissed)
	}
	evs := eventsOfKind(e.events(), "round.replied")
	if len(evs) != 1 || evs[0].Message != "replied in 2 threads (1 acknowledgement, 1 rebuttal); no new review: the verdict of review 900 stands" {
		t.Fatalf("round.replied events: %+v", evs)
	}
}

// A judge that finds nothing to answer posts nothing: its replied result
// with no replies ends the round replied. One that claims replies GitHub
// does not show needs attention; and a round that is no reply round asked
// for a review, so a replied result there needs attention too.
func TestARepliedResultIsVerifiedByTheRepliesOnGitHub(t *testing.T) {
	for name, c := range map[string]struct {
		claimed int
		reply   bool
		want    string
		errPart string
	}{
		"nothing to answer":         {claimed: 0, reply: true, want: OutcomeReplied},
		"replies GitHub lacks":      {claimed: 1, reply: true, want: OutcomeNeedsAttention, errPart: "GitHub shows none by talkable[bot]"},
		"replied in a review round": {claimed: 0, reply: false, want: OutcomeNeedsAttention, errPart: "this round asked for a review"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			gh := &replyGitHub{fakeGitHub: e.gh, threads: prThreads()}
			e.r.GitHub = gh
			e.ag.behaviors[agents.RoleJudge] = []behavior{judgeReplies(t, gh, nil, c.claimed), judgeReplies(t, gh, nil, c.claimed)}
			in := e.replyInput()
			if !c.reply {
				in.Replies = 0
			}
			res, _ := e.r.RunRound(e.ctx, in)
			if res.Outcome != c.want || !strings.Contains(res.Error, c.errPart) {
				t.Fatalf("outcome %s error %q, want %s with %q", res.Outcome, res.Error, c.want, c.errPart)
			}
			if c.want == OutcomeReplied {
				if evs := eventsOfKind(e.events(), "round.replied"); len(evs) != 1 || !strings.HasPrefix(evs[0].Message, "nothing to answer; no new review") {
					t.Fatalf("round.replied events: %+v", evs)
				}
			}
		})
	}
}

// A reply round whose verdict or event changes posts a review as any round:
// posted, the replies aside.
func TestAReplyRoundWhoseVerdictChangesPostsAReview(t *testing.T) {
	e := newEnv(t)
	gh := &replyGitHub{fakeGitHub: e.gh, threads: prThreads()}
	e.r.GitHub = gh
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(902, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.replyInput())
	if err != nil || res.Outcome != OutcomePosted || res.ReviewID != 902 || len(res.Replies) != 0 {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
}

// A reply in a thread is a review of its own, by the reviewer, on the head,
// with no body: it is never taken for the round's review (the marker-less
// last resort).
func TestAThreadRepliesOwnReviewIsNeverTheRoundsReview(t *testing.T) {
	e := newEnv(t)
	gh := &replyGitHub{fakeGitHub: e.gh, threads: prThreads()}
	e.r.GitHub = gh
	e.ag.behaviors[agents.RoleJudge] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		id := markerRunID(t, text)
		gh.reply(101, 800, "talkable", "Bot", "Still open.\n\n"+postreview.ReplyMarker(id, postreview.ReplyRebuttal))
		e.gh.add(github.Review{DatabaseID: 950, State: "COMMENTED", Body: "", SubmittedAt: f.st.Clock(), CommitOid: target,
			AuthorLogin: "talkable", AuthorType: "Bot"},
			github.RESTReview{ID: 950, UserLogin: "talkable[bot]", UserType: "Bot", State: "COMMENTED", SubmittedAt: f.st.Clock(), CommitID: target})
		if err := writeJudgeJSON(store.Deref(run.ReportPath), map[string]any{"status": "replied", "run_id": id,
			"replies": []map[string]any{{"comment_id": 101, "id": 800, "kind": "rebuttal"}}}); err != nil {
			return err
		}
		return f.end(run.ID)
	}}
	res, err := e.r.RunRound(e.ctx, e.replyInput())
	if err != nil || res.Outcome != OutcomeReplied || res.ReviewID != 0 {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
}

// After two of the reviewer's rebuttals in a thread (its replies of kind
// rebuttal, and its replies without a reply marker, as rebuttals were
// before) an author's answer marks the thread stop: the threads file says
// so, the prompt says not to reply there, and the round reports it. Acks
// and answers are no rebuttals, and a thread whose last reply is the
// reviewer's is not stopped.
func TestTwoRebuttalsAndAnAnswerStopTheThread(t *testing.T) {
	e := newEnv(t)
	fa := captureJudge(e)
	own := func(id int64, body string) github.ThreadComment {
		return github.ThreadComment{ID: id, AuthorLogin: "talkable", AuthorType: "Bot", Body: body, URL: "https://example.test/c", CreatedAt: t0.Add(-time.Hour)}
	}
	author := func(id int64) github.ThreadComment {
		return github.ThreadComment{ID: id, AuthorLogin: "alice", AuthorType: "User", Body: "(Claude) Won't fix.", CreatedAt: t0.Add(-time.Hour)}
	}
	marked := func(kind string) string { return "One sentence.\n\n" + postreview.ReplyMarker("r-old", kind) }
	gh := &replyGitHub{fakeGitHub: e.gh, threads: []github.Thread{
		{ID: "PRRT_A", Comments: []github.ThreadComment{own(201, "**[P2] A**"), author(202), own(203, "An unmarked rebuttal."), author(204), own(205, marked("rebuttal")), author(206)}},
		{ID: "PRRT_B", Comments: []github.ThreadComment{own(301, "**[P2] B**"), author(302), own(303, marked("ack")), author(304), own(305, marked("answer")), author(306)}},
		{ID: "PRRT_C", Comments: []github.ThreadComment{own(401, "**[P2] C**"), author(402), own(403, marked("rebuttal")), author(404), own(405, marked("rebuttal"))}},
	}}
	e.r.GitHub = gh
	e.ag.behaviors[agents.RoleJudge] = []behavior{judgeReplies(t, gh, nil, 0)}
	res, err := e.r.RunRound(e.ctx, e.replyInput())
	if err != nil || res.Outcome != OutcomeReplied {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if len(res.Stops) != 1 || res.Stops[0].ID != "PRRT_A" || res.Stops[0].LastReply != 206 || res.Stops[0].URL != "https://example.test/c" {
		t.Fatalf("stops = %+v", res.Stops)
	}
	b, err := os.ReadFile(filepath.Join(res.ReportDir, ThreadsFile))
	if err != nil {
		t.Fatal(err)
	}
	var threads []agents.ReviewThread
	if err := json.Unmarshal(b, &threads); err != nil {
		t.Fatal(err)
	}
	if len(threads) != 3 || !threads[0].Stop || threads[0].Rebuttals != 2 || threads[1].Stop || threads[1].Rebuttals != 0 ||
		threads[2].Stop || threads[2].Rebuttals != 2 {
		t.Fatalf("threads: %+v", threads)
	}
	if k := threads[0].Replies[3].Kind; k != postreview.ReplyRebuttal || threads[0].Replies[1].Kind != "" || threads[1].Replies[1].Kind != postreview.ReplyAck {
		t.Fatalf("reply kinds: %+v / %+v", threads[0].Replies, threads[1].Replies)
	}
	if jd := fa.data(t); jd.StopThreads != 1 {
		t.Fatalf("judge data stop threads = %d", jd.StopThreads)
	}
	mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "1 thread is marked `stop`: you rebutted twice there")
}
