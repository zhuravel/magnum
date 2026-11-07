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
	want := "round 1 of the local review left 2 new untracked files in the copy (probe/deep/credit_spec.rb, spec/probe_spec.rb); " +
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
		// The guard is asked again after the save, just before the put-back.
		{"you came into a pane while the factory saved it", "you are in the fx-writer pane",
			func(r *reviewRig) {
				r.setFailGit(func(args []string) error {
					if slices.Contains(args, "diff") && slices.Contains(args, "--binary") {
						r.rt.setOperatorIn("fx-writer")
					}
					return nil
				})
			},
			func(r *reviewRig) { r.rt.setOperatorIn("") }},
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
			restore := "git -C " + r.copyDir + " --literal-pathspecs restore --staged --worktree -- README"
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
			if saved := r.saved(task.ID, "round-1-left.patch"); saved != tc.saved || r.saved(task.ID, "round-1-left") {
				t.Fatalf("round-1-left.patch saved %v, want %v (or a directory of no new file)", saved, tc.saved)
			}
			if tc.saved && !strings.Contains(got.WaitsWhat, "round-1-left.patch") {
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
			if r.inCopy("probe/new_spec.rb") != tc.stays || !strings.Contains(r.readTask(task.ID, "round-1-left.patch"), "probe/new_spec.rb") {
				t.Fatalf("the staged probe in the copy: %v, want %v; or it is not saved", r.inCopy("probe/new_spec.rb"), tc.stays)
			}
			if st := r.git(r.copyDir, "status", "--porcelain"); st != "?? probe/" {
				t.Fatalf("status after the put-back:\n%s", st)
			}
			if r.saved(task.ID, "round-1-left") {
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
	if r.saved(task.ID, "round-1-left/app/old_probe.rb") {
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
	if !strings.Contains(r.readTask(task.ID, "round-1-left.patch"), "a stopped reviewer's mutant") ||
		r.readTask(task.ID, "round-1-left/spec/probe_spec.rb") != "a stopped reviewer's probe\n" {
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
		r.put(r.layout.FactoryTaskDir(task.ID), "round-1-left.patch", "an earlier save\n")
		r.put(dir, "README", "shop\nrake (13.3.0)\n")
	})
	task, _ = r.reviewTask("PR-12397")
	r.run(6, func() bool { return r.reload(task.ID).Stage != "local_review" || !isLive(r.reload(task.ID)) })
	if got := r.reload(task.ID); got.Stage != "pr_body" {
		t.Fatalf("task %s at %s waits %q", got.State, got.Stage, got.WaitsWhat)
	}
	if r.readTask(task.ID, "round-1-left.patch") != "an earlier save\n" ||
		!strings.Contains(r.readTask(task.ID, "round-1-left-2.patch"), "+rake (13.3.0)") {
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

// The wait for you over more than ten paths in a step names a file in the
// task's directory that lists exactly them (NUL-separated): the round's
// tracked changes for `git restore --pathspec-from-file`, the new files for
// `xargs -0 rm`, never every tracked change of the copy nor an event that
// lists them all. Its steps, run as they are written, put the copy back.
func TestTheWaitOverManyLeftoversNamesFilesThatListThem(t *testing.T) {
	r := leavingRig(t, func(r *reviewRig, dir string) {
		for i := range 12 {
			r.put(dir, fmt.Sprintf("spec/staged_%02d_spec.rb", i), "a staged probe\n")
			r.put(dir, fmt.Sprintf("spec/probe_%02d_spec.rb", i), "a probe\n")
		}
		r.gitIn(dir, "add", "--", "spec")
		for i := range 12 {
			r.put(dir, fmt.Sprintf("spec/probe_%02d_spec.rb", i), "a probe\n") // the add took them; these are new again
			r.gitIn(dir, "rm", "--quiet", "--cached", "--", fmt.Sprintf("spec/probe_%02d_spec.rb", i))
		}
		r.rt.setOperatorIn("fx-writer")
	})
	task, _ := r.reviewTask("PR-12399")
	r.run(6, func() bool { return !isLive(r.reload(task.ID)) })
	got := r.reload(task.ID)
	dir := r.layout.FactoryTaskDir(task.ID)
	restore, remove := filepath.Join(dir, "round-1-restore.paths"), filepath.Join(dir, "round-1-remove.paths")
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "--pathspec-from-file="+restore) ||
		!strings.Contains(got.WaitsWhat, "xargs -0 rm -- < "+remove) || strings.Contains(got.WaitsWhat, ":/") {
		t.Fatalf("task %s waits %q; want the steps over %s and %s", got.State, got.WaitsWhat, restore, remove)
	}
	var staged, probes []string
	for i := range 12 {
		staged = append(staged, fmt.Sprintf("spec/staged_%02d_spec.rb", i))
		probes = append(probes, filepath.Join(r.copyDir, fmt.Sprintf("spec/probe_%02d_spec.rb", i)))
	}
	if b, _ := os.ReadFile(restore); string(b) != strings.Join(staged, "\x00")+"\x00" {
		t.Fatalf("%s lists %q", restore, b)
	}
	if b, _ := os.ReadFile(remove); string(b) != strings.Join(probes, "\x00")+"\x00" {
		t.Fatalf("%s lists %q", remove, b)
	}
	for _, ev := range r.events(task.ID, "review") {
		if strings.Count(ev.Text, "_spec.rb") > 10 {
			t.Fatalf("an event lists them all: %q", ev.Text)
		}
	}
	for _, m := range regexp.MustCompile("`([^`]*)`").FindAllStringSubmatch(got.WaitsWhat, -1) {
		if strings.HasPrefix(m[1], "magnum ") {
			continue
		}
		if _, err := r.real.Run(r.ctx, execx.Cmd{Name: "/bin/sh", Args: []string{"-c", m[1]}}); err != nil {
			t.Fatalf("the step %q: %v", m[1], err)
		}
	}
	if st := r.git(r.copyDir, "status", "--porcelain"); st != "" {
		t.Fatalf("status after the steps:\n%s", st)
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
	if got := r.readTask(task.ID, "round-1-left/app/credit.rb"); got != "a reviewer's mutant\n" {
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
