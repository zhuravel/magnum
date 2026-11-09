package pipeline

import (
	"context"
	"encoding/json"
	"maps"
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
		"post no review: answer in the threads", "post_replies: ", " --replies ",
		"A thread another reviewer or the author started is theirs, never answered inline; a reply round with nothing of yours to answer ends `replied` with no replies, never `blocked`.")
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

// The live case (2026-10-15), for one release: a judge on the old skill
// answered the author's own threads, post-review refused them, and it wrote
// blocked, which parked the PR. A blocked result in a reply round whose
// blocker names the refused replies to threads magnum did not start ends
// the round replied with nothing answered (a warning says why), its run
// verified; any other blocker, and the same blocker in a round that asked
// for a review, stay blocked.
func TestAnOldJudgesForeignThreadBlockIsNotAnAttention(t *testing.T) {
	for name, c := range map[string]struct {
		blocker string
		reply   bool
		want    string
	}{
		"the live blocker": {"Reply validation rejected all eight comments (comment 31 does not start a thread of talkable[bot]); nothing was posted.", true, OutcomeReplied},
		"the tool's words": {"post_replies refused: comment 31 does not start a thread of talkable[bot].", true, OutcomeReplied},
		"another blocker":  {"The checkout has no database for the specs.", true, OutcomeBlocked},
		"a review round":   {"Reply validation rejected all eight comments; nothing was posted.", false, OutcomeBlocked},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			gh := &replyGitHub{fakeGitHub: e.gh, threads: prThreads()}
			e.r.GitHub = gh
			blocked := func(f *fakeAgents, run store.Run, text string) error {
				if err := writeJudgeJSON(store.Deref(run.ReportPath), map[string]any{"status": "blocked", "run_id": markerRunID(t, text), "blocker": c.blocker}); err != nil {
					return err
				}
				return f.end(run.ID)
			}
			e.ag.behaviors[agents.RoleJudge] = []behavior{blocked, blocked}
			in := e.replyInput()
			if !c.reply {
				in.Replies = 0
			}
			res, _ := e.r.RunRound(e.ctx, in)
			if res.Outcome != c.want {
				t.Fatalf("outcome %s (error %q), want %s", res.Outcome, res.Error, c.want)
			}
			warned := slices.ContainsFunc(eventsOfKind(e.events(), "round.warning"), func(ev store.Event) bool {
				return ev.Level == "warn" && strings.HasPrefix(ev.Message, "the judge wrote blocked over replies to threads it did not start")
			})
			if c.want == OutcomeBlocked {
				if warned || !strings.Contains(res.Error, "judge blocked") {
					t.Fatalf("warned %v, error %q", warned, res.Error)
				}
				return
			}
			if res.Error != "" || len(res.Replies) != 0 || res.ReviewID != 0 || !warned {
				t.Fatalf("result: error %q replies %+v review %d, warned %v", res.Error, res.Replies, res.ReviewID, warned)
			}
			judge := e.runOf(agents.RoleJudge, KindRereview)
			if judge.State != store.RunVerified || store.Deref(judge.Outcome) != OutcomeReplied {
				t.Fatalf("judge run: state %s outcome %v", judge.State, judge.Outcome)
			}
			if evs := eventsOfKind(e.events(), "round.replied"); len(evs) != 1 || !strings.HasPrefix(evs[0].Message, "nothing to answer; no new review") {
				t.Fatalf("round.replied events: %+v", evs)
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

// The live cases (2026-11-09): a round whose judge accepted the author's
// last answer in a stopped thread ("Fixed: …", "Answered: … accepted")
// flagged that thread as a stalemate all the same, and since a stopped
// thread stays stopped the flag never went. The judge names the stopped
// threads whose finding it keeps open (stops_open), and the round's stops
// are those alone: the id of a thread that is not stopped adds nothing. A
// result that names none, or not as a list, keeps every stop, as before;
// an empty list keeps none, in a reply round too. A list with an entry
// that is no thread's id in the threads file (a thread's URL, an id the
// file does not list) decides nothing: every stop stays, with a warning,
// since filtering on it would drop the threads the judge meant to keep. A
// round whose judge wrote its result but posted nothing (needs attention)
// keeps every stop too. The threads file and the prompt still mark both
// threads stop.
func TestTheRoundKeepsOnlyTheStopsTheJudgeHoldsOpen(t *testing.T) {
	own := func(id int64, body string) github.ThreadComment {
		return github.ThreadComment{ID: id, AuthorLogin: "talkable", AuthorType: "Bot", Body: body, URL: "https://example.test/c", CreatedAt: t0.Add(-time.Hour)}
	}
	author := func(id int64) github.ThreadComment {
		return github.ThreadComment{ID: id, AuthorLogin: "alice", AuthorType: "User", Body: "(Claude) Fixed in 1a2b3c4.", CreatedAt: t0.Add(-time.Hour)}
	}
	rebuttal := "One sentence.\n\n" + postreview.ReplyMarker("r-old", postreview.ReplyRebuttal)
	stopped := func(id string, root int64) github.Thread {
		return github.Thread{ID: id, Comments: []github.ThreadComment{own(root, "**[P2] "+id+"**"), author(root + 1), own(root+2, rebuttal),
			author(root + 3), own(root+4, rebuttal), author(root + 5)}}
	}
	answered := github.Thread{ID: "PRRT_B", Comments: []github.ThreadComment{own(301, "**[P3] PRRT_B**"), author(302)}}
	ids := func(ss []StopThread) []string {
		out := []string{}
		for _, s := range ss {
			out = append(out, s.ID)
		}
		return out
	}
	both := []string{"PRRT_A", "PRRT_D"}
	for _, tc := range []struct {
		name     string
		reply    bool // a reply round; else a re-review that posts
		noReview bool // the re-review's judge writes its result but posts nothing
		extra    map[string]any
		want     []string
		warns    bool // the round warns that stops_open decided nothing
	}{
		{name: "the judge holds one open", extra: map[string]any{"stops_open": []string{"PRRT_D", "PRRT_B"}}, want: []string{"PRRT_D"}},
		{name: "the judge settles both", extra: map[string]any{"stops_open": []string{}}, want: []string{}},
		{name: "a reply round's judge settles both", reply: true, extra: map[string]any{"stops_open": []string{}}, want: []string{}},
		{name: "a result without stops_open", want: both},
		{name: "a stops_open that is not a list", extra: map[string]any{"stops_open": "x"}, want: both},
		{name: "a stops_open naming a thread by its URL", extra: map[string]any{"stops_open": []string{"https://example.test/c"}}, want: both, warns: true},
		{name: "a stops_open with an id the file does not list", extra: map[string]any{"stops_open": []string{"PRRT_D", "PRRT_X"}}, want: both, warns: true},
		{name: "a judge that posted nothing", noReview: true, extra: map[string]any{"stops_open": []string{}}, want: both},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			fa := captureJudge(e)
			gh := &replyGitHub{fakeGitHub: e.gh, threads: []github.Thread{stopped("PRRT_A", 201), answered, stopped("PRRT_D", 501)}}
			e.r.GitHub = gh
			in, outcome := e.replyInput(), OutcomeReplied
			if tc.reply {
				e.ag.behaviors[agents.RoleJudge] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
					result := map[string]any{"status": "replied", "run_id": markerRunID(t, text), "replies": []map[string]any{}}
					maps.Copy(result, tc.extra)
					if err := writeJudgeJSON(store.Deref(run.ReportPath), result); err != nil {
						return err
					}
					return f.end(run.ID)
				}}
			} else {
				p := e.judgePosts(506, "COMMENTED", "COMMENT")
				p.extra = tc.extra
				outcome = OutcomePosted
				if tc.noReview {
					p.gh, outcome = nil, OutcomeNeedsAttention
				}
				e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t), p.behavior(t)}
				in = e.input(KindRereview)
				in.Round, in.Since = 2, t0.Add(-3*time.Hour)
				in.Previous = &PreviousReview{ID: 900, Event: "COMMENTED", SHA: prevSHA, SubmittedAt: t0.Add(-3 * time.Hour), Login: "talkable[bot]"}
			}
			res, err := e.r.RunRound(e.ctx, in)
			if err != nil || res.Outcome != outcome {
				t.Fatalf("RunRound = %+v, %v; want %s", res, err, outcome)
			}
			if got := ids(res.Stops); !res.ThreadsRead || !slices.Equal(got, tc.want) {
				t.Fatalf("threads read %v, stops %v; want %v", res.ThreadsRead, got, tc.want)
			}
			if res.StopsOpenGiven || res.StopsOpen != nil {
				t.Fatalf("a round that read the threads carries stops_open %v (given %v) for the engine", res.StopsOpen, res.StopsOpenGiven)
			}
			warned := slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "stops_open") })
			if warned != tc.warns {
				t.Fatalf("warned %v, want %v: %q", warned, tc.warns, res.Warnings)
			}
			b, err := os.ReadFile(filepath.Join(res.ReportDir, ThreadsFile))
			if err != nil {
				t.Fatal(err)
			}
			var threads []agents.ReviewThread
			if err := json.Unmarshal(b, &threads); err != nil {
				t.Fatal(err)
			}
			if len(threads) != 3 || !threads[0].Stop || threads[1].Stop || !threads[2].Stop {
				t.Fatalf("threads: %+v", threads)
			}
			if jd := fa.data(t); jd.StopThreads != 2 {
				t.Fatalf("judge data stop threads = %d", jd.StopThreads)
			}
			mustContain(t, "judge prompt", e.ag.submitsFor(agents.RoleJudge)[0].Text, "2 threads are marked `stop`", "`stops_open`")
		})
	}
}

// writeStoppedThreads writes the threads file a paused or unverified
// round's addThreads left on the head: PRRT_A and PRRT_D marked stop,
// PRRT_B answered.
func (e *env) writeStoppedThreads() {
	e.t.Helper()
	threads := []agents.ReviewThread{{ID: "PRRT_A", Stop: true}, {ID: "PRRT_B"}, {ID: "PRRT_D", Stop: true}}
	b, err := json.Marshal(threads)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.MkdirAll(e.reportDir(), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.reportDir(), ThreadsFile), b, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// A round paused on a usage limit flagged its stopped threads before its
// judge decided them, and the continued turn that finishes the judge's
// work reads no threads, so the flag stayed though the judge accepted the
// answer. The continue carries the judge's stops_open to the engine
// (StopsOpen), checked against the threads file its paused round gave the
// judge on this head: an entry that is no thread there, a result without
// the field, a missing file or a turn that posted nothing carry none.
func TestAContinuedTurnCarriesTheStopsItsJudgeHoldsOpen(t *testing.T) {
	for _, tc := range []struct {
		name     string
		noFile   bool
		noReview bool
		extra    map[string]any
		want     []string // nil: none given
		warns    bool
	}{
		{name: "the judge holds one open", extra: map[string]any{"stops_open": []string{"PRRT_D", "PRRT_B"}}, want: []string{"PRRT_D", "PRRT_B"}},
		{name: "the judge settles both", extra: map[string]any{"stops_open": []string{}}, want: []string{}},
		{name: "a result without stops_open"},
		{name: "an id the file does not list", extra: map[string]any{"stops_open": []string{"PRRT_X"}}, warns: true},
		{name: "no threads file on the head", noFile: true, extra: map[string]any{"stops_open": []string{}}},
		{name: "a judge that posted nothing", noReview: true, extra: map[string]any{"stops_open": []string{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if !tc.noFile {
				e.writeStoppedThreads()
			}
			p := e.judgePosts(517, "COMMENTED", "COMMENT")
			p.extra = tc.extra
			outcome := OutcomePosted
			if tc.noReview {
				p.gh, outcome = nil, OutcomeNeedsAttention
			}
			e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t), p.behavior(t)}
			in := e.input(KindContinue)
			in.ContinueRunID = "r-20261003T100000-7"
			res, err := e.r.RunRound(e.ctx, in)
			if err != nil || res.Outcome != outcome {
				t.Fatalf("RunRound = %+v, %v; want %s", res, err, outcome)
			}
			if res.ThreadsRead || res.Stops != nil {
				t.Fatalf("a continue read the threads: %v %+v", res.ThreadsRead, res.Stops)
			}
			if res.StopsOpenGiven != (tc.want != nil) || !slices.Equal(res.StopsOpen, tc.want) {
				t.Fatalf("stops open %#v given %v, want %#v", res.StopsOpen, res.StopsOpenGiven, tc.want)
			}
			if warned := slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "stops_open") }); warned != tc.warns {
				t.Fatalf("warned %v, want %v: %q", warned, tc.warns, res.Warnings)
			}
		})
	}
}

// The judge posted its review but GitHub could not verify it, so its round
// ended in error with its stopped threads flagged; the next round on the
// head adopts the review, prompting nothing, and carries that judge's
// stops_open to the engine as a continued turn does.
func TestAnAdoptedReviewCarriesTheStopsItsJudgeHoldsOpen(t *testing.T) {
	e := newEnv(t)
	e.writeStoppedThreads()
	post := e.judgePosts(803, "COMMENTED", "COMMENT")
	post.extra = map[string]any{"stops_open": []string{"PRRT_A"}}
	e.unverifiedRound(post)
	res, err := e.r.RunRound(e.ctx, e.nextRound())
	if err != nil || res.Outcome != OutcomePosted || res.ReviewID != 803 {
		t.Fatalf("adopting round: result = %+v, err = %v", res, err)
	}
	if res.ThreadsRead || !res.StopsOpenGiven || !slices.Equal(res.StopsOpen, []string{"PRRT_A"}) {
		t.Fatalf("threads read %v, stops open %#v given %v", res.ThreadsRead, res.StopsOpen, res.StopsOpenGiven)
	}
}
