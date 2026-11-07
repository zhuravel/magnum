package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

const (
	botName  = "fixer-app[bot]"
	botEmail = "4242+fixer-app[bot]@users.noreply.github.com"
	prURL    = "https://github.com/talkable/talkable/pull/7"
)

// paneEnv is a fixer App pane's env as magnum push reads it.
func paneEnv() map[string]string {
	return map[string]string{
		"MAGNUM_PR_URL": prURL, "MAGNUM_PUSH_BRANCH": "feature", "MAGNUM_DEFAULT_BRANCH": "main", "MAGNUM_BASE_BRANCH": "main",
		"GIT_AUTHOR_NAME": botName, "GIT_AUTHOR_EMAIL": botEmail, "GIT_COMMITTER_NAME": botName, "GIT_COMMITTER_EMAIL": botEmail,
		"MAGNUM_PUSH_NO_WORKFLOWS": "1",
	}
}

// withPane makes magnum push read env as its pane's, and run git with r.
func withPane(t *testing.T, env map[string]string, r execx.Runner) {
	t.Helper()
	storetest.Serial(t)
	prevGet, prevRun := pushGetenv, pushRunner
	pushGetenv = func(k string) string { return env[k] }
	pushRunner = func() execx.Runner { return r }
	t.Cleanup(func() { pushGetenv, pushRunner = prevGet, prevRun })
}

// push runs `magnum push` with args and returns its exit code, its JSON
// outcome and its stderr line.
func push(t *testing.T, args ...string) (int, pushOutcome, string) {
	t.Helper()
	c, out, errb := bareContext(t)
	c.layoutErr = os.ErrPermission // no magnum layout: the command needs none
	code := execute(c, append([]string{"push"}, args...))
	var o pushOutcome
	if err := json.Unmarshal(out.Bytes(), &o); err != nil {
		t.Fatalf("stdout %q is no JSON outcome: %v (stderr %q)", out.String(), err, errb.String())
	}
	return code, o, strings.TrimSpace(errb.String())
}

// ---- real git, hermetic ----

// pushRepo is a throwaway origin whose feature branch is the PR's, and the
// fixer's slot: a clone detached at feature's head whose origin reads as
// github.com/talkable/talkable (as a pool clone's does), while magnum push
// reaches the throwaway origin (pushTarget). The slot commits as the bot,
// as the fixer App's pane does.
type pushRepo struct {
	*syncRepo
	env map[string]string
}

func newPushRepo(t *testing.T) *pushRepo {
	t.Helper()
	s := newSyncRepo(t)
	for i, kv := range s.r.BaseEnv {
		switch {
		case strings.HasPrefix(kv, "GIT_AUTHOR_NAME="), strings.HasPrefix(kv, "GIT_COMMITTER_NAME="):
			s.r.BaseEnv[i] = kv[:strings.Index(kv, "=")+1] + botName
		case strings.HasPrefix(kv, "GIT_AUTHOR_EMAIL="), strings.HasPrefix(kv, "GIT_COMMITTER_EMAIL="):
			s.r.BaseEnv[i] = kv[:strings.Index(kv, "=")+1] + botEmail
		}
	}
	s.git(s.slot, "remote", "set-url", "origin", "git@github.com:talkable/talkable.git")
	p := &pushRepo{syncRepo: s, env: paneEnv()}
	withPane(t, p.env, s.r)
	prev := pushTarget
	pushTarget = func(owner, repo string) string {
		if owner != "talkable" || repo != "talkable" {
			t.Errorf("pushTarget(%s, %s), want talkable/talkable", owner, repo)
		}
		return s.origin
	}
	t.Cleanup(func() { pushTarget = prev })
	return p
}

// fix commits a change of config.rb's last line in the slot, as the bot.
func (p *pushRepo) fix() string {
	p.t.Helper()
	return p.commit(p.slot, "config.rb", strings.Replace(strings.Replace(syncConfig, "a\n", "A\n", 1), "h\n", "H\n", 1), "Fix the last line")
}

// feature is origin's feature head.
func (p *pushRepo) feature() string { return p.git(p.origin, "rev-parse", "refs/heads/feature") }

// The fixer's commit goes to the PR's branch on GitHub with a lease on the
// head magnum push read there: exit 0, "pushed", and the branch is at HEAD.
func TestPushPushesTheFixersCommitToThePRsBranch(t *testing.T) {
	p := newPushRepo(t)
	head := p.fix()
	code, o, errb := push(t, "--checkout", p.slot)
	if code != 0 || o.Status != pushStatusPushed || o.SHA != head || o.Expect != p.start || o.Branch != "feature" {
		t.Fatalf("exit %d, %+v (%s)", code, o, errb)
	}
	if got := p.feature(); got != head {
		t.Errorf("origin's feature = %s, want %s", got, head)
	}
	if !strings.Contains(errb, "pushed") {
		t.Errorf("stderr = %q", errb)
	}
}

// Nothing new on HEAD: exit 0, "up_to_date", and nothing is pushed.
func TestPushSaysUpToDate(t *testing.T) {
	p := newPushRepo(t)
	if code, o, _ := push(t, "--checkout", p.slot); code != 0 || o.Status != pushStatusUpToDate {
		t.Fatalf("exit %d, %+v", code, o)
	}
}

// The branch moved on GitHub since the fixer started (someone pushed):
// HEAD does not descend from its head, so magnum push pushes nothing and
// says "fetch first" (exit 3), the fix-sync line's cue.
func TestPushRefusesACommitThatDoesNotDescend(t *testing.T) {
	p := newPushRepo(t)
	p.fix()
	moved := p.onFeature(func() { p.commit(p.origin, "docs/x.md", "x\n", "someone pushes") })
	code, o, _ := push(t, "--checkout", p.slot)
	if code != pushRejected || o.Status != pushStatusRejected || !strings.Contains(o.Message, "fetch first") {
		t.Fatalf("exit %d, %+v", code, o)
	}
	if got := p.feature(); got != moved {
		t.Errorf("origin's feature = %s, want it left at %s", got, moved)
	}
}

// Every commit the push adds on the first-parent chain must be the bot's:
// a commit by anyone else (the operator's own, an agent's with another
// identity) is refused (exit 2), never pushed as the App's.
func TestPushRefusesACommitNotByTheFixer(t *testing.T) {
	p := newPushRepo(t)
	p.fix()
	p.git(p.slot, "-c", "user.name=alice", "-c", "user.email=alice@example.com", "commit", "--quiet", "--allow-empty",
		"--author=alice <alice@example.com>", "-m", "not the bot's")
	code, o, _ := push(t, "--checkout", p.slot)
	if code != pushRefused || o.Status != pushStatusRefused || !strings.Contains(o.Message, "is not by "+botName) {
		t.Fatalf("exit %d, %+v", code, o)
	}
	if got := p.feature(); got != p.start {
		t.Errorf("origin's feature moved to %s", got)
	}
}

// The committer counts as the author does: a commit the bot authored but
// someone else committed (a cherry-pick or an amend after the pane's
// GIT_COMMITTER_* were unset) is not the fixer's alone, so it is refused
// (exit 2), never pushed as the App's.
func TestPushRefusesACommitTheBotAuthoredButAnotherCommitted(t *testing.T) {
	p := newPushRepo(t)
	p.fix()
	if _, err := p.r.Run(context.Background(), execx.Cmd{Name: "git", Args: []string{"-C", p.slot, "commit", "--quiet", "--allow-empty", "-m", "committed by alice"},
		Env: map[string]string{"GIT_COMMITTER_NAME": "alice", "GIT_COMMITTER_EMAIL": "alice@example.com"}}); err != nil {
		t.Fatal(err)
	}
	if got := p.git(p.slot, "log", "-1", "--format=%ae %ce"); got != botEmail+" alice@example.com" {
		t.Fatalf("the commit's author and committer = %q", got)
	}
	code, o, _ := push(t, "--checkout", p.slot)
	if code != pushRefused || o.Status != pushStatusRefused || !strings.Contains(o.Message, "is not by "+botName) {
		t.Fatalf("exit %d, %+v", code, o)
	}
	if got := p.feature(); got != p.start {
		t.Errorf("origin's feature moved to %s", got)
	}
}

// The base merge the fixer makes when the PR conflicts with its base is
// the bot's commit on the first-parent chain, and the base commits it
// brings were on GitHub already: the push goes.
func TestPushAllowsTheBaseCommitsAMergeBrings(t *testing.T) {
	p := newPushRepo(t)
	p.fix()
	p.commit(p.origin, "docs/main.md", "main moves\n", "main moves on") // by Test, not the bot
	p.git(p.slot, "fetch", "--quiet", p.origin, "+refs/heads/main:refs/remotes/origin/main")
	p.git(p.slot, "merge", "--quiet", "--no-edit", "origin/main")
	head := p.git(p.slot, "rev-parse", "HEAD")
	if code, o, _ := push(t, "--checkout", p.slot); code != 0 || o.Status != pushStatusPushed {
		t.Fatalf("exit %d, %+v", code, o)
	}
	if got := p.feature(); got != head {
		t.Errorf("origin's feature = %s, want the merge %s", got, head)
	}
}

// A merge of anything but the base brings someone's work the push would
// publish as the bot's: refused (exit 2).
func TestPushRefusesAMergeOfABranchOtherThanTheBase(t *testing.T) {
	p := newPushRepo(t)
	p.fix()
	p.git(p.origin, "switch", "--quiet", "-c", "other", "main")
	p.commit(p.origin, "docs/other.md", "other\n", "another branch")
	p.git(p.origin, "switch", "--quiet", "main")
	p.git(p.slot, "fetch", "--quiet", p.origin, "+refs/heads/other:refs/remotes/origin/other")
	p.git(p.slot, "merge", "--quiet", "--no-edit", "origin/other")
	code, o, _ := push(t, "--checkout", p.slot)
	if code != pushRefused || !strings.Contains(o.Message, "merges a branch other than main") {
		t.Fatalf("exit %d, %+v", code, o)
	}
}

// A PR retargeted while its fixer's pane stays live (a stacked PR whose
// base merged) keeps the old base in the pane's MAGNUM_BASE_BRANCH, made
// with the pane: the push line names the PR's base each cycle (--base),
// which wins, so the fixer's merge of the new base goes. The pane's old
// base stays a branch magnum push never pushes to, and a --base a shell
// would read is refused (exit 2) before any git.
func TestPushTakesTheBaseOfItsLineOverThePanes(t *testing.T) {
	p := newPushRepo(t)
	p.env["MAGNUM_BASE_BRANCH"] = "release" // the base when the pane was made
	p.fix()
	p.commit(p.origin, "docs/main.md", "main moves\n", "main moves on")
	p.git(p.slot, "fetch", "--quiet", p.origin, "+refs/heads/main:refs/remotes/origin/main")
	p.git(p.slot, "merge", "--quiet", "--no-edit", "origin/main")
	head := p.git(p.slot, "rev-parse", "HEAD")
	if code, o, _ := push(t, "--checkout", p.slot); code != pushRefused || !strings.Contains(o.Message, "merges a branch other than release") {
		t.Fatalf("the pane's stale base: exit %d, %+v; want the merge of main refused", code, o)
	}
	if code, o, _ := push(t, "--checkout", p.slot, "--base", "main"); code != 0 || o.Status != pushStatusPushed {
		t.Fatalf("--base main: exit %d, %+v", code, o)
	}
	if got := p.feature(); got != head {
		t.Errorf("origin's feature = %s, want the merge %s", got, head)
	}

	for name, tc := range map[string]struct {
		branch, base string
	}{
		"the pane's old base": {"release", "main"},
		"the line's base":     {"next", "next"},
		"an unsafe base":      {"feature", "main;rm -rf"},
	} {
		t.Run(name, func(t *testing.T) {
			env := paneEnv()
			env["MAGNUM_BASE_BRANCH"], env["MAGNUM_PUSH_BRANCH"] = "release", tc.branch
			f := &execx.Fake{}
			withPane(t, env, f)
			if code, o, _ := push(t, "--checkout", t.TempDir(), "--base", tc.base); code != pushRefused || o.Status != pushStatusRefused {
				t.Errorf("exit %d, %+v; want refused", code, o)
			}
			if len(f.Calls) != 0 {
				t.Errorf("git ran: %v", f.Calls)
			}
		})
	}
}

// A fixer App has no workflows permission, and GitHub refuses its push of
// a workflow change: magnum push refuses it first (exit 2), with the words
// that send it under Needs you. Without MAGNUM_PUSH_NO_WORKFLOWS (a
// fixer that may push workflows) the same push goes.
func TestPushRefusesAWorkflowChangeForAnApp(t *testing.T) {
	p := newPushRepo(t)
	p.commit(p.slot, ".github/workflows/ci.yml", "on: push\n", "Run CI on push")
	code, o, _ := push(t, "--checkout", p.slot)
	if code != pushRefused || !strings.Contains(o.Message, ".github/workflows/ci.yml") || !strings.Contains(o.Message, "Needs you") {
		t.Fatalf("exit %d, %+v", code, o)
	}
	delete(p.env, "MAGNUM_PUSH_NO_WORKFLOWS")
	if code, o, _ := push(t, "--checkout", p.slot); code != 0 || o.Status != pushStatusPushed {
		t.Fatalf("without the refusal: exit %d, %+v", code, o)
	}
}

// The App's identity is the pane's env alone: magnum push runs no git
// config that writes, and the clone's config is byte for byte the same
// after its push.
func TestPushWritesNoGitConfig(t *testing.T) {
	p := newPushRepo(t)
	p.fix()
	rec := &recordingRunner{Runner: p.r}
	pushRunner = func() execx.Runner { return rec }
	cfgPath := filepath.Join(p.git(p.slot, "rev-parse", "--absolute-git-dir"), "config")
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if code, o, _ := push(t, "--checkout", p.slot); code != 0 {
		t.Fatalf("exit %d, %+v", code, o)
	}
	if w := configWrites(rec.calls); len(w) != 0 {
		t.Errorf("magnum push wrote git config: %q", w)
	}
	if after, err := os.ReadFile(cfgPath); err != nil || string(after) != string(before) {
		t.Errorf("the clone's config changed:\nbefore %q\nafter  %q (%v)", before, after, err)
	}
}

// ---- fake git ----

const (
	fakeHead   = "1111111111111111111111111111111111111111"
	fakeRemote = "2222222222222222222222222222222222222222"
	httpsURL   = "https://github.com/talkable/talkable.git"
)

// fakePush is a git that answers magnum push's calls in its checkout dir
// (a real directory, nothing in it): origin is origin, HEAD fakeHead, the
// branch on GitHub fakeRemote until a push, then after (fakeRemote again
// when after is ""); pushErr fails the push.
type fakePush struct {
	*execx.Fake
	dir    string
	mu     sync.Mutex
	pushed bool
}

func newFakePush(t *testing.T, origin, after string, pushErr *execx.Result) *fakePush {
	t.Helper()
	f := &fakePush{dir: resolvedDir(t, t.TempDir())}
	in := func(args ...string) []string { return append([]string{"git", "-C", f.dir}, args...) }
	push := execx.Rule{Prefix: in("-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential", "push"),
		Fn: func(execx.Cmd) (execx.Result, error) {
			f.mu.Lock()
			f.pushed = true
			f.mu.Unlock()
			return execx.Result{}, nil
		}}
	if pushErr != nil {
		push = execx.Rule{Prefix: push.Prefix, Result: *pushErr}
	}
	f.Fake = &execx.Fake{Rules: []execx.Rule{
		{Prefix: in("remote", "get-url", "origin"), Result: execx.Result{Stdout: []byte(origin + "\n")}},
		{Prefix: in("rev-parse", "--verify", "--quiet", "HEAD^{commit}"), Result: execx.Result{Stdout: []byte(fakeHead + "\n")}},
		{Prefix: in("rev-parse", "--verify", "--quiet", fakeRemote+"^{commit}"), Result: execx.Result{Stdout: []byte(fakeRemote + "\n")}},
		{Prefix: in("-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential", "ls-remote"),
			Fn: func(execx.Cmd) (execx.Result, error) {
				f.mu.Lock()
				sha := fakeRemote
				if f.pushed && after != "" {
					sha = after
				}
				f.mu.Unlock()
				return execx.Result{Stdout: []byte(sha + "\trefs/heads/feature\n")}, nil
			}},
		{Prefix: in("merge-base", "--is-ancestor"), Result: execx.Result{}},
		{Prefix: in("log", "--first-parent"), Result: execx.Result{Stdout: []byte(fakeHead + "\x00" + fakeRemote + "\x00" + botEmail + "\x00" + botEmail + "\n")}},
		{Prefix: in("diff", "--name-only"), Result: execx.Result{}},
		push,
	}}
	return f
}

// argvs are the git command lines f ran, as one string each.
func (f *fakePush) argvs() []string {
	var out []string
	for _, c := range f.Calls {
		out = append(out, strings.Join(append([]string{c.Name}, c.Args...), " "))
	}
	return out
}

// pushArgv is the push command line f ran, nil for none.
func (f *fakePush) pushArgv() []string {
	for _, c := range f.Calls {
		if slices.Contains(c.Args, "push") {
			return c.Args
		}
	}
	return nil
}

// The push goes to the PR's repository over HTTPS, with gh as git's only
// credential helper (the pane's GH_CONFIG_DIR makes it the App), a lease on
// the head magnum push read there and HEAD's commit to the PR's branch.
func TestPushPushesHeadToThePRBranchOverHTTPSWithALease(t *testing.T) {
	f := newFakePush(t, "git@github.com:talkable/talkable.git", fakeHead, nil)
	withPane(t, paneEnv(), f)
	code, o, _ := push(t, "--checkout", f.dir)
	if code != 0 || o.Status != pushStatusPushed {
		t.Fatalf("exit %d, %+v; calls %q", code, o, f.argvs())
	}
	want := []string{"-C", f.dir, "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential",
		"push", "--quiet", "--force-with-lease=refs/heads/feature:" + fakeRemote, httpsURL, fakeHead + ":refs/heads/feature"}
	if got := f.pushArgv(); !slices.Equal(got, want) {
		t.Errorf("push argv = %q\nwant %q", got, want)
	}
}

// An HTTPS origin would make git use the operator's own credential helper
// (a keychain login), and an SSH origin the operator's key: magnum push
// builds the URL from MAGNUM_PR_URL and names gh as the only helper,
// whatever form origin has.
func TestPushUsesTheGhHelperWhateverTheOriginsForm(t *testing.T) {
	var argvs [][]string
	for _, origin := range []string{"https://github.com/talkable/talkable.git", "git@github.com:talkable/talkable.git", "ssh://git@github.com/talkable/talkable"} {
		f := newFakePush(t, origin, fakeHead, nil)
		withPane(t, paneEnv(), f)
		if code, o, _ := push(t, "--checkout", f.dir); code != 0 {
			t.Fatalf("origin %s: exit %d, %+v", origin, code, o)
		}
		argvs = append(argvs, f.pushArgv()[2:]) // past -C and the checkout
	}
	for _, a := range argvs[1:] {
		if !slices.Equal(a, argvs[0]) {
			t.Errorf("push argvs differ by origin: %q and %q", argvs[0], a)
		}
	}
	if !slices.Contains(argvs[0], httpsURL) || slices.Contains(argvs[0], "origin") {
		t.Errorf("push argv %q, want the HTTPS URL and never origin", argvs[0])
	}
}

// magnum push always pushes to MAGNUM_PUSH_BRANCH: it refuses a branch that
// is master, main, the default branch or the base, a name a shell would
// read, no branch at all, and any argument (it cannot name another branch),
// each with exit 2 and before any git command.
func TestPushRefusesMasterMainAndAnotherBranch(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(map[string]string)
		args []string
	}{
		"master":             {func(e map[string]string) { e["MAGNUM_PUSH_BRANCH"] = "master" }, nil},
		"main":               {func(e map[string]string) { e["MAGNUM_PUSH_BRANCH"] = "main" }, nil},
		"the default branch": {func(e map[string]string) { e["MAGNUM_PUSH_BRANCH"], e["MAGNUM_DEFAULT_BRANCH"] = "develop", "develop" }, nil},
		"the base":           {func(e map[string]string) { e["MAGNUM_PUSH_BRANCH"], e["MAGNUM_BASE_BRANCH"] = "release", "release" }, nil},
		"no branch":          {func(e map[string]string) { delete(e, "MAGNUM_PUSH_BRANCH") }, nil},
		"an unsafe name":     {func(e map[string]string) { e["MAGNUM_PUSH_BRANCH"] = "feat;rm -rf" }, nil},
		"an argument":        {func(map[string]string) {}, []string{"other-branch"}},
		"no PR":              {func(e map[string]string) { delete(e, "MAGNUM_PR_URL") }, nil},
	} {
		t.Run(name, func(t *testing.T) {
			env := paneEnv()
			tc.edit(env)
			f := &execx.Fake{}
			withPane(t, env, f)
			code, o, _ := push(t, append([]string{"--checkout", t.TempDir()}, tc.args...)...)
			if code != pushRefused || o.Status != pushStatusRefused {
				t.Errorf("exit %d, %+v; want refused", code, o)
			}
			if len(f.Calls) != 0 {
				t.Errorf("git ran: %v", f.Calls)
			}
		})
	}
}

// magnum push is a fixer App's command, whose pane sets the bot as both the
// author and the committer: without GIT_AUTHOR_EMAIL, or with a committer
// of another email, it refuses (exit 2) and runs no git.
func TestPushRefusesWithoutTheBotsEmails(t *testing.T) {
	for name, edit := range map[string]func(map[string]string){
		"no author email":    func(e map[string]string) { delete(e, "GIT_AUTHOR_EMAIL") },
		"another committer":  func(e map[string]string) { e["GIT_COMMITTER_EMAIL"] = "zhuravel@example.com" },
		"no committer email": func(e map[string]string) { delete(e, "GIT_COMMITTER_EMAIL") },
	} {
		t.Run(name, func(t *testing.T) {
			env := paneEnv()
			edit(env)
			f := &execx.Fake{}
			withPane(t, env, f)
			if code, o, _ := push(t, "--checkout", t.TempDir()); code != pushRefused || !strings.Contains(o.Message, "GIT_AUTHOR_EMAIL") {
				t.Errorf("exit %d, %+v; want refused", code, o)
			}
			if len(f.Calls) != 0 {
				t.Errorf("git ran: %v", f.Calls)
			}
		})
	}
}

// A checkout whose origin is another repository than the PR's would push
// the PR's commits where they do not belong: refused (exit 2), as is an
// origin that is no GitHub repository.
func TestPushRefusesAnOriginOfAnotherRepository(t *testing.T) {
	for _, origin := range []string{"git@github.com:talkable/other.git", "https://example.com/talkable/talkable.git", "/srv/git/talkable"} {
		f := newFakePush(t, origin, fakeHead, nil)
		withPane(t, paneEnv(), f)
		if code, o, _ := push(t, "--checkout", f.dir); code != pushRefused || !strings.Contains(o.Message, "origin") {
			t.Errorf("origin %s: exit %d, %+v; want refused", origin, code, o)
		}
		if f.pushArgv() != nil {
			t.Errorf("origin %s: pushed", origin)
		}
	}
}

// A push GitHub took that the branch does not show afterwards is not a
// push: exit 1, "not seen on GitHub".
func TestPushConfirmsTheBranchAfterThePush(t *testing.T) {
	f := newFakePush(t, "git@github.com:talkable/talkable.git", "", nil)
	withPane(t, paneEnv(), f)
	if code, o, _ := push(t, "--checkout", f.dir); code != pushFailed || !strings.Contains(o.Message, "not seen on GitHub") {
		t.Fatalf("exit %d, %+v", code, o)
	}
}

// Everything magnum push prints passes execx.Redact: a token in git's
// answer is masked on stdout and stderr.
func TestPushRedactsWhatItPrints(t *testing.T) {
	f := newFakePush(t, "git@github.com:talkable/talkable.git", fakeHead,
		&execx.Result{Code: 128, Stderr: []byte("remote: Invalid username or token ghs_abcdefghijklmnopqrstuvwxyz0123.\nfatal: Authentication failed\n")})
	withPane(t, paneEnv(), f)
	code, o, errb := push(t, "--checkout", f.dir)
	if code != pushFailed || o.Status != pushStatusError {
		t.Fatalf("exit %d, %+v", code, o)
	}
	if strings.Contains(o.Message+errb, "ghs_") {
		t.Errorf("a token was printed: %q / %q", o.Message, errb)
	}
}

// A lease GitHub refuses because the branch moved between the read and the
// push is "fetch first" (exit 3), as a commit that does not descend is.
func TestPushSaysFetchFirstOnAStaleLease(t *testing.T) {
	f := newFakePush(t, "git@github.com:talkable/talkable.git", fakeHead,
		&execx.Result{Code: 1, Stderr: []byte(" ! [rejected]        1111111 -> feature (stale info)\nerror: failed to push some refs\n")})
	withPane(t, paneEnv(), f)
	if code, o, _ := push(t, "--checkout", f.dir); code != pushRejected || !strings.Contains(o.Message, "fetch first") {
		t.Fatalf("exit %d, %+v", code, o)
	}
}

// magnum push never forces: no command it runs, in any test above's shape,
// holds a bare --force or a + refspec.
func TestPushNeverForces(t *testing.T) {
	f := newFakePush(t, "git@github.com:talkable/talkable.git", fakeHead, nil)
	withPane(t, paneEnv(), f)
	if code, _, _ := push(t, "--checkout", f.dir); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, c := range f.Calls {
		for _, a := range c.Args {
			if a == "--force" || a == "-f" || (strings.HasPrefix(a, "--force") && !strings.HasPrefix(a, "--force-with-lease=")) || strings.HasPrefix(a, "+") {
				t.Errorf("%v forces with %q", c.Args, a)
			}
		}
	}
}

// magnum push needs no magnum home, config or registry: the pane's env and
// git are all it reads (push's context has no layout at all).
func TestPushRunsWithoutConfigOrRegistry(t *testing.T) {
	f := newFakePush(t, "git@github.com:talkable/talkable.git", fakeHead, nil)
	withPane(t, paneEnv(), f)
	c, out, _ := bareContext(t)
	c.layoutErr = os.ErrPermission
	c.cfgPath = filepath.Join(t.TempDir(), "missing.toml")
	if code := execute(c, []string{"push", "--checkout", f.dir}); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
}
