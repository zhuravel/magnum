package factory

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/prstatus"
	"github.com/zhuravel/magnum/internal/store"
)

// statusPath is the status file of the follow rig's PR.
func (r *followRig) statusPath() string {
	r.t.Helper()
	p, err := prstatus.Path(r.layout, "example", "shop", followRef.Number)
	if err != nil {
		r.t.Fatal(err)
	}
	return p
}

// status is the status file of the follow rig's PR, as magnum reads it;
// ok is false when there is none.
func (r *followRig) status() (prstatus.Status, bool) {
	r.t.Helper()
	s, ok, err := prstatus.Read(r.statusPath())
	if err != nil {
		r.t.Fatal(err)
	}
	return s, ok
}

// mustStatus is status for a file that must be there.
func (r *followRig) mustStatus() prstatus.Status {
	r.t.Helper()
	s, ok := r.status()
	if !ok {
		r.t.Fatal("no status file for the published PR")
	}
	return s
}

// taskJSON writes v as file name of the rig's task directory.
func (r *followRig) taskJSON(name string, v any) {
	r.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		r.t.Fatal(err)
	}
	r.taskFile(r.task.ID, name, string(b))
}

// published records what the factory did for the rig's task before its
// publish: three questions asked (one voided by a later revision), two of
// them answered; a ready card of 3 commits whose local review ended clean
// after 2 rounds with 3 notes left open; a proof red on the base and green
// at the head with 2 spec files. Every text in them carries marker.
func (r *followRig) published(marker string) {
	r.t.Helper()
	ctx, id := r.ctx, r.task.ID
	if err := r.reg.ReplaceQuestions(ctx, id, 1, []registry.Question{question("Q1 " + marker), question("Q2 " + marker)}); err != nil {
		r.t.Fatal(err)
	}
	if err := r.reg.AnswerBatch(ctx, id, 1, map[int]string{0: "A1 " + marker}); err != nil {
		r.t.Fatal(err)
	}
	if err := r.reg.ReplaceQuestions(ctx, id, 2, []registry.Question{question("Q3 " + marker), question("Q4 " + marker)}); err != nil {
		r.t.Fatal(err)
	}
	if err := r.reg.AnswerBatch(ctx, id, 2, map[int]string{0: "A3 " + marker}); err != nil {
		r.t.Fatal(err)
	}
	item := ReviewItem{ID: "F1", Severity: "P3", Title: "title " + marker, Path: "app/" + marker + ".rb", Body: "body " + marker}
	declined := item
	declined.Risk = "risk " + marker
	r.taskJSON(readyCardFile, ReadyCardFile{Revision: 1, Drawn: r.clock.Now(), Head: prHead, Base: strings.Repeat("b", 40), Commits: 3,
		Branch: "zhuravel-PR-12360", Title: "PR-12360 " + marker, Flags: []string{"flag " + marker},
		Review: ReviewSummary{Exit: registry.ExitClean, Rounds: 2, Head: prHead, Leftovers: []ReviewItem{item}, Declined: []ReviewItem{declined},
			Missing: []string{"codex-review"}},
		Files: []CardFile{{Path: "app/" + marker + ".rb", Added: 3, InPlan: true}}})
	r.taskJSON(factoryProofFile, Proof{Specs: []string{"spec/a_spec.rb", "spec/b_spec.rb"}, HeadSHA: prHead, BaseSHA: strings.Repeat("b", 40),
		Base: ProofRun{Command: "rspec " + marker, Exit: 1, Output: "output " + marker}, Head: ProofRun{Command: "rspec " + marker, Output: marker},
		Passed: true})
	r.taskFile(id, reportFile, "## Cause\n\n"+marker+"\n")
	r.taskFile(id, prBodyFile, "## What\n\n"+marker+"\n")
}

// A published task's status file holds what the factory did, in counts and
// fixed words: the questions asked and answered, the commits, the proof
// and its specs, the local review's rounds, verdict and notes left open,
// the publish time, magnum's fix loop, no staging, and who the PR waits on.
// The task's Jira key is left out while the factory knows no status of it.
func TestAPublishedPRsStatusFileHoldsTheFactorysCountsAndWords(t *testing.T) {
	r := newFollowRig(t)
	r.published("words")
	link, err := r.reg.LinkedPR(r.ctx, r.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.magnum.setPR(followRef, &PRState{Found: true, DaemonUp: true, Watched: true, OpenWatch: true, GHState: store.GHOpen, Head: prHead,
		State: store.PRReviewed, HasOpen: true, Open: engine.OpenVerdict{Verdict: engine.OpenIdle, Why: "nothing open", Head: prHead}})
	r.magnum.setFix(followRef, FixRecord{Loops: 1, Cycle: 2, Pushes: 2, Threads: 5})
	r.clock.Add(time.Hour)
	r.tick()

	got := r.mustStatus()
	want := prstatus.Status{V: prstatus.Version, Digest: got.Digest, UpdatedAt: r.clock.Now().UTC(), TaskID: r.task.ID,
		Plan:        prstatus.Plan{QuestionsAsked: 3, QuestionsAnswered: 2},
		Code:        prstatus.Code{Commits: 3, Proof: prstatus.ProofRedToGreen, Specs: 2},
		LocalReview: prstatus.LocalReview{Rounds: 2, Verdict: prstatus.VerdictClean, NotesOpen: 3},
		PublishedAt: link.Opened.UTC().Truncate(time.Second),
		FixLoop:     prstatus.FixLoop{State: prstatus.FixDone, Cycle: 2, Pushes: 2, ThreadsAnswered: 5},
		Staging:     prstatus.Staging{State: prstatus.StagingNone},
		Now:         prstatus.Now{Who: prstatus.WhoReviewers, Stage: prstatus.StagePublished}}
	if got != want {
		t.Fatalf("status file\n%+v\nwant\n%+v", got, want)
	}
}

// Nothing is written for a task without a published PR: one at its
// publish whose PR is linked but whose move to published was cut, one
// still at work, and one that ended before the writer ever wrote its file.
func TestNoStatusFileForATaskWithoutAPublishedPR(t *testing.T) {
	r := newFollowRig(t)
	cut := r.harness.task("PR-12361", registry.TaskActive, "publish")
	if err := r.reg.LinkPR(r.ctx, registry.PRLink{TaskID: cut.ID, Repo: "example/shop", Number: 4531, Branch: "zhuravel-PR-12361"}); err != nil {
		t.Fatal(err)
	}
	r.harness.task("PR-12362", registry.TaskActive, "coding")
	old, err := r.reg.CreateTask(r.ctx, registry.Task{JiraKey: "PR-12363", Kind: "bug", Repo: "example/shop", Title: "old", State: registry.TaskDone,
		PRNumber: 4533})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.reg.LinkPR(r.ctx, registry.PRLink{TaskID: old.ID, Repo: "example/shop", Number: 4533, Branch: "zhuravel-PR-12363"}); err != nil {
		t.Fatal(err)
	}
	r.tick()
	r.tick()
	if _, ok := r.status(); !ok {
		t.Fatal("the published task has no status file")
	}
	var names []string
	if err := filepath.WalkDir(r.layout.FactoryPRStatus(), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, filepath.Base(p))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "4530.json" {
		t.Errorf("status files %v, want only the published task's 4530.json", names)
	}
}

// A second tick with nothing changed writes nothing: the file keeps the
// time of its last change, byte for byte.
func TestAnUnchangedPRStatusIsNotWrittenAgain(t *testing.T) {
	r := newFollowRig(t)
	r.published("same")
	r.tick()
	first := r.mustStatus()
	before, err := os.ReadFile(r.statusPath())
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(r.statusPath(), old, old); err != nil {
		t.Fatal(err)
	}
	r.clock.Add(10 * time.Minute)
	r.tick()
	after, err := os.ReadFile(r.statusPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := r.mustStatus(); !got.UpdatedAt.Equal(first.UpdatedAt) || string(before) != string(after) {
		t.Errorf("a tick with nothing changed rewrote the file: updated %v, was %v", got.UpdatedAt, first.UpdatedAt)
	}
	if st, err := os.Stat(r.statusPath()); err != nil || !st.ModTime().Equal(old) {
		t.Errorf("the file was written again (%v, %v)", st, err)
	}
}

// A change of magnum's fix loop on the PR changes the file's fix loop, and
// its time: a loop that runs, its pushes and threads, its end on you while
// the PR needs your attention, done once it no longer does, and fix off.
func TestAChangeOfMagnumsFixLoopChangesTheStatusFile(t *testing.T) {
	r := newFollowRig(t)
	r.published("fix")
	r.tick()
	if got := r.mustStatus().FixLoop; got != (prstatus.FixLoop{State: prstatus.FixNone}) {
		t.Fatalf("no loop yet: %+v", got)
	}
	open := PRState{Found: true, DaemonUp: true, Watched: true, GHState: store.GHOpen, Head: prHead, State: store.PRReviewed}
	fixing := func(cycle int) func(*PRState) {
		return func(p *PRState) {
			p.Fixing, p.Fix = true, engine.FixState{Cycle: cycle, Cap: 3, Phase: engine.FixPhaseFixing}
		}
	}
	after := FixRecord{Loops: 1, Cycle: 2, Pushes: 1, Threads: 4}
	onYou := after
	onYou.LeftToYou = true
	steps := []struct {
		name string
		pr   func(*PRState)
		fix  FixRecord
		want prstatus.FixLoop
	}{
		{"a loop runs", fixing(1), FixRecord{Loops: 1, Cycle: 1}, prstatus.FixLoop{State: prstatus.FixRunning, Cycle: 1}},
		{"it pushed and replied", fixing(2), after, prstatus.FixLoop{State: prstatus.FixRunning, Cycle: 2, Pushes: 1, ThreadsAnswered: 4}},
		{"it ended on you", func(p *PRState) { p.State = store.PRNeedsAttention }, onYou,
			prstatus.FixLoop{State: prstatus.FixNeedsYou, Cycle: 2, Pushes: 1, ThreadsAnswered: 4}},
		{"you dealt with it", nil, onYou, prstatus.FixLoop{State: prstatus.FixDone, Cycle: 2, Pushes: 1, ThreadsAnswered: 4}},
		{"fix is off", func(p *PRState) { p.FixOff = true }, after, prstatus.FixLoop{State: prstatus.FixOff, Cycle: 2, Pushes: 1, ThreadsAnswered: 4}},
	}
	for _, s := range steps {
		pr := open
		if s.pr != nil {
			s.pr(&pr)
		}
		r.magnum.setPR(followRef, &pr)
		r.magnum.setFix(followRef, s.fix)
		r.clock.Add(time.Minute)
		r.tick()
		got := r.mustStatus()
		if got.FixLoop != s.want || !got.UpdatedAt.Equal(r.clock.Now().UTC()) {
			t.Errorf("%s: fix loop %+v updated %v, want %+v updated %v", s.name, got.FixLoop, got.UpdatedAt, s.want, r.clock.Now().UTC())
		}
	}
}

// A task that ends gets one last write with its end as its stage and
// waits on nobody; nothing it or magnum does after that is written, a
// restart of the daemon included.
func TestAnEndedTaskGetsOneLastWriteAndNoneAfter(t *testing.T) {
	for _, end := range []struct {
		name  string
		stage prstatus.Stage
		move  func(r *followRig)
	}{
		{"merged", prstatus.StageDone, func(r *followRig) { r.prIs(store.GHMerged, r.clock.Now()) }},
		{"stopped", prstatus.StageStopped, func(r *followRig) {
			if err := r.reg.TransitionTask(r.ctx, r.task.ID, []string{registry.TaskPublished}, registry.TaskStopped, nil); err != nil {
				r.t.Fatal(err)
			}
		}},
	} {
		t.Run(end.name, func(t *testing.T) {
			r := newFollowRig(t)
			r.published("end")
			r.tick()
			if got := r.mustStatus().Now; got.Stage != prstatus.StagePublished {
				t.Fatalf("before the end: %+v", got)
			}
			end.move(r)
			for range 3 {
				r.clock.Add(time.Minute)
				r.tick()
			}
			final := r.mustStatus()
			if final.Now != (prstatus.Now{Who: prstatus.WhoNone, Stage: end.stage}) {
				t.Fatalf("the last write's now: %+v, want none at %s", final.Now, end.stage)
			}
			before, err := os.ReadFile(r.statusPath())
			if err != nil {
				t.Fatal(err)
			}
			r.magnum.setFix(followRef, FixRecord{Loops: 2, Cycle: 1, Pushes: 3, Threads: 6})
			r.clock.Add(time.Minute)
			r.tick()
			r.restart()
			r.clock.Add(time.Minute)
			r.tick()
			after, err := os.ReadFile(r.statusPath())
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Errorf("the ended task's file was written again:\n%s\nwas\n%s", after, before)
			}
		})
	}
}

// A status file that cannot be written is logged once until a write
// succeeds, whatever the error says each time (a refused temporary file
// has a new random name on each tick), and costs the tick nothing: the PR
// is still followed. A failure after a write that worked is logged again.
func TestAFailedStatusWriteIsLoggedOnceAndTheTickGoesOn(t *testing.T) {
	for _, c := range []struct {
		name    string
		breakIt func(r *followRig) (mend func())
	}{
		{"a file where its folder goes", func(r *followRig) func() {
			path := filepath.Join(r.layout.FactoryPRStatus(), "example")
			if err := os.MkdirAll(r.layout.FactoryPRStatus(), 0o700); err != nil {
				r.t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("not a folder"), 0o600); err != nil {
				r.t.Fatal(err)
			}
			return func() {
				if err := os.Remove(path); err != nil {
					r.t.Fatal(err)
				}
			}
		}},
		{"a folder that refuses new files", func(r *followRig) func() {
			dir := filepath.Dir(r.statusPath())
			if err := os.MkdirAll(dir, 0o700); err != nil {
				r.t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o500); err != nil {
				r.t.Fatal(err)
			}
			r.t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			return func() {
				if err := os.Chmod(dir, 0o700); err != nil {
					r.t.Fatal(err)
				}
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newFollowRig(t)
			r.published("fail")
			mend := c.breakIt(r)
			for range 3 {
				r.clock.Add(time.Minute)
				if err := r.e.Tick(r.ctx); err != nil {
					t.Fatalf("a failed status write failed the tick: %v", err)
				}
			}
			if n := strings.Count(r.log.String(), "status file"); n != 1 {
				t.Errorf("the failed write is logged %d times over 3 ticks, want once:\n%s", n, r.log.String())
			}
			mend()
			r.tick()
			if _, ok := r.status(); !ok {
				t.Fatal("no status file once the folder takes it")
			}
			dir := filepath.Dir(r.statusPath())
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			r.magnum.setFix(followRef, FixRecord{Loops: 1, Cycle: 1, Pushes: 1})
			r.tick()
			r.tick()
			if n := strings.Count(r.log.String(), "status file"); n != 2 {
				t.Errorf("a failure after a write that worked is logged %d times in all, want 2:\n%s", n, r.log.String())
			}
			r.prIs(store.GHMerged, r.clock.Now())
			r.tick()
			if got := r.reload(r.task.ID); got.State != registry.TaskDone {
				t.Fatalf("after a failed status write the merged PR's task is %s, want done", got.State)
			}
		})
	}
}

// The fix loop's pushes and threads in the file never go down: magnum
// prunes old events ([daemon] keep_events), and a daemon that starts after
// it counts fewer, so the file keeps the larger of its count and the
// record's.
func TestTheFixLoopsCountsInTheStatusFileNeverGoDown(t *testing.T) {
	r := newFollowRig(t)
	r.published("pruned")
	r.magnum.setFix(followRef, FixRecord{Loops: 1, Cycle: 2, Pushes: 3, Threads: 6})
	r.tick()
	r.magnum.setFix(followRef, FixRecord{Loops: 1, Cycle: 2, Pushes: 1, Threads: 2})
	r.restart()
	r.clock.Add(time.Minute)
	r.tick()
	if got := r.mustStatus().FixLoop; got.Pushes != 3 || got.ThreadsAnswered != 6 {
		t.Errorf("after magnum pruned events: %+v, want 3 pushes and 6 threads kept", got)
	}
	r.magnum.setFix(followRef, FixRecord{Loops: 2, Cycle: 1, Pushes: 4, Threads: 7})
	r.tick()
	if got := r.mustStatus().FixLoop; got.Pushes != 4 || got.ThreadsAnswered != 7 {
		t.Errorf("after a new push: %+v, want 4 pushes and 7 threads", got)
	}
}

// No text of the PR, the ticket, a question, an agent or magnum's verdict
// reaches the file, whatever the records the writer reads hold.
func TestNoFreeTextReachesThePRStatusFile(t *testing.T) {
	const marker = "zq-free-text"
	r := newFollowRig(t)
	r.published(marker)
	if err := r.reg.TransitionTask(r.ctx, r.task.ID, nil, "", func(u *registry.TaskUpdate) {
		u.Set("title", "title "+marker)
		u.Set("guidance", "guidance "+marker)
	}); err != nil {
		t.Fatal(err)
	}
	r.magnum.setPR(followRef, &PRState{Found: true, DaemonUp: true, Watched: true, OpenWatch: true, GHState: store.GHOpen, Head: prHead,
		Title: "PR " + marker, URL: "https://github.com/example/shop/pull/4530", State: store.PRNeedsAttention, Attention: marker,
		AttentionMsg: "attention " + marker, HasOpen: true, Open: engine.OpenVerdict{Verdict: engine.OpenNeedsYou, Why: "why " + marker,
			Head: prHead}, Fixing: true, Fix: engine.FixState{Cycle: 1, Cap: 3, Phase: engine.FixPhaseFixing, Report: marker, Role: marker}})
	r.magnum.setFix(followRef, FixRecord{Loops: 1, Cycle: 1, Pushes: 1, Threads: 1})
	r.tick()
	b, err := os.ReadFile(r.statusPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), marker) {
		t.Errorf("free text reached the status file:\n%s", b)
	}
	if got := r.mustStatus(); got.Now.Who != prstatus.WhoMagnum || got.FixLoop.State != prstatus.FixRunning {
		t.Errorf("now %+v, fix loop %+v; want the PR waiting on magnum, which fixes it", got.Now, got.FixLoop)
	}
}

// Who a published PR waits on, in the file's fixed words, from the task's
// own wait and magnum's view of the PR (as the task's row says it).
func TestWhoAPublishedPRWaitsOnInTheStatusFile(t *testing.T) {
	up := PRState{Found: true, DaemonUp: true, Watched: true, OpenWatch: true, GHState: store.GHOpen, Head: prHead, State: store.PRReviewed}
	verdict := func(v, why string, approved bool) PRState {
		p := up
		p.HasOpen, p.Open, p.Approved = true, engine.OpenVerdict{Verdict: v, Why: why, Head: prHead}, approved
		return p
	}
	published := registry.Task{ID: 1, State: registry.TaskPublished, PRNumber: 4530}
	closedYou := published
	closedYou.WaitsWho, closedYou.WaitsWhat = waitsYou, closedWait(4530)
	grace := time.Hour
	closedAt := func(ago time.Duration) PRState {
		return PRState{Ref: followRef, Found: true, DaemonUp: true, GHState: store.GHClosed, ClosedAt: new(t0.Add(-ago))}
	}
	cases := []struct {
		name string
		t    registry.Task
		pr   PRState
		want prstatus.Who
	}{
		{"magnum looks", published, PRState{DaemonUp: true, Watched: true}, prstatus.WhoMagnum},
		{"magnum's daemon down", published, PRState{Found: true, Watched: true, GHState: store.GHOpen}, prstatus.WhoMagnum},
		{"magnum fixes", published, func() PRState { p := up; p.Fixing, p.Fix = true, engine.FixState{Cycle: 1, Cap: 3}; return p }(), prstatus.WhoMagnum},
		{"CI runs", published, verdict(engine.OpenWaits, "CI at abc", false), prstatus.WhoCI},
		{"the reviewers settle", published, verdict(engine.OpenWaits, "settling", false), prstatus.WhoReviewers},
		{"no approval yet", published, verdict(engine.OpenIdle, "nothing open", false), prstatus.WhoReviewers},
		{"approved: the merge is yours", published, verdict(engine.OpenIdle, "nothing open", true), prstatus.WhoYou},
		{"the PR needs you", published, verdict(engine.OpenNeedsYou, "a thread", false), prstatus.WhoYou},
		{"it needs your attention", published, func() PRState { p := up; p.OpenWatch, p.State = false, store.PRNeedsAttention; return p }(), prstatus.WhoYou},
		{"fix is off", published, func() PRState { p := verdict(engine.OpenOff, "", false); p.Open.Off = engine.OpenOffFixOff; return p }(),
			prstatus.WhoYou},
		{"a repository magnum does not watch", published, PRState{Ref: followRef, DaemonUp: true}, prstatus.WhoYou},
		{"closed, past the grace", closedYou, closedAt(2 * time.Hour), prstatus.WhoYou},
		{"closed past the grace, before the tick marks it", published, closedAt(2 * time.Hour), prstatus.WhoYou},
		{"closed, within the grace", published, closedAt(time.Minute), prstatus.WhoMagnum},
		{"merged", published, PRState{Found: true, DaemonUp: true, GHState: store.GHMerged}, prstatus.WhoNone},
	}
	for _, c := range cases {
		if got := nowOf(c.t, c.pr, prClock{now: t0, grace: grace, poll: 30 * time.Second}); got != (prstatus.Now{Who: c.want, Stage: prstatus.StagePublished}) {
			t.Errorf("%s: %+v, want %s at published", c.name, got, c.want)
		}
	}
	for state, stage := range map[string]prstatus.Stage{registry.TaskDone: prstatus.StageDone, registry.TaskStopped: prstatus.StageStopped,
		registry.TaskFailed: prstatus.StageFailed} {
		ended := published
		ended.State = state
		if got := nowOf(ended, verdict(engine.OpenNeedsYou, "a thread", false), prClock{now: t0, grace: grace, poll: 30 * time.Second}); got != (prstatus.Now{Who: prstatus.WhoNone, Stage: stage}) {
			t.Errorf("a task %s: %+v, want none at %s", state, got, stage)
		}
	}
}

// The proof's and the local review's words in the status file: the
// factory's proof as it stands (none, red on the base only, red to green,
// green for a kind whose proof is green) and the review's end.
func TestTheProofAndTheReviewInTheStatusFile(t *testing.T) {
	red := ProofRun{Command: "rspec", Exit: 1}
	for name, c := range map[string]struct {
		p    Proof
		ok   bool
		want prstatus.Proof
	}{
		"no proof":       {Proof{}, false, prstatus.ProofNone},
		"not run":        {Proof{Why: "proof.json names no spec"}, true, prstatus.ProofNone},
		"red on base":    {Proof{Base: red, Head: ProofRun{Command: "rspec", Exit: 1}}, true, prstatus.ProofRed},
		"passes on base": {Proof{Base: ProofRun{Command: "rspec"}}, true, prstatus.ProofNone},
		"red to green":   {Proof{Base: red, Head: ProofRun{Command: "rspec"}, Passed: true}, true, prstatus.ProofRedToGreen},
		"green":          {Proof{Head: ProofRun{Command: "rspec"}, Passed: true}, true, prstatus.ProofGreen},
	} {
		if got := proofOf(c.p, c.ok); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	for exit, want := range map[string]prstatus.Verdict{"": prstatus.VerdictNone, registry.ExitClean: prstatus.VerdictClean,
		registry.ExitOscillation: prstatus.VerdictFindings, registry.ExitCap: prstatus.VerdictFindings,
		registry.ExitOperator: prstatus.VerdictFindings, registry.ExitUnavailable: prstatus.VerdictFailed, "a word of later": prstatus.VerdictNone} {
		if got := verdictOf(exit); got != want {
			t.Errorf("exit %q: %s, want %s", exit, got, want)
		}
	}
}

// A failed read of magnum's fix records writes nothing (the file keeps
// its last content), logged once while it stays the same.
func TestAnUnreadableFixRecordKeepsTheStatusFile(t *testing.T) {
	r := newFollowRig(t)
	r.published("unread")
	r.tick()
	before, err := os.ReadFile(r.statusPath())
	if err != nil {
		t.Fatal(err)
	}
	r.magnum.setFixErr(errors.New("magnum's registry: events: disk I/O error"))
	r.magnum.setPR(followRef, &PRState{Found: true, DaemonUp: true, Watched: true, OpenWatch: true, GHState: store.GHOpen, Head: prHead,
		HasOpen: true, Open: engine.OpenVerdict{Verdict: engine.OpenNeedsYou, Why: "a thread", Head: prHead}})
	r.clock.Add(time.Minute)
	r.tick()
	r.tick()
	after, err := os.ReadFile(r.statusPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("an unreadable fix record changed the file:\n%s\nwas\n%s", after, before)
	}
	if n := strings.Count(r.log.String(), "disk I/O error"); n != 1 {
		t.Errorf("the failed read is logged %d times over 2 ticks, want once:\n%s", n, r.log.String())
	}
}

// The file of a PR the factory published counts what the factory did as
// its records hold it: the branch's commits after the base (the ready card
// counts them as it draws), the proof red on the base and green at the
// head with its spec, the local review's end and what it left open, and
// the publish time of the PR's link.
func TestThePublishedPRsStatusFileCountsTheCommitsOfItsCard(t *testing.T) {
	r := newReadyRig(t)
	task, _ := r.toCard("PR-12388")
	c := r.card(task.ID)
	if c.Commits != 1 {
		t.Fatalf("the card counts %d commits, want the writer's one", c.Commits)
	}
	if q := r.decideOn(task.ID, c, ChoicePush, "", ""); q.State != registry.RequestDone {
		t.Fatalf("push request %s: %q", q.State, q.Result)
	}
	got := r.published(task.ID)
	if got.State != registry.TaskPublished {
		t.Fatalf("task %s waits %q, want it published", got.State, got.WaitsWhat)
	}
	r.e.d.Magnum = &fakeMagnum{}
	r.tick()
	link, err := r.reg.LinkedPR(r.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	path, err := prstatus.Path(r.layout, "example", "shop", got.PRNumber)
	if err != nil {
		t.Fatal(err)
	}
	s, ok, err := prstatus.Read(path)
	if err != nil || !ok {
		t.Fatalf("the published PR's status file: %v, %v", ok, err)
	}
	wantReview := prstatus.LocalReview{Rounds: c.Review.Rounds, Verdict: prstatus.VerdictClean, NotesOpen: len(knownLines(c.Review, false))}
	if s.Code != (prstatus.Code{Commits: 1, Proof: prstatus.ProofRedToGreen, Specs: 1}) || s.LocalReview != wantReview || c.Review.Rounds < 1 ||
		!s.PublishedAt.Equal(link.Opened.Truncate(time.Second)) || s.TaskID != task.ID || s.Now.Stage != prstatus.StagePublished {
		t.Errorf("status file %+v; want 1 commit red to green with 1 spec, the review %+v, published at %v", s, wantReview, link.Opened)
	}
}
