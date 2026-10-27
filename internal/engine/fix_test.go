package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	// fixHead is the head magnum reviewed, which the fixer starts from.
	fixHead = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	// fixPushed is the head the fixer pushes.
	fixPushed = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	// fixReviewURL is the review the fixer fixes.
	fixReviewURL = "https://github.com/talkable/talkable/pull/2#pullrequestreview-101"
	// fixReplied is a report's "## Replied" line.
	fixReplied = "3 threads (2 fixed, 0 already fixed, 1 not a bug, 0 won't fix)"
)

// fixBaseTip is the tip of the PR's base branch the fix job fetches.
const fixBaseTip = "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0"

// fixGit is the engine's git for the fix job's tests: HEAD in the slot is
// head (the reviewed head until the fake fixer commits), origin/<base> is
// fixBaseTip, every other ref fakeGit's. lostPush makes the pushes the fake
// fixer reports never reach GitHub; LsRemote answers remote (lsErr fails
// it) and records its calls. FetchBranch fails with fetchErr and records
// its calls ("<main clone>:<base>"); MergeConflicts answers conflicts (nil:
// a clean merge) and records its calls ("<dir>:<head>:<base>").
type fixGit struct {
	fakeGit
	mu        sync.Mutex
	head      string
	lostPush  bool
	remote    string
	lsErr     error
	lsCalls   []string
	fetchErr  error
	fetches   []string
	conflicts []string
	merges    []string
}

func (g *fixGit) FetchBranch(_ context.Context, mainClone, base string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fetches = append(g.fetches, mainClone+":"+base)
	return g.fetchErr
}

func (g *fixGit) MergeConflicts(_ context.Context, dir, head, base string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.merges = append(g.merges, dir+":"+head+":"+base)
	return slices.Clone(g.conflicts), nil
}

// conflict makes the head conflict with the base in paths (none: a clean
// merge).
func (g *fixGit) conflict(paths ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.conflicts = paths
}

// calls are the fetches and merge checks so far.
func (g *fixGit) calls() (fetches, merges []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.fetches), slices.Clone(g.merges)
}

// LsRemote answers `git ls-remote origin patterns...` in dir.
func (g *fixGit) LsRemote(_ context.Context, dir string, _ time.Duration, patterns ...string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lsCalls = append(g.lsCalls, dir+":"+strings.Join(patterns, " "))
	return g.remote, g.lsErr
}

func (g *fixGit) pushLost() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lostPush
}

func (g *fixGit) setHead(sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.head = sha
}

func (g *fixGit) RevParse(ctx context.Context, dir, ref string) (string, error) {
	g.mu.Lock()
	head := g.head
	g.mu.Unlock()
	switch {
	case ref == "HEAD" && head != "":
		return head, nil
	case strings.HasPrefix(ref, "refs/remotes/origin/"):
		return fixBaseTip, nil
	}
	return g.fakeGit.RevParse(ctx, dir, ref)
}

// fixHarness is a harness whose git knows the slot's HEAD. Its watch is
// shaped as `magnum init` writes one, posting as the App and polling as the
// operator's gh login, which the built-in claude-fix, naming no identity,
// acts as.
func fixHarness(t *testing.T, mods ...func(*harness)) (*harness, *fixGit) {
	t.Helper()
	git := &fixGit{head: fixHead}
	h := newHarness(t, append([]func(*harness){func(h *harness) { h.d.Git = git }}, mods...)...)
	return h, git
}

// fixRole is the configured claude-fix role of h, to edit.
func (h *harness) fixRole() *config.Role {
	h.t.Helper()
	for i := range h.cfg.Roles {
		if h.cfg.Roles[i].Fix {
			return &h.cfg.Roles[i]
		}
	}
	h.t.Fatal("no fix role declared")
	return nil
}

// realSleep makes the engine's waits take a real millisecond each: the fix
// job's wait then polls its run until the fake fixer ends it, or the test
// stops the job, without spinning.
func realSleep(h *harness) {
	h.d.Sleep = func(ctx context.Context, _ time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
			return nil
		}
	}
}

// fixablePR opens PR #2 by author at fixHead, has it reviewed and, with
// findings (nil = none recorded), records the posted review's judge run with
// those findings: the state `magnum fix` starts from.
func fixablePR(h *harness, author string, findings map[string]int) store.PR {
	h.t.Helper()
	return fixablePROpen(h, author, findings, nil)
}

// fixablePROpen is fixablePR whose review, with findings, also keeps the
// earlier findings open (previous_findings.open by priority; nil = no
// previous_findings in its result).
func fixablePROpen(h *harness, author string, findings, open map[string]int) store.PR {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: author})
	h.tick() // #2 queued
	h.advance(5 * time.Minute)
	h.tick() // dispatched and reviewed
	pr := h.wantState(2, store.PRReviewed)
	if findings == nil {
		return pr
	}
	body := map[string]any{"event": "COMMENTED", "findings": findings}
	if open != nil {
		body["previous_findings"] = map[string]any{"fixed": 1, "open": open, "answered": 0}
	}
	result, err := json.Marshal(body)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.st.DB().ExecContext(h.ctx, `INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login,
  state, outcome, review_id, review_url, prompt_text, result_json, created_at)
  VALUES ('run-review-2', ?, 1, 'codex-judge', 'initial', ?, ?, 'talkable[bot]', 'verified', 'posted', 101, ?, 'p', ?, ?)`,
		pr.ID, pr.HeadSHA, pr.Identity, fixReviewURL, string(result), store.FormatTime(h.clock.Now())); err != nil {
		h.t.Fatal(err)
	}
	return pr
}

// fixReport is a fixer's report of run runID.
func fixReport(runID, pushed, replied, needs string) string {
	return agents.ReportMarker(runID) + "\n## Pushed\n" + pushed + "\n## Replied\n" + replied + "\n## Needs you\n" + needs + "\n"
}

// fixer makes h's fake fixer: once its prompt is sent it writes report(run)
// to the run's report path ("" = it writes none), moves the slot's HEAD to
// head as its commits do ("" = it commits nothing), puts head on GitHub
// when the report lists pushed commits (its push; never with
// fixGit.lostPush) and ends its run, as the observer does after two idle
// ticks.
func fixer(h *harness, git *fixGit, head string, report func(run store.Run) string) {
	fixerBy(h, git, func(run store.Run) (string, string) { return head, report(run) })
}

// fixerBy is fixer with the head and the report of each run from work.
func fixerBy(h *harness, git *fixGit, work func(run store.Run) (head, report string)) {
	h.ag.mu.Lock()
	defer h.ag.mu.Unlock()
	h.ag.onSubmit = func(run store.Run, _ string) {
		head, text := work(run)
		if text != "" {
			path := deref(run.ReportPath)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				h.t.Error(err)
				return
			}
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				h.t.Error(err)
				return
			}
		}
		if head != "" {
			git.setHead(head)
			if rep, err := ParseFixReport(text, run.ID); err == nil && len(rep.Pushed) > 0 && !rep.PushFailed && !git.pushLost() {
				if pr, err := h.st.PRByID(h.ctx, run.PRID); err == nil {
					h.gh.setHead("talkable/talkable", pr.Number, head)
				}
			}
		}
		if err := h.st.TransitionRun(h.ctx, run.ID, []string{store.RunWorking}, store.RunEnded, func(u *store.RunUpdate) {
			u.Set("ended_at", h.clock.Now())
		}); err != nil {
			h.t.Error(err)
		}
	}
}

// fixRuns are the PR's runs of kind fix, oldest first.
func (h *harness) fixRuns(prID int64) []store.Run {
	h.t.Helper()
	runs, err := h.st.RunsByPR(h.ctx, prID)
	if err != nil {
		h.t.Fatal(err)
	}
	return slices.DeleteFunc(runs, func(r store.Run) bool { return r.Kind != store.RunFix })
}

// fix asks for a fix loop on PR #2 and returns the request.
func (h *harness) fix(p FixPayload) int64 {
	h.t.Helper()
	p.PRTarget = PRTarget{Ref: "2"}
	return h.enqueue(ReqFix, p)
}

// wantDone fails unless request id is done and returns its answer.
func (h *harness) wantDone(id int64) string {
	h.t.Helper()
	r := h.request(id)
	if r.State != store.RequestDone {
		h.t.Fatalf("request %d: %s %q", id, r.State, deref(r.Result))
	}
	return deref(r.Result)
}

// fixStateOf is PR #n's fix loop (ok false: none).
func (h *harness) fixStateOf(n int) (FixState, bool) {
	h.t.Helper()
	st, ok, err := h.e.fixState(h.ctx, h.pr(n).ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return st, ok
}

// fixEvent is PR #2's newest event of kind (ok false: none).
func (h *harness) fixEvent(kind string) (store.Event, bool) {
	h.t.Helper()
	evs, err := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#2", 0)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, ev := range slices.Backward(evs) {
		if ev.Kind == kind {
			return ev, true
		}
	}
	return store.Event{}, false
}

// wantReportInData fails unless PR #2's event kind names report in its data.
func (h *harness) wantReportInData(kind, report string) {
	h.t.Helper()
	ev, ok := h.fixEvent(kind)
	if !ok {
		h.t.Fatalf("no %s event", kind)
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil || data["report"] != report {
		h.t.Errorf("%s data = %s, want report %s", kind, ev.Data, report)
	}
}

// awaitSubmit waits until the fix job sent its prompt.
func (h *harness) awaitSubmit() {
	h.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); h.ag.count("submit:claude-fix:") == 0; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			h.t.Fatalf("the fixer's prompt was never sent: %v", h.ag.all())
		}
	}
}

// A `magnum fix` on the operator's reviewed PR starts the fix job: the loop's
// row says fixing, cycle 1 of 3, the fixer's run of kind fix is submitted in
// the review's round with the prompt's facts, the fixer's pane acts as the
// operator (not as the posting App), dispatch holds a push to the PR with
// WaitFixing, and `magnum review` and a second `magnum fix` are refused.
func TestAFixRequestStartsTheJobAndHoldsThePR(t *testing.T) {
	h, git := fixHarness(t, realSleep)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	slot := h.slot("review1")

	id := h.fix(FixPayload{})
	h.e.handleRequests(h.ctx) // the job runs on: the fixer works
	if res := h.wantDone(id); !strings.Contains(res, "cycle 1 of 3") || !strings.Contains(res, "claude-fix") {
		t.Errorf("answer = %q", res)
	}
	h.awaitSubmit()

	st, ok := h.fixStateOf(2)
	if !ok || st.Cycle != 1 || st.Cap != 3 || st.Phase != FixPhaseFixing || st.By != "magnum fix" || st.Head != fixHead || st.Since != fixHead {
		t.Fatalf("fix state = %+v, %v", st, ok)
	}
	runs := h.fixRuns(pr.ID)
	if len(runs) != 1 {
		t.Fatalf("fix runs = %+v", runs)
	}
	run := runs[0]
	wantReport := filepath.Join(h.layout.ReviewDir("talkable", "talkable", 2, fixHead), "claude-fix.md")
	if run.Role != "claude-fix" || run.Round != 1 || run.State != store.RunWorking || deref(run.ReportPath) != wantReport || st.Report != wantReport {
		t.Errorf("fix run = %+v (report %s), state report %s", run, deref(run.ReportPath), st.Report)
	}
	reviewFile := filepath.Join(h.layout.ReviewDir("talkable", "talkable", 2, fixHead), "review.json")
	external := filepath.Join(h.layout.ReviewDir("talkable", "talkable", 2, fixHead), "external-threads.json")
	wantPrompt := fmt.Sprintf("fix_prompt:claude-fix:%s|feature|%s|%s|talkable[bot]|%s|%s", run.ID, fixReviewURL, reviewFile,
		agents.DBLockLine(h.layout.Binary(), slot.Path, "claude-fix"), external)
	if !slices.Contains(h.ag.all(), wantPrompt) {
		t.Errorf("no %q in %v", wantPrompt, h.ag.all())
	}
	if !h.lastWorkspaceHas(pr.ID, "claude-fix") {
		t.Error("the PR's workspace has no claude-fix pane")
	}
	if judge, fix := h.ag.paneEnv("codex-judge"), h.ag.paneEnv("claude-fix"); judge["GH_CONFIG_DIR"] != "/state/gh/talkable-app" || fix == nil {
		t.Errorf("pane envs: judge %v, fixer %v", judge, fix)
	} else if _, ok := fix["GH_CONFIG_DIR"]; ok || fix["WT_BRANCH"] != "review1" {
		t.Errorf("the fixer's pane env %v acts as the posting App, or lost the slot's env", fix)
	}
	if !h.hasEvent("pr:talkable/talkable#2", "fix.start") {
		t.Error("no fix.start event")
	}
	if got := deref(h.slot("review1").LastUsedAt); !got.Equal(h.clock.Now()) {
		t.Errorf("slot last_used_at = %v, want the fix's start %v", got, h.clock.Now())
	}

	// Someone pushes meanwhile: the re-review waits for the fixer.
	rounds := len(h.rd.all())
	h.advance(time.Hour)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "c3c3c3c", author: "zhuravel"})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.wantState(2, store.PRRereviewPending)
	if w := waitOf(t, h, 2); w.Reason != WaitFixing || !strings.Contains(w.Sentence("talkable/talkable#2", h.clock.Now()), "magnum abort") {
		t.Errorf("wait = %+v", w)
	}
	if got := len(h.rd.all()); got != rounds {
		t.Errorf("rounds = %d, want %d: a round started while the fixer works", got, rounds)
	}

	review := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	again := h.fix(FixPayload{})
	h.e.handleRequests(h.ctx)
	for _, id := range []int64{review, again} {
		if r := h.request(id); r.State != store.RequestFailed || !strings.Contains(deref(r.Result), "fixing, cycle 1 of 3; `magnum abort` stops it") {
			t.Errorf("request %d: %s %q", id, r.State, deref(r.Result))
		}
	}

	// The fixer ends without pushing: the loop ends and the PR is free.
	git.setHead(fixHead)
	if err := os.MkdirAll(filepath.Dir(wantReport), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wantReport, []byte(fixReport(run.ID, "nothing", fixReplied, "none")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.st.TransitionRun(h.ctx, run.ID, []string{store.RunWorking}, store.RunEnded, nil); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop's row outlived the job")
	}
}

// The request names why it will not start a loop.
func TestFixRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		author   string
		findings map[string]int
		mod      func(h *harness, pr store.PR)
		want     string
	}{
		{name: "no review", author: "zhuravel", want: "nothing to fix: no review on a1a1a1a"},
		{name: "found nothing", author: "zhuravel", findings: map[string]int{}, want: othersOnly},
		{name: "another author", author: "alice", findings: map[string]int{"P1": 1},
			want: "PR #2 is alice's, not yours; the fixer pushes and replies as you"},
		{name: "fork", author: "zhuravel", findings: map[string]int{"P1": 1}, mod: func(h *harness, pr store.PR) {
			if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("is_cross_repo", true) }); err != nil {
				h.t.Fatal(err)
			}
		}, want: "the head lives in a fork magnum cannot push to"},
		{name: "running", author: "zhuravel", findings: map[string]int{"P1": 1}, mod: func(h *harness, pr store.PR) {
			if err := h.e.setFixState(h.ctx, pr.ID, FixState{Cycle: 2, Cap: 3, Phase: FixPhaseReviewing}); err != nil {
				h.t.Fatal(err)
			}
		}, want: "fixing, cycle 2 of 3; `magnum abort` stops it"},
		{name: "no fix role", author: "zhuravel", findings: map[string]int{"P1": 1}, mod: func(h *harness, _ store.PR) {
			h.cfg.Watches[0].FixRole = new("")
		}, want: "no fix role for talkable/talkable"},
		{name: "an undeclared identity", author: "zhuravel", findings: map[string]int{"P1": 1}, mod: func(h *harness, _ store.PR) {
			h.fixRole().Identity = "me"
		}, want: "no identity me for the fix role"},
		{name: "a poll identity that is an App", author: "zhuravel", findings: map[string]int{"P1": 1}, mod: func(h *harness, _ store.PR) {
			h.cfg.Watches[0].PollIdentity = "talkable-app"
		}, want: "the fix role's identity talkable-app is an App; name a gh identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, git := fixHarness(t)
			pr := fixablePR(h, tc.author, tc.findings)
			if tc.mod != nil {
				tc.mod(h, pr)
			}
			id := h.fix(FixPayload{})
			h.handle()
			if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != tc.want {
				t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), tc.want)
			}
			if h.ag.count("submit:") != 0 {
				t.Error("a refused request started the fixer")
			}
			// A conflict with the base would be work (fix_merge_test.go):
			// the review that found nothing is refused once the PR merges
			// its base cleanly, and no other refusal looks at the base.
			fetches, merges := git.calls()
			if want := tc.want == othersOnly; (len(fetches) == 1 && len(merges) == 1) != want || len(fetches) > 1 || len(merges) > 1 {
				t.Errorf("fetches %v, merge checks %v; want one of each only for the review that found nothing", fetches, merges)
			}
		})
	}
}

// A re-review that posts nothing new but keeps an earlier P2 open says "Fix
// 1 problem before merging", and the fix skill fixes the threads its body
// names as still open: `magnum fix` (the board's f) starts on it, and the
// answer and the fix.start event say how many of the findings are open from
// earlier reviews. An earlier P3 left open is optional: alone it leaves
// nothing to fix.
func TestAFixStartsOnAReviewThatKeepsAnEarlierFindingOpen(t *testing.T) {
	const what = "fixes the 1 finding of the review on a1a1a1a, 1 of them open from earlier reviews, in review1"
	t.Run("an open P2", func(t *testing.T) {
		h, git := fixHarness(t)
		fixablePROpen(h, "zhuravel", map[string]int{"P0": 0, "P1": 0, "P2": 0, "P3": 0}, map[string]int{"P0": 0, "P1": 0, "P2": 1, "P3": 0})
		fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
		id := h.fix(FixPayload{})
		h.handle()
		if res := h.wantDone(id); !strings.Contains(res, "claude-fix "+what+", cycle 1 of 3") {
			t.Errorf("answer = %q, want it to say %q", res, what)
		}
		if ev, ok := h.fixEvent("fix.start"); !ok || !strings.Contains(ev.Message, what+" (run ") {
			t.Errorf("fix.start = %q, %v; want it to say %q", ev.Message, ok, what)
		}
		if n := len(h.fixRuns(h.pr(2).ID)); n != 1 {
			t.Errorf("fix runs = %d, want the fixer's", n)
		}
	})
	t.Run("only an open P3", func(t *testing.T) {
		h, _ := fixHarness(t)
		fixablePROpen(h, "zhuravel", map[string]int{}, map[string]int{"P3": 1})
		id := h.fix(FixPayload{})
		h.handle()
		if r, want := h.request(id), othersOnly; r.State != store.RequestFailed || deref(r.Result) != want {
			t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), want)
		}
		if h.ag.count("submit:") != 0 {
			t.Error("a refused request started the fixer")
		}
	})
}

// pinnedFixablePR is fixablePR with findings, then `magnum pin` on PR #2,
// which pins the PR and its slot review1; it returns when the pin was, an
// hour before the clock.
func pinnedFixablePR(h *harness) time.Time {
	h.t.Helper()
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	id := h.enqueue(ReqPin, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if r := h.request(id); r.State != store.RequestDone {
		h.t.Fatalf("pin: %s %q", r.State, deref(r.Result))
	}
	if pr, sl := h.pr(2), h.slot("review1"); !pr.Pinned || !sl.Pinned {
		h.t.Fatalf("not pinned: PR %v slot %v", pr.Pinned, sl.Pinned)
	}
	at := h.clock.Now()
	h.advance(time.Hour)
	return at
}

// `magnum fix` (the board's f) on a pinned PR unpins it, as `magnum review`
// does: the pin `magnum open` or `magnum pin` left would otherwise refuse the
// fix the operator asked for. The answer and the pr.unpinned event name
// magnum fix and who pinned it, and the fixer runs; a dry run only says it
// would unpin. A hold a slot guard persisted still refuses, with the hold's
// reason, before anything is unpinned, and a person's agent the guard finds
// once the pin went refuses with the guard's own words.
func TestAFixOnAPinnedSlotUnpinsIt(t *testing.T) {
	t.Run("pinned", func(t *testing.T) {
		h, git := fixHarness(t)
		pinnedAt := pinnedFixablePR(h)
		fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
		id := h.fix(FixPayload{})
		h.handle()

		note := "unpinned review1 (pinned by magnum pin at " + clockText(pinnedAt) + ") to fix it"
		if res := h.wantDone(id); !strings.Contains(res, "fixing talkable/talkable#2") || !strings.Contains(res, "; "+note) {
			t.Errorf("answer = %q, want the fix and %q", res, note)
		}
		if pr, sl := h.pr(2), h.slot("review1"); pr.Pinned || sl.Pinned {
			t.Errorf("still pinned: PR %v slot %v", pr.Pinned, sl.Pinned)
		}
		if !hasEvent(h, "pr.unpinned", "magnum fix: "+note) {
			t.Error("no pr.unpinned event naming magnum fix")
		}
		if n := len(h.fixRuns(h.pr(2).ID)); n != 1 {
			t.Errorf("fix runs = %d, want the fixer's", n)
		}
	})
	t.Run("dry run", func(t *testing.T) {
		h, _ := fixHarness(t)
		pinnedAt := pinnedFixablePR(h)
		id := h.fix(FixPayload{DryRun: true})
		h.handle()
		want := "would unpin review1 (pinned by magnum pin at " + clockText(pinnedAt) + ") to fix it"
		if res := h.wantDone(id); !strings.HasPrefix(res, "dry run: would start fixing") || !strings.Contains(res, want) {
			t.Errorf("answer = %q, want %q", res, want)
		}
		if pr, sl := h.pr(2), h.slot("review1"); !pr.Pinned || !sl.Pinned {
			t.Errorf("a dry run unpinned: PR %v slot %v", pr.Pinned, sl.Pinned)
		}
	})
	t.Run("persisted hold", func(t *testing.T) {
		h, _ := fixHarness(t)
		pinnedFixablePR(h)
		if err := h.st.UpdateSlotFields(h.ctx, h.slot("review1").ID, func(u *store.SlotUpdate) {
			u.Set("hold_reason", slots.HoldDirtyWorktree)
		}); err != nil {
			t.Fatal(err)
		}
		id := h.fix(FixPayload{})
		h.handle()
		want := `talkable/talkable#2: slot review1 is held: dirty_worktree (magnum unpin "review1")`
		if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != want {
			t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), want)
		}
		if pr, sl := h.pr(2), h.slot("review1"); !pr.Pinned || !sl.Pinned {
			t.Errorf("a refused fix unpinned: PR %v slot %v", pr.Pinned, sl.Pinned)
		}
		if h.ag.count("submit:") != 0 {
			t.Error("the fixer started in a held slot")
		}
	})
	t.Run("a person's agent in the slot", func(t *testing.T) {
		h, _ := fixHarness(t)
		pinnedFixablePR(h)
		h.sl.mu.Lock()
		h.sl.guardErr = slots.ErrHold{Reason: slots.HoldForeignAgent, Detail: `pane p9: claude agent "alice" is idle in /tmp/review1`}
		h.sl.mu.Unlock()
		id := h.fix(FixPayload{})
		h.handle()
		r := h.request(id)
		if res := deref(r.Result); r.State != store.RequestFailed || !strings.Contains(res, "unpinned review1") ||
			!strings.Contains(res, `but slot review1 is held: foreign_agent: pane p9: claude agent "alice"`) {
			t.Errorf("%s %q, want failed naming the unpin and the guard", r.State, res)
		}
		if _, ok := h.fixStateOf(2); ok || h.ag.count("submit:") != 0 {
			t.Error("the fixer started beside a person's agent")
		}
	})
}

// `magnum fix` on a PR whose slot is broken or lost refuses with the slot's
// last error and the command that repairs it, as dispatch's wait names them,
// not with the bare state.
func TestAFixRefusedForABrokenSlotNamesTheRepair(t *testing.T) {
	for _, tc := range []struct{ state, lastError, want string }{
		{store.SlotBroken, "schema reset failed",
			"talkable/talkable#2: slot review1 is broken: schema reset failed (magnum slots repair review1)"},
		{store.SlotLost, "", "talkable/talkable#2: slot review1 is lost (magnum slots repair review1)"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			h, _ := fixHarness(t)
			fixablePR(h, "zhuravel", map[string]int{"P2": 2})
			sl := h.slot("review1")
			if err := h.st.TransitionSlot(h.ctx, sl.ID, []string{sl.State}, tc.state, func(u *store.SlotUpdate) {
				if tc.lastError != "" {
					u.Set("last_error", tc.lastError)
				}
			}); err != nil {
				t.Fatal(err)
			}
			id := h.fix(FixPayload{})
			h.handle()
			if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != tc.want {
				t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), tc.want)
			}
			if h.ag.count("submit:") != 0 {
				t.Error("the fixer started in an unusable slot")
			}
		})
	}
}

// handBack takes PR #2's slot back, as the pool does when another PR needs
// it: a release parks the PR's sessions and review1 is free again, while the
// PR stays reviewed. No poll runs, so a head the test set on the PR's row
// stays.
func handBack(h *harness) {
	h.t.Helper()
	rel := h.enqueue(ReqRelease, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if r := h.request(rel); r.State != store.RequestDone {
		h.t.Fatalf("release: %s %q", r.State, deref(r.Result))
	}
	if sl := h.slot("review1"); sl.State != store.SlotFree || sl.PRID != nil {
		h.t.Fatalf("slot not handed back: %+v", sl)
	}
	h.wantState(2, store.PRReviewed)
}

// slotClaims are the calls among calls that give a PR a slot (a free pool
// slot reserved or claimed, a per-PR worktree made) or check one out.
func slotClaims(calls []string) (claims, checkouts []string) {
	for _, c := range calls {
		switch {
		case strings.HasPrefix(c, "reserve:"), strings.HasPrefix(c, "claim:"), strings.HasPrefix(c, "worktree:"):
			claims = append(claims, c)
		case strings.HasPrefix(c, "checkout:"):
			checkouts = append(checkouts, c)
		}
	}
	return claims, checkouts
}

// `magnum fix` on a reviewed PR whose slot the pool took back (on 2026-10-27
// talkable#11907 was refused: "has no checkout of its reviewed head: its slot
// was handed back") claims a free slot at the reviewed head, as `magnum open`
// does, and the fixer works there: one reservation, one checkout of the
// reviewed head, the slot's databases made to fit it, and no pin. The answer
// says it claims a slot and the fix.start event names it; once the loop let
// go of the PR the slot is held, so the pool can take it back again.
func TestAFixOnAPRWhoseSlotWasHandedBackClaimsOneAtTheReviewedHead(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	handBack(h)
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	before := len(h.sl.all())

	id := h.fix(FixPayload{})
	h.handle()
	if res := h.wantDone(id); !strings.Contains(res, "claude-fix fixes the 2 findings of the review on a1a1a1a in a slot it claims, cycle 1 of 3") {
		t.Errorf("answer = %q", res)
	}
	if n := h.ag.count("submit:claude-fix:"); n != 1 {
		t.Fatalf("fixer prompts = %d, want 1: %v", n, h.ag.all())
	}
	calls := h.sl.all()[before:]
	claims, checkouts := slotClaims(calls)
	if want := []string{"reserve:" + itoa(pr.ID)}; !slices.Equal(claims, want) {
		t.Errorf("claims = %v, want %v (all: %v)", claims, want, calls)
	}
	if want := []string{"checkout:review1:" + itoa(pr.ID) + ":" + fixHead}; !slices.Equal(checkouts, want) {
		t.Errorf("checkouts = %v, want %v (all: %v)", checkouts, want, calls)
	}
	if !slices.Contains(calls, "ensure_schema:review1") {
		t.Errorf("slot calls %v lack the schema load of the claimed slot", calls)
	}
	if slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "pin:") }) {
		t.Errorf("the claim pinned the slot: %v", calls)
	}
	if ev, ok := h.fixEvent("fix.start"); !ok || !strings.Contains(ev.Message, "in review1 (run ") {
		t.Errorf("fix.start = %q, %v; want it to name review1", ev.Message, ok)
	}
	sl := h.slot("review1")
	if deref(sl.PRID) != pr.ID || sl.Pinned || sl.State != store.SlotHeld || deref(sl.CheckedOutSHA) != fixHead {
		t.Errorf("slot = %s pr %v pinned %v at %s, want held for PR %d, unpinned, at %s",
			sl.State, deref(sl.PRID), sl.Pinned, deref(sl.CheckedOutSHA), pr.ID, fixHead)
	}
	if cur := h.pr(2); cur.Pinned {
		t.Error("the claim pinned the PR")
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on after a fixer that pushed nothing")
	}
}

// With no slot free, `magnum fix` on a PR whose slot was handed back is
// refused as `magnum open` is: a slot is freed for it (an idle held slot is
// evicted), the answer names the retry, and no fixer starts and nothing is
// claimed. The retry claims the slot freed.
func TestAFixWithoutAFreeSlotRefusesAndNamesTheRetry(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	handBack(h)
	takeOnlySlot(h)
	before := len(h.sl.all())

	id := h.fix(FixPayload{})
	h.handle()
	want := "no free talkable/talkable slot right now; run `magnum fix talkable/talkable#2` again shortly"
	if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != want {
		t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), want)
	}
	if h.ag.count("submit:") != 0 {
		t.Error("a refused request started the fixer")
	}
	calls := h.sl.all()[before:]
	if claims, checkouts := slotClaims(calls); len(claims)+len(checkouts) != 0 {
		t.Errorf("a refused request claimed %v and checked out %v", claims, checkouts)
	}
	if !slices.Contains(calls, "release:review1:evicted") {
		t.Errorf("no slot was freed for the fix: %v", calls)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("a refused request left a loop")
	}

	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	again := h.fix(FixPayload{})
	h.handle()
	h.wantDone(again)
	if n := h.ag.count("submit:claude-fix:"); n != 1 {
		t.Errorf("fixer prompts after the retry = %d, want 1", n)
	}
	if sl := h.slot("review1"); deref(sl.PRID) != pr.ID || deref(sl.CheckedOutSHA) != fixHead {
		t.Errorf("slot after the retry = pr %v at %s, want PR %d at %s", deref(sl.PRID), deref(sl.CheckedOutSHA), pr.ID, fixHead)
	}
}

// A claim whose checkout fails after the reservation hands the slot back, as
// a failed `magnum open` does: the slot is released and free again, no fixer
// starts, and the loop ends failed with the claim's error.
func TestAFixWhoseClaimFailsHandsTheSlotBack(t *testing.T) {
	h, _ := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	handBack(h)
	h.sl.holdErr = errors.New("checkout failed")
	before := len(h.sl.all())

	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id) // the claim runs with the job, after the answer
	calls := h.sl.all()[before:]
	if !slices.Contains(calls, "reserve:"+itoa(pr.ID)) || !slices.Contains(calls, "release:review1:fix failed") {
		t.Errorf("slot calls %v lack the reservation and its release", calls)
	}
	if sl := h.slot("review1"); sl.State != store.SlotFree || sl.PRID != nil {
		t.Errorf("slot = %s for PR %v, want free", sl.State, deref(sl.PRID))
	}
	if h.ag.count("submit:") != 0 {
		t.Error("the fixer started without a checkout")
	}
	want := "fix claim failed: check out a1a1a1a in review1: checkout failed"
	if ev, ok := h.fixEvent("fix.failed"); !ok || ev.Message != want {
		t.Errorf("fix.failed = %q, %v; want %q", ev.Message, ok, want)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on after a failed claim")
	}
}

// A claim whose schema load fails is no failure (the next round reloads the
// schema), but the fixer's specs then run on the databases of the slot's
// last PR: the fix.start event says so, as `magnum open`'s answer does.
func TestAFixWhoseClaimCouldNotLoadTheSchemaSaysSo(t *testing.T) {
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	handBack(h)
	h.sl.ensureErr = errors.New("reset_db failed")
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })

	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id)
	if n := h.ag.count("submit:claude-fix:"); n != 1 {
		t.Fatalf("fixer prompts = %d, want 1", n)
	}
	note := "its databases' schema could not be loaded (the next round reloads it): reset_db failed"
	ev, ok := h.fixEvent("fix.start")
	if !ok || !strings.HasSuffix(ev.Message, "; "+note) {
		t.Errorf("fix.start = %q, %v; want it to end with %q", ev.Message, ok, note)
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil || data["schema"] != note {
		t.Errorf("fix.start data = %s, want schema %q", ev.Data, note)
	}
}

// A free slot another round takes between the request's check and the
// claim ends the loop quietly with the refusal's sentence, which names the
// retry: a fix.ended event and an info toast, no failure, no attention.
func TestAFixThatLosesItsFreeSlotEndsQuietly(t *testing.T) {
	h, _ := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	handBack(h)
	h.sl.reserveErr = fmt.Errorf("slots: reserve for PR #2 in talkable/talkable: %w", slots.ErrNoFreeSlot)

	id := h.fix(FixPayload{})
	h.handle()
	h.wantDone(id) // the check found review1 free
	sentence := "no free talkable/talkable slot right now; run `magnum fix talkable/talkable#2` again shortly"
	if ev, ok := h.fixEvent("fix.ended"); !ok || ev.Message != "the fix loop ends: "+sentence {
		t.Errorf("fix.ended = %q, %v; want %q", ev.Message, ok, "the fix loop ends: "+sentence)
	}
	if ev, ok := h.fixEvent("fix.failed"); ok {
		t.Errorf("fix.failed = %q: a lost slot is no failure", ev.Message)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on without a slot")
	}
	if h.ag.count("submit:") != 0 {
		t.Error("the fixer started without a slot")
	}
	h.wantState(2, store.PRReviewed)
	h.flush()
	if got := toastsWith(h, sentence); len(got) != 1 {
		t.Errorf("toasts = %v, want one that says %q", h.nh.shown(), sentence)
	}
	h.wantToasts("needs attention", 0)
}

// `magnum abort` while the claim checks the slot out stops the job and hands
// the slot it reserved back, as a failed claim does: a slot left to the PR
// would keep the last PR's checkout record, and a later `magnum open` would
// take it as the PR's own.
func TestAnAbortDuringTheClaimHandsTheSlotBack(t *testing.T) {
	h, _ := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	handBack(h)
	h.sl.mu.Lock()
	h.sl.checkoutGate = make(chan struct{})
	h.sl.checkoutStarted = make(chan struct{}, 1)
	started := h.sl.checkoutStarted
	h.sl.mu.Unlock()
	before := len(h.sl.all())

	id := h.fix(FixPayload{})
	h.e.handleRequests(h.ctx)
	h.wantDone(id)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the claim never reached its checkout")
	}
	abort := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if r := h.request(abort); r.State != store.RequestDone {
		t.Errorf("abort: %s %q", r.State, deref(r.Result))
	}
	calls := h.sl.all()[before:]
	if !slices.Contains(calls, "reserve:"+itoa(pr.ID)) || !slices.Contains(calls, "release:review1:fix stopped") {
		t.Errorf("slot calls %v lack the reservation and its release", calls)
	}
	if sl := h.slot("review1"); sl.State != store.SlotFree || sl.PRID != nil {
		t.Errorf("slot = %s for PR %v, want free", sl.State, deref(sl.PRID))
	}
	if h.ag.count("submit:") != 0 {
		t.Error("the fixer started after the abort")
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on after the abort")
	}
}

// A PR without a slot that is pinned holds autofix and a loop's next cycle
// as a pin holds every automatic round: they refuse it and claim nothing,
// while `magnum fix` takes the pin off and claims a slot.
func TestAPinnedPRWithoutASlotHoldsAutofixButNotMagnumFix(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	handBack(h)
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("pinned", true) }); err != nil {
		t.Fatal(err)
	}
	before := len(h.sl.all())

	_, err := h.e.startFix(h.ctx, h.pr(2), autofixBy, FixState{}, nil)
	if want := "talkable/talkable#2 is pinned (magnum unpin)"; err == nil || err.Error() != want {
		t.Errorf("autofix: %v, want %q", err, want)
	}
	if claims, checkouts := slotClaims(h.sl.all()[before:]); len(claims)+len(checkouts) != 0 {
		t.Errorf("a refused autofix claimed %v and checked out %v", claims, checkouts)
	}

	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	id := h.fix(FixPayload{})
	h.handle()
	if res := h.wantDone(id); !strings.Contains(res, "unpinned the PR") {
		t.Errorf("answer = %q, want it to say it unpinned the PR", res)
	}
	if claims, _ := slotClaims(h.sl.all()[before:]); !slices.Equal(claims, []string{"reserve:" + itoa(pr.ID)}) {
		t.Errorf("claims = %v, want one reservation", claims)
	}
	if h.pr(2).Pinned {
		t.Error("the PR is still pinned")
	}
}

// A PR without a slot whose head kept the verdict of the last review (a push
// that only merged the base, then a round whose judge only replied) claims
// its slot at that head, the reviewed one, and the fixer works there.
func TestAFixOnASlotlessPRWhoseHeadKeptTheVerdictClaimsAtItsHead(t *testing.T) {
	h, git := fixHarness(t)
	pr := stoodPR(h, git, "replied")
	handBack(h)
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	before := len(h.sl.all())

	id := h.fix(FixPayload{})
	h.handle()
	want := "the review on a1a1a1a (the head d4d4d4d kept its verdict), 1 of them open from earlier reviews, in a slot it claims"
	if res := h.wantDone(id); !strings.Contains(res, want) {
		t.Errorf("answer = %q, want it to say %q", res, want)
	}
	if _, checkouts := slotClaims(h.sl.all()[before:]); !slices.Equal(checkouts, []string{"checkout:review1:" + itoa(pr.ID) + ":" + keptHead}) {
		t.Errorf("checkouts = %v, want one of %s", checkouts, keptHead)
	}
	if runs := h.fixRuns(pr.ID); len(runs) != 1 || runs[0].TargetSHA != keptHead {
		t.Errorf("fix runs = %+v, want one on %s", runs, keptHead)
	}
}

// takeOnlySlot has PR #3 reviewed in the pool's only slot after PR #2's was
// handed back, and its slot then idle past min_warm: evictable, so a fix
// that asks for a slot starts that eviction.
func takeOnlySlot(h *harness) {
	h.t.Helper()
	h.advance(31 * time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: fixHead, author: "zhuravel"}, prSpec{n: 3, head: "c1"})
	h.tick()
	h.advance(5 * time.Minute)
	h.tick()
	h.wantState(3, store.PRReviewed)
	if sl := h.slot("review1"); deref(sl.PRID) != h.pr(3).ID {
		h.t.Fatalf("review1 holds PR %v, want #3", deref(sl.PRID))
	}
	h.advance(31 * time.Minute)
}

// A PR without a slot whose head moved since the review has nothing to fix:
// `magnum fix` refuses as before, before it looks for a slot, so it claims
// none and frees none (the pool's only slot is another PR's and evictable).
func TestAFixOnASlotlessPRWhoseHeadMovedRefusesAndClaimsNothing(t *testing.T) {
	h, _ := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	handBack(h)
	takeOnlySlot(h)
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("head_sha", fixPushed) }); err != nil {
		t.Fatal(err)
	}
	before := len(h.sl.all())

	id := h.fix(FixPayload{})
	h.handle()
	if r, want := h.request(id), "nothing to fix: no review on b2b2b2b"; r.State != store.RequestFailed || deref(r.Result) != want {
		t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), want)
	}
	if h.ag.count("submit:") != 0 {
		t.Error("a refused request started the fixer")
	}
	calls := h.sl.all()[before:]
	if claims, checkouts := slotClaims(calls); len(claims)+len(checkouts) != 0 {
		t.Errorf("a refused request claimed %v and checked out %v", claims, checkouts)
	}
	if slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "release:") }) {
		t.Errorf("a refused request freed a slot: %v", calls)
	}
	if sl := h.slot("review1"); deref(sl.PRID) != h.pr(3).ID {
		t.Errorf("review1 holds PR %v, want #3 still", deref(sl.PRID))
	}
}

// A fixer that pushed with nobody needed counts the cycle: the slot is put
// back in order at the pushed head, the push is recorded on the PR row as
// the poller records one (so the poller's own sight of it changes nothing),
// the loop waits for the re-review (reviewing, cycle 1), and a forced
// re-review of the pushed head runs.
func TestAPushedReportForcesTheReReview(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 2})
	fixer(h, git, fixPushed, func(run store.Run) string {
		return fixReport(run.ID, "b2b2b2b Guard the nil owner", fixReplied, "none")
	})
	id := h.fix(FixPayload{})
	h.handle() // no dispatch yet: the fake slots fetch the head the poller recorded
	h.wantDone(id)
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixPushed) {
		t.Errorf("slot calls %v lack the resync to the pushed head", h.sl.all())
	}
	if sl := h.slot("review1"); deref(sl.CheckedOutSHA) != fixPushed {
		t.Errorf("slot checked out %s", deref(sl.CheckedOutSHA))
	}
	st, ok := h.fixStateOf(2)
	report := deref(h.fixRuns(pr.ID)[0].ReportPath)
	if !ok || st.Phase != FixPhaseReviewing || st.Cycle != 1 || st.Head != fixPushed || st.Report != report || len(st.NeedsYou) != 0 {
		t.Fatalf("fix state = %+v, %v", st, ok)
	}
	if cur := h.wantState(2, store.PRRereviewPending); !cur.Forced {
		t.Error("the re-review is not forced")
	}
	if r := h.fixRuns(pr.ID)[0]; r.State != store.RunVerified {
		t.Errorf("fix run %s", r.State)
	}
	h.wantReportInData("fix.pushed", report)
	h.wantReportInData("fix.replied", report)
	// The push is on the PR row as the poller records one.
	if cur := h.pr(2); cur.HeadSHA != fixPushed || !cur.HeadChangedAt.Equal(h.clock.Now()) {
		t.Errorf("PR head %s changed at %v, want %s now", cur.HeadSHA, cur.HeadChangedAt, fixPushed)
	}
	if n := h.eventCount("pr:talkable/talkable#2", "pr.head_changed"); n != 1 {
		t.Errorf("pr.head_changed events = %d, want 1", n)
	}
	if got := h.e.pushTimes(h.ctx, pr.ID); len(got) == 0 || !got[len(got)-1].Equal(h.clock.Now()) {
		t.Errorf("recorded pushes %v lack the fixer's", got)
	}

	// The poller's own sight of the push changes nothing; the forced
	// re-review runs on the pushed head.
	h.advance(time.Minute)
	reqPoll(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
	if n := h.eventCount("pr:talkable/talkable#2", "pr.head_changed"); n != 1 {
		t.Errorf("pr.head_changed events after the poll = %d, want 1", n)
	}
	reqWantRounds(t, h, 2)
	if in := h.rd.all()[1]; in.TargetSHA != fixPushed || !in.PR.Forced {
		t.Errorf("the re-review: target %s forced %v", in.TargetSHA, in.PR.Forced)
	}
}

// The loop's forced re-review is no act of the operator: the threads magnum
// stopped arguing in (KVPRStalemate) stay flagged for them, where a
// `magnum review` they ask for marks them seen.
func TestTheLoopsReReviewLeavesStalematesFlagged(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	const flagged = `{"threads":[{"id":"PRRT_A","url":"https://github.com/talkable/talkable/pull/2#discussion_r206","last_reply":206}]}`
	h.e.setKV(h.ctx, KVPRStalemate(pr.ID), flagged)
	pushingFixer(h, git, fixPushed)
	h.fix(FixPayload{})
	h.handle()
	if cur := h.wantState(2, store.PRRereviewPending); !cur.Forced {
		t.Fatal("no forced re-review")
	}
	if v, ok := kvValue(h, KVPRStalemate(pr.ID)); !ok || v != flagged {
		t.Errorf("stalemate = %q, %v: the loop's force marked the operator's threads seen", v, ok)
	}
	if v, ok := kvValue(h, kvPRStalemateSeen(pr.ID)); ok {
		t.Errorf("stalemate_seen = %q, want none", v)
	}
}

// A fixer that pushed nothing and needs nobody answered every finding: the
// slot goes back to the reviewed head, the loop ends and an info toast says
// so.
func TestAFixThatPushedNothingResyncsTheSlot(t *testing.T) {
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 3})
	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	h.fix(FixPayload{})
	h.tick()
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v lack the resync to the reviewed head", h.sl.all())
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
	h.wantState(2, store.PRReviewed)
	h.flush()
	if got := toastsWith(h, "PR #2: claude-fix pushed nothing, replied to 3 findings"); len(got) != 1 {
		t.Errorf("toasts = %v", h.nh.shown())
	}
	h.wantToasts("needs attention", 0)
}

// Findings the fixer leaves to the author without a push end the loop as
// needs attention, the fix job's kind, naming how many and the report; the
// slot goes back to the reviewed head.
func TestNeedsYouEndsTheLoopAsNeedsAttention(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P1": 1, "P2": 2})
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", fixReplied, "- 2101 P1 app/models/offer.rb:42: wants a schema change\n- 2102 P2 app/lib/x.rb:7: contested")
	})
	h.fix(FixPayload{})
	h.tick()
	report := deref(h.fixRuns(pr.ID)[0].ReportPath)
	cur := h.wantState(2, store.PRNeedsAttention)
	if want := "fix: 2 findings need your call: " + report; deref(cur.LastError) != want {
		t.Errorf("last_error = %q, want %q", deref(cur.LastError), want)
	}
	if kind, _ := h.e.getKV(h.ctx, KVPRAttention(pr.ID)); kind != attention.KindFix {
		t.Errorf("attention kind = %q", kind)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v", h.sl.all())
	}
	h.wantToasts("needs attention", 1)
	h.wantReportInData("fix.needs_you", report)
}

// Findings the fixer leaves to the author after a push wait for the push's
// re-review: the slot moves to the pushed head, the cycle counts, the loop
// waits in phase reviewing with the entries kept for the round's end, and
// the re-review is forced; the operator is told at once (a toast), the PR's
// state waits for the round.
func TestAPushWithNeedsYouIsReReviewedFirst(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P1": 1, "P2": 2})
	fixer(h, git, fixPushed, func(run store.Run) string {
		return fixReport(run.ID, "b2b2b2b Guard the nil owner", fixReplied, "- 2102 P2 app/lib/x.rb:7: contested")
	})
	h.fix(FixPayload{})
	h.handle()
	report := deref(h.fixRuns(pr.ID)[0].ReportPath)
	st, ok := h.fixStateOf(2)
	if !ok || st.Phase != FixPhaseReviewing || st.Cycle != 1 || st.Head != fixPushed ||
		!slices.Equal(st.NeedsYou, []string{"2102 P2 app/lib/x.rb:7: contested"}) {
		t.Fatalf("fix state = %+v, %v", st, ok)
	}
	if cur := h.wantState(2, store.PRRereviewPending); !cur.Forced || deref(cur.LastError) != "" {
		t.Errorf("the re-review: forced %v, last_error %q", cur.Forced, deref(cur.LastError))
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixPushed) {
		t.Errorf("slot calls %v", h.sl.all())
	}
	h.wantReportInData("fix.needs_you", report)
	h.wantToasts("needs attention", 1)
}

// A run that ends without a report of its own fails the job: the loop ends
// as needs attention, the slot goes back to the reviewed head.
func TestAMissingFixReportFailsTheJob(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report func(run store.Run) string
		want   string
	}{
		{name: "no report", report: func(store.Run) string { return "" }, want: "wrote no report at "},
		{name: "another run's", report: func(store.Run) string { return fixReport("r-older", "nothing", fixReplied, "none") },
			want: "the first line is not the run's marker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, git := fixHarness(t)
			pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
			fixer(h, git, "", tc.report)
			h.fix(FixPayload{})
			h.tick()
			cur := h.wantState(2, store.PRNeedsAttention)
			if le := deref(cur.LastError); !strings.HasPrefix(le, "fix run failed: ") || !strings.Contains(le, tc.want) {
				t.Errorf("last_error = %q", le)
			}
			if _, ok := h.fixStateOf(2); ok {
				t.Error("the loop goes on")
			}
			if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
				t.Errorf("slot calls %v", h.sl.all())
			}
			if r := h.fixRuns(pr.ID)[0]; r.State != store.RunFailed {
				t.Errorf("fix run %s", r.State)
			}
			h.wantReportInData("fix.failed", deref(h.fixRuns(pr.ID)[0].ReportPath))
		})
	}
}

// A push the fixer reports failed (someone pushed meanwhile) is the failed
// case: the fixer's local commits go, the slot is back at the reviewed head.
func TestAFixWhosePushFailedEndsAsFailed(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	fixer(h, git, "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3", func(run store.Run) string {
		return fixReport(run.ID, "push failed: ! [rejected] HEAD -> feature (non-fast-forward)", fixReplied, "- 2101 P2 a.rb:1: committed, not pushed")
	})
	h.fix(FixPayload{})
	h.tick()
	report := deref(h.fixRuns(pr.ID)[0].ReportPath)
	cur := h.wantState(2, store.PRNeedsAttention)
	if want := "fix run failed: the fixer's push failed: " + report; deref(cur.LastError) != want {
		t.Errorf("last_error = %q, want %q", deref(cur.LastError), want)
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v", h.sl.all())
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
	h.wantReportInData("fix.failed", report)
}

// A push the fixer reports but GitHub does not show (the branch's head is
// still the reviewed one) is no push: the settle fails with "push not
// confirmed on GitHub", the PR row keeps GitHub's head, no re-review of a
// commit GitHub lacks is forced, and the slot goes back to the reviewed head.
func TestAPushGitHubDoesNotShowFailsTheFix(t *testing.T) {
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	git.lostPush = true
	git.lsErr = errors.New("git ls-remote: not reached")
	fixer(h, git, fixPushed, func(run store.Run) string { return fixReport(run.ID, "b2b2b2b Fix", fixReplied, "none") })
	h.fix(FixPayload{})
	h.handle()

	cur := h.wantState(2, store.PRNeedsAttention)
	if le := deref(cur.LastError); !strings.Contains(le, "push not confirmed on GitHub") || !strings.Contains(le, "a1a1a1a") {
		t.Errorf("last_error = %q", le)
	}
	if cur.HeadSHA != fixHead || cur.Forced {
		t.Errorf("PR head %s forced %v, want GitHub's %s and no re-review", cur.HeadSHA, cur.Forced, fixHead)
	}
	if n := h.eventCount("pr:talkable/talkable#2", "pr.head_changed"); n != 0 {
		t.Errorf("pr.head_changed events = %d, want none", n)
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) || slices.Contains(h.sl.all(), "resync:review1:"+fixPushed) {
		t.Errorf("slot calls %v, want the slot back at the reviewed head only", h.sl.all())
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
	if _, ok := h.fixEvent("fix.pushed"); ok {
		t.Error("fix.pushed recorded for a push GitHub does not show")
	}
}

// When the poll identity's GitHub client cannot say where the PR's head is,
// the branch on origin answers (git ls-remote): a push it shows is
// confirmed and re-reviewed; with neither answering, the push is not
// confirmed and the loop fails.
func TestAPushIsConfirmedOnTheBranchWhenGitHubCannotSay(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote string
		lsErr  error
		want   string // the PR's state after the settle
	}{
		{name: "the branch shows it", remote: fixPushed + "\trefs/heads/feature\n", want: store.PRRereviewPending},
		{name: "nothing answers", lsErr: errors.New("git ls-remote: not reached"), want: store.PRNeedsAttention},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, git := fixHarness(t)
			fixablePR(h, "zhuravel", map[string]int{"P2": 1})
			h.gh.fail(errors.New("graphql: 502 Bad Gateway"), nil)
			git.lostPush, git.remote, git.lsErr = true, tc.remote, tc.lsErr
			fixer(h, git, fixPushed, func(run store.Run) string { return fixReport(run.ID, "b2b2b2b Fix", fixReplied, "none") })
			h.fix(FixPayload{})
			h.handle()

			cur := h.wantState(2, tc.want)
			git.mu.Lock()
			calls := slices.Clone(git.lsCalls)
			git.mu.Unlock()
			if len(calls) == 0 || calls[0] != h.slot("review1").Path+":refs/heads/feature" {
				t.Errorf("ls-remote calls %v", calls)
			}
			if tc.want == store.PRRereviewPending {
				if cur.HeadSHA != fixPushed || !cur.Forced {
					t.Errorf("PR head %s forced %v, want the confirmed push re-reviewed", cur.HeadSHA, cur.Forced)
				}
				return
			}
			if le := deref(cur.LastError); !strings.Contains(le, "push not confirmed on GitHub") {
				t.Errorf("last_error = %q", le)
			}
		})
	}
}

// A push by the operator while the fixer works makes the fixer's push fail.
// The poller records the operator's head, so the PR waits for its re-review
// (the fixer holds it); the failed push then ends the loop with the urgent
// toast alone, no needs_attention (the re-review of the operator's head
// still runs), the slot back at the reviewed head and the loop's row gone;
// the operator's head is re-reviewed next.
func TestAnOperatorPushDuringTheFixIsReviewedAfterTheFixersFailedPush(t *testing.T) {
	const operator = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	h, git := fixHarness(t, realSleep)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.fix(FixPayload{})
	h.e.handleRequests(h.ctx)
	h.awaitSubmit()

	h.advance(time.Hour)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: operator, author: "zhuravel"})
	if err := h.e.Tick(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.wantState(2, store.PRRereviewPending)
	rounds := len(h.rd.all())

	run := h.fixRuns(pr.ID)[0]
	if err := os.MkdirAll(filepath.Dir(deref(run.ReportPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	report := fixReport(run.ID, "push failed: ! [rejected] HEAD -> feature (fetch first)", fixReplied, "- 2101 P2 a.rb:1: committed, not pushed")
	if err := os.WriteFile(deref(run.ReportPath), []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	git.setHead(fixPushed) // the fixer's local commit
	if err := h.st.TransitionRun(h.ctx, run.ID, []string{store.RunWorking}, store.RunEnded, nil); err != nil {
		t.Fatal(err)
	}
	h.settle()

	cur := h.wantState(2, store.PRRereviewPending)
	if cur.HeadSHA != operator || deref(cur.LastError) != "" {
		t.Errorf("PR head %s, last_error %q: want the operator's head waiting for its re-review", cur.HeadSHA, deref(cur.LastError))
	}
	if got := toastsWith(h, "talkable#2 needs attention"); len(got) != 1 {
		t.Errorf("urgent toasts = %v, want one", h.nh.shown())
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v lack the resync to the reviewed head", h.sl.all())
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop's row outlived the failed push")
	}
	if ev, ok := h.fixEvent("fix.failed"); !ok || !strings.Contains(ev.Message, "the fixer's push failed") {
		t.Errorf("fix.failed = %+v, %v", ev, ok)
	}

	h.advance(time.Hour)
	h.tick()
	if got := h.rd.all(); len(got) != rounds+1 || got[len(got)-1].TargetSHA != operator {
		t.Fatalf("rounds after the fix: %d (want %d), the last one's target %v", len(got), rounds+1, got[len(got)-1].TargetSHA)
	}
	h.wantState(2, store.PRReviewed)
}

// `magnum abort` during the fixer's run interrupts the fixer, abandons its
// run, puts the slot back and ends the loop quietly: no attention.
func TestAbortEndsAFix(t *testing.T) {
	var kh *keyHerdr
	h, _ := fixHarness(t, realSleep, withKeys(&kh))
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.fix(FixPayload{})
	h.e.handleRequests(h.ctx)
	h.awaitSubmit()
	h.hd.mu.Lock()
	h.hd.agents = append(h.hd.agents, herdr.AgentInfo{PaneID: "p-claude-fix", Name: "mg-t-2-claude-fix", Agent: "claude",
		AgentStatus: herdr.StatusWorking})
	h.hd.mu.Unlock()

	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.e.handleRequests(h.ctx)
	h.settle()
	res := h.wantDone(id)
	if !strings.Contains(res, "aborted the fix of talkable/talkable#2") || !strings.Contains(res, "fixer interrupted") {
		t.Errorf("answer = %q", res)
	}
	if got, want := kh.all(), []string{"agent:mg-t-2-claude-fix:ctrl+c", "agent:mg-t-2-claude-fix:ctrl+c"}; !slices.Equal(got, want) {
		t.Errorf("keys %v, want %v", got, want)
	}
	if r := h.fixRuns(pr.ID)[0]; r.State != store.RunAbandoned || deref(r.Error) != abortedBy {
		t.Errorf("fix run %s %q", r.State, deref(r.Error))
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v", h.sl.all())
	}
	h.wantState(2, store.PRReviewed)
	h.flush()
	h.wantToasts("needs attention", 0)
	if got := toastsWith(h, "fix stopped"); len(got) != 1 {
		t.Errorf("toasts = %v", h.nh.shown())
	}
}

// `magnum fix --off` switches autofix off for the PR and `--on` back on;
// neither starts a loop, and a loop asked for by hand runs anyway, saying
// so.
func TestFixOffAndOnToggleTheFlag(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	off := h.fix(FixPayload{Off: true})
	h.handle()
	if res := h.wantDone(off); res != "fix is off for PR #2: autofix leaves it to you; `magnum fix --on` lifts it" {
		t.Errorf("--off answer = %q", res)
	}
	o, ok, err := h.e.fixOff(h.ctx, pr.ID)
	if err != nil || !ok || o.By != "magnum fix --off" || !o.Since.Equal(h.clock.Now()) {
		t.Fatalf("fix off = %+v, %v, %v", o, ok, err)
	}
	if _, ok := h.fixStateOf(2); ok || h.ag.count("submit:") != 0 {
		t.Fatal("--off started a loop")
	}

	fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
	id := h.fix(FixPayload{})
	h.tick()
	if res := h.wantDone(id); !strings.Contains(res, "fix is off for this PR: this requested loop runs anyway") {
		t.Errorf("answer = %q", res)
	}
	if h.ag.count("submit:claude-fix:") != 1 {
		t.Error("the requested loop did not run")
	}

	on := h.fix(FixPayload{On: true})
	h.handle()
	if res := h.wantDone(on); res != "autofix is on again for PR #2" {
		t.Errorf("--on answer = %q", res)
	}
	if _, ok, err := h.e.fixOff(h.ctx, pr.ID); ok || err != nil {
		t.Errorf("fix off after --on: %v, %v", ok, err)
	}
	if h.ag.count("submit:claude-fix:") != 1 {
		t.Error("--on started a loop")
	}
}

// A dry run says what would happen and does nothing; on a watch that posts
// as the operator it says that the re-review will not read the fixer's
// replies as the author's.
func TestAFixDryRunOnAWatchThatPostsAsYouSaysSo(t *testing.T) {
	h, _ := fixHarness(t, func(h *harness) { h.cfg.Watches[0].Identity = "zhuravel" })
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	id := h.fix(FixPayload{DryRun: true})
	h.handle()
	res := h.wantDone(id)
	if !strings.HasPrefix(res, "dry run: would") || !strings.Contains(res, "this watch posts as you: the re-review will not read the fixer's replies as yours") {
		t.Errorf("answer = %q", res)
	}
	if _, ok := h.fixStateOf(2); ok || h.ag.count("submit:") != 0 || len(h.fixRuns(pr.ID)) != 0 {
		t.Error("a dry run started the fixer")
	}
}

// A failed guard (a person's agent in the slot) ends the job before the
// fixer starts, as needs attention with the reason; the slot is not touched.
func TestAFailedGuardEndsTheFixAsNeedsAttention(t *testing.T) {
	h, _ := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.sl.mu.Lock()
	h.sl.guardErr = slots.ErrHold{Reason: slots.HoldForeignAgent, Detail: `pane p9: claude agent "x" is idle in /tmp/review1`}
	h.sl.mu.Unlock()
	h.fix(FixPayload{})
	h.tick()
	cur := h.wantState(2, store.PRNeedsAttention)
	if le := deref(cur.LastError); !strings.HasPrefix(le, "fix guard failed: ") || !strings.Contains(le, "pane p9") {
		t.Errorf("last_error = %q", le)
	}
	if h.ag.count("submit:") != 0 || slices.ContainsFunc(h.sl.all(), func(c string) bool { return strings.HasPrefix(c, "resync:") }) {
		t.Errorf("the fixer started or the slot was touched: %v, %v", h.ag.all(), h.sl.all())
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
}

// A resync that fails (herdr down) is the job's failure, never swallowed:
// the loop ends as needs attention and no re-review is forced.
func TestAResyncFailureEndsTheFixAsNeedsAttention(t *testing.T) {
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.sl.mu.Lock()
	h.sl.resyncErr = errors.New("slots: guard review1: herdr snapshot: connection refused")
	h.sl.mu.Unlock()
	fixer(h, git, fixPushed, func(run store.Run) string { return fixReport(run.ID, "b2b2b2b Fix", fixReplied, "none") })
	h.fix(FixPayload{})
	h.tick()
	cur := h.wantState(2, store.PRNeedsAttention)
	if le := deref(cur.LastError); !strings.HasPrefix(le, "fix settle failed: ") || !strings.Contains(le, "connection refused") {
		t.Errorf("last_error = %q", le)
	}
	if cur.Forced {
		t.Error("a re-review was forced")
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
}

// A fixer whose time runs out is told to write its report now; one that
// still writes none fails the job, and the slot goes back to the reviewed
// head (the fixer's local commits would hold it for head drift).
func TestAFixThatTimesOutIsAskedForItsReportThenFails(t *testing.T) {
	h, _ := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.e.d.Sleep = func(ctx context.Context, d time.Duration) error { h.clock.Add(d); return ctx.Err() }
	h.fix(FixPayload{})
	h.tick()
	report := deref(h.fixRuns(pr.ID)[0].ReportPath)
	if !slices.ContainsFunc(h.ag.all(), func(c string) bool {
		return strings.HasPrefix(c, "tell:claude-fix:Time is up") && strings.Contains(c, report)
	}) {
		t.Errorf("no time-up line in %v", h.ag.all())
	}
	cur := h.wantState(2, store.PRNeedsAttention)
	if le := deref(cur.LastError); !strings.HasPrefix(le, "fix run failed: ") || !strings.Contains(le, "ran out of its 1h") {
		t.Errorf("last_error = %q", le)
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v lack the resync to the reviewed head: the fixer's commits stay in the slot", h.sl.all())
	}
	if r := h.fixRuns(pr.ID)[0]; r.State != store.RunFailed && r.State != store.RunAbandoned {
		t.Errorf("fix run %s", r.State)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
}

// After a restart no fixer run survives: a loop whose fixer was at work ends
// as needs attention and its run fails; a loop waiting for its re-review
// stays (the round's own recovery re-reviews).
func TestAFixRunLostInARestartEndsTheLoop(t *testing.T) {
	h, _ := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	run, err := h.st.CreateRun(h.ctx, store.Run{ID: "r-fix-lost", PRID: pr.ID, Round: 1, Role: "claude-fix", Kind: store.RunFix,
		TargetSHA: fixHead, Identity: pr.Identity, ReviewerLogin: "x", State: store.RunPending})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.TransitionRun(h.ctx, run.ID, []string{store.RunPending}, store.RunWorking, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.e.setFixState(h.ctx, pr.ID, FixState{Cycle: 1, Cap: 3, Phase: FixPhaseFixing, Head: fixHead}); err != nil {
		t.Fatal(err)
	}
	other := h.pr(1) // waiting for its loop's re-review
	if err := h.st.TransitionPR(h.ctx, other.ID, nil, store.PRQueued, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.e.setFixState(h.ctx, other.ID, FixState{Cycle: 2, Cap: 3, Phase: FixPhaseReviewing, Head: other.HeadSHA}); err != nil {
		t.Fatal(err)
	}

	h.startup()
	cur := h.wantState(2, store.PRNeedsAttention)
	if deref(cur.LastError) != "fix run lost in a daemon restart" {
		t.Errorf("last_error = %q", deref(cur.LastError))
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the lost loop goes on")
	}
	if r, err := h.st.RunByID(h.ctx, run.ID); err != nil || r.State != store.RunFailed {
		t.Errorf("the lost run: %+v, %v", r, err)
	}
	if st, ok := h.fixStateOf(1); !ok || st.Phase != FixPhaseReviewing {
		t.Errorf("the reviewing loop = %+v, %v", st, ok)
	}
}

// liveFixer gives PR pr a live session of the fix role whose agent herdr
// shows working: a fixer the engine did not start, as one that outlived a
// daemon (herdr keeps its pane after a crash or `daemon-restart --now`).
func liveFixer(h *harness, pr store.PR) {
	h.t.Helper()
	name := "mg-t-2-claude-fix"
	if _, err := h.st.CreateSession(h.ctx, store.Session{PRID: pr.ID, Role: "claude-fix", AgentName: &name, AgentKind: new("claude"),
		HerdrPaneID: new("p-claude-fix"), HerdrWorkspaceID: new("w1"), State: store.SessionLive}); err != nil {
		h.t.Fatal(err)
	}
	h.hd.mu.Lock()
	h.hd.agents = append(h.hd.agents, herdr.AgentInfo{PaneID: "p-claude-fix", Name: name, Agent: "claude", AgentStatus: herdr.StatusWorking})
	h.hd.mu.Unlock()
}

// A fixer that outlived its daemon is stopped before the loop is declared
// lost: it is interrupted until quiet and the slot goes back to the reviewed
// head, so it cannot push or reply as the operator after magnum said "fix
// run lost", and the next round's guard finds no drift.
func TestARestartStopsAFixerThatSurvivedIt(t *testing.T) {
	var kh *keyHerdr
	h, _ := fixHarness(t, withKeys(&kh))
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	liveFixer(h, pr)
	if err := h.e.setFixState(h.ctx, pr.ID, FixState{Cycle: 1, Cap: 3, Phase: FixPhaseFixing, Head: fixHead, Role: "claude-fix"}); err != nil {
		t.Fatal(err)
	}

	h.startup()
	if got, want := kh.all(), []string{"agent:mg-t-2-claude-fix:ctrl+c", "agent:mg-t-2-claude-fix:ctrl+c"}; !slices.Equal(got, want) {
		t.Errorf("keys %v, want %v: the surviving fixer was not interrupted", got, want)
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v lack the resync to the reviewed head", h.sl.all())
	}
	if cur := h.wantState(2, store.PRNeedsAttention); deref(cur.LastError) != "fix run lost in a daemon restart" {
		t.Errorf("last_error = %q", deref(cur.LastError))
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the lost loop goes on")
	}
}

// A fix job that panics after the fixer got its prompt stops the fixer and
// puts the slot back at the reviewed head before it fails the loop, as a
// failed run does: the fixer must not go on acting as the operator.
func TestAFixJobThatPanicsStopsTheFixer(t *testing.T) {
	var kh *keyHerdr
	h, git := fixHarness(t, withKeys(&kh))
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.ag.mu.Lock()
	h.ag.onSubmit = func(store.Run, string) {
		h.hd.mu.Lock()
		h.hd.agents = append(h.hd.agents, herdr.AgentInfo{PaneID: "p-claude-fix", Name: "mg-t-2-claude-fix", Agent: "claude",
			AgentStatus: herdr.StatusWorking})
		h.hd.mu.Unlock()
		git.setHead("c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3") // the fixer committed
		panic("boom in the fix job")
	}
	h.ag.mu.Unlock()
	h.fix(FixPayload{})
	h.tick()

	if got := kh.all(); !slices.Contains(got, "agent:mg-t-2-claude-fix:ctrl+c") {
		t.Errorf("keys %v: the fixer was not interrupted", got)
	}
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v lack the resync to the reviewed head", h.sl.all())
	}
	if cur := h.wantState(2, store.PRNeedsAttention); !strings.Contains(deref(cur.LastError), "panic: boom in the fix job") {
		t.Errorf("last_error = %q", deref(cur.LastError))
	}
	if r := h.fixRuns(pr.ID)[0]; r.State != store.RunAbandoned && r.State != store.RunFailed {
		t.Errorf("fix run %s", r.State)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
}

// `magnum abort` between the fixer's push and its re-review takes the
// forced re-review back and ends the loop, so no row is left that would
// refuse `magnum review` with no loop running.
func TestAbortEndsALoopWaitingForItsReReview(t *testing.T) {
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	fixer(h, git, fixPushed, func(run store.Run) string { return fixReport(run.ID, "b2b2b2b Fix", fixReplied, "none") })
	h.fix(FixPayload{})
	h.handle()
	if st, ok := h.fixStateOf(2); !ok || st.Phase != FixPhaseReviewing {
		t.Fatalf("fix state = %+v, %v", st, ok)
	}
	id := h.enqueue(ReqAbort, TargetPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	if res := h.wantDone(id); !strings.Contains(res, "took back the forced review") || !strings.Contains(res, "fix loop ended (cycle 1 of 3, reviewing)") {
		t.Errorf("answer = %q", res)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop's row outlived the abort")
	}
	h.wantState(2, store.PRReviewed)
	review := h.enqueue(ReqReview, ReviewPayload{PRTarget: PRTarget{Ref: "2"}})
	h.handle()
	h.wantDone(review)
}

// A loop's row holds the PR's rounds by itself, with or without its job:
// in phase fixing, and when it cannot be read (no round starts over a loop
// that may run); the wait says why.
func TestAFixingRowHoldsDispatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  string
	}{
		{name: "fixing", row: `{"cycle":1,"cap":3,"phase":"fixing"}`},
		{name: "unreadable", row: "not json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := fixHarness(t)
			pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
			h.e.setKV(h.ctx, KVPRFix(pr.ID), tc.row)
			rounds := len(h.rd.all())
			h.advance(time.Hour)
			reqPoll(h, prSpec{n: 2, head: "c3c3c3c", author: "zhuravel"})
			h.advance(time.Hour)
			h.tick()
			h.wantState(2, store.PRRereviewPending)
			if got := len(h.rd.all()); got != rounds {
				t.Errorf("rounds = %d, want %d", got, rounds)
			}
			if w := waitOf(t, h, 2); w.Reason != WaitFixing {
				t.Errorf("wait = %+v", w)
			}
		})
	}
}

// A second loop on a PR the first one left in needs_attention says why again:
// its failure replaces the PR's message and toasts, although the first
// loop's attention toast was within the dedupe window.
func TestASecondLoopOnANeedsAttentionPRSaysWhyAgain(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P1": 1})
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", fixReplied, "- 2101 P1 app/models/offer.rb:42: wants a schema change")
	})
	h.fix(FixPayload{})
	h.tick()
	h.wantState(2, store.PRNeedsAttention)
	h.wantToasts("needs attention", 1)

	h.advance(10 * time.Minute)
	fixer(h, git, "", func(store.Run) string { return "" }) // the second run writes no report of its own
	id := h.fix(FixPayload{})
	h.tick()
	h.wantDone(id)
	runs := h.fixRuns(pr.ID)
	if len(runs) != 2 {
		t.Fatalf("fix runs = %d, want 2", len(runs))
	}
	cur := h.wantState(2, store.PRNeedsAttention)
	if le := deref(cur.LastError); !strings.HasPrefix(le, "fix run failed: ") || !strings.Contains(le, deref(runs[1].ReportPath)) {
		t.Errorf("last_error = %q, want the second run's failure", le)
	}
	h.wantToasts("needs attention", 2)
}

// A re-review that cannot be forced after the fixer's push moved the PR's
// head still leaves the push seen: the head change is recorded as the
// poller records one and the normal re-review path takes over (the PR waits
// for its re-review, the loop ends), so the poller's later sight of the
// head changes nothing and the push is not lost unreviewed.
func TestAFailedForceAfterAPushStillQueuesItsReReview(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	// The force's compare-and-set fails: the registry refuses forced = 1 on the PR.
	if _, err := h.st.DB().ExecContext(h.ctx, fmt.Sprintf(`CREATE TRIGGER refuse_force BEFORE UPDATE OF forced ON prs
  WHEN NEW.id = %d AND NEW.forced = 1 BEGIN SELECT RAISE(ABORT, 'the force is refused'); END`, pr.ID)); err != nil {
		t.Fatal(err)
	}
	fixer(h, git, fixPushed, func(run store.Run) string { return fixReport(run.ID, "b2b2b2b Fix", fixReplied, "none") })
	h.fix(FixPayload{})
	h.handle()
	if _, err := h.st.DB().ExecContext(h.ctx, "DROP TRIGGER refuse_force"); err != nil {
		t.Fatal(err)
	}

	cur := h.pr(2)
	if cur.HeadSHA != fixPushed || cur.Forced || cur.State != store.PRRereviewPending {
		t.Fatalf("PR = state %s head %s forced %v, want rereview_pending, waiting for the re-review of %s", cur.State, cur.HeadSHA, cur.Forced, fixPushed)
	}
	if n := h.eventCount("pr:talkable/talkable#2", "pr.head_changed"); n != 1 {
		t.Errorf("pr.head_changed events = %d, want 1", n)
	}
	if got := h.e.pushTimes(h.ctx, pr.ID); len(got) == 0 || !got[len(got)-1].Equal(h.clock.Now()) {
		t.Errorf("recorded pushes %v lack the fixer's", got)
	}
	if ev, ok := h.fixEvent("fix.failed"); !ok || !strings.Contains(ev.Message, "the re-review could not be queued") {
		t.Errorf("fix.failed = %+v, %v", ev, ok)
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}

	h.advance(time.Minute)
	reqPollNoSettle(h, prSpec{n: 2, head: fixPushed, author: "zhuravel"})
	h.settle()
	if n := h.eventCount("pr:talkable/talkable#2", "pr.head_changed"); n != 1 {
		t.Errorf("pr.head_changed events after the poll = %d, want 1", n)
	}
}

// The fix job lays out only the fixer's pane: the round's roles stay parked
// (or get their panes from the next round's prepare), so no "starting"
// session rows exist for agents nobody started, which `magnum open` would
// try to focus (a live loop on 2026-10-11: agent_not_found on the judge's pane).
func TestTheFixJobLaysOutOnlyTheFixersPane(t *testing.T) {
	h, _ := fixHarness(t, realSleep)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	h.slot("review1")
	id := h.fix(FixPayload{})
	h.e.handleRequests(h.ctx) // the job runs on: the fixer works
	h.wantDone(id)
	h.awaitSubmit()
	last := "" // the round's own workspace call comes first; the job's is the last
	for _, c := range h.ag.all() {
		if strings.HasPrefix(c, fmt.Sprintf("ensure_workspace:%d:", pr.ID)) {
			last = c
		}
	}
	if layout := last[strings.LastIndex(last, ":")+1:]; last == "" || layout != "claude-fix" {
		t.Errorf("the fix job's workspace layout = %q, want only the fixer's pane", layout)
	}
}

// A change to the PR description with no commit ends the loop like a cycle
// that pushed nothing, and the toast and the event say the description
// changed: nothing to re-review, the next round verifies the description.
func TestADescriptionChangeAloneEndsTheLoopWithItsToast(t *testing.T) {
	h, git := fixHarness(t)
	pr := fixablePR(h, "zhuravel", map[string]int{"P2": 3})
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "description: the Deploy section lists the four migrations", fixReplied, "none")
	})
	h.fix(FixPayload{})
	h.tick()
	if !slices.Contains(h.sl.all(), "resync:review1:"+fixHead) {
		t.Errorf("slot calls %v lack the resync to the reviewed head", h.sl.all())
	}
	if _, ok := h.fixStateOf(2); ok {
		t.Error("the loop goes on")
	}
	h.wantState(2, store.PRReviewed)
	if !h.hasEvent(fmt.Sprintf("pr:talkable/talkable#%d", pr.Number), "fix.description") {
		t.Error("no fix.description event")
	}
	h.flush()
	if got := toastsWith(h, "PR #2: claude-fix updated the description, replied to 3 findings"); len(got) != 1 {
		t.Errorf("toasts = %v", h.nh.shown())
	}
	h.wantToasts("needs attention", 0)
}

// keptHead is a head that kept the verdict of the review on fixHead: a push
// that only merged the base, then a round of the judge that only replied.
const keptHead = "d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4"

// stoodPR is PR #2 reviewed on fixHead with an earlier P2 kept open, whose
// head then moved to keptHead with the review standing (reviewed at
// keptHead, its slot and HEAD there), with the judge's own pass of the
// review's round recorded after the review (outcome ok, as it ends) and a
// round on keptHead whose judge run ended outcome.
func stoodPR(h *harness, git *fixGit, outcome string) store.PR {
	h.t.Helper()
	pr := fixablePROpen(h, "zhuravel", map[string]int{"P0": 0, "P1": 0, "P2": 0, "P3": 0}, map[string]int{"P0": 0, "P1": 0, "P2": 1, "P3": 0})
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) {
		u.Set("head_sha", keptHead)
		u.Set("reviewed_sha", keptHead)
	}); err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.UpdateSlotFields(h.ctx, h.slot("review1").ID, func(u *store.SlotUpdate) { u.Set("checked_out_sha", keptHead) }); err != nil {
		h.t.Fatal(err)
	}
	git.setHead(keptHead)
	insert := func(id string, round int, kind, sha, outcome string) {
		h.advance(time.Minute)
		if _, err := h.st.DB().ExecContext(h.ctx, `INSERT INTO runs (id, pr_id, round, role, kind, target_sha, identity, reviewer_login,
  state, outcome, prompt_text, created_at) VALUES (?, ?, ?, 'codex-judge', ?, ?, ?, 'talkable[bot]', 'verified', ?, 'p', ?)`,
			id, pr.ID, round, kind, sha, pr.Identity, outcome, store.FormatTime(h.clock.Now())); err != nil {
			h.t.Fatal(err)
		}
	}
	insert("run-own-2", 1, store.RunOwnPass, fixHead, "ok")
	insert("run-reply-2", 2, store.RunRecovery, keptHead, outcome)
	return h.pr(2)
}

// A push that only merges the base keeps the review's verdict, and a round
// of the judge on the new head that only replied in the threads (its
// verdict and event stayed the last review's) posts no review: the head is
// reviewed, by the last posted review, and the earlier P2 that review keeps
// open is still to fix. `magnum fix` (the board's f) starts on the head
// with that review: the fixer's run is on the head, its prompt names the
// review and the review's file, the slot goes back to the head after it,
// and the answer and the fix.start event say whose review it is. A round on
// the head that ended otherwise (an error) leaves the head without a review
// to fix, as before.
func TestAFixStartsAfterARoundThatOnlyReplied(t *testing.T) {
	t.Run("replied", func(t *testing.T) {
		const what = "fixes the 1 finding of the review on a1a1a1a (the head d4d4d4d kept its verdict), 1 of them open from earlier reviews, in review1"
		h, git := fixHarness(t)
		pr := stoodPR(h, git, "replied")
		fixer(h, git, "", func(run store.Run) string { return fixReport(run.ID, "nothing", fixReplied, "none") })
		id := h.fix(FixPayload{})
		h.handle()
		if res := h.wantDone(id); !strings.Contains(res, "claude-fix "+what+", cycle 1 of 3") {
			t.Errorf("answer = %q, want it to say %q", res, what)
		}
		if ev, ok := h.fixEvent("fix.start"); !ok || !strings.Contains(ev.Message, what+" (run ") {
			t.Errorf("fix.start = %q, %v; want it to say %q", ev.Message, ok, what)
		}
		runs := h.fixRuns(pr.ID)
		if len(runs) != 1 || runs[0].TargetSHA != keptHead || runs[0].Round != 1 {
			t.Fatalf("fix runs = %+v, want one on %s in the review's round", runs, keptHead)
		}
		reviewFile := filepath.Join(h.layout.ReviewDir("talkable", "talkable", 2, fixHead), "review.json")
		want := fmt.Sprintf("fix_prompt:claude-fix:%s|feature|%s|%s|", runs[0].ID, fixReviewURL, reviewFile)
		if !slices.ContainsFunc(h.ag.all(), func(c string) bool { return strings.HasPrefix(c, want) }) {
			t.Errorf("no prompt naming the review %s and its file in %v", fixReviewURL, h.ag.all())
		}
		if !slices.Contains(h.sl.all(), "resync:review1:"+keptHead) {
			t.Errorf("slot calls %v lack the resync to the head %s", h.sl.all(), keptHead)
		}
	})
	t.Run("a round that ended otherwise", func(t *testing.T) {
		h, git := fixHarness(t)
		stoodPR(h, git, "error")
		id := h.fix(FixPayload{})
		h.handle()
		if r, want := h.request(id), "nothing to fix: no review on d4d4d4d"; r.State != store.RequestFailed || deref(r.Result) != want {
			t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), want)
		}
		if h.ag.count("submit:") != 0 {
			t.Error("a refused request started the fixer")
		}
	})
	// A push that only merged the base, with no round since, keeps the
	// verdict too, but moves no checkout: the fix is refused before it
	// starts, instead of failing its guard.
	t.Run("no round since, the slot at the review", func(t *testing.T) {
		h, git := fixHarness(t)
		stoodPR(h, git, "replied")
		if _, err := h.st.DB().ExecContext(h.ctx, "DELETE FROM runs WHERE id = 'run-reply-2'"); err != nil {
			t.Fatal(err)
		}
		if err := h.st.UpdateSlotFields(h.ctx, h.slot("review1").ID, func(u *store.SlotUpdate) { u.Set("checked_out_sha", fixHead) }); err != nil {
			t.Fatal(err)
		}
		id := h.fix(FixPayload{})
		h.handle()
		want := "talkable/talkable#2: slot review1 holds a1a1a1a, not its head d4d4d4d, which kept the verdict of the review on a1a1a1a; " +
			"`magnum review talkable/talkable#2` reviews it first"
		if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != want {
			t.Errorf("%s %q, want failed %q", r.State, deref(r.Result), want)
		}
		if h.ag.count("submit:") != 0 {
			t.Error("a refused request started the fixer")
		}
	})
}
