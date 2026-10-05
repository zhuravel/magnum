package engine

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

var (
	retroShaA = strings.Repeat("a", 40) // the commit magnum reviewed
	retroShaB = strings.Repeat("b", 40) // a later push it did not review
)

// retroBody is a comment long enough to be a candidate; events and logs
// must never carry it.
const retroBody = "SECRET-BODY: this lookup drops the tenant scope, so a coupon of another site applies."

// retroPR registers PR n of talkable/talkable as merged ago before now,
// authored by alice, with a review magnum posted on retroShaA two days ago.
func retroPR(h *harness, n int, ago time.Duration) store.PR {
	h.t.Helper()
	repo, err := h.st.UpsertRepo(h.ctx, store.Repo{NodeID: "R_talkable", Owner: "talkable", Name: "talkable", WatchOwner: "talkable", Mode: store.RepoModePool})
	if err != nil {
		h.t.Fatal(err)
	}
	closed := h.clock.Now().Add(-ago)
	up, err := h.st.UpsertPRFromGitHub(h.ctx, store.GitHubPR{
		RepoID: repo.ID, NodeID: "PR_" + strconv.Itoa(n), Number: n, URL: "https://github.com/talkable/talkable/pull/" + strconv.Itoa(n),
		HeadSHA: retroShaB, AuthorLogin: store.Ptr("alice"), AuthorType: store.Ptr("User"),
		GHState: store.GHMerged, MergedAt: &closed, ClosedAt: &closed, InitialState: store.PRReviewed, Identity: "talkable-app",
	})
	if err != nil {
		h.t.Fatal(err)
	}
	run, err := h.st.CreateRun(h.ctx, store.Run{ID: "r-" + strconv.Itoa(n), PRID: up.PR.ID, Round: 1, Role: "codex-judge", Kind: store.RunInitial,
		TargetSHA: retroShaA, Identity: "talkable-app", ReviewerLogin: "talkable[bot]", State: store.RunPending})
	if err != nil {
		h.t.Fatal(err)
	}
	posted := h.clock.Now().Add(-48 * time.Hour)
	if err := h.st.UpdateRun(h.ctx, run.ID, func(u *store.RunUpdate) {
		u.Set("review_id", int64(9000+n))
		u.Set("review_commit", retroShaA)
		u.Set("verified_at", posted)
	}); err != nil {
		h.t.Fatal(err)
	}
	return up.PR
}

// retroThread is a thread rev-ann (or who) started on sha at path:line.
func retroThread(id int64, who, path string, line int, sha string) github.Thread {
	return github.Thread{Path: path, Line: line, OriginalLine: line, Comments: []github.ThreadComment{{
		ID: id, AuthorLogin: who, AuthorType: "User", Body: retroBody, OriginalCommitOid: sha, DiffHunk: "@@ -1,3 +1,3 @@",
		URL: "https://github.com/talkable/talkable/pull/7#discussion_r" + itoa(id), CreatedAt: time.Date(2026, 10, 4, 9, 0, 0, 0, time.Local),
	}}}
}

// seedRetroGitHub gives PR n: a thread on the reviewed commit (t101), one
// on a later commit whose file changed (t102, outside), one on a later
// commit whose file did not (t103), the author's own (t104), one next to a
// finding magnum posted (t105), and a review body on the reviewed commit
// (r201).
func seedRetroGitHub(h *harness, n int) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	if h.gh.threads == nil {
		h.gh.threads, h.gh.contents, h.gh.allReviews, h.gh.files = map[int][]github.Thread{}, map[string][]byte{}, map[int][]github.Review{}, map[string][]github.FileDelta{}
	}
	h.gh.threads[n] = []github.Thread{
		retroThread(101, "rev-ann", "app/models/coupon.rb", 42, retroShaA),
		retroThread(102, "rev-ann", "app/models/moved.rb", 5, retroShaB),
		retroThread(103, "bob-rev", "app/models/same.rb", 9, retroShaB),
		retroThread(104, "alice", "app/models/coupon.rb", 10, retroShaA),
		retroThread(105, "rev-ann", "app/models/coupon.rb", 80, retroShaA),
	}
	for i := range h.gh.threads[n] { // comments of another PR have other URLs
		c := &h.gh.threads[n][i].Comments[0]
		c.URL = strings.Replace(c.URL, "/pull/7#", "/pull/"+strconv.Itoa(n)+"#", 1)
	}
	h.gh.allReviews[n] = []github.Review{{DatabaseID: 201, State: "COMMENTED", Body: retroBody, CommitOid: retroShaA,
		AuthorLogin: "rev-ann", AuthorType: "User", SubmittedAt: time.Date(2026, 10, 4, 9, 0, 0, 0, time.Local),
		URL: "https://github.com/talkable/talkable/pull/" + strconv.Itoa(n) + "#pullrequestreview-201"}}
	h.gh.files[retroShaA+"..."+retroShaB] = []github.FileDelta{{Path: "app/models/moved.rb", Status: "modified"}}
	h.gh.contents["app/models/coupon.rb@"+retroShaA] = []byte("class Coupon\nend\n")
	h.gh.contents["app/models/same.rb@"+retroShaA] = []byte("class Same\nend\n")
}

// retroFinding records the judge's posted finding next to t105.
func retroFinding(h *harness, pr store.PR) {
	h.t.Helper()
	if err := h.st.RecordFindings(h.ctx, "r-"+strconv.Itoa(pr.Number), pr.ID, 1, []store.Finding{
		{FindingID: "F1", Severity: "P2", Path: "app/models/coupon.rb", Line: 81, Verdict: store.FindingPosted},
	}); err != nil {
		h.t.Fatal(err)
	}
}

// requestRetro queues a `magnum retro` request and runs a tick (the retro
// runs to its end in settle).
func (h *harness) requestRetro(p RetroPayload) store.Request {
	h.t.Helper()
	id, err := h.st.EnqueueRequest(h.ctx, ReqRetro, p)
	if err != nil {
		h.t.Fatal(err)
	}
	h.tick()
	req, err := h.st.RequestByID(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return req
}

func (h *harness) misses(prID int64) map[string]store.Miss {
	h.t.Helper()
	ms, err := h.st.Misses(h.ctx, store.MissFilter{PRID: prID})
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]store.Miss{}
	for _, m := range ms {
		out[m.SourceURL[strings.LastIndexAny(m.SourceURL, "r-")+1:]] = m
	}
	return out
}

func (h *harness) retroLast() RetroSummary {
	h.t.Helper()
	v, ok, err := h.st.GetKV(h.ctx, KVRetroLast)
	if err != nil || !ok {
		h.t.Fatalf("no %s: %v", KVRetroLast, err)
	}
	var s RetroSummary
	if err := json.Unmarshal([]byte(v), &s); err != nil {
		h.t.Fatal(err)
	}
	return s
}

// noCommentText fails when an event of subject carries a comment's text.
func (h *harness) noCommentText(subject string) []store.Event {
	h.t.Helper()
	evs, err := h.st.EventsBySubject(h.ctx, subject, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, ev := range evs {
		if strings.Contains(ev.Message, "SECRET-BODY") || strings.Contains(string(ev.Data), "SECRET-BODY") {
			h.t.Fatalf("event %s carries comment text: %s %s", ev.Kind, ev.Message, ev.Data)
		}
	}
	return evs
}

func eventKinds(evs []store.Event) []string {
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Kind)
	}
	return out
}

// TestRetroStoresCandidatesUnclassifiedWithoutAClassifier: a forced retro
// with no classifier reads the threads and reviews, keeps other reviewers'
// comments on what magnum reviewed (moved to the reviewed commit when the
// file did not change), stores a comment on a changed file as outside,
// drops the author's and the one magnum caught, writes the PR's directory
// with the commented files, and records the PR as unclassified.
func TestRetroStoresCandidatesUnclassifiedWithoutAClassifier(t *testing.T) {
	h := newHarness(t)
	pr := retroPR(h, 7, 24*time.Hour)
	retroFinding(h, pr)
	seedRetroGitHub(h, 7)

	req := h.requestRetro(RetroPayload{})
	if req.State != store.RequestDone || !strings.Contains(deref(req.Result), "retro started") {
		t.Fatalf("request = %s %q", req.State, deref(req.Result))
	}
	ms := h.misses(pr.ID)
	if len(ms) != 4 {
		t.Fatalf("misses = %v, want t101, t102, t103 and r201", slices.Sorted(maps.Keys(ms)))
	}
	for id, class := range map[string]string{"101": store.MissUnclassified, "103": store.MissUnclassified, "201": store.MissUnclassified, "102": store.MissOutside} {
		if ms[id].Class != class {
			t.Errorf("%s: class %s, want %s", id, ms[id].Class, class)
		}
	}
	if m := ms["103"]; m.ReviewedSHA != retroShaA || m.Reviewer != "bob-rev" || m.Line != 9 {
		t.Errorf("t103 moves to the reviewed commit: %+v", m)
	}
	if m := ms["201"]; m.SourceKind != store.MissSourceReview || m.Path != "" {
		t.Errorf("r201: %+v", m)
	}
	rp, err := h.st.RetroPRByID(h.ctx, pr.ID)
	if err != nil || rp.Status != store.RetroUnclassified || rp.Candidates != 3 {
		t.Fatalf("retro_prs = %+v, %v", rp, err)
	}
	sum := h.retroLast()
	if sum.PRs != 1 || sum.Caught != 1 || sum.Classified != 0 || sum.Failed != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	dir := filepath.Join(h.layout.Learn(), "retro", sum.Run, "talkable", "talkable", "7")
	cands, err := learn.ReadCandidates(filepath.Join(dir, learn.CandidatesFile))
	if err != nil || len(cands.Candidates) != 3 || cands.PR != pr.URL || !slices.Equal(cands.ReviewedSHAs, []string{retroShaA}) {
		t.Fatalf("candidates file = %+v, %v", cands, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, learn.FilePath(retroShaA, "app/models/same.rb"))); err != nil || string(b) != "class Same\nend\n" {
		t.Fatalf("prefetched file: %q, %v", b, err)
	}
	if c := cands.Candidates[0]; c.File != learn.FilePath(retroShaA, "app/models/coupon.rb") || c.FileSkipped != "" {
		t.Fatalf("candidate file = %+v", c)
	}
	if kinds := eventKinds(h.noCommentText("pr:talkable/talkable#7")); !slices.Equal(kinds, []string{"retro.begin", "retro.ok"}) {
		t.Fatalf("PR events = %v", kinds)
	}
	// Done once: a second retro skips the PR unless asked again.
	reads := h.gh.count("threads:")
	h.requestRetro(RetroPayload{})
	if h.gh.count("threads:") != reads {
		t.Fatal("a second retro read the PR again")
	}
	h.requestRetro(RetroPayload{PRs: []int64{pr.ID}, Again: true})
	if h.gh.count("threads:") != reads+1 {
		t.Fatal("--again did not read the PR again")
	}
}

// fakeClassifier answers with answer (nil = a valid answer: t101 a miss,
// every other candidate style) and records its jobs.
type fakeClassifier struct {
	mu     sync.Mutex
	jobs   []ClassifyJob
	runs   []RetroRun
	closed int
	answer func(job ClassifyJob) (ClassifyResult, error)
}

func (f *fakeClassifier) Classify(_ context.Context, job ClassifyJob) (ClassifyResult, error) {
	f.mu.Lock()
	f.jobs = append(f.jobs, job)
	answer := f.answer
	f.mu.Unlock()
	if answer != nil {
		return answer(job)
	}
	var out learn.Output
	for _, c := range job.Candidates {
		it := learn.Item{ID: c.ID, Class: store.MissStyle}
		if c.ID == "t101" {
			it = learn.Item{ID: c.ID, Class: store.MissMiss, Severity: "P1", Title: "Coupon lookup ignores the site",
				Lesson: "When a finder takes a code from the request, check it is scoped to the current tenant.",
				Scope:  store.MissScopeGeneral, Lines: []int{40, 42}, Match: []string{"tenant", "scope"}}
		}
		out.Items = append(out.Items, it)
	}
	b, _ := json.Marshal(out)
	return ClassifyResult{}, os.WriteFile(job.OutputPath, b, 0o600)
}

func (f *fakeClassifier) Close(context.Context) error {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
	return nil
}

func withClassifier(f *fakeClassifier) func(*harness) {
	return func(h *harness) {
		h.d.Classifier = func(_ context.Context, run RetroRun) (Classifier, error) {
			f.mu.Lock()
			f.runs = append(f.runs, run)
			f.mu.Unlock()
			return f, nil
		}
	}
}

// TestRetroStoresTheClassifiersAnswer: the classifier's answer becomes the
// candidates' classes; a miss keeps its severity, title, lesson, scope,
// lines and match, a lesson that names a reviewer is dropped (and said so
// in the event), and the classifier serves the whole retro and is closed
// once.
func TestRetroStoresTheClassifiersAnswer(t *testing.T) {
	fc := &fakeClassifier{}
	h := newHarness(t, withClassifier(fc))
	pr := retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	base := fc.answer
	fc.answer = func(job ClassifyJob) (ClassifyResult, error) {
		res, err := (&fakeClassifier{answer: base}).Classify(context.Background(), job)
		if err != nil {
			return res, err
		}
		b, _ := os.ReadFile(job.OutputPath)
		var out learn.Output
		_ = json.Unmarshal(b, &out)
		for i := range out.Items {
			if out.Items[i].ID == "t103" {
				out.Items[i] = learn.Item{ID: "t103", Class: store.MissMiss, Severity: "P2", Title: "Cache key misses the tenant",
					Lesson: "As rev-ann said, add the tenant to the cache key.", Scope: store.MissScopeRepo, Lines: []int{9, 9}, Match: []string{"cache"}}
			}
		}
		b, _ = json.Marshal(out)
		return ClassifyResult{}, os.WriteFile(job.OutputPath, b, 0o600)
	}
	h.requestRetro(RetroPayload{})

	ms := h.misses(pr.ID)
	if m := ms["101"]; m.Class != store.MissMiss || m.Severity != "P1" || m.Title == "" || m.Lesson == "" ||
		m.Scope != store.MissScopeGeneral || !slices.Equal(m.Lines, []int{40, 42}) || !slices.Equal(m.Match, []string{"tenant", "scope"}) || m.State != store.MissNew {
		t.Fatalf("t101 = %+v", m)
	}
	if m := ms["103"]; m.Class != store.MissMiss || m.Lesson != "" || m.Title == "" {
		t.Fatalf("t103 keeps its title, loses its lesson: %+v", m)
	}
	if ms["201"].Class != store.MissStyle || ms["102"].Class != store.MissOutside {
		t.Fatalf("r201 %s, t102 %s", ms["201"].Class, ms["102"].Class)
	}
	if rp, _ := h.st.RetroPRByID(h.ctx, pr.ID); rp.Status != store.RetroClassified {
		t.Fatalf("retro_prs = %+v", rp)
	}
	if sum := h.retroLast(); sum.Classified != 1 || sum.Misses != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	evs := h.noCommentText("pr:talkable/talkable#7")
	ok := evs[len(evs)-1]
	if ok.Kind != "retro.ok" || !strings.Contains(ok.Message, "lesson_rejected") || !strings.Contains(string(ok.Data), `"lesson_rejected":["t103"]`) {
		t.Fatalf("last event = %s %s %s", ok.Kind, ok.Message, ok.Data)
	}
	var dropped []store.Event
	for _, ev := range evs {
		if ev.Kind == "retro.lesson_rejected" {
			dropped = append(dropped, ev)
		}
	}
	if len(dropped) != 1 || dropped[0].Level != "info" || !strings.Contains(string(dropped[0].Data), `"reason":"login"`) ||
		!strings.Contains(string(dropped[0].Data), `"candidate":"t103"`) || strings.Contains(dropped[0].Message+string(dropped[0].Data), "cache key") {
		t.Fatalf("lesson_rejected events = %+v", dropped)
	}
	if len(fc.runs) != 1 || fc.closed != 1 || len(fc.jobs) != 1 {
		t.Fatalf("classifier runs %d, closed %d, jobs %d", len(fc.runs), fc.closed, len(fc.jobs))
	}
	if j := fc.jobs[0]; j.CandidatesPath != filepath.Join(j.Dir, learn.CandidatesFile) || !strings.HasPrefix(j.Dir, fc.runs[0].Dir) {
		t.Fatalf("job = %+v", j)
	}
}

// TestRetroInvalidAnswerFailsOnlyThatPR: an answer off schema fails its
// PR (stored unclassified, the error in retro_prs) and the retro goes on
// with the next PR.
func TestRetroInvalidAnswerFailsOnlyThatPR(t *testing.T) {
	fc := &fakeClassifier{answer: func(job ClassifyJob) (ClassifyResult, error) {
		return ClassifyResult{}, os.WriteFile(job.OutputPath, []byte(`{"items":[{"id":"t101","class":"bug"}]}`), 0o600)
	}}
	h := newHarness(t, withClassifier(fc))
	pr7 := retroPR(h, 7, 24*time.Hour)
	pr8 := retroPR(h, 8, 48*time.Hour)
	seedRetroGitHub(h, 7)
	seedRetroGitHub(h, 8)
	h.requestRetro(RetroPayload{})

	for _, pr := range []store.PR{pr7, pr8} {
		rp, err := h.st.RetroPRByID(h.ctx, pr.ID)
		if err != nil || rp.Status != store.RetroFailed || !strings.Contains(rp.Error, "retro.json is invalid") {
			t.Fatalf("PR %d retro_prs = %+v, %v", pr.Number, rp, err)
		}
		if m := h.misses(pr.ID)["101"]; m.Class != store.MissUnclassified {
			t.Fatalf("PR %d t101 = %+v", pr.Number, m)
		}
	}
	if sum := h.retroLast(); sum.PRs != 2 || sum.Failed != 2 {
		t.Fatalf("summary = %+v", sum)
	}
}

// TestRetroStopsAtAUsageLimit: a classifier that hits a usage limit pauses
// its tool as a round would and stops the retro; the PR it was on gets no
// retro_prs row (the limit is not its fault), so it stays due with the rest
// (TestRetroLeavesThePRDueWhenTheRetroIsCutShort covers the other stops).
func TestRetroStopsAtAUsageLimit(t *testing.T) {
	fc := &fakeClassifier{}
	h := newHarness(t, withClassifier(fc))
	fc.answer = func(ClassifyJob) (ClassifyResult, error) {
		return ClassifyResult{Pause: &pipeline.Pause{Kind: "usage_limit", Tool: "claude", Detail: "limit reached", Until: h.clock.Now().Add(time.Hour)}},
			errors.New("usage limit")
	}
	pr7 := retroPR(h, 7, 24*time.Hour)
	pr8 := retroPR(h, 8, 48*time.Hour)
	seedRetroGitHub(h, 7)
	seedRetroGitHub(h, 8)
	h.requestRetro(RetroPayload{})

	for _, pr := range []store.PR{pr7, pr8} {
		if rp, err := h.st.RetroPRByID(h.ctx, pr.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("PR %d was recorded: %+v, %v", pr.Number, rp, err)
		}
	}
	if len(fc.jobs) != 1 {
		t.Fatalf("classified %d PRs, want the retro to stop at the first", len(fc.jobs))
	}
	if p, ok := h.e.toolPause(h.ctx, "claude"); !ok || p.Reason != "usage_limit" {
		t.Fatalf("claude pause = %+v, %v", p, ok)
	}
	if sum := h.retroLast(); sum.Stopped == "" || sum.PRs != 0 || sum.Failed != 0 {
		t.Fatalf("summary = %+v", sum)
	}
}

// TestRetroHonoursMaxPRs: only max_prs PRs with candidates are classified
// per retro; the rest wait for the next one.
func TestRetroHonoursMaxPRs(t *testing.T) {
	fc := &fakeClassifier{}
	h := newHarness(t, withClassifier(fc), func(h *harness) { h.cfg.Learn.MaxPRs = 1 })
	retroPR(h, 7, 24*time.Hour)
	pr8 := retroPR(h, 8, 48*time.Hour)
	seedRetroGitHub(h, 7)
	seedRetroGitHub(h, 8)
	h.requestRetro(RetroPayload{})
	if len(fc.jobs) != 1 || fc.jobs[0].PR.Number != 7 {
		t.Fatalf("jobs = %d, want PR 7 only (newest closed first)", len(fc.jobs))
	}
	if _, err := h.st.RetroPRByID(h.ctx, pr8.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("PR 8: %v", err)
	}
}

// TestDailyRetroRunsOncePerDayAfterDailyAt: with [learn] enabled the
// retro runs on the first tick past daily_at, once per local day, never
// while magnum is paused or draining, and never when disabled.
func TestDailyRetroRunsOncePerDayAfterDailyAt(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.cfg.Learn.Enabled = true
		h.cfg.Learn.DailyAt = "11:00"
	})
	retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	ran := func() bool {
		_, ok, _ := h.st.GetKV(h.ctx, KVRetroLast)
		_ = h.st.DeleteKV(h.ctx, KVRetroLast)
		return ok
	}
	h.tick()
	if ran() {
		t.Fatal("the retro ran before daily_at")
	}
	h.advance(90 * time.Minute)
	if err := h.st.SetKV(h.ctx, KVDaemonPaused, "1"); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if ran() {
		t.Fatal("the retro ran while magnum was paused")
	}
	_ = h.st.DeleteKV(h.ctx, KVDaemonPaused)
	_ = h.st.SetKV(h.ctx, KVDaemonDraining, store.FormatTime(h.clock.Now()))
	h.tick()
	if ran() {
		t.Fatal("the retro ran while draining")
	}
	_ = h.st.DeleteKV(h.ctx, KVDaemonDraining)
	h.tick()
	if !ran() {
		t.Fatal("the retro did not run after daily_at")
	}
	if day, _, _ := h.st.GetKV(h.ctx, KVRetroDay); day != store.DayKey(h.clock.Now()) {
		t.Fatalf("retro day = %q", day)
	}
	h.tick()
	if ran() {
		t.Fatal("the retro ran twice in a day")
	}
	h.advance(24 * time.Hour)
	h.tick()
	if !ran() {
		t.Fatal("the retro did not run the next day")
	}
	h.cfg.Learn.Enabled = false
	h.advance(24 * time.Hour)
	h.tick()
	if ran() {
		t.Fatal("a disabled schedule ran")
	}
}

// TestRetroRequestRefusedWhileDrainingOrRunning: `magnum retro` passes a
// user pause but not a drain, and a second one while a retro runs only
// says so.
func TestRetroRequestRefusedWhileDrainingOrRunning(t *testing.T) {
	gate := make(chan struct{})
	fc := &fakeClassifier{}
	h := newHarness(t, withClassifier(fc))
	fc.answer = func(job ClassifyJob) (ClassifyResult, error) {
		<-gate
		return ClassifyResult{Unclassified: true}, nil
	}
	retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)

	_ = h.st.SetKV(h.ctx, KVDaemonDraining, store.FormatTime(h.clock.Now()))
	if req := h.requestRetro(RetroPayload{}); req.State != store.RequestFailed || !strings.Contains(deref(req.Result), "draining") {
		t.Fatalf("while draining: %s %q", req.State, deref(req.Result))
	}
	_ = h.st.DeleteKV(h.ctx, KVDaemonDraining)
	_ = h.st.SetKV(h.ctx, KVDaemonPaused, "1")

	first, _ := h.st.EnqueueRequest(h.ctx, ReqRetro, RetroPayload{})
	second, _ := h.st.EnqueueRequest(h.ctx, ReqRetro, RetroPayload{})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]string{first: "retro started", second: "already running"} {
		req, _ := h.st.RequestByID(h.ctx, id)
		if req.State != store.RequestDone || !strings.Contains(deref(req.Result), want) {
			t.Errorf("request %d = %s %q, want %q", id, req.State, deref(req.Result), want)
		}
	}
	close(gate)
	h.settle()
	if rp, err := h.st.RetroPRByID(h.ctx, h.pr(7).ID); err != nil || rp.Status != store.RetroUnclassified {
		t.Fatalf("retro_prs = %+v, %v", rp, err)
	}
	if req := h.requestRetro(RetroPayload{Lookback: "fortnight"}); req.State != store.RequestFailed {
		t.Fatalf("a bad lookback: %s %q", req.State, deref(req.Result))
	}
}

// TestRetroKeepsCommentedFilesInsideItsDirectory: a commented path that
// would climb out of the PR's retro directory is never fetched or written
// (GitHub's client refuses ".." like the fake); the candidate says why it
// has no copy, and no escape.rb exists anywhere outside the PR directory.
func TestRetroKeepsCommentedFilesInsideItsDirectory(t *testing.T) {
	h := newHarness(t)
	retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	h.gh.mu.Lock()
	h.gh.threads[7] = nil
	for i, p := range []string{"../escape.rb", "../../escape.rb", "../../../../../../escape.rb", "app/../../escape.rb"} {
		h.gh.threads[7] = append(h.gh.threads[7], retroThread(int64(101+i), "rev-ann", p, 3, retroShaA))
		h.gh.contents[p+"@"+retroShaA] = []byte("x")
	}
	h.gh.allReviews[7] = nil
	h.gh.mu.Unlock()
	h.requestRetro(RetroPayload{})

	sum := h.retroLast()
	prDir := filepath.Join(h.layout.Learn(), "retro", sum.Run, "talkable", "talkable", "7")
	cands, err := learn.ReadCandidates(filepath.Join(prDir, learn.CandidatesFile))
	if err != nil || len(cands.Candidates) != 4 {
		t.Fatalf("candidates = %+v, %v", cands, err)
	}
	for _, c := range cands.Candidates {
		if c.File != "" || c.FileSkipped == "" {
			t.Errorf("%s (%s): file %q, skipped %q", c.ID, c.Path, c.File, c.FileSkipped)
		}
	}
	err = filepath.WalkDir(h.layout.Home, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "escape.rb" && !strings.HasPrefix(p, prDir+string(filepath.Separator)) {
			t.Errorf("escape.rb written outside the PR directory: %s", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPruneRetroRemovesRunsOlderThan30Days: the reconcile's prune removes
// retro run directories past retroKeep and leaves newer runs and anything
// not named like a run.
func TestPruneRetroRemovesRunsOlderThan30Days(t *testing.T) {
	h := newHarness(t)
	root := filepath.Join(h.layout.Learn(), "retro")
	now := h.clock.Now()
	old, recent := now.Add(-31*24*time.Hour).Format(retroRunFormat), now.Add(-29*24*time.Hour).Format(retroRunFormat)
	for _, d := range []string{old, recent, "notes"} {
		if err := os.MkdirAll(filepath.Join(root, d, "talkable"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.e.prune(h.ctx); err != nil {
		t.Fatal(err)
	}
	for d, keep := range map[string]bool{old: false, recent: true, "notes": true} {
		if _, err := os.Stat(filepath.Join(root, d)); (err == nil) != keep {
			t.Errorf("%s: kept = %v, want %v", d, err == nil, keep)
		}
	}
}

// TestRetroWaitsWhileTheAgentsCLIIsPaused: with a classifier set up, a
// usage-limit pause of its CLI holds the daily retro and refuses a forced
// one, as rounds that need the CLI wait; without a classifier nothing needs
// the CLI.
func TestRetroWaitsWhileTheAgentsCLIIsPaused(t *testing.T) {
	fc := &fakeClassifier{}
	h := newHarness(t, withClassifier(fc), func(h *harness) { h.cfg.Learn.Enabled = true })
	retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	h.e.setToolPause(h.ctx, "claude", "usage_limit", "limit reached", h.clock.Now().Add(time.Hour))

	h.tick()
	if _, ok, _ := h.st.GetKV(h.ctx, KVRetroLast); ok {
		t.Fatal("the daily retro ran while its CLI was paused")
	}
	if req := h.requestRetro(RetroPayload{}); req.State != store.RequestFailed || !strings.Contains(deref(req.Result), "claude paused") {
		t.Fatalf("forced retro = %s %q", req.State, deref(req.Result))
	}
	h.advance(2 * time.Hour)
	h.tick()
	if _, ok, _ := h.st.GetKV(h.ctx, KVRetroLast); !ok {
		t.Fatal("the daily retro did not run once the pause ended")
	}

	plain := newHarness(t)
	plain.e.setToolPause(plain.ctx, "claude", "usage_limit", "limit reached", plain.clock.Now().Add(time.Hour))
	if req := plain.requestRetro(RetroPayload{}); req.State != store.RequestDone {
		t.Fatalf("without a classifier: %s %q", req.State, deref(req.Result))
	}
}
