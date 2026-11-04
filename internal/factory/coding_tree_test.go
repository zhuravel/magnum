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

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// The factory's look at a copy before a switch sees what git would lose
// there, and only that: a file git does not track (untracked or ignored) at
// a path the switch writes, or in a directory there. And no switch or
// fast-forward of the factory's writes over an ignored file, which git's
// default does.

// readmeToDir turns the copy's README into a directory, committed: the
// head has README/index.md where the base has a file.
func (r *codingRig) readmeToDir() {
	r.t.Helper()
	r.git(r.copyDir, "rm", "--quiet", "README")
	r.write(r.copyDir, "README/index.md", "shop\n")
	r.git(r.copyDir, "add", "-A")
	r.git(r.copyDir, "commit", "--quiet", "-m", "README becomes a directory")
}

// A head that turns a file into a directory, renames a file in case only
// (macOS's file system does not tell the two names apart), or changes more
// than git's output cap leaves nothing in the base run's way: the proof runs
// on the base and passes at the head, and a copy left detached over such a
// change comes back after a restart.
func TestTheBaseRunsLookAtTheTreeSeesOnlyWhatGitWouldLose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(r *codingRig)
	}{
		{"a file the head turned into a directory", (*codingRig).readmeToDir},
		{"a case-only rename", func(r *codingRig) {
			r.git(r.copyDir, "mv", "README", "Readme")
			r.git(r.copyDir, "commit", "--quiet", "-m", "Readme")
		}},
		{"a change past git's output cap", func(r *codingRig) {
			r.commit("a large file", map[string]string{"data/large.txt": strings.Repeat("0123456789abcdef\n", (execx.MaxOutput+1<<20)/17)})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCodingRig(t)
			task := r.codingTask("PR-12411", "bug")
			r.fix(task)
			tc.change(r)
			head := r.git(r.copyDir, "rev-parse", "HEAD")
			r.copies.reserve(task.ID, "factory1")
			p, err := r.e.runProof(r.ctx, task, r.cp)
			if err != nil || !p.Passed || p.Base.Exit == 0 {
				t.Fatalf("proof %+v, %v; want a red base run and a proof", p, err)
			}
			r.onBranch(task.Branch, head)

			base := r.git(r.copyDir, "merge-base", "HEAD", "origin/master")
			if err := r.e.detachForBase(r.ctx, task, r.cp, base, head, []string{pointsSpec}); err != nil {
				t.Fatal(err)
			}
			r.restart()
			if err := r.e.startup(r.ctx); err != nil {
				t.Fatal(err)
			}
			r.onBranch(task.Branch, head)
		})
	}
}

// What git would lose stays in the base run's way: an untracked file in the
// directory the head made of a file, and an ignored file at a path the base
// has. The proof needs you, naming the file, and the file stays.
func TestTheBaseRunsLookAtTheTreeKeepsWhatGitWouldLose(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		master     func(r *codingRig)
		change     func(r *codingRig)
	}{
		{"an untracked file in a directory the head made of a file", "README/notes.txt", nil, func(r *codingRig) {
			r.readmeToDir()
			r.write(r.copyDir, "README/notes.txt", "your notes\n")
		}},
		{"an ignored file at a path the base has", "tmp/seed.txt", func(r *codingRig) {
			r.write(r.work, "tmp/seed.txt", "seed\n")
			r.git(r.work, "add", "-f", "--", "tmp/seed.txt")
			r.git(r.work, "commit", "--quiet", "-m", "master tracks a seed")
			r.git(r.work, "push", "--quiet", "origin", "master")
			r.git(r.main, "fetch", "--quiet", "origin")
		}, func(r *codingRig) {
			r.git(r.copyDir, "rm", "--quiet", "tmp/seed.txt")
			r.git(r.copyDir, "commit", "--quiet", "-m", "no seed")
			r.write(r.copyDir, "tmp/seed.txt", "your notes\n") // ignored (tmp/)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCodingRig(t)
			if tc.master != nil {
				tc.master(r)
			}
			task := r.codingTask("PR-12412", "bug")
			r.fix(task)
			tc.change(r)
			head := r.git(r.copyDir, "rev-parse", "HEAD")
			p, err := r.e.runProof(r.ctx, task, r.cp)
			if err != nil || p.Passed || !p.ForYou || !strings.Contains(p.Why, tc.path) {
				t.Fatalf("proof %+v, %v; want it failed for you, naming %s", p, err, tc.path)
			}
			if s := r.read(tc.path); s != "your notes\n" {
				t.Fatalf("%s = %q, want it kept", tc.path, s)
			}
			if n := len(r.ran("bin/specs")); n != 0 {
				t.Fatalf("%d spec runs; want no base run over %s", n, tc.path)
			}
			r.onBranch(task.Branch, head)
		})
	}
}

// The coding stage's fast-forward of a fresh branch never writes over your
// ignored file at a path the new base tracks: the branch stays where it is,
// and the file as you left it.
func TestTheFactoryNeverMovesACopyOverAnIgnoredFile(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12413", "bug")
	tip := r.git(r.copyDir, "rev-parse", "HEAD")
	r.write(r.copyDir, "tmp/seed.txt", "your notes\n") // ignored (tmp/)
	r.write(r.work, "tmp/seed.txt", "seed\n")
	r.git(r.work, "add", "-f", "--", "tmp/seed.txt")
	r.git(r.work, "commit", "--quiet", "-m", "master tracks a seed")
	r.git(r.work, "push", "--quiet", "origin", "master")
	if err := r.e.startCoding(r.ctx, &task, r.cp); err != nil {
		t.Fatal(err)
	}
	if s := r.read("tmp/seed.txt"); s != "your notes\n" {
		t.Fatalf("tmp/seed.txt = %q, want your ignored file kept", s)
	}
	if got := r.git(r.copyDir, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("the branch moved to %s over your ignored file, want it at %s", got, tip)
	}
}

// A file git does not track where a switch makes a directory (your file
// notes, where a commit has notes/a.md) is in the way too: the restore of a
// copy a base run left detached keeps it for you, and the task hears of it
// once, not on every tick; the base run's detach names it for you as well.
func TestAFileWhereTheSwitchMakesADirectoryIsKeptForYou(t *testing.T) {
	t.Run("the switch back to the branch", func(t *testing.T) {
		r := newCodingRig(t)
		task := r.codingTask("PR-12418", "bug")
		base := r.git(r.copyDir, "rev-parse", "HEAD")
		r.fix(task)
		head := r.commit("PR-12418 notes", map[string]string{"notes/a.md": "a\n"})
		r.copies.reserve(task.ID, "factory1")
		if err := r.e.detachForBase(r.ctx, task, r.cp, base, head, []string{pointsSpec}); err != nil {
			t.Fatal(err)
		}
		r.write(r.copyDir, "notes", "your notes\n")
		r.restart()
		if err := r.e.startup(r.ctx); err != nil {
			t.Fatal(err)
		}
		if got := r.reload(task.ID); got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "notes") {
			t.Fatalf("task %s waits %q; want needs_you naming notes", got.State, got.WaitsWhat)
		}
		for range 5 {
			r.clock.Add(45 * time.Second)
			r.tick()
		}
		if b, f, _ := r.restoreEvents(task.ID); b != 0 || f != 1 {
			t.Fatalf("%d begin and %d fail events; want it said once, with no begin", b, f)
		}
		if s := r.read("notes"); s != "your notes\n" || !r.detached() {
			t.Fatalf("notes = %q, detached %v; want your file kept and the copy left as it is", s, r.detached())
		}
	})
	t.Run("the base run's detach", func(t *testing.T) {
		r := newCodingRig(t)
		r.write(r.work, "notes/a.md", "a\n")
		r.git(r.work, "add", "--", "notes/a.md")
		r.git(r.work, "commit", "--quiet", "-m", "master has notes")
		r.git(r.work, "push", "--quiet", "origin", "master")
		r.git(r.main, "fetch", "--quiet", "origin")
		task := r.codingTask("PR-12419", "bug")
		r.fix(task)
		r.git(r.copyDir, "rm", "--quiet", "notes/a.md")
		r.git(r.copyDir, "commit", "--quiet", "-m", "no notes")
		head := r.git(r.copyDir, "rev-parse", "HEAD")
		r.write(r.copyDir, "notes", "your notes\n")
		p, err := r.e.runProof(r.ctx, task, r.cp)
		if err != nil || p.Passed || !p.ForYou || !strings.Contains(p.Why, "notes") {
			t.Fatalf("proof %+v, %v; want it failed for you, naming notes", p, err)
		}
		if n := len(r.ran("bin/specs")); n != 0 {
			t.Fatalf("%d spec runs; want no base run over notes", n)
		}
		if s := r.read("notes"); s != "your notes\n" {
			t.Fatalf("notes = %q, want it kept", s)
		}
		r.onBranch(task.Branch, head)
	})
}

// gitRefuses makes git refuse the switch back to a branch as it refuses one
// over a file it does not track (here, one the factory's look did not
// foresee).
func (r *codingRig) gitRefuses(file string) {
	r.t.Helper()
	r.setFailGit(func(args []string) error {
		if slices.Contains(args, "switch") && slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "refs/heads/") }) {
			return errors.New("error: The following untracked working tree files would be overwritten by checkout:\n\t" + file +
				"\nPlease move or remove them before you switch branches.\nAborting")
		}
		return nil
	})
}

// A switch back that git refuses is a change kept for you too: a task the
// loop does not advance hears of it once, naming the file, and the sweep
// tries again only when it is due, not on every tick. A restore that fails
// for another reason is tried again only when due as well.
func TestARestoreGitRefusesIsKeptForYouAndPaced(t *testing.T) {
	t.Run("git refuses", func(t *testing.T) {
		r := newCodingRig(t)
		task, _ := r.detachedAtNeedsYou("PR-12421")
		r.gitRefuses("notes")
		for range 6 {
			r.clock.Add(45 * time.Second)
			r.tick()
		}
		if b, f, _ := r.restoreEvents(task.ID); b != 1 || f != 1 {
			t.Fatalf("%d begin and %d fail events in 6 ticks; want one restore tried, and said once", b, f)
		}
		if !hasEvent(r.events(task.ID, "proof"), registry.PhaseFail, "notes") {
			t.Fatalf("no fail event names notes: %+v", r.events(task.ID, "proof"))
		}
		if !r.detached() || !r.hasRecord(task.ID) {
			t.Fatal("the copy was moved, or its record dropped")
		}
	})
	t.Run("git fails otherwise", func(t *testing.T) {
		r := newCodingRig(t)
		task, head := r.detachedAtNeedsYou("PR-12429")
		r.setFailGit(func(args []string) error {
			if slices.Contains(args, "switch") && slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "refs/heads/") }) {
				return errors.New("fatal: Unable to create index.lock: File exists")
			}
			return nil
		})
		for range 6 {
			r.clock.Add(45 * time.Second)
			r.tick()
		}
		begins := 0
		for _, ev := range r.events(task.ID, "proof") {
			if ev.Phase == registry.PhaseBegin && strings.HasPrefix(ev.Text, "restores ") {
				begins++
			}
		}
		if begins != 1 {
			t.Fatalf("%d restores begun in 6 ticks; want one, then none until it is due", begins)
		}
		r.setFailGit(nil)
		r.clock.Add(keptRecheck)
		r.tick()
		r.onBranch(task.Branch, head)
	})
}

// A detach for the base run that git refuses over a file it does not track
// (one the look did not foresee) is a change kept for you, named, and no
// spec runs; the copy stays on its branch.
func TestADetachGitRefusesIsKeptForYou(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12430", "bug")
	head := r.fix(task)
	r.setFailGit(func(args []string) error {
		if slices.Contains(args, "switch") && slices.Contains(args, "--detach") &&
			!slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "refs/heads/") }) {
			return errors.New("error: Untracked working tree file 'notes' would be overwritten by merge")
		}
		return nil
	})
	p, err := r.e.runProof(r.ctx, task, r.cp)
	if err != nil || p.Passed || !p.ForYou || !strings.Contains(p.Why, "notes") || !strings.Contains(p.Why, "git refused") {
		t.Fatalf("proof %+v, %v; want it failed for you, naming notes and git's refusal", p, err)
	}
	if n := len(r.ran("bin/specs")); n != 0 {
		t.Fatalf("%d spec runs; want none", n)
	}
	r.setFailGit(nil)
	r.onBranch(task.Branch, head)
	if r.hasRecord(task.ID) {
		t.Fatal("the base run's record is left")
	}
}

// seedOnMaster has master track tmp/seed.txt, which the copy ignores, and
// your ignored tmp/seed.txt in the copy.
func (r *codingRig) seedOnMaster() {
	r.t.Helper()
	r.write(r.work, "tmp/seed.txt", "seed\n")
	r.git(r.work, "add", "-f", "--", "tmp/seed.txt")
	r.git(r.work, "commit", "--quiet", "-m", "master tracks a seed")
	r.git(r.work, "push", "--quiet", "origin", "master")
	r.write(r.copyDir, "tmp/seed.txt", "your notes\n")
}

// The coding stage's own switch never writes over your ignored file: a new
// branch from origin/master (a first coding attempt), or one with the next
// free name (the task's name holds commits not the task's), is refused, and
// the task waits on you, saying which file and what to do. Once you moved it
// and let the task go on, the stage starts.
func TestTheCodingStagesSwitchOverAnIgnoredFileWaitsOnYou(t *testing.T) {
	for _, tc := range []struct {
		name string
		task func(r *codingRig) registry.Task
	}{
		{"a new branch", func(r *codingRig) registry.Task { return r.kindTask("PR-12422", "bug", "coding") }},
		{"the next free name", func(r *codingRig) registry.Task {
			task := r.codingTask("PR-12423", "bug")
			r.commit("someone else's work", map[string]string{"app/other.rb": "class Other; end\n"})
			return task
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCodingRig(t)
			task := tc.task(r)
			r.seedOnMaster()
			r.startStage(task)
			got := r.reload(task.ID)
			if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "tmp/seed.txt") || !strings.Contains(got.WaitsWhat, "git refused") {
				t.Fatalf("task %s waits %q; want needs_you naming tmp/seed.txt and git's refusal", got.State, got.WaitsWhat)
			}
			if n := len(r.rt.promptsTo("fx-writer")); n != 0 {
				t.Fatalf("the writer got %d prompts", n)
			}
			if s := r.read("tmp/seed.txt"); s != "your notes\n" {
				t.Fatalf("tmp/seed.txt = %q, want your ignored file kept", s)
			}
			if err := os.Remove(filepath.Join(r.copyDir, "tmp", "seed.txt")); err != nil {
				t.Fatal(err)
			}
			r.letGoOn(task.ID)
			r.tick()
			if n := len(r.rt.promptsTo("fx-writer")); n != 1 {
				t.Fatalf("the writer got %d prompts once you moved the file; want its first", n)
			}
		})
	}
}

// A claim whose switch git refuses over your ignored file waits on you,
// saying which file and what to do, and costs the task nothing; once you let
// it go on, the claim runs again and the stage starts.
func TestAClaimGitRefusesWaitsOnYouNamingTheFile(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12424", "bug")
	refused := &execx.ExitError{Code: 1, Stderr: "error: The following untracked working tree files would be overwritten by checkout:\n" +
		"\ttmp/seed.txt\nPlease move or remove them before you switch branches.\nAborting"}
	r.copies.mu.Lock()
	r.copies.claimErr = fmt.Errorf("claim factory1 for PR-12424: step slot:factory1:claim:1 switch_branch: gitx: switch %s: %w", task.Branch, refused)
	r.copies.mu.Unlock()
	r.startStage(task)
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || !strings.Contains(got.WaitsWhat, "tmp/seed.txt") || !strings.Contains(got.WaitsWhat, "git refused") {
		t.Fatalf("task %s waits %q; want needs_you naming tmp/seed.txt and git's refusal", got.State, got.WaitsWhat)
	}
	r.copies.mu.Lock()
	r.copies.claimErr = nil
	r.copies.mu.Unlock()
	r.letGoOn(task.ID)
	r.startStage(task)
	if n := len(r.rt.promptsTo("fx-writer")); n != 1 {
		t.Fatalf("the writer got %d prompts after the claim ran again; want its first", n)
	}
}
