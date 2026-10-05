package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

// readinessCheckout is a checkout directory holding files (name -> content).
func readinessCheckout(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// zshRule answers `zsh -lc <script>`.
func zshRule(script string, res execx.Result, err error) execx.Rule {
	return execx.Rule{Prefix: []string{ReadinessShell, "-lc", script}, Result: res, Err: err}
}

func readReadinessFile(t *testing.T, dir string) readinessFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ReadinessFile))
	if err != nil {
		t.Fatalf("readiness file: %v", err)
	}
	var f readinessFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("readiness file %s: %v", b, err)
	}
	return f
}

// A failed prepare does not stop the round: the reviewers run after every
// readiness command, the review posts, and the judge learns what failed
// (the file next to its result file holds the commands' last lines; the
// events never carry the PR's output).
func TestReadinessRunsBeforeTheReviewersAndFailuresDoNotStopTheRound(t *testing.T) {
	e := newEnv(t)
	checkout := readinessCheckout(t, map[string]string{".ruby-version": "ruby-3.3.4\n"})
	e.exec.Rules = []execx.Rule{
		zshRule("bin/rails db:test:prepare", execx.Result{Code: 1, Duration: 4200 * time.Millisecond,
			Stderr: []byte("rails aborted!\nActiveRecord::NoDatabaseError: Unknown database 'talkable_test__review1'\n")}, nil),
		zshRule("bin/rails runner 'exit 0'", execx.Result{Stdout: []byte("ok\n")}, nil),
		zshRule(RubyCheckCommand, execx.Result{Stdout: []byte("ruby 3.2.2 (2023-03-30 revision e51014f9c0) [arm64-darwin23]\n")}, nil),
	}
	ready := func() int { return len(e.exec.CallsWithPrefix(ReadinessShell)) }
	e.ag.behaviors[agents.RoleClaude] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		if n := ready(); n != 3 {
			t.Errorf("claude-review was prompted after %d readiness commands, want all 3 first", n)
		}
		return writeReport("## P2 something\n")(f, run, text)
	}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(501, "COMMENTED", "COMMENT").behavior(t)}

	in := e.input(KindInitial)
	in.SlotPath = checkout
	in.Readiness = ReadinessPlan{Prepare: []string{"bin/rails db:test:prepare"}, Ready: []string{"bin/rails runner 'exit 0'"},
		Timeout: 3 * time.Minute, Env: map[string]string{"WT_BRANCH": "review1"}}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}

	calls := e.exec.CallsWithPrefix(ReadinessShell)
	var scripts []string
	for _, c := range calls {
		scripts = append(scripts, c.Args[1])
		if c.Dir != checkout || c.Env["WT_BRANCH"] != "review1" || c.Timeout <= 0 || c.Timeout > 3*time.Minute {
			t.Errorf("%s: dir %q env %v timeout %s", c.Args[1], c.Dir, c.Env, c.Timeout)
		}
		if prepare := c.Args[1] == "bin/rails db:test:prepare"; c.Mutates != prepare || c.Probe == prepare {
			t.Errorf("%s: mutates %v probe %v", c.Args[1], c.Mutates, c.Probe)
		}
	}
	if want := []string{"bin/rails db:test:prepare", "bin/rails runner 'exit 0'", RubyCheckCommand}; !slices.Equal(scripts, want) {
		t.Fatalf("readiness commands = %q, want %q", scripts, want)
	}

	f := readReadinessFile(t, e.reportDir())
	if f.HeadSHA != target || f.Timeout != "3m0s" || len(f.Checks) != 3 {
		t.Fatalf("readiness file = %+v", f)
	}
	prep, probe, ruby := f.Checks[0], f.Checks[1], f.Checks[2]
	if prep.Kind != agents.ReadinessPrepare || prep.OK || prep.Status != agents.ReadinessFailed || prep.Detail != "exit 1" ||
		prep.Duration != "4.2s" || !strings.Contains(prep.LastLine, "Unknown database 'talkable_test__review1'") {
		t.Errorf("prepare check = %+v", prep)
	}
	if probe.Kind != agents.ReadinessReady || !probe.OK || probe.Status != agents.ReadinessOK || probe.LastLine != "ok" {
		t.Errorf("ready check = %+v", probe)
	}
	if ruby.Kind != agents.ReadinessRuby || ruby.OK ||
		ruby.Detail != "the checkout pins Ruby 3.3.4 (.ruby-version) but `zsh -lc 'ruby -v'` in it runs 3.2.2" {
		t.Errorf("ruby check = %+v", ruby)
	}

	var ev *store.Event
	for _, x := range e.events() {
		if x.Kind == "round.readiness" {
			ev = &x
		}
	}
	if ev == nil {
		t.Fatal("no round.readiness event")
	}
	if ev.Level != "warn" || !strings.Contains(ev.Message, "1 of 3 checks ok") ||
		!strings.Contains(ev.Message, "prepare `bin/rails db:test:prepare`: exit 1") || strings.Contains(ev.Message+string(ev.Data), "Unknown database") {
		t.Errorf("round.readiness event = %s %q %s", ev.Level, ev.Message, ev.Data)
	}
}

// A checkout that changes the pool's schema (ReadinessPlan.ResetDB) loads
// it into the slot's databases before anything else runs, through the same
// path as prepare (the login shell, the slot's env, the step's budget, a
// command that may change things); a reset that fails does not stop the
// round, it reaches the judge's readiness list.
func TestReadinessResetsTheSchemaFirstAndAFailureReachesTheJudge(t *testing.T) {
	e := newEnv(t)
	checkout := t.TempDir()
	e.exec.Rules = []execx.Rule{
		zshRule("bin/rails db:schema:load", execx.Result{Code: 1, Duration: 2 * time.Second,
			Stderr: []byte("ActiveRecord::StatementInvalid: Table 'talkable_test__review1.offers' doesn't exist\n")}, nil),
		zshRule("RAILS_ENV=test bin/rails db:schema:load", execx.Result{}, nil),
		zshRule("bin/rails db:test:prepare", execx.Result{}, nil),
	}
	ready := func() int { return len(e.exec.CallsWithPrefix(ReadinessShell)) }
	e.ag.behaviors[agents.RoleClaude] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		if n := ready(); n != 3 {
			t.Errorf("claude-review was prompted after %d readiness commands, want the schema reset and prepare first", n)
		}
		return writeReport("## P2 something\n")(f, run, text)
	}}
	judge := e.judgePosts(501, "COMMENTED", "COMMENT").behavior(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
		if !strings.Contains(text, "  - reset_db `bin/rails db:schema:load`: failed in 2s (exit 1)\n") ||
			!strings.Contains(text, "  - reset_db `RAILS_ENV=test bin/rails db:schema:load`: ok") {
			t.Errorf("the judge's prompt does not list the schema reset:\n%s", text)
		}
		if strings.Contains(text, "offers' doesn't exist") {
			t.Errorf("the judge's prompt carries a command's output:\n%s", text)
		}
		return judge(f, run, text)
	}}

	in := e.input(KindInitial)
	in.SlotPath = checkout
	in.Readiness = ReadinessPlan{ResetDB: []string{"bin/rails db:schema:load", "RAILS_ENV=test bin/rails db:schema:load"},
		Prepare: []string{"bin/rails db:test:prepare"}, Timeout: 3 * time.Minute, Env: map[string]string{"WT_BRANCH": "review1"}}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}

	var scripts []string
	for _, c := range e.exec.CallsWithPrefix(ReadinessShell) {
		scripts = append(scripts, c.Args[1])
		// The reset has the release's reset_db budget (slots.ResetDBTimeout,
		// the plan sets none), prepare the whole ready_timeout after it.
		limit := 3 * time.Minute
		if strings.HasSuffix(c.Args[1], "db:schema:load") {
			limit = slots.ResetDBTimeout
		}
		if c.Dir != checkout || c.Env["WT_BRANCH"] != "review1" || c.Timeout != limit || !c.Mutates || c.Probe {
			t.Errorf("%s: dir %q env %v timeout %s (want %s) mutates %v probe %v", c.Args[1], c.Dir, c.Env, c.Timeout, limit, c.Mutates, c.Probe)
		}
	}
	if want := []string{"bin/rails db:schema:load", "RAILS_ENV=test bin/rails db:schema:load", "bin/rails db:test:prepare"}; !slices.Equal(scripts, want) {
		t.Fatalf("readiness commands = %q, want %q", scripts, want)
	}
	f := readReadinessFile(t, e.reportDir())
	if len(f.Checks) != 3 {
		t.Fatalf("readiness file = %+v", f)
	}
	if c := f.Checks[0]; c.Kind != agents.ReadinessResetDB || c.OK || c.Status != agents.ReadinessFailed || c.Detail != "exit 1" ||
		!strings.Contains(c.LastLine, "doesn't exist") {
		t.Errorf("reset_db check = %+v", c)
	}
	if c := f.Checks[1]; c.Kind != agents.ReadinessResetDB || !c.OK {
		t.Errorf("second reset_db check = %+v", c)
	}
	var ev *store.Event
	for _, x := range e.events() {
		if x.Kind == "round.readiness" {
			ev = &x
		}
	}
	if ev == nil || ev.Level != "warn" || !strings.Contains(ev.Message, "reset_db `bin/rails db:schema:load`: exit 1") {
		t.Errorf("round.readiness event = %+v", ev)
	}
}

// The schema reset has a budget of its own (ReadinessPlan.ResetDBTimeout,
// the release's reset_db timeout), not counted against ready_timeout: a
// slow reset leaves prepare and ready their whole ready_timeout, and a reset
// that outlives its own budget is stopped and the reset_db commands after it
// skipped, while prepare and ready still run.
func TestReadinessSchemaResetHasItsOwnBudget(t *testing.T) {
	for _, tc := range []struct {
		name        string
		resetTakes  time.Duration
		wantResets  []string // statuses of the two reset_db checks
		wantDetails []string
	}{
		{"a slow reset", 20 * time.Minute, []string{agents.ReadinessOK, agents.ReadinessOK}, []string{"", ""}},
		{"a reset past its budget", 40 * time.Minute, []string{agents.ReadinessTimeout, agents.ReadinessSkipped},
			[]string{"stopped when reset_db's budget (30m0s) ran out", "reset_db's budget (30m0s) was spent before it ran"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.exec.Rules = []execx.Rule{
				{Prefix: []string{ReadinessShell, "-lc", "bin/rails db:schema:load"}, Fn: func(c execx.Cmd) (execx.Result, error) {
					if c.Timeout != 30*time.Minute {
						t.Errorf("first reset_db timeout = %s, want its own 30m budget", c.Timeout)
					}
					e.clock.Add(tc.resetTakes)
					if tc.resetTakes > c.Timeout {
						return execx.Result{Code: -1, Duration: c.Timeout}, &execx.RunError{Cmd: c, Err: context.DeadlineExceeded}
					}
					return execx.Result{Duration: tc.resetTakes}, nil
				}},
				{Prefix: []string{ReadinessShell, "-lc", "bin/rails db:seed"}, Fn: func(c execx.Cmd) (execx.Result, error) {
					if c.Timeout != 10*time.Minute {
						t.Errorf("second reset_db timeout = %s, want what the reset budget has left (10m)", c.Timeout)
					}
					return execx.Result{}, nil
				}},
				{Prefix: []string{ReadinessShell, "-lc", "bin/rails db:test:prepare"}, Fn: func(c execx.Cmd) (execx.Result, error) {
					if c.Timeout != 5*time.Minute {
						t.Errorf("prepare timeout = %s, want the whole ready_timeout (5m) after the reset", c.Timeout)
					}
					e.clock.Add(time.Minute)
					return execx.Result{}, nil
				}},
				{Prefix: []string{ReadinessShell, "-lc", "bin/ready"}, Fn: func(c execx.Cmd) (execx.Result, error) {
					if c.Timeout != 4*time.Minute {
						t.Errorf("ready timeout = %s, want what ready_timeout has left (4m)", c.Timeout)
					}
					return execx.Result{}, nil
				}},
			}
			rd := readinessRound(t, e, KindInitial, t.TempDir(), ReadinessPlan{
				ResetDB: []string{"bin/rails db:schema:load", "bin/rails db:seed"}, ResetDBTimeout: 30 * time.Minute,
				Prepare: []string{"bin/rails db:test:prepare"}, Ready: []string{"bin/ready"}, Timeout: 5 * time.Minute})
			if err := rd.readiness(e.ctx); err != nil {
				t.Fatal(err)
			}
			got := rd.readinessData()
			if len(got.Checks) != 4 {
				t.Fatalf("readiness = %+v", got)
			}
			for i, want := range tc.wantResets {
				if c := got.Checks[i]; c.Kind != agents.ReadinessResetDB || c.Status != want || c.Detail != tc.wantDetails[i] {
					t.Errorf("reset_db check %d = %+v, want %s %q", i, c, want, tc.wantDetails[i])
				}
			}
			for _, c := range got.Checks[2:] {
				if !c.OK {
					t.Errorf("%s check after the reset = %+v, want ok within its own ready_timeout", c.Kind, c)
				}
			}
			if f := readReadinessFile(t, rd.dir); f.Timeout != "5m0s" || f.ResetDBTimeout != "30m0s" {
				t.Errorf("readiness file budgets = %q and %q, want 5m0s and 30m0s", f.Timeout, f.ResetDBTimeout)
			}
		})
	}
}

// readinessRound is a round ready to run its readiness step on checkout.
func readinessRound(t *testing.T, e *env, kind, checkout string, plan ReadinessPlan) *round {
	t.Helper()
	in := e.input(kind)
	in.SlotPath, in.Readiness = checkout, plan
	rd, err := e.r.newRound(e.ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rd.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return rd
}

func TestReadinessDataListsEveryCheckForTheJudge(t *testing.T) {
	e := newEnv(t)
	e.exec.Rules = []execx.Rule{zshRule("bin/setup-test-db", execx.Result{}, nil)}
	rd := readinessRound(t, e, KindInitial, t.TempDir(), ReadinessPlan{Prepare: []string{"bin/setup-test-db"}})
	if got := rd.readinessData(); len(got.Checks) != 0 || got.File != "" {
		t.Fatalf("before the step: %+v", got)
	}
	if err := rd.readiness(e.ctx); err != nil {
		t.Fatal(err)
	}
	got := rd.readinessData()
	if got.Failed != 0 || got.File != filepath.Join(rd.dir, ReadinessFile) || len(got.Checks) != 1 ||
		got.Checks[0] != (agents.ReadinessCheck{Kind: agents.ReadinessPrepare, Command: "bin/setup-test-db", OK: true, Status: agents.ReadinessOK, Duration: "0s"}) {
		t.Fatalf("readinessData = %+v", got)
	}
	got.Checks[0].Status = "mutated"
	if rd.readinessData().Checks[0].Status != agents.ReadinessOK {
		t.Error("readinessData shares its slice with the round")
	}
}

// ready_timeout is the budget of the whole step: a command that outlives
// it is a timeout and the commands after it are skipped, not run.
func TestReadinessBudgetCoversTheWholeStep(t *testing.T) {
	e := newEnv(t)
	e.exec.Rules = []execx.Rule{{Prefix: []string{ReadinessShell, "-lc", "slow"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		if c.Timeout != time.Minute {
			t.Errorf("first command timeout = %s, want the whole budget", c.Timeout)
		}
		e.clock.Add(2 * time.Minute)
		return execx.Result{Code: -1, Duration: time.Minute}, &execx.RunError{Cmd: c, Err: context.DeadlineExceeded}
	}}}
	rd := readinessRound(t, e, KindRereview, t.TempDir(), ReadinessPlan{Prepare: []string{"slow"}, Ready: []string{"probe"}, Timeout: time.Minute})
	if err := rd.readiness(e.ctx); err != nil {
		t.Fatal(err)
	}
	got := rd.readinessData()
	if got.Failed != 2 || len(got.Checks) != 2 {
		t.Fatalf("readiness = %+v", got)
	}
	if c := got.Checks[0]; c.Status != agents.ReadinessTimeout || c.Detail != "stopped when ready_timeout (1m0s) ran out" {
		t.Errorf("slow = %+v", c)
	}
	if c := got.Checks[1]; c.Status != agents.ReadinessSkipped || c.Detail != "ready_timeout (1m0s) was spent before it ran" {
		t.Errorf("probe = %+v", c)
	}
	if n := len(e.exec.CallsWithPrefix(ReadinessShell, "-lc", "probe")); n != 0 {
		t.Errorf("probe ran %d times after the budget was spent", n)
	}
}

func TestReadinessCommandThatCannotStart(t *testing.T) {
	e := newEnv(t)
	e.exec.Rules = []execx.Rule{zshRule("bin/x", execx.Result{Code: -1}, &execx.RunError{Cmd: execx.Cmd{Name: "zsh"}, Err: os.ErrNotExist})}
	rd := readinessRound(t, e, KindInitial, t.TempDir(), ReadinessPlan{Ready: []string{"bin/x"}})
	if err := rd.readiness(e.ctx); err != nil {
		t.Fatal(err)
	}
	if c := rd.readinessData().Checks[0]; c.Status != agents.ReadinessFailed || c.Detail != "could not run: file does not exist" {
		t.Errorf("check = %+v", c)
	}
}

// Nothing to run: no commands and no pinned Ruby, a continued turn, or a
// runner without Exec write no file and leave the judge's list empty.
func TestReadinessRunsNothingWhenThereIsNothingToCheck(t *testing.T) {
	pinned := map[string]string{".ruby-version": "3.3.4\n"}
	for _, tc := range []struct {
		name  string
		kind  string
		files map[string]string
		plan  ReadinessPlan
		exec  bool
	}{
		{"no commands, no pin", KindInitial, nil, ReadinessPlan{}, true},
		{"continue", KindContinue, pinned, ReadinessPlan{Prepare: []string{"bin/x"}}, true},
		{"no exec", KindInitial, pinned, ReadinessPlan{Prepare: []string{"bin/x"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if !tc.exec {
				e.r.Exec = nil
			}
			rd := readinessRound(t, e, tc.kind, readinessCheckout(t, tc.files), tc.plan)
			if err := rd.readiness(e.ctx); err != nil {
				t.Fatal(err)
			}
			if got := rd.readinessData(); len(got.Checks) != 0 {
				t.Errorf("readiness = %+v", got)
			}
			if len(e.exec.Calls) != 0 {
				t.Errorf("commands ran: %v", e.exec.Calls)
			}
			if _, err := os.Stat(filepath.Join(rd.dir, ReadinessFile)); !os.IsNotExist(err) {
				t.Errorf("readiness file: %v", err)
			}
		})
	}
}

func TestReadinessCancelledRoundStops(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(e.ctx)
	e.exec.Rules = []execx.Rule{{Prefix: []string{ReadinessShell, "-lc", "first"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		cancel()
		return execx.Result{}, &execx.RunError{Cmd: c, Err: context.Canceled}
	}}}
	rd := readinessRound(t, e, KindInitial, t.TempDir(), ReadinessPlan{Prepare: []string{"first", "second"}})
	if err := rd.readiness(ctx); err == nil {
		t.Fatal("readiness of a cancelled round returned nil")
	}
	got := rd.readinessData()
	if len(got.Checks) != 2 || got.Checks[0].Status != agents.ReadinessSkipped || got.Checks[1].Status != agents.ReadinessSkipped {
		t.Errorf("readiness = %+v", got)
	}
	if n := len(e.exec.CallsWithPrefix(ReadinessShell, "-lc", "second")); n != 0 {
		t.Errorf("second ran after the cancel")
	}
}

func TestRubyPin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		files  map[string]string
		want   rubyPinned
		pinned bool
	}{
		{"none", nil, rubyPinned{}, false},
		{"ruby-version", map[string]string{".ruby-version": "3.3.4\n"}, rubyPinned{".ruby-version", "3.3.4"}, true},
		{"ruby-version prefixed", map[string]string{".ruby-version": "ruby-3.3.4\n"}, rubyPinned{".ruby-version", "3.3.4"}, true},
		{"ruby-version minor", map[string]string{".ruby-version": "3.3"}, rubyPinned{".ruby-version", "3.3"}, true},
		{"ruby-version garbage", map[string]string{".ruby-version": "$(curl evil)\n"}, rubyPinned{".ruby-version", ""}, true},
		{"mise string", map[string]string{".mise.toml": "[tools]\nruby = \"3.3.5\"\nnode = \"22\"\n"}, rubyPinned{".mise.toml", "3.3.5"}, true},
		{"mise list", map[string]string{".mise.toml": "[tools]\nruby = [\"3.4.1\", \"3.3\"]\n"}, rubyPinned{".mise.toml", "3.4.1"}, true},
		{"mise table", map[string]string{".mise.toml": "[tools]\nruby = { version = \"3.3.6\" }\n"}, rubyPinned{".mise.toml", "3.3.6"}, true},
		{"mise latest", map[string]string{".mise.toml": "[tools]\nruby = \"latest\"\n"}, rubyPinned{".mise.toml", ""}, true},
		{"mise wins over ruby-version", map[string]string{".mise.toml": "[tools]\nruby = \"3.4.1\"\n", ".ruby-version": "3.3.4"}, rubyPinned{".mise.toml", "3.4.1"}, true},
		{"mise without ruby", map[string]string{".mise.toml": "[tools]\nnode = \"22\"\n", ".ruby-version": "3.3.4"}, rubyPinned{".ruby-version", "3.3.4"}, true},
		{"mise broken", map[string]string{".mise.toml": "[tools\n", ".ruby-version": "3.3.4"}, rubyPinned{".ruby-version", "3.3.4"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := rubyPin(readinessCheckout(t, tc.files))
			if ok != tc.pinned || got != tc.want {
				t.Errorf("rubyPin = %+v, %v; want %+v, %v", got, ok, tc.want, tc.pinned)
			}
		})
	}
}

// The pin files belong to the PR: a symlink pointing out of the checkout
// is not followed.
func TestRubyPinDoesNotFollowSymlinksOutOfTheCheckout(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("3.3.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkout := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(checkout, ".ruby-version")); err != nil {
		t.Fatal(err)
	}
	if got, ok := rubyPin(checkout); ok {
		t.Errorf("rubyPin followed a symlink out of the checkout: %+v", got)
	}
}

func TestRubyVerdict(t *testing.T) {
	ran := func(out string) agents.ReadinessCheck {
		return agents.ReadinessCheck{Kind: agents.ReadinessRuby, Command: RubyCheckCommand, Status: agents.ReadinessOK, LastLine: out}
	}
	const v334 = "ruby 3.3.4 (2024-07-09 revision be1089c8ec) [arm64-darwin23]"
	for _, tc := range []struct {
		name, out  string
		pin        rubyPinned
		status     string
		detailPart string
	}{
		{"exact", v334, rubyPinned{".ruby-version", "3.3.4"}, agents.ReadinessOK, "runs Ruby 3.3.4"},
		{"minor pin", v334, rubyPinned{".mise.toml", "3.3"}, agents.ReadinessOK, "runs Ruby 3.3.4"},
		{"patch is not a prefix", "ruby 3.3.40 (x)", rubyPinned{".ruby-version", "3.3.4"}, agents.ReadinessFailed, "runs 3.3.40"},
		{"mismatch", "ruby 3.2.2 (x)", rubyPinned{".ruby-version", "3.3.4"}, agents.ReadinessFailed, "pins Ruby 3.3.4 (.ruby-version)"},
		{"no version printed", "zsh: command not found: ruby", rubyPinned{".ruby-version", "3.3.4"}, agents.ReadinessFailed, "printed no Ruby version"},
		{"uncomparable pin", v334, rubyPinned{".mise.toml", ""}, agents.ReadinessOK, "not a version this check compares"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rubyVerdict(ran(tc.out), tc.pin)
			if got.Status != tc.status || !strings.Contains(got.Detail, tc.detailPart) {
				t.Errorf("rubyVerdict = %+v, want %s with %q", got, tc.status, tc.detailPart)
			}
			if strings.Contains(got.Detail, "command not found") {
				t.Errorf("detail carries output: %q", got.Detail)
			}
		})
	}
}

// A Ruby the pin names but mise has not installed makes `ruby -v` fail; the
// judge is told which version the checkout wants.
func TestRubyCheckThatFailsNamesThePin(t *testing.T) {
	e := newEnv(t)
	e.exec.Rules = []execx.Rule{zshRule(RubyCheckCommand, execx.Result{Code: 127,
		Stderr: []byte("mise ERROR ruby@3.3.4 is not installed\n")}, nil)}
	rd := readinessRound(t, e, KindInitial, readinessCheckout(t, map[string]string{".ruby-version": "3.3.4"}), ReadinessPlan{})
	if err := rd.readiness(e.ctx); err != nil {
		t.Fatal(err)
	}
	c := rd.readinessData().Checks[0]
	if c.Status != agents.ReadinessFailed || c.Detail != "the checkout pins Ruby 3.3.4 (.ruby-version) but `ruby -v` failed (exit 127)" ||
		c.LastLine != "mise ERROR ruby@3.3.4 is not installed" {
		t.Errorf("check = %+v", c)
	}
}

func TestLastLine(t *testing.T) {
	long := strings.Repeat("x", readinessLineMax+50)
	for _, tc := range []struct {
		name   string
		res    execx.Result
		failed bool
		want   string
	}{
		{"stdout last non-empty", execx.Result{Stdout: []byte("a\nb\n\n  \n")}, false, "b"},
		{"stderr first when failed", execx.Result{Stdout: []byte("out\n"), Stderr: []byte("boom\n")}, true, "boom"},
		{"stdout when stderr empty", execx.Result{Stdout: []byte("out\n")}, true, "out"},
		{"control characters dropped", execx.Result{Stdout: []byte("\x1b[31mred\x1b[0m\r\n")}, false, "[31mred[0m"},
		{"truncation marker skipped", execx.Result{Stdout: []byte("kept\n" + execx.TruncationMarker)}, false, "kept"},
		{"shortened", execx.Result{Stdout: []byte(long)}, false, strings.Repeat("x", readinessLineMax-1) + "…"},
		{"token redacted", execx.Result{Stdout: []byte("token ghp_" + strings.Repeat("a", 36) + "\n")}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lastLine(tc.res, tc.failed)
			if tc.name == "token redacted" {
				if strings.Contains(got, strings.Repeat("a", 36)) {
					t.Errorf("lastLine kept a token: %q", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("lastLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// The daemon's judges read the skill copy taken at startup, not the file
// as it is now.
func TestJudgeReadsTheStartupCopyOfItsSkill(t *testing.T) {
	e := newEnv(t)
	skill := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(skill, []byte("# skill at startup\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range e.cfg.Roles {
		if e.cfg.Roles[i].Judge {
			e.cfg.Roles[i].Skill = skill
		}
	}
	if _, err := e.cfg.SnapshotPrompts(filepath.Join(e.layout.State(), "skill"), t0); err != nil {
		t.Fatal(err)
	}
	cp := e.cfg.SkillFile(skill)
	if cp == skill {
		t.Fatal("no skill copy")
	}
	if err := os.WriteFile(skill, []byte("# edited after startup\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(501, "COMMENTED", "COMMENT").behavior(t)}
	if res, err := e.r.RunRound(e.ctx, e.input(KindInitial)); err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	judge := e.ag.submitsFor(agents.RoleJudge)
	if len(judge) != 1 {
		t.Fatalf("judge submits = %d", len(judge))
	}
	mustContain(t, "judge prompt", judge[0].Text, fmt.Sprintf("[$magnum-review](%s)", cp))
	if b, _ := os.ReadFile(cp); string(b) != "# skill at startup\n" {
		t.Errorf("copy = %q", b)
	}
}

// nextPrompt reads prompts/<name>.next, the edit waiting for the daemon's
// restart, else prompts/<name> once it was swapped in.
func nextPrompt(t *testing.T, name string) config.Prompt {
	t.Helper()
	dir := filepath.Join("..", "..", "prompts")
	for _, file := range []string{name + ".next", name} {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err == nil {
			return config.Prompt{Name: name, Path: filepath.Join(dir, file), Text: string(b)}
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	t.Fatalf("no prompt %s", name)
	return config.Prompt{}
}

// judgeDataWithReadiness renders the judge prompt with a readiness list
// whether or not agents.JudgeData carries the field yet (the outer field
// wins over a promoted one).
type judgeDataWithReadiness struct {
	agents.JudgeData
	Readiness agents.Readiness
}

func TestJudgeInitialPromptListsReadiness(t *testing.T) {
	p := nextPrompt(t, "judge-initial.md")
	base := agents.JudgeData{RunID: "r1", Owner: "talkable", Repo: "talkable", Number: 1, URL: "https://github.com/talkable/talkable/pull/1",
		HeadSHA: target, SkillPath: "/s/SKILL.md", ResultFile: "/r/codex-judge.json", ReviewerLogin: "talkable[bot]"}
	render := func(r agents.Readiness) string {
		t.Helper()
		out, err := agents.RenderPrompt(p, judgeDataWithReadiness{JudgeData: base, Readiness: r})
		if err != nil {
			t.Fatalf("render %s: %v", p.Path, err)
		}
		return out
	}

	none := render(agents.Readiness{})
	if strings.Contains(none, "readiness") {
		t.Errorf("a round without a readiness step mentions it:\n%s", none)
	}

	out := render(agents.Readiness{Failed: 1, File: "/r/readiness.json", Checks: []agents.ReadinessCheck{
		{Kind: agents.ReadinessPrepare, Command: "bin/rails db:test:prepare", Status: agents.ReadinessFailed, Detail: "exit 1", Duration: "4.2s",
			LastLine: "ActiveRecord::NoDatabaseError"},
		{Kind: agents.ReadinessRuby, Command: RubyCheckCommand, OK: true, Status: agents.ReadinessOK, Detail: "runs Ruby 3.3.4", Duration: "0.3s"},
	}})
	// The facts are <magnum> fields; what to do about them is the skill's.
	mustContain(t, "judge prompt", out,
		"readiness: /r/readiness.json\n  - prepare `bin/rails db:test:prepare`: failed in 4.2s (exit 1)\n  - ruby `ruby -v`: ok in 0.3s (runs Ruby 3.3.4)\nresult_file: /r/codex-judge.json")
	if strings.Contains(out, "NoDatabaseError") {
		t.Errorf("the prompt carries a command's output (PR text):\n%s", out)
	}
	if strings.Contains(out[:strings.Index(out, "<magnum>")], "readiness") {
		t.Errorf("the prompt repeats the skill's readiness paragraph:\n%s", out)
	}

	allOK := render(agents.Readiness{File: "/r/readiness.json", Checks: []agents.ReadinessCheck{
		{Kind: agents.ReadinessReady, Command: "bin/db-ready", OK: true, Status: agents.ReadinessOK, Duration: "1s"}}})
	mustContain(t, "judge prompt", allOK, "readiness: /r/readiness.json\n  - ready `bin/db-ready`: ok in 1s\n")
	if strings.Contains(allOK, "did not pass") {
		t.Errorf("all checks passed, yet the prompt warns:\n%s", allOK)
	}
}

// A round without a readiness step renders the judge prompt exactly as the
// prompt without the readiness lines did (the embedded copy, until the
// edit is swapped in).
func TestJudgeInitialPromptWithoutReadinessIsUnchanged(t *testing.T) {
	next := nextPrompt(t, "judge-initial.md")
	live, err := (&config.Config{}).ResolvePrompt("judge-initial.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []agents.JudgeData{
		{RunID: "r1", Owner: "talkable", Repo: "talkable", Number: 1, HeadSHA: target, ResultFile: "/r/codex-judge.json"},
		{RunID: "r1", Owner: "talkable", Repo: "talkable", Number: 1, HeadSHA: target, NotesPath: "/n/talkable.md",
			Reports: []agents.Report{{Role: "claude-review", Path: "/r/claude-review.md"}, {Role: "codex-review", Missing: true}}},
	} {
		want, err := agents.RenderPrompt(live, judgeDataWithReadiness{JudgeData: d})
		if err != nil {
			t.Fatal(err)
		}
		got, err := agents.RenderPrompt(next, judgeDataWithReadiness{JudgeData: d})
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("without readiness the prompt changed:\n--- live\n%s\n--- next\n%s", want, got)
		}
	}
}
