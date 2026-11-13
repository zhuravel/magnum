package fixwork

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// heldClone is the clone that holds the merge; no directory: the fake
	// git never reads it.
	heldClone = "/clones/talkable/talkable"
	// heldIso is the repository of the daemon's own the fake git reads in.
	heldIso = "/isolated/talkable"
	// heldPlainDiff is what the held merge's push changes in the workflow
	// files against the head.
	heldPlainDiff = "diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml\n@@ -1 +1 @@\n-runs-on: ubuntu-22.04\n+runs-on: ubuntu-24.04\n"
)

// fakeHeldGit is the git a held merge's diff is read with: a repository
// of its own at heldIso, the objects' check (verify: its answer), the
// remerge and plain diffs it gives, and the calls it got in their order.
type fakeHeldGit struct {
	verify  error
	remerge string
	plain   string
	calls   []string
}

func (g *fakeHeldGit) Isolated(_ context.Context, dir string, fn func(iso string) error) error {
	g.calls = append(g.calls, "isolated "+dir)
	return fn(heldIso)
}

func (g *fakeHeldGit) VerifyObjects(_ context.Context, dir, path string, commits ...string) error {
	var short []string
	for _, c := range commits {
		short = append(short, textx.ShortSHA(c))
	}
	g.calls = append(g.calls, fmt.Sprintf("verify %s %s at %s", dir, path, strings.Join(short, " ")))
	return g.verify
}

func (g *fakeHeldGit) RemergeDiff(_ context.Context, dir, merge string, paths ...string) (string, error) {
	g.calls = append(g.calls, fmt.Sprintf("remerge-diff %s %s %s", dir, textx.ShortSHA(merge), strings.Join(paths, " ")))
	return g.remerge, nil
}

func (g *fakeHeldGit) PathsDiff(_ context.Context, dir, a, b string, paths ...string) (string, error) {
	g.calls = append(g.calls, fmt.Sprintf("diff %s %s..%s %s", dir, textx.ShortSHA(a), textx.ShortSHA(b), strings.Join(paths, " ")))
	return g.plain, nil
}

// heldMerge is claude-fix's merge d3d3d3d of master at c0c0c0c into PR
// #2's head a1a1a1a, held for your y.
func heldMerge() HeldMerge {
	return HeldMerge{Repo: "talkable/talkable", Number: 2, Merge: strings.Repeat("d3", 20), Head: fixHead, Base: "master", Tip: strings.Repeat("c0", 20),
		Workflows: []string{".github/workflows/ci.yml"}, Conflicts: 1, Role: "claude-fix"}
}

// heldDiffHeader is held-merge.diff's first line for heldMerge.
const heldDiffHeader = "Held merge d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3: claude-fix's merge of master at c0c0c0c into the PR's head a1a1a1a.\n"

// heldReads are the calls of a read of heldMerge's diff: the repository
// of its own on the clone's objects, the check of the workflow files'
// objects at the head, the tip and the merge, then both diffs read there.
var heldReads = []string{"isolated " + heldClone, "verify " + heldIso + " .github/workflows at a1a1a1a c0c0c0c d3d3d3d",
	"remerge-diff " + heldIso + " d3d3d3d .github/workflows", "diff " + heldIso + " a1a1a1a..d3d3d3d .github/workflows"}

// wantHeldDiff fails unless HeldDiff of heldMerge on g is want, read with
// heldReads.
func wantHeldDiff(t *testing.T, g *fakeHeldGit, want string) {
	t.Helper()
	b, err := HeldDiff(t.Context(), g, heldClone, heldMerge())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Errorf("held-merge.diff = %q\nwant %q", b, want)
	}
	if !slices.Equal(g.calls, heldReads) {
		t.Errorf("git calls = %q, want %q", g.calls, heldReads)
	}
}

// A held merge that resolved a conflict in a workflow file shows the
// merge, then how the fixer resolved it (the remerge diff, ended with a
// newline), then what its push changes against the head, both read in a
// repository of the daemon's own after the workflow files' objects at the
// head, the tip and the merge were hashed again: no setting of the clone
// applies.
func TestAHeldMergeThatResolvedAWorkflowConflictShowsTheResolutionThenThePlainDiff(t *testing.T) {
	remerge := "diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml\nremerge CONFLICT (content): Merge conflict in .github/workflows/ci.yml\n" +
		"-<<<<<<< a1a1a1a\n-runs-on: ubuntu-22.04\n-=======\n-runs-on: ubuntu-latest\n->>>>>>> c0c0c0c\n+runs-on: ubuntu-24.04"
	wantHeldDiff(t, &fakeHeldGit{remerge: remerge, plain: heldPlainDiff}, heldDiffHeader+remerge+"\n"+heldPlainDiff)
}

// A held merge whose workflow files merged with no conflict has an empty
// remerge diff there (blank lines alone): held-merge.diff says so in one
// line that names the base, then shows the plain diff.
func TestAHeldMergeWithNoWorkflowConflictSaysSoInOneLineThenThePlainDiff(t *testing.T) {
	const line = "No conflict in a workflow file was resolved: the workflow changes come unchanged from master and the PR.\n"
	wantHeldDiff(t, &fakeHeldGit{remerge: "\n \n", plain: heldPlainDiff}, heldDiffHeader+line+heldPlainDiff)
}

// An object of the workflow files in the clone whose content is not its
// name (gitx.ErrObjectMismatch) ends the read before either diff: what git
// would show of it is not what GitHub would get.
func TestAForgedWorkflowObjectEndsTheHeldDiffBeforeAnyDiffIsRead(t *testing.T) {
	forged := fmt.Errorf("gitx: verify objects: %s: %w", strings.Repeat("e5", 20), gitx.ErrObjectMismatch)
	g := &fakeHeldGit{verify: forged, remerge: "+run: deploy\n", plain: heldPlainDiff}
	b, err := HeldDiff(t.Context(), g, heldClone, heldMerge())
	if !errors.Is(err, gitx.ErrObjectMismatch) || b != nil {
		t.Fatalf("HeldDiff = %q, %v; want no diff and gitx.ErrObjectMismatch", b, err)
	}
	if want := heldReads[:2]; !slices.Equal(g.calls, want) {
		t.Errorf("git calls = %q, want %q", g.calls, want)
	}
}

// Workflow content an agent wrote reaches a pager that runs with LESS=R:
// held-merge.diff, as WriteHeldDiff writes it, has every control
// character but tab and newline (C0, DEL, C1) written as \xNN or \uNNNN,
// and every bidi or invisible format character and line or paragraph
// separator as \uNNNN, in the remerge diff and the plain diff alike; a
// byte that is no UTF-8 stays. The file is private, in a private
// directory it makes.
func TestTheHeldDiffWrittenEscapesTheControlAndFormatCharactersThatCouldHideALine(t *testing.T) {
	g := &fakeHeldGit{
		remerge: "+run: deploy\x1b[2K\r+run: echo ok\x7f\t# tab\n+if: ${{ github.actor == 'alice' \u202e && true }}\u2066\u200b\ufeff\n" +
			"+run: curl 10.0.\u061c0.1\u061c\u2028\u2029\u0085\n+bytes: \xff\n",
		plain: "+env: A=\x00\x1f\u200e\u200f\n",
	}
	b, err := HeldDiff(t.Context(), g, heldClone, heldMerge())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "reviews", "talkable", "talkable", "2")
	path := filepath.Join(dir, HeldMergeFile)
	if err := WriteHeldDiff(dir, path, b); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := heldDiffHeader + "+run: deploy\\x1b[2K\\x0d+run: echo ok\\x7f\t# tab\n+if: ${{ github.actor == 'alice' \\u202e && true }}\\u2066\\u200b\\ufeff\n" +
		"+run: curl 10.0.\\u061c0.1\\u061c\\u2028\\u2029\\u0085\n+bytes: \xff\n" + "+env: A=\\x00\\x1f\\u200e\\u200f\n"
	if string(got) != want {
		t.Errorf("held-merge.diff = %q\nwant %q", got, want)
	}
	for p, mode := range map[string]os.FileMode{path: 0o600, dir: 0o700 | os.ModeDir} {
		if info, err := os.Stat(p); err != nil || info.Mode() != mode {
			t.Errorf("%s: %v, %v; want mode %v", p, info.Mode(), err, mode)
		}
	}
}
