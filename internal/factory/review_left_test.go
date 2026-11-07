package factory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/evalrun"
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
// line to Gemfile.lock) while nobody else could have made it, nobody at
// work in the copy and nobody in the task's panes since the round began,
// has it saved and put back: the change goes to round-1-left.patch in the
// task's directory, the copy is as the round found it, one info event says
// so, and the round's result is read: the task goes on, never to you. Your
// input into a pane before the round began does not hold it.
func TestARoundsTrackedChangeIsSavedAndPutBackWhenNobodyElseCouldHaveMadeIt(t *testing.T) {
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
	if patch := r.readTask(task.ID, "round-1-left.patch"); !strings.Contains(patch, "diff --git a/README b/README") ||
		!strings.Contains(patch, "+rake (13.3.0)") {
		t.Fatalf("round-1-left.patch:\n%s", patch)
	}
	want := "round 1 of the local review left 1 tracked change in the copy (README); saved to round-1-left.patch, " +
		"the copy is back as the round found it"
	if infos := r.reviewInfos(task.ID); !slices.Equal(infos, []string{want}) {
		t.Fatalf("review info events %q, want %q", infos, want)
	}
	if ps := r.rt.promptsTo("fx-writer"); len(ps) != 0 {
		t.Fatalf("the writer got %d prompts over the round's leftovers", len(ps))
	}
}

// A round that leaves new untracked files while nobody else could have
// made them has each saved under round-1-left/ in the task's directory, at
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
	if got := r.readTask(task.ID, "round-1-left/spec/probe_spec.rb"); got != "a reviewer's probe\n" {
		t.Fatalf("saved probe %q", got)
	}
	if got := r.readTask(task.ID, "round-1-left/probe/deep/credit_spec.rb"); got != "a deeper probe\n" {
		t.Fatalf("saved deeper probe %q", got)
	}
	if r.saved(task.ID, "round-1-left/probe/tmp/cache") || r.saved(task.ID, "round-1-left.patch") {
		t.Fatal("the factory saved an ignored file or a patch of no tracked change")
	}
	if r.inCopy("spec/probe_spec.rb") || r.inCopy("probe/deep") || !r.inCopy("probe/tmp/cache") {
		t.Fatal("the copy is not as the round found it, or lost an ignored file")
	}
	want := "round 1 of the local review left 2 new untracked files in the copy (probe/, spec/probe_spec.rb); " +
		"saved to round-1-left/, the copy is back as the round found it"
	if infos := r.reviewInfos(task.ID); !slices.Equal(infos, []string{want}) {
		t.Fatalf("review info events %q, want %q", infos, want)
	}
}

// What a round left while somebody else could have changed the copy
// stays as it is: you typed into one of the task's panes since the round
// began, or an agent of the task works in the copy. Nothing is saved or put
// back, and the task needs you with why and the steps: the git restore of
// the tracked changes, the rm of the new files and the retry. Once you take
// them, the round's result is read.
func TestARoundsLeftoversWaitForYouWhenSomebodyElseCouldHaveMadeThem(t *testing.T) {
	for _, tc := range []struct {
		name, why string
		other     func(r *reviewRig)
		gone      func(r *reviewRig)
	}{
		{"you typed into a pane since the round began", "you typed into a pane of PR-12392 at 10:00:00",
			func(r *reviewRig) {
				r.rt.setTouchedAt("you typed into a pane of PR-12392 at 10:00:00", r.clock.Now().Add(time.Second))
			},
			func(*reviewRig) {}},
		{"an agent at work in the copy", "the fx-writer agent works in it",
			func(r *reviewRig) { r.rt.setAtWork("fx-writer") },
			func(r *reviewRig) { r.rt.setAtWork("") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := leavingRig(t, func(r *reviewRig, dir string) {
				r.put(dir, "README", "a reviewer's mutant\n")
				r.put(dir, "spec/probe_spec.rb", "a reviewer's probe\n")
				tc.other(r)
			})
			task, _ := r.reviewTask("PR-12392")
			r.run(6, func() bool { return !isLive(r.reload(task.ID)) })
			got := r.reload(task.ID)
			restore := "git -C " + r.copyDir + " restore --staged --worktree -- README"
			rm := "rm -- " + filepath.Join(r.copyDir, "spec/probe_spec.rb")
			retry := fmt.Sprintf("magnum factory retry %d", task.ID)
			if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "round 1 of the local review left") ||
				!strings.Contains(got.WaitsWhat, tc.why) || !strings.Contains(got.WaitsWhat, restore) ||
				!strings.Contains(got.WaitsWhat, rm) || !strings.Contains(got.WaitsWhat, retry) {
				t.Fatalf("task %s waits %q; want you, with %q, %q, %q and %q", got.State, got.WaitsWhat, tc.why, restore, rm, retry)
			}
			if r.read("README") != "a reviewer's mutant\n" || !r.inCopy("spec/probe_spec.rb") {
				t.Fatal("the factory changed what the round left")
			}
			if r.saved(task.ID, "round-1-left.patch") || r.saved(task.ID, "round-1-left") {
				t.Fatal("the factory saved what it did not put back")
			}
			tc.gone(r)
			r.git(r.copyDir, "restore", "--staged", "--worktree", "--", "README")
			if err := os.Remove(filepath.Join(r.copyDir, "spec/probe_spec.rb")); err != nil {
				t.Fatal(err)
			}
			r.letGoOn(task.ID)
			r.run(5, func() bool { return r.reload(task.ID).Stage != "local_review" })
			if got := r.reload(task.ID); got.Stage != "pr_body" || len(r.rv.calls()) != 1 {
				t.Fatalf("task at %s waits %q after %d rounds; want pr_body on the one round's result", got.Stage, got.WaitsWhat,
					len(r.rv.calls()))
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
		{"a symlink among the new files", "spec/link is not a regular file",
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
			if saved := r.saved(task.ID, "round-1-left.patch"); saved != tc.saved || r.saved(task.ID, "round-1-left") {
				t.Fatalf("round-1-left.patch saved %v, want %v (or a directory of no new file)", saved, tc.saved)
			}
			if tc.saved && !strings.Contains(got.WaitsWhat, "round-1-left.patch") {
				t.Fatalf("the wait %q does not name the saved patch", got.WaitsWhat)
			}
		})
	}
}
