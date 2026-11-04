package factory

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
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
