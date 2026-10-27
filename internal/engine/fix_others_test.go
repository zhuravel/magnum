package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/store"
)

// othersOnly is the refusal of a fix on a review that found nothing while
// no other reviewer waits for an answer.
const othersOnly = "nothing to fix: the review on a1a1a1a found nothing and no other reviewer's thread or comment waits for an answer"

// On talkable#12054 `f` answered "found nothing to fix" while a reviewer's
// MINOR follow-up had no reply: spec 93 had the fix job answer the other
// reviewers, but a review of magnum's that found nothing still refused its
// start. Now such a review starts the job when another reviewer's thread or
// comment waits for an answer: the job reads the PR once, at the request,
// and writes what it read to the external file, the answer and the prompt
// say the review found nothing, and a fixer that only replied ends the loop
// clean, one that pushed goes on to the re-review as usual.
func TestACleanReviewStartsAFixForAnotherReviewersUnansweredThread(t *testing.T) {
	const what = "claude-fix answers the other reviewers' 1 unanswered thread or comment in review1 (magnum's review of a1a1a1a found nothing), cycle 1 of 3"
	for _, pushed := range []bool{false, true} {
		t.Run(map[bool]string{false: "replied only", true: "pushed"}[pushed], func(t *testing.T) {
			h, git := fixHarness(t)
			pr := fixablePR(h, "zhuravel", map[string]int{})
			h.gh.mu.Lock()
			h.gh.threads = map[int][]github.Thread{2: {
				extThread(4322458360, "lib/sandbox.rb", 185, h.clock.Now().Add(-time.Hour), "MINOR: `run` drops stderr", "rev-ann"),
			}}
			h.gh.mu.Unlock()
			head, commits := "", "nothing"
			if pushed {
				head, commits = fixPushed, "b2b2b2b Log the sandbox's stderr (review comment 4322458360)"
			}
			var started atomic.Int64 // the conversation's reads when the fixer starts, before the settle's own look
			fixer(h, git, head, func(run store.Run) string {
				started.Store(int64(h.gh.count("issue_comments:talkable/talkable#2")))
				return fixReport(run.ID, commits, "1 threads (1 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none")
			})
			id := h.fix(FixPayload{})
			h.handle()

			if res := h.wantDone(id); !strings.Contains(res, what) {
				t.Errorf("answer = %q, want it to say %q", res, what)
			}
			runs := h.fixRuns(pr.ID)
			if len(runs) != 1 {
				t.Fatalf("fix runs = %+v, want the fixer's", runs)
			}
			x := readExternalOf(t, runs[0])
			if len(x.Threads) != 1 || x.Threads[0].CommentID != 4322458360 || x.Threads[0].Answered {
				t.Errorf("external threads = %+v, want the 12054 thread unanswered", x.Threads)
			}
			external := filepath.Join(filepath.Dir(deref(runs[0].ReportPath)), externalFile)
			if !slices.ContainsFunc(h.ag.all(), func(c string) bool {
				return strings.HasPrefix(c, "fix_prompt:claude-fix:"+runs[0].ID+"|") && strings.HasSuffix(c, "|"+external+"|no findings")
			}) {
				t.Errorf("the fix prompt does not say the review found nothing and name %s: %v", external, h.ag.all())
			}
			if n := started.Load(); n != 1 {
				t.Errorf("the conversation was read %d times before the fixer started, want once: the job writes what the request read", n)
			}
			if ev, ok := h.fixEvent("fix.start"); !ok || !strings.Contains(ev.Message, "answers the other reviewers' 1 unanswered thread or comment") {
				t.Errorf("fix.start = %q, %v", ev.Message, ok)
			}

			st, ok := h.fixStateOf(2)
			if pushed {
				if !ok || st.Phase != FixPhaseReviewing || st.Head != fixPushed {
					t.Errorf("loop = %+v, %v; want it waiting for the re-review of %s", st, ok, fixPushed)
				}
				return
			}
			if ok {
				t.Errorf("the loop goes on: %+v", st)
			}
			h.flush()
			if got := toastsWith(h, "PR #2: claude-fix pushed nothing, replied to 1 thread or comment of other reviewers"); len(got) != 1 {
				t.Errorf("toasts = %v", h.nh.shown())
			}
			h.wantToasts("needs attention", 0)
		})
	}
}

// A review that found nothing, on a PR whose other reviewers wait for
// nothing, still refuses the fix, in words that say both: a thread the
// author answered, one the fixer answered, magnum's own, a resolved one,
// what outsiders wrote and a review body before the author's last word
// leave nothing to answer. The dry run refuses the same way.
func TestACleanReviewRefusesAFixWhenNoOtherReviewerWaitsForAnAnswer(t *testing.T) {
	h, _ := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{})
	base := h.clock.Now().Add(-3 * time.Hour)
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	resolved := extThread(4322458800, "app/f.rb", 6, at(5), "Done already", "rev-ann")
	resolved.Resolved = true
	byFixer := extThread(4322458700, "app/e.rb", 5, at(6), "Nit: spacing", "review-bot", "zhuravel")
	byFixer.Comments[1].Body = "Not a bug: the linter wants it.\n\n" + postreview.FixReplyMarker
	outsider := extThread(4322458900, "app/g.rb", 7, at(7), "Use my gem", "alice")
	outsider.Comments[0].AuthorAssociation = "CONTRIBUTOR"
	h.gh.mu.Lock()
	h.gh.threads = map[int][]github.Thread{2: {
		extThread(4322458400, "app/b.rb", 9, at(1), "Rename this", "bob-rev", "zhuravel"),
		extThread(4322458500, "app/c.rb", 3, at(2), "**[P2] magnum's own**", "talkable"),
		resolved, byFixer, outsider,
	}}
	h.gh.allReviews = map[int][]github.Review{2: {
		{DatabaseID: 9001, State: "COMMENTED", Body: "Overall: add a spec", URL: prPage + "#pullrequestreview-9001", SubmittedAt: at(10),
			AuthorLogin: "rev-ann", AuthorType: "User", AuthorAssociation: "MEMBER"},
	}}
	h.gh.prComments = map[int][]github.PRComment{2: {
		{ID: 701, URL: prPage + "#issuecomment-701", Body: "Added the spec", CreatedAt: at(20), AuthorLogin: "zhuravel", AuthorType: "User"},
		{ID: 702, URL: prPage + "#issuecomment-702", Body: "Please merge my idea", CreatedAt: at(30), AuthorLogin: "alice", AuthorType: "User",
			AuthorAssociation: "NONE"},
	}}
	h.gh.mu.Unlock()
	for _, dry := range []bool{false, true} {
		id := h.fix(FixPayload{DryRun: dry})
		h.handle()
		if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != othersOnly {
			t.Errorf("dry run %v: %s %q, want failed %q", dry, r.State, deref(r.Result), othersOnly)
		}
	}
	if h.ag.count("submit:") != 0 || len(h.fixRuns(pr.ID)) != 0 {
		t.Error("a refused request started the fixer")
	}
}

// A review that found nothing starts a fix only for what the other
// reviewers wrote, so a GitHub that cannot list it refuses the fix with
// the cause: a job started blind would have nothing to do.
func TestACleanReviewWhoseOtherReviewersCannotBeReadRefusesWithTheCause(t *testing.T) {
	const want = "talkable/talkable#2: the review on a1a1a1a found nothing, and the other reviewers' threads and comments could not be read: HTTP 502"
	h, _ := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{})
	h.gh.mu.Lock()
	h.gh.threadsErr = errors.New("HTTP 502")
	h.gh.mu.Unlock()
	id := h.fix(FixPayload{})
	h.handle()
	if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != want {
		t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), want)
	}
	if h.ag.count("submit:") != 0 || len(h.fixRuns(pr.ID)) != 0 {
		t.Error("a refused request started the fixer")
	}
}

// A fix on a review that found nothing has the other reviewers' items as
// its whole work: when their file cannot be written beside the report, the
// fixer is never prompted, and the loop fails and says why, where a job
// with findings would go on with them alone.
func TestACleanReviewsFixFailsWhenTheOtherReviewersFileCannotBeWritten(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{})
	h.gh.mu.Lock()
	h.gh.threads = map[int][]github.Thread{2: {
		extThread(4322458360, "lib/sandbox.rb", 185, h.clock.Now().Add(-time.Hour), "MINOR: `run` drops stderr", "rev-ann"),
	}}
	h.gh.mu.Unlock()
	// A directory where the file goes: neither written nor removed.
	blocked := filepath.Join(h.layout.ReviewDir("talkable", "talkable", 2, fixHead), externalFile)
	if err := os.MkdirAll(filepath.Join(blocked, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", "0 threads", "none") })
	h.fix(FixPayload{})
	h.handle()
	if h.ag.count("submit:") != 0 {
		t.Error("the fixer was prompted without the other reviewers' file")
	}
	if ev, ok := h.fixEvent("fix.failed"); !ok || !strings.Contains(ev.Message, "the other reviewers' threads and comments") {
		t.Errorf("fix.failed = %+v, %v", ev, ok)
	}
	if runs := h.fixRuns(pr.ID); len(runs) != 1 || runs[0].State == store.RunEnded || runs[0].State == store.RunVerified {
		t.Errorf("fix runs = %+v, want the one that never got its prompt", runs)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
}

// GitHub lists at most so many review threads, reviews and conversation
// comments: a read cut short that shows nothing waiting cannot say that
// nothing waits (1,000 resolved threads, then one open), so the request is
// refused with what was cut and a word to answer by hand, never as nothing
// to fix. A cut read that shows an item waiting starts the fix on it.
func TestACleanReviewWhoseOtherReviewersAreCutShortRefusesWithWhatWasCut(t *testing.T) {
	const lead = "talkable/talkable#2: the review on a1a1a1a found nothing, and the other reviewers' threads and comments could not all be read ("
	resolved := extThread(4322458800, "app/f.rb", 6, time.Date(2026, 10, 20, 7, 0, 0, 0, time.UTC), "Done already", "rev-ann")
	resolved.Resolved = true
	for _, c := range []struct {
		name, cut string
		cutShort  func(g *fakeGH)
	}{
		{"threads", fmt.Sprintf("over %d review threads", github.MaxReviewThreads), func(g *fakeGH) { g.threadsCut = map[int]bool{2: true} }},
		{"reviews", fmt.Sprintf("over %d reviews", github.MaxReviews), func(g *fakeGH) { g.reviewsCut = map[int]bool{2: true} }},
		{"comments", fmt.Sprintf("over %d comments since the author's last", github.MaxIssueComments), func(g *fakeGH) {
			at := time.Date(2026, 10, 20, 7, 0, 0, 0, time.UTC)
			g.prComments = map[int][]github.PRComment{2: { // outsiders' comments, which are no items, past the read's last page
				{ID: 701, Body: "Please merge my idea", CreatedAt: at, AuthorLogin: "alice", AuthorType: "User", AuthorAssociation: "NONE"},
				{ID: 702, Body: "And mine", CreatedAt: at.Add(time.Minute), AuthorLogin: "alice", AuthorType: "User", AuthorAssociation: "NONE"},
			}}
			g.prCommentsPage, g.prCommentsPages = 1, 1
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := fixHarness(t)
			pr := fixablePR(h, "zhuravel", map[string]int{})
			h.gh.mu.Lock()
			h.gh.threads = map[int][]github.Thread{2: {resolved}}
			c.cutShort(h.gh)
			h.gh.mu.Unlock()
			id := h.fix(FixPayload{})
			h.handle()
			if r, want := h.request(id), lead+c.cut+"); answer them by hand"; r.State != store.RequestFailed || deref(r.Result) != want {
				t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), want)
			}
			if h.ag.count("submit:") != 0 || len(h.fixRuns(pr.ID)) != 0 {
				t.Error("a refused request started the fixer")
			}
		})
	}
	t.Run("an item waits", func(t *testing.T) {
		h, git := fixHarness(t)
		pr := fixablePR(h, "zhuravel", map[string]int{})
		h.gh.mu.Lock()
		h.gh.threads = map[int][]github.Thread{2: {resolved,
			extThread(4322458360, "lib/sandbox.rb", 185, h.clock.Now().Add(-time.Hour), "MINOR: `run` drops stderr", "rev-ann")}}
		h.gh.threadsCut = map[int]bool{2: true}
		h.gh.mu.Unlock()
		fixer(h, git, "", func(run store.Run) string {
			return fixReport(run.ID, "nothing", "1 threads (0 fixed, 0 already fixed, 1 not a bug, 0 won't fix)", "none")
		})
		id := h.fix(FixPayload{})
		h.handle()
		h.wantDone(id)
		if runs := h.fixRuns(pr.ID); len(runs) != 1 {
			t.Errorf("fix runs = %+v, want the fixer's", runs)
		}
	})
}
