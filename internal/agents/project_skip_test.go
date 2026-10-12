package agents

import (
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// markCheckout makes dir look like a git work tree to the code that decides
// whether to run git at all: a slot's root has a .git (a file, in a
// worktree).
func markCheckout(t *testing.T, dir string) {
	t.Helper()
	writeFiles(t, dir, map[string]string{".git": "gitdir: /elsewhere/.git/worktrees/review1\n"})
}

// The retro and the notes curation run an agent in magnum's own directory,
// which is no git repository: there is nothing to compare with a merge
// base, so no git runs and nothing is logged above debug (it was two WARN
// lines on every daily retro). The session still leaves the directory's
// project config out, as it does for anything it cannot compare.
func TestASessionWithoutAPRBaseSkipsTheProjectCompareQuietly(t *testing.T) {
	t.Run("a directory that is no git repository", func(t *testing.T) {
		e, dir, ws := projectEnvOutsideGit(t, map[string]string{".codex/config.toml": "x = 1\n"})
		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
			t.Fatal(err)
		}
		if got, want := e.h.starts[0].Args, slices.Concat(judgeLaunchArgs, untrustArgs(dir)); !slices.Equal(got, want) {
			t.Fatalf("args = %q\nwant %q (the config stays out)", got, want)
		}
		for _, sub := range [][]string{{"merge-base"}, {"diff"}, {"ls-files"}, {"rev-parse", "--verify", "--quiet", "HEAD^{commit}"}} {
			if calls := e.run.CallsWithPrefix(slices.Concat([]string{"git", "-C", dir}, sub)...); len(calls) != 0 {
				t.Fatalf("git %s ran for a directory that is no repository: %v", sub[0], calls)
			}
		}
		if recs := e.logs.records("cannot compare"); len(recs) != 0 {
			t.Fatalf("logged %+v, want nothing above debug", recs)
		}
		recs := e.logs.records("no git checkout")
		if len(recs) != 1 || recs[0].level != slog.LevelDebug {
			t.Fatalf("records = %+v, want one debug line saying why", recs)
		}
	})

	t.Run("a checkout without a base to compare with", func(t *testing.T) {
		e, dir, _ := projectEnv(t, map[string]string{".codex/config.toml": "x = 1\n"}, nil, nil)
		got := e.m.checkoutProject(e.ctx, e.spec(RoleJudge), dir, "", "")
		if !got.declined || got.compared || got.files != 0 {
			t.Fatalf("scope = %+v, want declined and not compared", got)
		}
		if calls := e.run.CallsWithPrefix("git", "-C", dir, "merge-base"); len(calls) != 0 {
			t.Fatalf("merge-base ran without a base: %v", calls)
		}
		if recs := e.logs.records("cannot compare"); len(recs) != 0 {
			t.Fatalf("logged %+v, want nothing above debug", recs)
		}
		if recs := e.logs.records("no base to compare with"); len(recs) != 1 || recs[0].level != slog.LevelDebug {
			t.Fatalf("records = %+v, want one debug line saying why", recs)
		}
	})

	t.Run("a PR checkout where git fails keeps the warning", func(t *testing.T) {
		e, dir, ws := projectEnv(t, map[string]string{".codex/config.toml": "x = 1\n"}, nil, nil)
		e.run.Rules = slices.Insert(e.run.Rules, 0, execx.Rule{Prefix: []string{"git", "-C", dir, "merge-base"}, Result: execx.Result{Code: 1}})
		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(RoleJudge), ws.Panes[RoleJudge], ""); err != nil {
			t.Fatal(err)
		}
		if got, want := e.h.starts[0].Args, slices.Concat(judgeLaunchArgs, untrustArgs(dir)); !slices.Equal(got, want) {
			t.Fatalf("args = %q\nwant %q", got, want)
		}
		recs := e.logs.records("cannot compare .codex/")
		if len(recs) != 1 || recs[0].level != slog.LevelWarn || !strings.Contains(recs[0].line, "merge base") {
			t.Fatalf("records = %+v, want one warning", recs)
		}
	})
}
