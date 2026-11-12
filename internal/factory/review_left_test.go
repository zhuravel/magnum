package factory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// leavingRig is a review rig whose round runs leave first: a reviewer's
// leftovers in the copy (dir), as a `bundle check` or a probe file does.
func leavingRig(t *testing.T, leave func(r *reviewRig, dir string)) *reviewRig {
	t.Helper()
	var rig *reviewRig
	rig = newReviewRig(t, func(c evalrun.LocalCase) evalrun.LocalResult {
		leave(rig, c.Checkout)
		return result()(c)
	})
	return rig
}

// leftAfterYouTyped runs task key's round, which leaves a tracked change
// while you typed into one of the task's panes since the round began: the
// change may be your own edit, so the factory saves it and asks you.
func leftAfterYouTyped(t *testing.T, key string) (*reviewRig, registry.Task) {
	t.Helper()
	r := leavingRig(t, func(r *reviewRig, dir string) {
		r.put(dir, "README", "your edit, or a reviewer's\n")
		r.rt.setTouchedAt("you typed into a pane of "+key+" at 10:00:00", r.clock.Now().Add(time.Second))
	})
	task, _ := r.reviewTask(key)
	r.run(6, func() bool { return !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	return r, got
}

// A round's tracked change while you typed into a pane of the task since
// the round began may be your own edit: the factory saves its diff to
// review-1-left.diff and asks you, with two options, never "retry", and puts
// nothing back by itself. "Restore the copy to the reviewed head" puts it
// back and the round's result stands; "keep the change" leaves the copy as
// it is and the round's result stands too.
func TestARoundsChangeAfterYouTypedAsksToRestoreOrKeepIt(t *testing.T) {
	for _, tc := range []struct {
		answer, readme string
	}{
		{optionRestoreCopy, "shop\n"},
		{optionKeepChange, "your edit, or a reviewer's\n"},
	} {
		t.Run(tc.answer, func(t *testing.T) {
			r, got := leftAfterYouTyped(t, "PR-12398")
			if got.State != registry.TaskNeedsYou || !strings.HasSuffix(got.WaitsWhat, "restore the copy to the reviewed head or keep the change?") ||
				strings.Contains(strings.ToLower(got.WaitsWhat), "retry") {
				t.Fatalf("task %s waits %q, want the leftovers' question", got.State, got.WaitsWhat)
			}
			qs := r.newestQuestions(got.ID)
			if len(qs) != 1 || !slices.Equal(optionLabels(qs[0]), []string{optionRestoreCopy, optionKeepChange}) ||
				!strings.Contains(qs[0].Example, "your edit, or a reviewer's") || !strings.Contains(qs[0].Evidence, "review-1-left.diff") {
				t.Fatalf("questions %+v, want restore and keep, with the saved diff", qs)
			}
			if r.read("README") != "your edit, or a reviewer's\n" || !strings.Contains(r.readTask(got.ID, "review-1-left.diff"), "+your edit") {
				t.Fatal("the factory put back what may be yours, or did not save it first")
			}
			r.rt.setTouchedAt("", time.Time{})
			r.mustAnswer(AnswerPayload{TaskID: got.ID, Revision: got.QuestionRev, Answers: map[int]string{0: tc.answer}})
			rounds := 1
			if tc.answer == optionKeepChange { // the writer commits what you kept; no round follows
				r.commitsKept(got, 1, tc.readme)
			}
			r.run(10, func() bool { return r.reload(got.ID).Stage != "local_review" || !isLive(r.reload(got.ID)) })
			if after := r.reload(got.ID); after.Stage != "pr_body" || !isLive(after) || len(r.rv.calls()) != rounds {
				t.Fatalf("task %s at %s waits %q after %d rounds; want pr_body after %d", after.State, after.Stage,
					after.WaitsWhat, len(r.rv.calls()), rounds)
			}
			if r.read("README") != tc.readme {
				t.Fatalf("README is %q after your answer, want %q", r.read("README"), tc.readme)
			}
		})
	}
}

// failSave makes the factory's save of what a round left fail (git cannot
// write the patch of its tracked changes): the put-back's steps go to you
// with nothing saved.
func (r *reviewRig) failSave() {
	r.setFailGit(func(args []string) error {
		if slices.Contains(args, "diff") && slices.Contains(args, "--binary") {
			return errors.New("fatal: unable to read the index")
		}
		return nil
	})
}

// put writes text to rel in the copy at dir from a round's goroutine (no
// t.Fatal there).
func (r *reviewRig) put(dir, rel, text string) {
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Error(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		r.t.Error(err)
	}
}

// inCopy reports whether rel is in the rig's copy (a symlink counts).
func (r *reviewRig) inCopy(rel string) bool {
	_, err := os.Lstat(filepath.Join(r.copyDir, rel))
	return err == nil
}

// saved reports whether rel is in task id's directory.
func (r *reviewRig) saved(id int64, rel string) bool {
	_, err := os.Lstat(filepath.Join(r.layout.FactoryTaskDir(id), rel))
	return err == nil
}

// reviewInfos are task id's info events of the review.
func (r *reviewRig) reviewInfos(id int64) []string {
	r.t.Helper()
	var out []string
	for _, ev := range r.events(id, "review") {
		if ev.Phase == registry.PhaseInfo {
			out = append(out, ev.Text)
		}
	}
	return out
}

// A round that leaves a tracked change (a reviewer's `bundle check` adds a
// line to Gemfile.lock) at the head it reviewed, with no turn of an agent
// of the task in flight in the copy, has it saved and put back: the change
// goes to review-1-left.diff in the task's directory, the copy is as the
// round found it, one info event says so, and the round's result is read:
// the task goes on, never to you.
func TestARoundsTrackedChangeIsSavedAndPutBackWhenNoTurnIsInFlight(t *testing.T) {
	r := leavingRig(t, func(r *reviewRig, dir string) { r.put(dir, "README", "shop\nrake (13.3.0)\n") })
	r.rt.setTouchedAt("you typed into a pane of PR-12390 at 09:00:00", r.clock.Now())
	r.clock.Add(time.Minute)
	task, _ := r.reviewTask("PR-12390")
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.Stage != "pr_body" || !isLive(got) {
		t.Fatalf("task %s at %s waits %q; want it on at pr_body", got.State, got.Stage, got.WaitsWhat)
	}
	if got := r.read("README"); got != "shop\n" {
		t.Fatalf("README is %q, want it back as the round found it", got)
	}
	if patch := r.readTask(task.ID, "review-1-left.diff"); !strings.Contains(patch, "diff --git a/README b/README") ||
		!strings.Contains(patch, "+rake (13.3.0)") {
		t.Fatalf("review-1-left.diff:\n%s", patch)
	}
	want := "round 1 of the local review left 1 tracked change in the copy (README); saved to review-1-left.diff, " +
		"the copy is back as the round found it"
	if infos := r.reviewInfos(task.ID); !slices.Equal(infos, []string{want}) {
		t.Fatalf("review info events %q, want %q", infos, want)
	}
	if ps := r.rt.promptsTo("fx-writer"); len(ps) != 0 {
		t.Fatalf("the writer got %d prompts over the round's leftovers", len(ps))
	}
}

// A round that leaves new untracked files while nobody else could have
// made them has each saved under review-1-left/ in the task's directory, at
// its path in the copy, and removed from the copy: a file, and each file of
// a new directory, whose emptied directories go too. A file git ignores in
// that directory is neither saved nor removed. The task goes on.
func TestARoundsNewUntrackedFilesAreSavedAndRemovedWhenNobodyElseCouldHaveMadeThem(t *testing.T) {
	r := leavingRig(t, func(r *reviewRig, dir string) {
		r.put(dir, "spec/probe_spec.rb", "a reviewer's probe\n")
		r.put(dir, "probe/deep/credit_spec.rb", "a deeper probe\n")
		r.put(dir, "probe/tmp/cache", "ignored\n")
	})
	task, _ := r.reviewTask("PR-12391")
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.Stage != "pr_body" || !isLive(got) {
		t.Fatalf("task %s at %s waits %q; want it on at pr_body", got.State, got.Stage, got.WaitsWhat)
	}
	if got := r.readTask(task.ID, "review-1-left/spec/probe_spec.rb"); got != "a reviewer's probe\n" {
		t.Fatalf("saved probe %q", got)
	}
	if got := r.readTask(task.ID, "review-1-left/probe/deep/credit_spec.rb"); got != "a deeper probe\n" {
		t.Fatalf("saved deeper probe %q", got)
	}
	if r.saved(task.ID, "review-1-left/probe/tmp/cache") || r.saved(task.ID, "review-1-left.diff") {
		t.Fatal("the factory saved an ignored file or a patch of no tracked change")
	}
	if r.inCopy("spec/probe_spec.rb") || r.inCopy("probe/deep") || !r.inCopy("probe/tmp/cache") {
		t.Fatal("the copy is not as the round found it, or lost an ignored file")
	}
	want := "round 1 of the local review left 2 new untracked files in the copy (probe/deep/credit_spec.rb, spec/probe_spec.rb); " +
		"saved to review-1-left/, the copy is back as the round found it"
	if infos := r.reviewInfos(task.ID); !slices.Equal(infos, []string{want}) {
		t.Fatalf("review info events %q, want %q", infos, want)
	}
}

// A round's tracked change is restored by the factory itself once no turn
// of an agent of the task is in flight in the copy and you are in none of
// its panes: it is saved to review-1-left.diff (mode 0600), put back, and the
// round's result stands. While an agent of the task works in the copy, or
// you are in its pane (asked again just before the put-back, after the
// save), nothing is put back and nothing is kept saved: the task waits,
// never on you, and the put-back runs once the turn ended or you left.
func TestARoundsTrackedChangeIsRestoredOnceNoTurnIsInFlight(t *testing.T) {
	for _, tc := range []struct {
		name, waits string
		other       func(r *reviewRig)
		gone        func(r *reviewRig)
	}{
		{"nobody else in the copy", "", func(*reviewRig) {}, func(*reviewRig) {}},
		{"an agent at work in the copy", "its turn ended (the fx-writer agent works in it)",
			func(r *reviewRig) { r.rt.setAtWork("fx-writer") },
			func(r *reviewRig) { r.rt.setAtWork("") }},
		// The guard is asked again after the save, just before the put-back.
		{"you came into a pane while the factory saved it", "you left the pane (you are in the fx-writer pane)",
			func(r *reviewRig) {
				r.setFailGit(func(args []string) error {
					if slices.Contains(args, "diff") && slices.Contains(args, "--binary") {
						r.rt.setOperatorIn("fx-writer")
					}
					return nil
				})
			},
			func(r *reviewRig) {
				r.setFailGit(nil)
				r.rt.setOperatorIn("")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := leavingRig(t, func(r *reviewRig, dir string) {
				r.put(dir, "README", "a reviewer's mutant\n")
				tc.other(r)
			})
			task, _ := r.reviewTask("PR-12392")
			if tc.waits != "" {
				r.run(6, func() bool { return strings.Contains(r.reload(task.ID).WaitsWhat, tc.waits) })
				got := r.reload(task.ID)
				if got.State == registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, tc.waits) {
					t.Fatalf("task %s waits %q; want a wait until %q, never on you", got.State, got.WaitsWhat, tc.waits)
				}
				if r.read("README") != "a reviewer's mutant\n" || r.saved(task.ID, "review-1-left.diff") {
					t.Fatal("the factory put back, or kept a save of, what a round left while a turn was in flight")
				}
				tc.gone(r)
			}
			r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
			if got := r.reload(task.ID); got.Stage != "pr_body" || !isLive(got) || len(r.rv.calls()) != 1 {
				t.Fatalf("task %s at %s waits %q after %d rounds; want pr_body on the one round's result", got.State, got.Stage,
					got.WaitsWhat, len(r.rv.calls()))
			}
			if r.read("README") != "shop\n" {
				t.Fatal("README is not back as the round found it")
			}
			fi, err := os.Stat(filepath.Join(r.layout.FactoryTaskDir(task.ID), "review-1-left.diff"))
			if err != nil || fi.Mode().Perm() != 0o600 || !strings.Contains(r.readTask(task.ID, "review-1-left.diff"), "+a reviewer's mutant") {
				t.Fatalf("review-1-left.diff %v (%v), want the round's change kept, private", fi, err)
			}
			if !slices.ContainsFunc(r.events(task.ID, "alert"), func(e registry.Event) bool {
				return strings.Contains(e.Text, "put back by itself")
			}) {
				t.Fatal("no alert says the factory put the copy back by itself")
			}
		})
	}
}

// What a round left that the factory cannot save first stays as it is,
// and so does what it cannot put back: a symlink among the new files is
// not saved (only regular files are), a patch git cannot write is none,
// and a put-back git refuses changes nothing. The task needs you with why;
// what was saved before a put-back failed stays in the task's directory,
// and a save that failed leaves nothing there.
func TestARoundsLeftoversThatCannotBeSavedOrPutBackWaitForYou(t *testing.T) {
	for _, tc := range []struct {
		name, why string
		leave     func(r *reviewRig, dir string)
		saved     bool
	}{
		{"a symlink among the new files", "spec/link: not a regular file",
			func(r *reviewRig, dir string) {
				if err := os.Symlink("../README", filepath.Join(dir, "spec", "link")); err != nil {
					r.t.Error(err)
				}
			}, false},
		{"a patch git cannot write", "the factory could not save it",
			func(r *reviewRig, _ string) {
				r.setFailGit(func(args []string) error {
					if slices.Contains(args, "diff") && slices.Contains(args, "--binary") {
						return errors.New("fatal: unable to read the index")
					}
					return nil
				})
			}, false},
		{"a put-back git refuses", "Unable to create index.lock",
			func(r *reviewRig, _ string) { r.failRestores(false) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := leavingRig(t, func(r *reviewRig, dir string) {
				r.put(dir, "README", "a reviewer's mutant\n")
				tc.leave(r, dir)
			})
			task, _ := r.reviewTask("PR-12393")
			r.run(6, func() bool { return !isLive(r.reload(task.ID)) })
			got := r.reload(task.ID)
			if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "round 1 of the local review left") ||
				!strings.Contains(got.WaitsWhat, tc.why) || !strings.Contains(got.WaitsWhat, fmt.Sprintf("magnum factory retry %d", task.ID)) {
				t.Fatalf("task %s waits %q; want you, naming %q", got.State, got.WaitsWhat, tc.why)
			}
			if r.read("README") != "a reviewer's mutant\n" {
				t.Fatal("the factory put back what it could not save")
			}
			if saved := r.saved(task.ID, "review-1-left.diff"); saved != tc.saved || r.saved(task.ID, "review-1-left") {
				t.Fatalf("review-1-left.diff saved %v, want %v (or a directory of no new file)", saved, tc.saved)
			}
			if tc.saved && !strings.Contains(got.WaitsWhat, "review-1-left.diff") {
				t.Fatalf("the wait %q does not name the saved patch", got.WaitsWhat)
			}
		})
	}
}

// A file that was in the copy before the round is never the round's, however
// git status groups it: an untracked directory's old file, which git names as
// the directory before the round and as itself once the round stages a new
// file beside it, stays in the copy, unsaved and unnamed, while the staged
// file goes back. A listing an older build took by git status entry (the
// directory) keeps it too, and cannot tell the staged file from the copy's
// own either: it goes back out of the index only, and stays.
func TestAFileFromBeforeTheRoundSurvivesThePutBack(t *testing.T) {
	for _, tc := range []struct {
		name    string
		listing string // the listing before the round as an older build wrote it ("" for this build's)
		stays   bool   // the round's staged file stays in the copy, untracked
	}{
		{"this build's listing, file by file", "", false},
		{"an older build's listing, by git status entry", `["probe/"]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var task registry.Task
			r := leavingRig(t, func(r *reviewRig, dir string) {
				if tc.listing != "" {
					r.put(r.layout.FactoryTaskDir(task.ID), untrackedBeforeFile(1), tc.listing)
				}
				r.put(dir, "probe/new_spec.rb", "a reviewer's staged probe\n")
				r.gitIn(dir, "add", "--", "probe/new_spec.rb")
			})
			task, _ = r.reviewTask("PR-12394")
			r.write(r.copyDir, "probe/old.rb", "yours, from before the round\n")
			r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
			if got := r.reload(task.ID); got.Stage != "pr_body" || !isLive(got) {
				t.Fatalf("task %s at %s waits %q; want it on at pr_body", got.State, got.Stage, got.WaitsWhat)
			}
			if got := r.read("probe/old.rb"); got != "yours, from before the round\n" {
				t.Fatalf("probe/old.rb is %q", got)
			}
			if r.inCopy("probe/new_spec.rb") != tc.stays || !strings.Contains(r.readTask(task.ID, "review-1-left.diff"), "probe/new_spec.rb") {
				t.Fatalf("the staged probe in the copy: %v, want %v; or it is not saved", r.inCopy("probe/new_spec.rb"), tc.stays)
			}
			if st := r.git(r.copyDir, "status", "--porcelain"); st != "?? probe/" {
				t.Fatalf("status after the put-back:\n%s", st)
			}
			if r.saved(task.ID, "review-1-left") {
				t.Fatal("the factory saved a file from before the round")
			}
			for _, info := range r.reviewInfos(task.ID) {
				if strings.Contains(info, "probe/old.rb") {
					t.Fatalf("the info event names a file from before the round: %q", info)
				}
			}
		})
	}
}

// A tracked file a round took out of the index (`git rm --cached`) is put
// back, never removed as a new file, though git status now names it, and
// the untracked file beside it from before the round, as one new directory.
func TestAPutBackNeverRemovesATrackedFileItRestored(t *testing.T) {
	r := leavingRig(t, func(r *reviewRig, dir string) { r.gitIn(dir, "rm", "--quiet", "--cached", "--", "app/credit.rb") })
	task, _ := r.reviewTask("PR-12395")
	credit := r.read("app/credit.rb")
	r.write(r.copyDir, "app/old_probe.rb", "yours, from before the round\n")
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.Stage != "pr_body" || !isLive(got) {
		t.Fatalf("task %s at %s waits %q; want it on at pr_body", got.State, got.Stage, got.WaitsWhat)
	}
	if got := r.read("app/credit.rb"); got != credit {
		t.Fatalf("app/credit.rb is %q, want the branch's", got)
	}
	if st := r.git(r.copyDir, "status", "--porcelain"); st != "?? app/old_probe.rb" {
		t.Fatalf("status after the put-back:\n%s", st)
	}
	if r.saved(task.ID, "review-1-left/app/old_probe.rb") {
		t.Fatal("the factory saved a file from before the round")
	}
}

// A round stopped before its end (a drain) leaves what it left for the run
// again in its cycle: before that run starts, the factory saves it and puts
// the copy back, as at a round's end, and the cycle keeps the listing of
// untracked files it took before its first run, so nothing the stopped
// round left counts as the copy's own.
func TestARoundStoppedMidwayHasItsLeftoversPutBackBeforeItRunsAgain(t *testing.T) {
	r := newReviewRig(t, result())
	r.rv.block = make(chan struct{})
	task, _ := r.reviewTask("PR-12396")
	r.startBlockedRound()
	r.e.stopBackground() // the drain cancels the round
	r.rv.mu.Lock()
	r.rv.block = nil
	r.rv.mu.Unlock()
	listing := filepath.Join(r.layout.FactoryTaskDir(task.ID), untrackedBeforeFile(1))
	first, err := os.Stat(listing)
	if err != nil {
		t.Fatal(err)
	}
	r.write(r.copyDir, "README", "a stopped reviewer's mutant\n")
	r.write(r.copyDir, "spec/probe_spec.rb", "a stopped reviewer's probe\n")
	r.restart()
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rv.calls()) != 2 {
		t.Fatalf("task %s at %s waits %q after %d rounds; want pr_body after the run again", got.State, got.Stage, got.WaitsWhat,
			len(r.rv.calls()))
	}
	if r.read("README") != "shop\n" || r.inCopy("spec/probe_spec.rb") {
		t.Fatal("the copy is not as the stopped round found it")
	}
	if !strings.Contains(r.readTask(task.ID, "review-1-left.diff"), "a stopped reviewer's mutant") ||
		r.readTask(task.ID, "review-1-left/spec/probe_spec.rb") != "a stopped reviewer's probe\n" {
		t.Fatal("what the stopped round left is not saved")
	}
	if again, err := os.Stat(listing); err != nil || !os.SameFile(first, again) {
		t.Fatalf("the run again took the listing before it afresh (%v)", err)
	}
}

// A save never writes over an earlier one: a round whose earlier leftovers
// are saved already (the daemon stopped after the save, before the
// put-back) saves under the next free name.
func TestASecondSaveOfARoundKeepsTheFirst(t *testing.T) {
	var task registry.Task
	r := leavingRig(t, func(r *reviewRig, dir string) {
		r.put(r.layout.FactoryTaskDir(task.ID), "review-1-left.diff", "an earlier save\n")
		r.put(dir, "README", "shop\nrake (13.3.0)\n")
	})
	task, _ = r.reviewTask("PR-12397")
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.Stage != "pr_body" {
		t.Fatalf("task %s at %s waits %q", got.State, got.Stage, got.WaitsWhat)
	}
	if r.readTask(task.ID, "review-1-left.diff") != "an earlier save\n" ||
		!strings.Contains(r.readTask(task.ID, "review-1-left-2.diff"), "+rake (13.3.0)") {
		t.Fatal("the second save wrote over the first, or is missing")
	}
}

// A put-back git says failed but that left the copy as the round found it
// is a put-back: the look after it decides, and the task goes on.
func TestAPutBackThatGitCallsFailedButThatPutTheCopyBackGoesOn(t *testing.T) {
	r := leavingRig(t, func(r *reviewRig, dir string) {
		r.put(dir, "README", "a reviewer's mutant\n")
		r.setFailGit(func(args []string) error {
			if !slices.Contains(args, "restore") {
				return nil
			}
			if _, err := r.real.Run(r.ctx, execx.Cmd{Name: "git", Args: args}); err != nil {
				r.t.Error(err)
			}
			return errors.New("warning: unable to unlink a lock file")
		})
	})
	task, _ := r.reviewTask("PR-12398")
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.Stage != "pr_body" || !isLive(got) {
		t.Fatalf("task %s at %s waits %q; want it on at pr_body", got.State, got.Stage, got.WaitsWhat)
	}
	if len(r.reviewInfos(task.ID)) != 1 {
		t.Fatalf("review info events %q, want the one put-back's", r.reviewInfos(task.ID))
	}
}

// runSteps runs the steps of a wait for you as they are written (each
// command in backticks but the retry), through /bin/sh.
func (r *reviewRig) runSteps(wait string) {
	r.t.Helper()
	for _, m := range regexp.MustCompile("`([^`]*)`").FindAllStringSubmatch(wait, -1) {
		if strings.HasPrefix(m[1], "magnum ") {
			continue
		}
		if _, err := r.real.Run(r.ctx, execx.Cmd{Name: "/bin/sh", Args: []string{"-c", m[1]}}); err != nil {
			r.t.Fatalf("the step %q: %v", m[1], err)
		}
	}
}

// The wait for you names each step's paths on its line up to ten, and past
// ten a file in the task's directory that lists exactly them
// (NUL-separated): the paths for `git restore --pathspec-from-file`, the new
// files for `xargs -0 rm`, never every tracked change of the copy nor an
// event that lists them all. Its steps, run as they are written, put the
// copy back: your files the round staged go back out of the index, with
// their content, and the round's new files go.
func TestTheWaitsStepsPutTheCopyBackAsTheyAreWritten(t *testing.T) {
	mine := func(n int) func(r *reviewRig) {
		return func(r *reviewRig) {
			for i := range n {
				r.write(r.copyDir, fmt.Sprintf("mine/f%02d.rb", i), fmt.Sprintf("yours %d\n", i))
			}
		}
	}
	for _, tc := range []struct {
		name   string
		before func(r *reviewRig)
		leave  func(r *reviewRig, dir string)
		file   string // the .paths file a step reads ("" for none)
		status string // git status once the steps ran
	}{
		{"two of your files staged", mine(2),
			func(r *reviewRig, dir string) {
				r.put(dir, "README", "a reviewer's mutant\n")
				r.gitIn(dir, "add", "-A")
			}, "", "?? mine/"},
		{"twelve of your files staged", mine(12),
			func(r *reviewRig, dir string) { r.gitIn(dir, "add", "-A") }, "round-1-unstage.paths", "?? mine/"},
		// Saved, as the put-back failed after the save: an rm step names them.
		{"twelve new files of the round, saved", func(*reviewRig) {},
			func(r *reviewRig, dir string) {
				r.put(dir, "README", "a reviewer's mutant\n")
				for i := range 12 {
					r.put(dir, fmt.Sprintf("spec/probe_%02d_spec.rb", i), "a probe\n")
				}
				r.failRestores(true)
			}, "round-1-remove.paths", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := leavingRig(t, func(r *reviewRig, dir string) {
				tc.leave(r, dir)
				if !strings.HasSuffix(tc.name, "saved") {
					r.failSave()
				}
			})
			task, _ := r.reviewTask("PR-12399")
			tc.before(r)
			r.run(6, func() bool { return !isLive(r.reload(task.ID)) })
			got := r.reload(task.ID)
			if got.State != registry.TaskNeedsYou || strings.Contains(got.WaitsWhat, ":/") ||
				tc.file != "" && !strings.Contains(got.WaitsWhat, filepath.Join(r.layout.FactoryTaskDir(task.ID), tc.file)) {
				t.Fatalf("task %s waits %q; want steps over %q", got.State, got.WaitsWhat, tc.file)
			}
			for _, ev := range r.events(task.ID, "review") {
				if strings.Count(ev.Text, ".rb") > 10 {
					t.Fatalf("an event lists them all: %q", ev.Text)
				}
			}
			r.runSteps(got.WaitsWhat)
			if st := r.git(r.copyDir, "status", "--porcelain"); st != tc.status {
				t.Fatalf("status after the steps:\n%s\nwant:\n%s", st, tc.status)
			}
			for i := range 12 {
				if rel := fmt.Sprintf("mine/f%02d.rb", i); r.inCopy(rel) && r.read(rel) != fmt.Sprintf("yours %d\n", i) {
					t.Fatalf("%s is %q", rel, r.read(rel))
				}
			}
		})
	}
}

// The wait's steps never remove a file the factory cannot prove the
// round's: after a round that changed the ignore rules no step removes a
// new file (git's listing then is the round's rules', and a file of yours it
// had ignored shows in it), and a file the round staged that the copy's
// listing did not have (it may be a file of yours git ignores) goes back out
// of the index only. The wait says so; your ignored file stays.
func TestTheWaitsStepsNeverRemoveAFileNotProvenTheRounds(t *testing.T) {
	for _, tc := range []struct {
		name, says string
		leave      func(r *reviewRig, dir string)
	}{
		{"the round changed the ignore rules", "ls-files --others --exclude-standard",
			func(r *reviewRig, dir string) { r.put(dir, ".gitignore", "") }},
		{"the round staged an ignored file", "they stay in the copy",
			func(r *reviewRig, dir string) { r.gitIn(dir, "add", "-f", "--", "tmp/cache.log") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := leavingRig(t, func(r *reviewRig, dir string) {
				tc.leave(r, dir)
				r.failSave()
			})
			task, _ := r.reviewTask("PR-12403")
			r.write(r.copyDir, "tmp/cache.log", "yours, ignored\n")
			r.run(6, func() bool { return !isLive(r.reload(task.ID)) })
			got := r.reload(task.ID)
			if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, tc.says) || strings.Contains(got.WaitsWhat, "rm ") {
				t.Fatalf("task %s waits %q; want no rm step and %q", got.State, got.WaitsWhat, tc.says)
			}
			r.runSteps(got.WaitsWhat)
			if r.read("tmp/cache.log") != "yours, ignored\n" || r.read(".gitignore") != "tmp/\n" {
				t.Fatal("the steps removed your ignored file or left the round's ignore rules")
			}
			if st := r.git(r.copyDir, "status", "--porcelain"); st != "" {
				t.Fatalf("status after the steps:\n%s", st)
			}
		})
	}
}

// A file of yours git ignores that the round staged stays in the copy,
// with its content: a file HEAD lacks goes back out of the index only, and
// is removed only when git lists it as a new file once the rest is back.
func TestAnIgnoredFileTheRoundStagedStaysInTheCopy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		leave func(r *reviewRig, dir string)
	}{
		{"git add -f", func(r *reviewRig, dir string) { r.gitIn(dir, "add", "-f", "--", "tmp/cache.log") }},
		{"the ignore rules emptied, then git add -A", func(r *reviewRig, dir string) {
			r.put(dir, ".gitignore", "")
			r.gitIn(dir, "add", "-A")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := leavingRig(t, tc.leave)
			task, _ := r.reviewTask("PR-12404")
			r.write(r.copyDir, "tmp/cache.log", "yours, ignored\n")
			r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
			if got := r.reload(task.ID); got.Stage != "pr_body" || !isLive(got) {
				t.Fatalf("task %s at %s waits %q; want it on at pr_body", got.State, got.Stage, got.WaitsWhat)
			}
			if r.read("tmp/cache.log") != "yours, ignored\n" || r.read(".gitignore") != "tmp/\n" {
				t.Fatal("the put-back removed your ignored file or left the round's ignore rules")
			}
			if st := r.git(r.copyDir, "status", "--porcelain"); st != "" {
				t.Fatalf("status after the put-back:\n%s", st)
			}
		})
	}
}

// A new ignore file of the round's goes before the factory looks for new
// files: a file of yours it showed (`!*.log` under a rule that ignores
// logs) is not taken for the round's, and stays.
func TestANewIgnoreFileOfTheRoundNeverShowsYourIgnoredFileForRemoval(t *testing.T) {
	r := leavingRig(t, func(r *reviewRig, dir string) { r.put(dir, "logs/.gitignore", "!*.log\n") })
	task, _ := r.reviewTask("PR-12405")
	exclude := filepath.Join(r.main, ".git", "info", "exclude")
	r.write(filepath.Dir(exclude), "exclude", "*.log\n")
	r.write(r.copyDir, "logs/app.log", "yours, ignored\n")
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.Stage != "pr_body" || !isLive(got) {
		t.Fatalf("task %s at %s waits %q; want it on at pr_body", got.State, got.Stage, got.WaitsWhat)
	}
	if r.read("logs/app.log") != "yours, ignored\n" || r.inCopy("logs/.gitignore") {
		t.Fatal("the put-back removed your ignored file, or left the round's ignore file")
	}
}

// Without the listing before the round (a cycle an older build began, or
// one that does not parse) nothing tells a file of yours from the round's:
// a file the round staged goes back out of the index only, and stays.
func TestWithoutAListingNoFileTheRoundStagedLeavesTheCopy(t *testing.T) {
	var task registry.Task
	r := leavingRig(t, func(r *reviewRig, dir string) {
		if err := os.Remove(filepath.Join(r.layout.FactoryTaskDir(task.ID), untrackedBeforeFile(1))); err != nil {
			r.t.Error(err)
		}
		r.gitIn(dir, "add", "-A")
	})
	task, _ = r.reviewTask("PR-12406")
	r.write(r.copyDir, "probe/old.rb", "yours, from before the round\n")
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.Stage != "pr_body" || !isLive(got) {
		t.Fatalf("task %s at %s waits %q; want it on at pr_body", got.State, got.Stage, got.WaitsWhat)
	}
	if r.read("probe/old.rb") != "yours, from before the round\n" {
		t.Fatal("the put-back removed your file")
	}
}

// A file of yours the round staged and then deleted keeps its one copy in
// the index: the factory leaves it staged and the task needs you, and no
// step of the wait drops it from the index.
func TestAFileTheRoundStagedAndDeletedKeepsItsIndexCopy(t *testing.T) {
	r := leavingRig(t, func(r *reviewRig, dir string) {
		r.gitIn(dir, "add", "-A")
		if err := os.Remove(filepath.Join(dir, "probe", "old.rb")); err != nil {
			r.t.Error(err)
		}
	})
	task, _ := r.reviewTask("PR-12407")
	r.write(r.copyDir, "probe/old.rb", "yours, from before the round\n")
	r.run(6, func() bool { return !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || strings.Contains(got.WaitsWhat, "restore --staged -- probe/old.rb") ||
		!strings.Contains(got.WaitsWhat, "probe/old.rb") {
		t.Fatalf("task %s waits %q; want you, with no step that drops probe/old.rb from the index", got.State, got.WaitsWhat)
	}
	if got := r.git(r.copyDir, "show", ":probe/old.rb"); got != "yours, from before the round" {
		t.Fatalf("the index holds %q", got)
	}
}

// A file from before the round that the round staged (`git add -A`, `git
// add -N`) goes back to untracked with its content: only the index is put
// back for it, never the file, though it is a tracked change now. The
// task goes on.
func TestARoundThatStagesAFileFromBeforeItLeavesTheFileInTheCopy(t *testing.T) {
	for _, tc := range []struct {
		name string
		add  []string
	}{
		{"git add -A", []string{"add", "-A"}},
		{"git add -N", []string{"add", "-N", "--", "probe/old.rb"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := leavingRig(t, func(r *reviewRig, dir string) { r.gitIn(dir, tc.add...) })
			task, _ := r.reviewTask("PR-12400")
			r.write(r.copyDir, "probe/old.rb", "yours, from before the round\n")
			r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
			if got := r.reload(task.ID); got.Stage != "pr_body" || !isLive(got) {
				t.Fatalf("task %s at %s waits %q; want it on at pr_body", got.State, got.Stage, got.WaitsWhat)
			}
			if got := r.read("probe/old.rb"); got != "yours, from before the round\n" {
				t.Fatalf("probe/old.rb is %q", got)
			}
			if st := r.git(r.copyDir, "status", "--porcelain"); st != "?? probe/" {
				t.Fatalf("status after the put-back:\n%s", st)
			}
		})
	}
}

// A file the round took out of the index and then changed has the round's
// change saved under round-<n>-left/ before the branch's file goes back:
// git's patch of it is only a deletion.
func TestARoundsEditToAFileItTookOutOfTheIndexIsSavedFirst(t *testing.T) {
	r := leavingRig(t, func(r *reviewRig, dir string) {
		r.gitIn(dir, "rm", "--quiet", "--cached", "--", "app/credit.rb")
		r.put(dir, "app/credit.rb", "a reviewer's mutant\n")
	})
	task, _ := r.reviewTask("PR-12401")
	credit := r.read("app/credit.rb")
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.Stage != "pr_body" || !isLive(got) {
		t.Fatalf("task %s at %s waits %q; want it on at pr_body", got.State, got.Stage, got.WaitsWhat)
	}
	if got := r.read("app/credit.rb"); got != credit {
		t.Fatalf("app/credit.rb is %q, want the branch's", got)
	}
	if got := r.readTask(task.ID, "review-1-left/app/credit.rb"); got != "a reviewer's mutant\n" {
		t.Fatalf("the round's change saved as %q", got)
	}
}

// A file git ignored before the round stays when the round changed the
// ignore rules: the put-back removes only the new files git still lists
// once the tracked files are back.
func TestAPutBackNeverRemovesAnIgnoredFileTheRoundUnignored(t *testing.T) {
	r := leavingRig(t, func(r *reviewRig, dir string) { r.put(dir, ".gitignore", "") })
	task, _ := r.reviewTask("PR-12402")
	r.write(r.copyDir, "tmp/cache.log", "yours, ignored\n")
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.Stage != "pr_body" || !isLive(got) {
		t.Fatalf("task %s at %s waits %q; want it on at pr_body", got.State, got.Stage, got.WaitsWhat)
	}
	if got := r.read("tmp/cache.log"); got != "yours, ignored\n" {
		t.Fatalf("tmp/cache.log is %q", got)
	}
	if got := r.read(".gitignore"); got != "tmp/\n" {
		t.Fatalf(".gitignore is %q", got)
	}
}

// A file of yours the round staged and then changed keeps the version the
// index holds of it, its only copy (git's patch holds the round's): the
// factory leaves it staged and the task needs you, and no step of the wait
// drops it from the index.
func TestAFileTheRoundStagedAndEditedKeepsItsIndexCopy(t *testing.T) {
	r := leavingRig(t, func(r *reviewRig, dir string) {
		r.gitIn(dir, "add", "-A")
		r.put(dir, "probe/old.rb", "the round's edit\n")
	})
	task, _ := r.reviewTask("PR-12408")
	r.write(r.copyDir, "probe/old.rb", "yours, from before the round\n")
	r.run(6, func() bool { return !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || strings.Contains(got.WaitsWhat, "restore --staged -- probe/old.rb") ||
		!strings.Contains(got.WaitsWhat, "probe/old.rb") {
		t.Fatalf("task %s waits %q; want you, with no step that drops probe/old.rb from the index", got.State, got.WaitsWhat)
	}
	if got := r.git(r.copyDir, "show", ":probe/old.rb"); got != "yours, from before the round" {
		t.Fatalf("the index holds %q", got)
	}
}

// An intent-to-add rename (git status ` R`: your ignored file added with
// -N -f, a file HEAD has of like content deleted) is a path HEAD lacks:
// only its index goes back, and your file stays.
func TestAnIntentToAddRenameOfYourIgnoredFileStays(t *testing.T) {
	r := leavingRig(t, func(r *reviewRig, dir string) {
		r.gitIn(dir, "add", "-N", "-f", "--", "tmp/cache.log")
		if err := os.Remove(filepath.Join(dir, "app", "credit.rb")); err != nil {
			r.t.Error(err)
		}
	})
	task, _ := r.reviewTask("PR-12409")
	yours := r.read("app/credit.rb") + "# yours\n"
	r.write(r.copyDir, "tmp/cache.log", yours)
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	if !r.inCopy("tmp/cache.log") || r.read("tmp/cache.log") != yours {
		t.Fatal("the put-back removed your ignored file")
	}
}

// older dates a file of yours in the copy before the cycle began (t0): a
// file the round makes is never older than the round.
func (r *reviewRig) older(rel string) {
	r.t.Helper()
	if err := os.Chtimes(filepath.Join(r.copyDir, rel), t0.Add(-time.Hour), t0.Add(-time.Hour)); err != nil {
		r.t.Fatal(err)
	}
}

// A file older than the round is never removed by the put-back, whatever
// showed it as new: the round emptied .git/info/exclude, or left a
// .gitignore that ignores itself and shows your logs. It stays, and the
// look after the put-back sends the task to you.
func TestAPutBackNeverRemovesAFileOlderThanTheRound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		leave func(r *reviewRig, dir string)
	}{
		{"info/exclude emptied", func(r *reviewRig, _ string) {
			r.put(filepath.Join(r.main, ".git", "info"), "exclude", "")
		}},
		{"a new ignore file that ignores itself", func(r *reviewRig, dir string) { r.put(dir, "logs/.gitignore", "*\n!*.log\n") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := leavingRig(t, tc.leave)
			task, _ := r.reviewTask("PR-12410")
			r.write(filepath.Join(r.main, ".git", "info"), "exclude", "*.log\n")
			r.write(r.copyDir, "logs/app.log", "yours, ignored\n")
			r.older("logs/app.log")
			r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
			if !r.inCopy("logs/app.log") || r.read("logs/app.log") != "yours, ignored\n" {
				t.Fatal("the put-back removed your ignored file")
			}
		})
	}
}

// The wait's rm step names only new files saved in the task's directory
// that are not older than the round: with nothing saved (a symlink among
// the new files, which no save takes) it
// names none, and a file of yours that the round's change of
// .git/info/exclude showed is never in it, saved or not. The wait names the
// files it leaves out.
func TestTheWaitsRmStepNamesOnlyTheRoundsSavedFiles(t *testing.T) {
	for _, tc := range []struct {
		name     string
		leave    func(r *reviewRig, dir string)
		rm, left string // a path the rm step names ("" for no rm step), one the wait names as left out
	}{
		{"nothing saved", func(r *reviewRig, dir string) {
			r.put(filepath.Join(r.main, ".git", "info"), "exclude", "")
			r.put(dir, "spec/probe_spec.rb", "a reviewer's probe\n")
			if err := os.Symlink("../README", filepath.Join(dir, "spec", "link")); err != nil {
				r.t.Error(err)
			}
		}, "", "logs/app.log"},
		{"saved, as the put-back failed", func(r *reviewRig, dir string) {
			r.put(filepath.Join(r.main, ".git", "info"), "exclude", "")
			r.put(dir, "README", "a reviewer's mutant\n")
			r.put(dir, "spec/probe_spec.rb", "a reviewer's probe\n")
			r.failRestores(false)
		}, "spec/probe_spec.rb", "logs/app.log"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := leavingRig(t, tc.leave)
			task, _ := r.reviewTask("PR-12411")
			r.write(filepath.Join(r.main, ".git", "info"), "exclude", "*.log\n")
			r.write(r.copyDir, "logs/app.log", "yours, ignored\n")
			r.older("logs/app.log")
			r.run(6, func() bool { return !isLive(r.reload(task.ID)) })
			got := r.reload(task.ID)
			rm := regexp.MustCompile("`rm [^`]*`").FindString(got.WaitsWhat)
			if got.State != registry.TaskNeedsYou || strings.Contains(rm, "logs/app.log") ||
				(tc.rm == "") != (rm == "") || !strings.Contains(rm, tc.rm) || !strings.Contains(got.WaitsWhat, tc.left) {
				t.Fatalf("task %s waits %q; want an rm step of %q, and %q named as left out", got.State, got.WaitsWhat, tc.rm, tc.left)
			}
		})
	}
}
