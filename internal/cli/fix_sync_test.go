package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/textx"
)

// syncRepo is a throwaway origin whose main has config.rb and whose branch
// feature (the PR's) changes its first line, and the fixer's slot: a clone
// detached at feature's head, the fix run's start head. fix-sync runs git
// with the same hermetic environment (no user or system config).
type syncRepo struct {
	t             *testing.T
	r             *execx.Real
	origin, slot  string
	start, config string
}

// syncConfig is config.rb on main: the PR changes its first line.
const syncConfig = "a\nb\nc\nd\ne\nf\ng\nh\n"

func newSyncRepo(t *testing.T) *syncRepo {
	t.Helper()
	r := &execx.Real{BaseEnv: []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"LC_ALL=C",
	}}
	if _, err := r.Run(context.Background(), execx.Cmd{Name: "git", Args: []string{"--version"}}); err != nil {
		t.Skipf("git not available: %v", err)
	}
	root := resolvedDir(t, t.TempDir())
	s := &syncRepo{t: t, r: r, origin: filepath.Join(root, "origin"), slot: filepath.Join(root, "talkable.review1"), config: "config.rb"}
	if err := os.MkdirAll(s.origin, 0o755); err != nil {
		t.Fatal(err)
	}
	s.git(s.origin, "init", "--quiet", "-b", "main")
	s.commit(s.origin, "config.rb", syncConfig, "the base")
	s.git(s.origin, "switch", "--quiet", "-c", "feature")
	s.start = s.commit(s.origin, "config.rb", strings.Replace(syncConfig, "a\n", "A\n", 1), "the PR")
	s.git(s.origin, "switch", "--quiet", "main")
	s.git(root, "clone", "--quiet", s.origin, s.slot)
	s.git(s.slot, "switch", "--quiet", "--detach", s.start)
	prev := fixSyncRunner
	fixSyncRunner = func() execx.Runner { return r }
	t.Cleanup(func() { fixSyncRunner = prev })
	return s
}

func (s *syncRepo) git(dir string, args ...string) string {
	s.t.Helper()
	res, err := s.r.Run(context.Background(), execx.Cmd{Name: "git", Args: append([]string{"-C", dir}, args...)})
	if err != nil {
		s.t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return res.Out()
}

func (s *syncRepo) write(dir, rel, content string) {
	s.t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *syncRepo) commit(dir, rel, content, msg string) string {
	s.t.Helper()
	s.write(dir, rel, content)
	s.git(dir, "add", "--", rel)
	s.git(dir, "commit", "--quiet", "-m", msg)
	return s.git(dir, "rev-parse", "HEAD")
}

// onFeature runs fn with origin's feature checked out (a push to the
// PR's branch), then checks main out again.
func (s *syncRepo) onFeature(fn func()) string {
	s.t.Helper()
	s.git(s.origin, "switch", "--quiet", "feature")
	fn()
	head := s.git(s.origin, "rev-parse", "HEAD")
	s.git(s.origin, "switch", "--quiet", "main")
	return head
}

// updateBranch moves main on in origin (a commit to path) and merges it
// into feature as GitHub's "Update branch" does; it returns the merge.
func (s *syncRepo) updateBranch(path, content string) string {
	s.t.Helper()
	s.commit(s.origin, path, content, "main moves on")
	return s.onFeature(func() { s.git(s.origin, "merge", "--quiet", "--no-edit", "main") })
}

// mergeChangingThePR commits fixed in the slot (the fixer's fix), moves main
// on and merges it into feature with the PR's first line of config.rb
// changed to line in the merge (a conflict resolved on GitHub, a change
// beside the merge); it returns the refusal fix-sync owes it.
func (s *syncRepo) mergeChangingThePR(fixed, line string) string {
	s.t.Helper()
	s.commit(s.slot, "config.rb", fixed, "Fix the last line")
	s.commit(s.origin, "docs/main.md", "main moves\n", "main moves on")
	s.onFeature(func() {
		s.git(s.origin, "merge", "--quiet", "--no-commit", "main")
		s.write(s.origin, "config.rb", strings.Replace(syncConfig, "a\n", line, 1))
		s.git(s.origin, "add", "config.rb")
		s.git(s.origin, "commit", "--quiet", "--no-edit")
	})
	return "refused: the branch's merges changed the PR's own diff against main (a conflict resolved, or a change beside the merge): someone works on the branch"
}

// fixSync runs `magnum fix-sync` on the slot for the run's start head.
func (s *syncRepo) fixSync(t *testing.T) (int, string, string) {
	t.Helper()
	c, out, errb := bareContext(t)
	c.layoutErr = os.ErrPermission // no magnum layout: the command needs none
	code := execute(c, []string{"fix-sync", "--checkout", s.slot, "--from", s.start, "--branch", "feature", "--base", "main"})
	return code, strings.TrimSpace(out.String()), strings.TrimSpace(errb.String())
}

// wantUnchanged fails unless the slot is still at head, clean, with
// nothing in progress.
func (s *syncRepo) wantUnchanged(t *testing.T, head string) {
	t.Helper()
	if got := s.git(s.slot, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want it as it was (%s)", got, head)
	}
	if got := s.git(s.slot, "status", "--porcelain", "--untracked-files=no"); got != "" {
		t.Errorf("the checkout after the refusal: %q", got)
	}
	if got := s.git(s.slot, "rev-parse", "--absolute-git-dir"); fileExists(filepath.Join(got, "rebase-merge")) || fileExists(filepath.Join(got, "rebase-apply")) {
		t.Error("a rebase is left in progress")
	}
	if b := s.git(s.slot, "branch", "--show-current"); b != "" {
		t.Errorf("the checkout is on %q, want the detached HEAD", b)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// On talkable#12114 GitHub's "Update branch" merged master into the PR's
// branch while the fixer worked; its push was rejected ("fetch first"), and
// the rule against pull, merge and rebase left its commit unpushed. When
// every commit the branch gained is a merge and the PR's own diff against
// the base is unchanged, fix-sync replays the fixer's commits, which are on
// no remote, onto the branch's new tip: HEAD descends from the merge, holds
// the fix, stays detached, and the push that was rejected goes out.
func TestFixSyncReplaysTheFixersCommitOntoABaseMergeTheBranchGained(t *testing.T) {
	s := newSyncRepo(t)
	s.commit(s.slot, "config.rb", strings.Replace(strings.Replace(syncConfig, "a\n", "A\n", 1), "h\n", "H\n", 1), "Fix the last line (review comment 4100000001)")
	merge := s.updateBranch("docs/main.md", "main moves\n")

	code, out, errb := s.fixSync(t)
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errb)
	}
	head := s.git(s.slot, "rev-parse", "HEAD")
	if want := "replayed 1 commit onto " + textx.ShortSHA(merge) + "; HEAD is " + textx.ShortSHA(head); out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if got := s.git(s.slot, "rev-parse", "HEAD~1"); got != merge {
		t.Fatalf("HEAD~1 = %s, want the merge %s", got, merge)
	}
	if got := s.git(s.slot, "show", "HEAD:config.rb"); !strings.HasSuffix(got, "\nH") || !strings.HasPrefix(got, "A\n") {
		t.Errorf("config.rb at HEAD = %q, want the PR's and the fixer's lines", got)
	}
	if got := s.git(s.slot, "show", "HEAD:docs/main.md"); got != "main moves" {
		t.Errorf("docs/main.md at HEAD = %q, want the merge's", got)
	}
	if got := s.git(s.slot, "log", "-1", "--format=%s"); got != "Fix the last line (review comment 4100000001)" {
		t.Errorf("the replayed commit's message = %q", got)
	}
	s.wantUnchanged(t, head)
	s.git(s.slot, "push", "--quiet", "origin", "HEAD:feature")
	if got := s.git(s.origin, "rev-parse", "feature"); got != head {
		t.Errorf("origin's feature = %s after the push, want %s", got, head)
	}
}

// A commit on the branch that is not a merge is someone's work on it:
// fix-sync changes nothing and says why, and the fixer stops as before.
func TestFixSyncRefusesWhenTheBranchGainedACommitOfItsOwn(t *testing.T) {
	s := newSyncRepo(t)
	head := s.commit(s.slot, "config.rb", strings.Replace(strings.Replace(syncConfig, "a\n", "A\n", 1), "h\n", "H\n", 1), "Fix the last line")
	theirs := s.onFeature(func() { s.commit(s.origin, "lib/theirs.rb", "theirs\n", "someone's commit") })

	code, out, _ := s.fixSync(t)
	want := "refused: the branch gained " + textx.ShortSHA(theirs) + ", which is not a merge: someone works on the branch; HEAD is still " + textx.ShortSHA(head)
	if code != 1 || out != want {
		t.Fatalf("exit %d, stdout %q; want 1, %q", code, out, want)
	}
	s.wantUnchanged(t, head)
}

// A merge that leaves the PR's own diff alone can still conflict with the
// fixer's commits (main changed the line the fixer fixed): the replay is
// aborted, and the checkout is back at the fixer's HEAD, clean.
func TestFixSyncAbortsAConflictingReplay(t *testing.T) {
	s := newSyncRepo(t)
	head := s.commit(s.slot, "config.rb", strings.Replace(strings.Replace(syncConfig, "a\n", "A\n", 1), "h\n", "fixer\n", 1), "Fix the last line")
	merge := s.updateBranch("config.rb", strings.Replace(syncConfig, "h\n", "main\n", 1))

	code, out, _ := s.fixSync(t)
	want := "refused: replaying your 1 commit onto " + textx.ShortSHA(merge) + " conflicts in config.rb; HEAD is still " + textx.ShortSHA(head)
	if code != 1 || out != want {
		t.Fatalf("exit %d, stdout %q; want 1, %q", code, out, want)
	}
	s.wantUnchanged(t, head)
}

// abortFails is a runner whose `git rebase --abort` fails: a replay that
// conflicts cannot be undone.
type abortFails struct{ execx.Runner }

func (r abortFails) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	if slices.Contains(c.Args, "rebase") && slices.Contains(c.Args, "--abort") {
		return execx.Result{}, errors.New("the abort fails")
	}
	return r.Runner.Run(ctx, c)
}

// Exit 1 tells the fixer that nothing changed. A replay that could not be
// undone (its abort failed) did change the checkout: fix-sync exits 3 and
// says what is in the way, never "refused".
func TestFixSyncSaysWhenTheCheckoutChanged(t *testing.T) {
	s := newSyncRepo(t)
	fixSyncRunner = func() execx.Runner { return abortFails{s.r} }
	s.commit(s.slot, "config.rb", strings.Replace(strings.Replace(syncConfig, "a\n", "A\n", 1), "h\n", "fixer\n", 1), "Fix the last line")
	merge := s.updateBranch("config.rb", strings.Replace(syncConfig, "h\n", "main\n", 1))

	code, out, _ := s.fixSync(t)
	want := "failed: replaying your 1 commit onto " + textx.ShortSHA(merge) + " conflicts in config.rb; a rebase is still in progress: run `git rebase --abort`"
	if code != 3 || out != want {
		t.Fatalf("exit %d, stdout %q; want 3, %q", code, out, want)
	}
	s.git(s.slot, "rebase", "--abort")
}

// Anything but merges of the base on top of the start head, and anything
// in the fixer's checkout that a replay could lose or misread, changes
// nothing: a merge that changed the PR's own diff (a conflict resolved on
// GitHub, a change beside the merge), a branch rewritten past the start
// head, a merge the fixer made itself (the base merge of a conflicting PR,
// which a replay would redo without its resolutions), uncommitted changes,
// no commit to replay, and a branch that did not move.
func TestFixSyncRefusesWhatIsNotABaseMergeOnTop(t *testing.T) {
	fixed := strings.Replace(strings.Replace(syncConfig, "a\n", "A\n", 1), "h\n", "H\n", 1)
	for _, tc := range []struct {
		name string
		// setup changes origin and the slot and returns the reason's start.
		setup func(s *syncRepo) string
	}{
		{"a merge that changed the PR's own diff", func(s *syncRepo) string {
			return s.mergeChangingThePR(fixed, "AA\n")
		}},
		// Whitespace means something in YAML, Haml, Python and Makefiles.
		{"a merge that re-indented the PR's line", func(s *syncRepo) string {
			return s.mergeChangingThePR(fixed, "  A\n")
		}},
		{"a merge that changed the spaces in the PR's line", func(s *syncRepo) string {
			return s.mergeChangingThePR(fixed, "A \n")
		}},
		// A merge of the PR into the base pushed as the branch (the base
		// first): on a base made of merges alone, every commit the branch
		// gained is a merge, but the chain does not start on the start head.
		{"a reverse merge on a base of merges", func(s *syncRepo) string {
			s.commit(s.slot, "config.rb", fixed, "Fix the last line")
			s.git(s.origin, "switch", "--quiet", "-c", "side", "main")
			s.commit(s.origin, "docs/side.md", "side\n", "a change on a side branch")
			s.git(s.origin, "switch", "--quiet", "main")
			s.git(s.origin, "merge", "--quiet", "--no-ff", "--no-edit", "side")
			s.git(s.origin, "merge", "--quiet", "--no-ff", "--no-edit", "feature")
			reverse := s.git(s.origin, "rev-parse", "HEAD")
			s.git(s.origin, "reset", "--quiet", "--hard", "HEAD~1")
			s.git(s.origin, "branch", "--quiet", "--force", "feature", reverse)
			return "refused: the branch's merges do not follow the start head " + textx.ShortSHA(s.start) + " on their first parent"
		}},
		{"a rewritten branch", func(s *syncRepo) string {
			s.commit(s.slot, "config.rb", fixed, "Fix the last line")
			tip := s.onFeature(func() {
				s.git(s.origin, "reset", "--quiet", "--hard", "main")
				s.commit(s.origin, "config.rb", strings.Replace(syncConfig, "a\n", "A\n", 1), "the PR, rebased")
			})
			return "refused: the branch was rewritten: its tip " + textx.ShortSHA(tip) + " does not descend from the start head " + textx.ShortSHA(s.start)
		}},
		{"the fixer's own merge", func(s *syncRepo) string {
			s.commit(s.origin, "docs/main.md", "main moves\n", "main moves on")
			s.git(s.slot, "fetch", "--quiet", "origin", "main")
			s.git(s.slot, "merge", "--quiet", "--no-edit", "FETCH_HEAD")
			own := s.git(s.slot, "rev-parse", "HEAD")
			s.commit(s.slot, "config.rb", fixed, "Fix the last line")
			s.updateBranch("docs/more.md", "more\n")
			return "refused: your commits include the merge " + textx.ShortSHA(own) + ", which fix-sync does not replay"
		}},
		{"uncommitted changes", func(s *syncRepo) string {
			s.commit(s.slot, "config.rb", fixed, "Fix the last line")
			s.write(s.slot, "config.rb", fixed+"dirty\n")
			s.updateBranch("docs/main.md", "main moves\n")
			return "refused: the checkout has uncommitted changes to tracked files"
		}},
		{"no commit to replay", func(s *syncRepo) string {
			s.updateBranch("docs/main.md", "main moves\n")
			return "refused: you have no commit to replay: HEAD is the start head"
		}},
		{"a branch that did not move", func(s *syncRepo) string {
			s.commit(s.slot, "config.rb", fixed, "Fix the last line")
			return "refused: the branch is still at the start head " + textx.ShortSHA(s.start) + ": GitHub rejected the push for another reason"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSyncRepo(t)
			want := tc.setup(s)
			head := s.git(s.slot, "rev-parse", "HEAD")
			dirty := s.git(s.slot, "status", "--porcelain", "--untracked-files=no")
			code, out, _ := s.fixSync(t)
			if code != 1 || !strings.HasPrefix(out, want) {
				t.Fatalf("exit %d, stdout %q; want 1, %q...", code, out, want)
			}
			if got := s.git(s.slot, "rev-parse", "HEAD"); got != head {
				t.Errorf("HEAD = %s, want it as it was (%s)", got, head)
			}
			if got := s.git(s.slot, "status", "--porcelain", "--untracked-files=no"); got != dirty {
				t.Errorf("the checkout after the refusal: %q, want %q", got, dirty)
			}
		})
	}
}

// The fixer's shell may stand anywhere when its push is rejected (a `cd`
// into a subproject, a temporary directory), and the line the prompt names
// took the checkout from the current directory: outside the checkout it
// ran nothing (exit 2), or in another repository. The line names the
// slot's checkout, as the db-lock line does, and replays there from any
// directory.
func TestTheFixSyncLineReplaysInTheCheckoutItNamesFromAnyDirectory(t *testing.T) {
	s := newSyncRepo(t)
	s.commit(s.slot, "config.rb", strings.Replace(strings.Replace(syncConfig, "a\n", "A\n", 1), "h\n", "H\n", 1), "Fix the last line")
	merge := s.updateBranch("docs/main.md", "main moves\n")
	line := agents.FixSyncLine("magnum", s.slot, s.start, "feature", "main")
	args := strings.Fields(line)
	if strings.ContainsAny(line, `'"\`) || len(args) < 2 || args[0] != "magnum" {
		t.Fatalf("the line %q needs no quoting here", line)
	}
	t.Chdir(t.TempDir()) // no git work tree here

	c, out, errb := bareContext(t)
	c.layoutErr = os.ErrPermission
	if code := execute(c, args[1:]); code != 0 {
		t.Fatalf("%s: exit %d\nstdout: %s\nstderr: %s", line, code, out, errb)
	}
	if got := s.git(s.slot, "rev-parse", "HEAD~1"); got != merge {
		t.Errorf("HEAD~1 = %s, want the merge %s: the replay ran elsewhere", got, merge)
	}
}

// fix-sync needs its three flags and a commit id it can resolve; without
// them it runs nothing (exit 2).
func TestFixSyncChecksItsFlags(t *testing.T) {
	s := newSyncRepo(t)
	for _, args := range [][]string{
		{"--from", s.start, "--branch", "feature"},
		{"--from", s.start, "--base", "main"},
		{"--branch", "feature", "--base", "main"},
		{"--from", s.start, "--branch", "-x", "--base", "main"},
		{"--from", "no-such-commit", "--branch", "feature", "--base", "main"},
	} {
		c, out, errb := bareContext(t)
		c.layoutErr = os.ErrPermission
		if code := execute(c, append([]string{"fix-sync", "--checkout", s.slot}, args...)); code != 2 {
			t.Errorf("%v: exit %d, want 2\nstdout: %s\nstderr: %s", args, code, out, errb)
		}
		if !strings.HasPrefix(errb.String(), "fix-sync: ") {
			t.Errorf("%v: stderr = %q", args, errb)
		}
	}
}
