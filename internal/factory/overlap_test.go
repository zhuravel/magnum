package factory

import (
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// The overlap line names what else touches the task's work: the commits
// that name its key on any branch but its own, the PRs that name the key
// (its own left out) and the open PRs that change the files its report
// names. It is advisory: a read that fails leaves the others' part, and
// the task goes on as it was.
func TestOverlapNamesCommitsPRsAndFilesButNeverStops(t *testing.T) {
	r := newIntakeRig(t)
	r.run.Rules = []execx.Rule{
		{Prefix: []string{"git", "log"}, Result: execx.Result{Stdout: []byte("abc1234\tSpec the reminder\ndef5678\tPR-12345 first try\n")}},
		{Prefix: []string{"gh", "pr", "list", "--repo", "talkable/talkable", "--search"}, Result: execx.Result{Stdout: []byte(
			`[{"number":4501,"state":"OPEN","title":"PR-12345 an older try"},{"number":4527,"state":"OPEN","title":"its own"},` +
				`{"number":4490,"state":"MERGED","title":"PR-12345 groundwork"}]`)}},
		{Prefix: []string{"gh", "pr", "list", "--repo", "talkable/talkable", "--state", "open"}, Result: execx.Result{Stdout: []byte(
			`[{"number":4512,"files":[{"path":"app/models/credit.rb"},{"path":"README.md"}]},` +
				`{"number":4527,"files":[{"path":"app/models/credit.rb"}]},` +
				`{"number":4520,"files":[{"path":"app/models/credit.rb"},{"path":"app/mailers/reminder_mailer.rb"}]},` +
				`{"number":4530,"files":[{"path":"app/models/other.rb"}]}]`)}},
	}
	task := r.mustStart("PR-12345")
	if err := r.reg.TransitionTask(r.ctx, task.ID, nil, "", func(u *registry.TaskUpdate) { u.Set("pr_number", 4527) }); err != nil {
		t.Fatal(err)
	}
	task = r.reload(task.ID)

	line, err := r.e.Overlap(r.ctx, task, []string{"app/models/credit.rb", "app/mailers/reminder_mailer.rb", "spec/models/credit_spec.rb"})
	if err != nil {
		t.Fatal(err)
	}
	want := "overlap: 2 commits name PR-12345 (newest abc1234) · PRs #4501 (open), #4490 (merged) name PR-12345 · " +
		"#4512 changes app/models/credit.rb, #4520 changes 2 of its files"
	if line != want {
		t.Fatalf("overlap\n%q\nwant\n%q", line, want)
	}
	logs := r.run.CallsWithPrefix("git", "log")
	if len(logs) == 0 {
		t.Fatal("no git log")
	}
	argv := logs[len(logs)-1].Args
	for _, arg := range []string{"--exclude=refs/heads/" + task.Branch, "--all", "--fixed-strings", "--grep=PR-12345"} {
		if !slices.Contains(argv, arg) {
			t.Errorf("git log %q lacks %q", argv, arg)
		}
	}
	if i, j := slices.Index(argv, "--exclude=refs/heads/"+task.Branch), slices.Index(argv, "--all"); i < 0 || i > j {
		t.Errorf("git log %q: the exclusion must come before --all", argv)
	}
	if logs[len(logs)-1].Dir != r.cfg.Factory.Repos[0].MainClone {
		t.Errorf("git log ran in %q, want the main clone", logs[len(logs)-1].Dir)
	}
	for _, c := range r.run.CallsWithPrefix("gh") {
		if c.Env["GH_HOST"] != "github.com" || c.Mutates {
			t.Errorf("gh %q: env %v, mutates %v; want a read of github.com", c.Args, c.Env, c.Mutates)
		}
	}
	// The start recorded the overlap it found then, as an advisory event.
	if evs := r.events(task.ID, "overlap"); len(evs) != 1 || evs[0].Phase != registry.PhaseInfo || !strings.Contains(evs[0].Text, "2 commits name PR-12345") {
		t.Fatalf("overlap events %+v, want the line of the start", evs)
	}

	// A failed read leaves the other parts and stops nothing.
	r.run.Rules = []execx.Rule{
		{Prefix: []string{"git", "log"}, Result: execx.Result{Stdout: []byte("abc1234\tSpec the reminder\n")}},
		{Prefix: []string{"gh"}, Result: execx.Result{Code: 1, Stderr: []byte("HTTP 502")}},
	}
	line, err = r.e.Overlap(r.ctx, task, []string{"app/models/credit.rb"})
	if err == nil || line != "overlap: 1 commit names PR-12345 (newest abc1234)" {
		t.Fatalf("overlap with gh down = %q, %v; want the commits' part and an error", line, err)
	}
	other := r.mustStart("PR-12346")
	if got := r.reload(other.ID); got.State != registry.TaskActive {
		t.Fatalf("a start whose overlap failed is %s, want active", got.State)
	}

	// A prompt's task has no key: without files there is nothing to read.
	calls := len(r.run.Calls)
	prompt := r.mustStart("Rename the referral helper")
	if line, err := r.e.Overlap(r.ctx, prompt, nil); line != "" || err != nil {
		t.Fatalf("a prompt's overlap without files = %q, %v", line, err)
	}
	if n := len(r.run.Calls); n != calls {
		t.Fatalf("a prompt's overlap ran %d commands", n-calls)
	}
}
