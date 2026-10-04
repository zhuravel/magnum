package slots

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
)

// talkableWT is a worktrunk config shaped like ~/Projects/talkable/.config/wt.toml
// plus a post-create hook, a second post-start entry (file order, not key
// order, must win) and a non-hook table whose template magnum ignores.
const talkableWT = `# Worktrunk hooks
[post-create]
deps = "echo create {{ repo }} {{base}}"

[post-start]
setup = "WT_BRANCH='{{ branch }}' ./bin/worktree-setup"
assets = "echo {{worktree_path}} {{ repo_path }} {{ branch | sanitize }}"

[pre-remove]
archive = "WT_BRANCH='{{ branch }}' ./bin/worktree-archive"

[list]
url = "http://localhost:{{ branch | hash_port }}"
`

func TestParseWTHooks(t *testing.T) {
	h, err := ParseWTHooks([]byte(talkableWT))
	if err != nil {
		t.Fatal(err)
	}
	want := WTHooks{
		PostCreate: []Hook{{Type: HookPostCreate, Name: "deps", Command: "echo create {{ repo }} {{base}}"}},
		PostStart: []Hook{
			{Type: HookPostStart, Name: "setup", Command: "WT_BRANCH='{{ branch }}' ./bin/worktree-setup"},
			{Type: HookPostStart, Name: "assets", Command: "echo {{worktree_path}} {{ repo_path }} {{ branch | sanitize }}"},
		},
		PreRemove: []Hook{{Type: HookPreRemove, Name: "archive", Command: "WT_BRANCH='{{ branch }}' ./bin/worktree-archive"}},
	}
	if !slices.Equal(h.PostCreate, want.PostCreate) || !slices.Equal(h.PostStart, want.PostStart) || !slices.Equal(h.PreRemove, want.PreRemove) {
		t.Fatalf("hooks = %+v\nwant %+v", h, want)
	}

	// A bare string is a one-command hook named after its type.
	h, err = ParseWTHooks([]byte("post-start = \"make dev\"\n"))
	if err != nil || len(h.PostStart) != 1 || h.PostStart[0] != (Hook{Type: HookPostStart, Name: HookPostStart, Command: "make dev"}) {
		t.Fatalf("bare = %+v, %v", h, err)
	}
	for _, bad := range []string{"[post-start]\nsetup = 1\n", "pre-remove = [\"a\"]\n", "[post-start\n"} {
		if _, err := ParseWTHooks([]byte(bad)); !errors.Is(err, ErrHook) {
			t.Errorf("ParseWTHooks(%q) err = %v, want ErrHook", bad, err)
		}
	}
	if h, err := ParseWTHooks(nil); err != nil || len(h.PostStart)+len(h.PreRemove)+len(h.PostCreate) != 0 {
		t.Fatalf("empty = %+v, %v", h, err)
	}
}

func TestExpandTemplate(t *testing.T) {
	vars := templateVars("magnum-pr-7", "zhuravel/widget", "/p/widget__worktrees/pr-7", "/p/widget", "main")
	for in, want := range map[string]string{
		"WT_BRANCH='{{ branch }}' ./bin/worktree-setup": "WT_BRANCH='magnum-pr-7' ./bin/worktree-setup",
		"{{branch}}|{{  branch  }}|{{- branch -}}":      "magnum-pr-7|magnum-pr-7|magnum-pr-7",
		"{{ repo }} {{ base }} {{ default_branch }}":    "widget main main",
		"{{ worktree_path }} {{ path }} {{ worktree }}": "/p/widget__worktrees/pr-7 /p/widget__worktrees/pr-7 /p/widget__worktrees/pr-7",
		"{{ repo_path }} {{ repo_root }}":               "/p/widget /p/widget",
		"{{ worktree_name }}":                           "pr-7",
		"{{ branch | sanitize }}":                       "magnum-pr-7",
		"plain $HOME ${X}":                              "plain $HOME ${X}",
	} {
		got, err := expandTemplate(in, vars)
		if err != nil || got != want {
			t.Errorf("expandTemplate(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"{{ commit }}", "{{ branch | hash_port }}", "{% if x %}y{% endif %}", "{{ branch }", "{{ branch }} {{"} {
		if _, err := expandTemplate(bad, vars); !errors.Is(err, ErrHook) {
			t.Errorf("expandTemplate(%q) err = %v, want ErrHook", bad, err)
		}
	}
}

// TestExpandTemplateShellEscapes: like worktrunk, every substituted value
// is one shell word (POSIX single quotes when it has characters the shell
// would interpret), so a PR's base ref or a path cannot inject commands.
func TestExpandTemplateShellEscapes(t *testing.T) {
	vars := templateVars("magnum-pr-7", "zhuravel/widget", "/p/my worktrees/pr-7", "/p/widget", "it's $(echo pwned)")
	for in, want := range map[string]string{
		"git diff {{ base }}":          `git diff 'it'\''s $(echo pwned)'`,
		"cd {{ worktree_path }}":       `cd '/p/my worktrees/pr-7'`,
		"{{ base | sanitize }}":        `'it'\''s $(echo pwned)'`,
		"{{ repo_path }} {{ branch }}": "/p/widget magnum-pr-7",
		"x={{ worktree_name }}":        "x=pr-7",
	} {
		if got, err := expandTemplate(in, vars); err != nil || got != want {
			t.Errorf("expandTemplate(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for v, want := range map[string]string{
		"":                      "''",
		"release/2.0":           "release/2.0",
		"a-b_c.d:e@f%g+h=i,j/k": "a-b_c.d:e@f%g+h=i,j/k",
		"a b":                   "'a b'",
		"~x":                    "'~x'",
		"'":                     `''\'''`,
		"é":                     "'é'",
	} {
		if got := shellQuote(v); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", v, got, want)
		}
	}
	// /bin/sh reads each substituted value back as exactly one word.
	for _, base := range []string{"it's", "$(echo pwned)", "`echo pwned`", "a; echo pwned", `a\nb\tc`, `"q" 'r' \`, "*", "", "--x", "x\ny", "a/b c"} {
		vars := templateVars("magnum-pr-7", "zhuravel/widget", "/p/w", "/p/c", base)
		script, err := expandTemplate(`printf '<%s>' {{ base }}`, vars)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Dir = t.TempDir()
		out, err := cmd.Output()
		if err != nil || string(out) != "<"+base+">" {
			t.Errorf("sh -c %q = %q, %v; want %q", script, out, err, "<"+base+">")
		}
	}
}

func TestSlugsAndPerPREnv(t *testing.T) {
	if PRSlug(7) != "magnum-pr-7" || DBSlug("magnum-pr-7") != "magnum_pr_7" {
		t.Fatalf("slugs: %s %s", PRSlug(7), DBSlug("magnum-pr-7"))
	}
	if got := DBSlug("feature/ABC-123.very-long-branch-name-here"); got != "feature_ABC_123_very_long_bran" {
		t.Fatalf("DBSlug long = %q", got)
	}
	env := PerPREnv(nil, 7, "/p/w", "/p/c")
	want := map[string]string{"WT_BRANCH": "magnum-pr-7", "CONDUCTOR_WORKSPACE_NAME": "", "EMDASH_TASK_NAME": "",
		"SUPERSET_WORKSPACE_NAME": "", "COMMANDER_CONTEXT_NAME": "", "CLAUDE_CODE_WORKTREE_NAME": "", "WM_HANDLE": ""}
	if !maps.Equal(env, want) {
		t.Fatalf("env = %v", env)
	}
	rc := &config.Repo{Repo: "zhuravel/widget", Env: map[string]string{
		"DATABASE_NAME": "widget_{slug}", "APP_ROOT": "{path}", "MAIN": "{clone}", "WM_HANDLE": "mine"}}
	env = PerPREnv(rc, 7, "/p/w", "/p/c")
	if env["DATABASE_NAME"] != "widget_magnum-pr-7" || env["APP_ROOT"] != "/p/w" || env["MAIN"] != "/p/c" ||
		env["WM_HANDLE"] != "mine" || env["WT_BRANCH"] != "magnum-pr-7" || env["CONDUCTOR_WORKSPACE_NAME"] != "" {
		t.Fatalf("repo env = %v", env)
	}
}

// hookRecorder wraps the harness's mise fake for one per-PR worktree: it
// records the slot state and whether the directory existed at each exec,
// and plays bin/worktree-setup (marker file) for the setup hook.
type hookRecorder struct {
	mu     sync.Mutex
	execs  []miseCall
	states []string
	dirs   []bool
}

func (f *perPRFixture) recordHooks(t *testing.T) *hookRecorder {
	t.Helper()
	h := f.h
	rec := &hookRecorder{}
	h.fake.Rules = append([]execx.Rule{{Prefix: []string{"mise"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		if mc, ok := parseMiseExec(c); ok {
			state := ""
			if sl, err := h.st.SlotByName(context.Background(), PRSlotName("zhuravel/widget", 7)); err == nil {
				state = sl.State
			}
			rec.mu.Lock()
			rec.execs = append(rec.execs, mc)
			rec.states = append(rec.states, state)
			rec.dirs = append(rec.dirs, exists(mc.Dir))
			rec.mu.Unlock()
			if strings.HasSuffix(mc.Script, "./bin/worktree-setup") && h.failScript[mc.Script] == 0 {
				writeFile(t, filepath.Join(mc.Dir, MarkerFile), DBSlug(mc.Env["WT_BRANCH"])+"\n")
			}
		}
		return h.mise(c)
	}}}, h.fake.Rules...)
	return rec
}

// callIndex is the position of the first recorded call matching pred (-1 when none).
func callIndex(r *recRunner, pred func(c execx.Cmd) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, c := range r.calls {
		if pred(c) {
			return i
		}
	}
	return -1
}

func isGit(sub ...string) func(c execx.Cmd) bool {
	return func(c execx.Cmd) bool {
		if c.Name != "git" {
			return false
		}
		a := c.Args
		if len(a) >= 2 && a[0] == "-C" {
			a = a[2:]
		}
		return len(a) >= len(sub) && slices.Equal(a[:len(sub)], sub)
	}
}

func isMiseScript(script string) func(c execx.Cmd) bool {
	return func(c execx.Cmd) bool {
		mc, ok := parseMiseExec(c)
		return ok && mc.Script == script
	}
}

// perPREnvArgs is the `env …` part every per-PR hook gets (keys sorted).
func perPREnvArgs(slug string) []string {
	return []string{"CLAUDE_CODE_WORKTREE_NAME=", "COMMANDER_CONTEXT_NAME=", "CONDUCTOR_WORKSPACE_NAME=",
		"EMDASH_TASK_NAME=", "SUPERSET_WORKSPACE_NAME=", "WM_HANDLE=", "WT_BRANCH=" + slug}
}

func miseArgv(dir string, envArgs []string, script string) []string {
	out := append([]string{"-C", dir, "exec", "--", "env"}, envArgs...)
	return append(out, "/bin/sh", "-c", script)
}

func eventKinds(t *testing.T, st *store.Store, subject, kind string) []store.Event {
	t.Helper()
	evs, err := st.EventsBySubject(context.Background(), subject, 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Event
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestPerPRWorktreeRunsWorktrunkHooks(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	writeFile(t, filepath.Join(f.main, WTConfigFile), talkableWT)
	writeFile(t, filepath.Join(f.main, MiseLocal),
		"[env]\nFAKE_AWS = \"1\"\nGITHUB_PERSONAL_ACCESS_TOKEN = \"ghp_abcdefghijklmnopqrstuvwxyz0123456789\"\nGH_TOKEN = \"gho_x\"\nWT_BRANCH = \"main\"\n")
	rec := f.recordHooks(t)
	path := filepath.Join(f.cloneRt, "widget__worktrees", "pr-7")

	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7)
	if err != nil {
		t.Fatalf("CreatePRWorktree: %v", err)
	}
	if sl.State != store.SlotClaimed || store.Deref(sl.DBSlug) != "magnum_pr_7" {
		t.Fatalf("slot = %+v (db_slug %q)", sl, store.Deref(sl.DBSlug))
	}
	if a, err := h.st.OpenAssignmentByPR(h.ctx, f.pr.ID); err != nil || store.Deref(a.DBSlug) != "magnum_pr_7" {
		t.Fatalf("assignment = %+v, %v", a, err)
	}

	// post-create, then post-start in file order, templates filled in, the
	// per-PR env exported, in the worktree, while the slot is still provisioning.
	env := perPREnvArgs("magnum-pr-7")
	wantScripts := []string{
		"echo create widget master",
		"WT_BRANCH='magnum-pr-7' ./bin/worktree-setup",
		"echo " + path + " " + f.main + " magnum-pr-7",
	}
	if len(rec.execs) != len(wantScripts) {
		t.Fatalf("execs = %+v", rec.execs)
	}
	for i, s := range wantScripts {
		c := rec.execs[i].Cmd
		if c.Name != "mise" || !slices.Equal(c.Args, miseArgv(path, env, s)) || c.Dir != path || !c.Mutates || c.Timeout != SetupTimeout {
			t.Fatalf("exec %d = %s %v (dir %s, timeout %s, mutates %v)", i, c.Name, c.Args, c.Dir, c.Timeout, c.Mutates)
		}
		if rec.states[i] != store.SlotProvisioning {
			t.Fatalf("hook %d ran with the slot %s, want provisioning", i, rec.states[i])
		}
	}
	add := callIndex(h.run, isGit("worktree", "add"))
	first := callIndex(h.run, isMiseScript(wantScripts[0]))
	if add < 0 || first < add {
		t.Fatalf("hooks ran before worktree add (add %d, first hook %d)", add, first)
	}

	// .mise.local.toml rendered with the slug, tokens stripped, and trusted.
	local := readFile(t, filepath.Join(path, MiseLocal))
	for _, want := range []string{`WT_BRANCH = "magnum-pr-7"`, `CONDUCTOR_WORKSPACE_NAME = ""`, `FAKE_AWS = "1"`} {
		if !strings.Contains(local, want) {
			t.Fatalf("rendered %s lacks %s:\n%s", MiseLocal, want, local)
		}
	}
	for _, gone := range []string{"GITHUB_PERSONAL_ACCESS_TOKEN", "GH_TOKEN", `"main"`} {
		if strings.Contains(local, gone) {
			t.Fatalf("rendered %s still has %s:\n%s", MiseLocal, gone, local)
		}
	}
	if fi, err := os.Stat(filepath.Join(path, MiseLocal)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", fi, err)
	}
	if trust := h.fake.CallsWithPrefix("mise", "-C", path, "trust"); len(trust) != 1 {
		t.Fatalf("mise trust calls = %+v", trust)
	}
	if evs := eventKinds(t, h.st, "slot:zhuravel/widget#7", "slot.hook_ran"); len(evs) != 3 ||
		!strings.Contains(evs[0].Message+evs[1].Message+evs[2].Message, "post-start hook \"setup\" (.config/wt.toml) ran as magnum-pr-7") {
		t.Fatalf("hook events = %+v", evs)
	}
	if b, err := os.ReadFile(filepath.Join(h.layout.Logs(), "perpr-magnum-pr-7.log")); err != nil || !strings.Contains(string(b), "worktree-setup") {
		t.Fatalf("log: %v\n%s", err, b)
	}

	// Remove: pre-remove under the same slug, in the still-present worktree,
	// before git removes it.
	rec.execs, rec.states, rec.dirs = nil, nil, nil
	if err := h.m.RemovePRWorktree(h.ctx, h.slot(sl.Name), false); err != nil {
		t.Fatalf("RemovePRWorktree: %v", err)
	}
	archive := "WT_BRANCH='magnum-pr-7' ./bin/worktree-archive"
	if len(rec.execs) != 1 || !slices.Equal(rec.execs[0].Cmd.Args, miseArgv(path, env, archive)) ||
		rec.execs[0].Cmd.Timeout != TeardownTimeout || !rec.dirs[0] || rec.states[0] != store.SlotRemoving {
		t.Fatalf("teardown execs = %+v dirs %v states %v", rec.execs, rec.dirs, rec.states)
	}
	if pre, rm := callIndex(h.run, isMiseScript(archive)), callIndex(h.run, isGit("worktree", "remove")); pre < 0 || rm < pre {
		t.Fatalf("pre-remove at %d, worktree remove at %d", pre, rm)
	}
	if got := h.slot(sl.Name); got.State != store.SlotRemoved || exists(path) {
		t.Fatalf("slot = %s, dir exists %v", got.State, exists(path))
	}
	// A second removal is a no-op.
	if err := h.m.RemovePRWorktree(h.ctx, h.slot(sl.Name), false); err != nil || len(rec.execs) != 1 {
		t.Fatalf("second removal: %v (%d execs)", err, len(rec.execs))
	}
}

func TestPerPRSetupFailureMarksSlotBroken(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	writeFile(t, filepath.Join(f.main, WTConfigFile), talkableWT)
	rec := f.recordHooks(t)
	setup := "WT_BRANCH='magnum-pr-7' ./bin/worktree-setup"
	h.failScript[setup] = 1

	_, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7)
	if !errors.Is(err, ErrBroken) {
		t.Fatalf("err = %v, want ErrBroken", err)
	}
	sl := h.slot(PRSlotName("zhuravel/widget", 7))
	if sl.State != store.SlotBroken || !strings.Contains(store.Deref(sl.LastError), "worktree-setup") || sl.PRID != nil {
		t.Fatalf("slot = %+v (last_error %q)", sl, store.Deref(sl.LastError))
	}
	if strings.Contains(store.Deref(sl.LastError), "ghp_") {
		t.Fatalf("last_error not redacted: %s", store.Deref(sl.LastError))
	}
	if _, err := h.st.OpenAssignmentByPR(h.ctx, f.pr.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("assignment opened for a broken slot: %v", err)
	}
	// The first post-start command failed: the next one never ran.
	if n := len(rec.execs); n != 2 {
		t.Fatalf("ran %d hooks, want post-create + the failed setup", n)
	}
	if evs := eventKinds(t, h.st, "slot:"+sl.Name, "slot.hook_failed"); len(evs) != 1 || evs[0].Level != "error" {
		t.Fatalf("failure events = %+v", evs)
	}

	// The next attempt starts over on the same row and succeeds.
	again, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again.ID != sl.ID || again.State != store.SlotClaimed || store.Deref(again.DBSlug) != "magnum_pr_7" || again.LastError != nil {
		t.Fatalf("retried slot = %+v", again)
	}
}

func TestPerPRBadTemplateMarksSlotBroken(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	writeFile(t, filepath.Join(f.main, WTConfigFile), "[post-start]\nrun = \"echo {{ commit }}\"\n")
	rec := f.recordHooks(t)
	_, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7)
	if !errors.Is(err, ErrBroken) || !errors.Is(err, ErrHook) || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("err = %v", err)
	}
	if got := h.slot(PRSlotName("zhuravel/widget", 7)); got.State != store.SlotBroken || len(rec.execs) != 0 {
		t.Fatalf("slot %s, %d hooks ran", got.State, len(rec.execs))
	}
}

func TestPerPRTeardownFailureStillRemoves(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	writeFile(t, filepath.Join(f.main, WTConfigFile),
		"[pre-remove]\narchive = \"WT_BRANCH='{{ branch }}' ./bin/worktree-archive\"\nnotify = \"echo bye\"\n")
	rec := f.recordHooks(t)
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	if sl.DBSlug != nil { // no setup hooks: no databases to report
		t.Fatalf("db_slug = %q without setup hooks", *sl.DBSlug)
	}
	archive := "WT_BRANCH='magnum-pr-7' ./bin/worktree-archive"
	h.failScript[archive] = 1
	if err := h.m.Release(h.ctx, sl, config.Pool{}, "pr_closed"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := h.slot(sl.Name); got.State != store.SlotRemoved || exists(sl.Path) {
		t.Fatalf("slot = %s, dir exists %v", got.State, exists(sl.Path))
	}
	// The failed hook is logged and the remaining pre-remove hooks still ran.
	if len(rec.execs) != 2 || rec.execs[1].Script != "echo bye" {
		t.Fatalf("teardown execs = %+v", rec.execs)
	}
	if evs := eventKinds(t, h.st, "slot:"+sl.Name, "slot.hook_failed"); len(evs) != 1 || evs[0].Level != "warn" {
		t.Fatalf("failure events = %+v", evs)
	}
}

func TestPerPRRemovalIsIdempotent(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	writeFile(t, filepath.Join(f.main, WTConfigFile), talkableWT)
	rec := f.recordHooks(t)
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	archive := "WT_BRANCH='magnum-pr-7' ./bin/worktree-archive"

	// A crash right after the teardown ran: the retry does not run it again
	// and finishes the removal.
	crash := errors.New("crash")
	ctx := steps.WithFailpoint(h.ctx, func(subject, name string, at steps.Point) error {
		if name == "worktree_remove" && at == steps.BeforeRun {
			return crash
		}
		return nil
	})
	if err := h.m.RemovePRWorktree(ctx, h.slot(sl.Name), false); !errors.Is(err, crash) {
		t.Fatalf("err = %v, want the crash", err)
	}
	if got := h.slot(sl.Name); got.State != store.SlotRemoving {
		t.Fatalf("state after crash = %s", got.State)
	}
	if err := h.m.RemovePRWorktree(h.ctx, h.slot(sl.Name), false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := len(h.scriptCalls(archive)); n != 1 {
		t.Fatalf("pre-remove ran %d times", n)
	}

	// A worktree whose directory is already gone: teardown is skipped.
	pr := f.h.pr(f.repo.ID, 7, f.sha7, store.PRClaiming)
	sl, err = h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(sl.Path); err != nil {
		t.Fatal(err)
	}
	before := len(rec.execs)
	if err := h.m.RemovePRWorktree(h.ctx, h.slot(sl.Name), false); err != nil {
		t.Fatalf("remove with the directory gone: %v", err)
	}
	if len(rec.execs) != before || h.slot(sl.Name).State != store.SlotRemoved {
		t.Fatalf("teardown ran without a directory (%d → %d) or slot %s", before, len(rec.execs), h.slot(sl.Name).State)
	}
}

func TestPerPRRepoConfigOverridesWorktrunk(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	writeFile(t, filepath.Join(f.main, WTConfigFile), talkableWT)
	writeFile(t, filepath.Join(f.main, MiseLocal), "[env]\nSECRET = \"s\"\nKEEP = \"k\"\n")
	writeFile(t, filepath.Join(f.main, "config", "initializers", "local.rb"), "LOCAL = 1\n")
	rc := config.Repo{
		Repo: "zhuravel/widget", Setup: []string{"bin/setup --db"}, Teardown: []string{"bin/drop-db"},
		CopyFiles: []string{"config/initializers/local.rb", "config/missing.yml", MiseLocal},
		StripEnv:  []string{"SECRET"},
		Env:       map[string]string{"DATABASE_NAME": "widget_{slug}", "MAIN": "{clone}"},
	}
	h.m = h.newManager(Deps{Repos: []config.Repo{rc}})
	rec := f.recordHooks(t)
	path := filepath.Join(f.cloneRt, "widget__worktrees", "pr-7")
	// The repository ignores the file it copies in (an untracked copy would
	// hold the removal: magnum cannot tell its own copies from new files).
	writeFile(t, filepath.Join(f.main, ".git", "info", "exclude"), "/config/initializers/local.rb\n")

	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7)
	if err != nil {
		t.Fatalf("CreatePRWorktree: %v", err)
	}
	env := append(perPREnvArgs("magnum-pr-7"), "DATABASE_NAME=widget_magnum-pr-7", "MAIN="+f.main)
	slices.Sort(env)
	if len(rec.execs) != 1 || !slices.Equal(rec.execs[0].Cmd.Args, miseArgv(path, env, "bin/setup --db")) {
		t.Fatalf("setup execs = %+v (want only the [[repo]] command)", rec.execs)
	}
	if store.Deref(sl.DBSlug) != "magnum_pr_7" { // no marker: the sanitized slug
		t.Fatalf("db_slug = %q", store.Deref(sl.DBSlug))
	}
	if got := readFile(t, filepath.Join(path, "config", "initializers", "local.rb")); got != "LOCAL = 1\n" {
		t.Fatalf("copied file = %q", got)
	}
	if exists(filepath.Join(path, "config", "missing.yml")) {
		t.Fatal("a missing copy_files source must be skipped")
	}
	local := readFile(t, filepath.Join(path, MiseLocal))
	if strings.Contains(local, "SECRET") || !strings.Contains(local, `KEEP = "k"`) || !strings.Contains(local, `DATABASE_NAME = "widget_magnum-pr-7"`) {
		t.Fatalf("rendered:\n%s", local)
	}
	if evs := eventKinds(t, h.st, "slot:"+sl.Name, "slot.hook_ran"); len(evs) != 1 || !strings.Contains(evs[0].Message, "[[repo]] zhuravel/widget") {
		t.Fatalf("events = %+v", evs)
	}

	rec.execs = nil
	if err := h.m.RemovePRWorktree(h.ctx, h.slot(sl.Name), false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(rec.execs) != 1 || rec.execs[0].Script != "bin/drop-db" || rec.execs[0].Env["WT_BRANCH"] != "magnum-pr-7" {
		t.Fatalf("teardown execs = %+v", rec.execs)
	}
}

func TestPerPRRepoConfigCanDisableWorktrunk(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	writeFile(t, filepath.Join(f.main, WTConfigFile), talkableWT)
	off := false
	h.m = h.newManager(Deps{Repos: []config.Repo{{Repo: "ZHURAVEL/Widget", WTHooks: &off}}})
	rec := f.recordHooks(t)
	sl, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.execs) != 0 || sl.DBSlug != nil {
		t.Fatalf("hooks ran (%+v) or db_slug set (%v)", rec.execs, sl.DBSlug)
	}
	if err := h.m.RemovePRWorktree(h.ctx, h.slot(sl.Name), false); err != nil || len(rec.execs) != 0 {
		t.Fatalf("remove: %v, execs %+v", err, rec.execs)
	}
}

func TestPerPRHooksWithoutMise(t *testing.T) {
	f := newPerPR(t, true)
	h := f.h
	writeFile(t, filepath.Join(f.main, WTConfigFile), "[post-start]\nsetup = \"./bin/setup {{ branch }}\"\n")
	writeFile(t, filepath.Join(f.main, MiseLocal), "[env]\nA = \"1\"\n")
	h.m = h.newManager(Deps{LookPath: func(string) (string, error) { return "", errors.New("not found") }})
	var shells []execx.Cmd
	h.fake.Rules = append([]execx.Rule{{Prefix: []string{"/bin/sh"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		shells = append(shells, c)
		return execx.Result{}, nil
	}}}, h.fake.Rules...)
	path := filepath.Join(f.cloneRt, "widget__worktrees", "pr-7")
	if _, err := h.m.CreatePRWorktree(h.ctx, f.watch, "zhuravel/widget", f.pr, f.sha7); err != nil {
		t.Fatal(err)
	}
	if len(shells) != 1 || !slices.Equal(shells[0].Args, []string{"-c", "./bin/setup magnum-pr-7"}) || shells[0].Dir != path ||
		!maps.Equal(shells[0].Env, PerPREnv(nil, 7, path, f.main)) || shells[0].Timeout != SetupTimeout || !shells[0].Mutates {
		t.Fatalf("shell calls = %+v", shells)
	}
	if n := len(h.fake.CallsWithPrefix("mise")); n != 0 {
		t.Fatalf("ran mise %d times without mise", n)
	}
	if !strings.Contains(readFile(t, filepath.Join(path, MiseLocal)), `WT_BRANCH = "magnum-pr-7"`) {
		t.Fatal(MiseLocal + " not rendered")
	}
}
