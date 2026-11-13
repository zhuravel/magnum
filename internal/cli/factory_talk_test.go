package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// talkTest is a factory start test whose daemon answers a talk, whose terminal
// is a custom launcher (the reveal after a talk runs one `/bin/sh` on the
// test's fake runner, never a real terminal), and whose herdr focus is
// recorded (never a real herdr): the agents the CLI focused, and the sockets.
type talkTest struct {
	*daemonGroupTest
	focused, sockets []string
	focusErr         error
}

// newFactoryTalkTest is a talkTest whose daemon answers every request with
// answer.
func newFactoryTalkTest(t *testing.T, answer func(registry.Request) (string, string)) *talkTest {
	t.Helper()
	tt := &talkTest{daemonGroupTest: newFactoryStartTest(t, 250, answer)}
	cfg := *tt.ctx.preset
	cfg.Terminal.App, cfg.Terminal.Launcher, cfg.Terminal.Session = "custom", "myterm -e {herdr} {args}", "default"
	cfg.Factory.HerdrSession = "factory"
	tt.ctx.preset = &cfg
	tt.fake.Rules = append([]execx.Rule{{Prefix: []string{"/bin/sh"}}}, tt.fake.Rules...)
	daemonSys.FocusAgent = func(_ context.Context, socket, target string) error {
		tt.sockets = append(tt.sockets, socket)
		tt.focused = append(tt.focused, target)
		return tt.focusErr
	}
	return tt
}

// task makes a task in the factory registry of tt.
func (tt *talkTest) task(t *testing.T, key, state string) registry.Task {
	t.Helper()
	reg, err := registry.Open(tt.ctx.Layout.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	task, err := reg.CreateTask(context.Background(), registry.Task{JiraKey: key, Kind: "bug", Repo: "talkable/talkable", State: state,
		Stage: "report"})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// liaison puts task's liaison in the factory's agents store: its live session
// in pane, as Runtime.Talk leaves it.
func (tt *talkTest) liaison(t *testing.T, task registry.Task, name, pane string) {
	t.Helper()
	ctx := context.Background()
	storetest.Seed(t, tt.ctx.Layout.FactoryAgentsDB())
	st, err := store.Open(tt.ctx.Layout.FactoryAgentsDB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	repo, err := st.UpsertRepo(ctx, store.Repo{NodeID: "factory-liaison", Owner: "factory-liaison", Name: "liaison", WatchOwner: "factory",
		Mode: store.RepoModePerPR})
	if err != nil {
		t.Fatal(err)
	}
	title := task.JiraKey
	up, err := st.UpsertPRFromGitHub(ctx, store.GitHubPR{RepoID: repo.ID, NodeID: "factory:liaison:" + strconv.FormatInt(task.ID, 10),
		Number: int(task.ID), URL: "file:///x", HeadSHA: "t1", Title: &title, GHState: store.GHOpen, InitialState: store.PRClaiming, Identity: "factory"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSession(ctx, store.Session{PRID: up.PR.ID, Role: runtime.LiaisonRole, State: store.SessionLive, AgentName: &name,
		HerdrPaneID: &pane}); err != nil {
		t.Fatal(err)
	}
}

// talked is the daemon's answer of a talk in pane.
func talked(t *testing.T, got *factory.TalkPayload, pane string) func(registry.Request) (string, string) {
	t.Helper()
	return func(q registry.Request) (string, string) {
		if q.Kind != factory.ReqTalk {
			t.Errorf("request kind %q", q.Kind)
		}
		if err := json.Unmarshal(q.Payload, got); err != nil {
			t.Error(err)
		}
		return registry.RequestDone, "the liaison of PR-12345 talks in pane " + pane + " of the factory's herdr session"
	}
}

// `magnum factory talk <task>` queues a talk request for the task, by its id
// or its Jira key, with the surface cli, and prints the daemon's answer. A
// task it does not know exits 1 naming it and queues nothing; the daemon's
// refusal is the command's error; it takes exactly one task.
func TestFactoryTalkQueuesATalkRequest(t *testing.T) {
	storetest.Serial(t)
	var got factory.TalkPayload
	refuse := false
	tt := newFactoryTalkTest(t, func(q registry.Request) (string, string) {
		if refuse {
			return registry.RequestFailed, "PR-12345 is stopped: a task that ended has no liaison to talk with"
		}
		return talked(t, &got, "p_9")(q)
	})
	task := tt.task(t, "PR-12345", registry.TaskNeedsYou)

	if code := tt.run("factory", "talk", "PR-12345"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, tt.stderr)
	}
	if got != (factory.TalkPayload{TaskID: task.ID, Surface: "cli"}) {
		t.Fatalf("payload %+v, want task %d from the cli", got, task.ID)
	}
	if out := tt.stdout.String(); !strings.Contains(out, "the liaison of PR-12345 talks in pane p_9 of the factory's herdr session") {
		t.Fatalf("stdout %q, want the daemon's answer", out)
	}

	got = factory.TalkPayload{}
	tt.stdout.Reset()
	if code := tt.run("factory", "talk", strconv.FormatInt(task.ID, 10)); code != 0 || got.TaskID != task.ID {
		t.Fatalf("talk by id: exit %d, payload %+v, stderr %s", code, got, tt.stderr)
	}

	before := len(startRequests(t, tt.ctx.Layout))
	tt.stderr.Reset()
	if code := tt.run("factory", "talk", "PR-12399"); code != 1 || !strings.Contains(tt.stderr.String(), "PR-12399") {
		t.Fatalf("talk of an unknown key: exit %d, stderr %s, want 1 naming PR-12399", code, tt.stderr)
	}
	if after := len(startRequests(t, tt.ctx.Layout)); after != before {
		t.Fatalf("an unknown key queued %d request(s)", after-before)
	}

	refuse = true
	tt.stderr.Reset()
	if code := tt.run("factory", "talk", "PR-12345"); code != 1 || !strings.Contains(tt.stderr.String(), "PR-12345 is stopped") {
		t.Fatalf("a refused talk: exit %d, stderr %s", code, tt.stderr)
	}
	for _, bad := range [][]string{{}, {"PR-12345", "PR-12346"}} {
		if code := tt.run("factory", append([]string{"talk"}, bad...)...); code != 2 {
			t.Errorf("talk %q: exit %d, want a usage error", bad, code)
		}
	}
}

// After a talk the CLI steps into the liaison as the screen's o steps into a
// task's agent: herdr focuses the liaison's agent on the factory's session
// (its socket), and the terminal on that session comes to the front, one
// launch of the configured terminal on `herdr --session factory`.
func TestFactoryTalkStepsIntoTheLiaisonsPane(t *testing.T) {
	storetest.Serial(t)
	var got factory.TalkPayload
	tt := newFactoryTalkTest(t, talked(t, &got, "p_9"))
	task := tt.task(t, "PR-12345", registry.TaskNeedsYou)
	tt.liaison(t, task, "factory-liaison/liaison-1-fx-liaison", "p_9")

	if code := tt.run("factory", "talk", "PR-12345"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, tt.stderr)
	}

	if len(tt.focused) != 1 || tt.focused[0] != "factory-liaison/liaison-1-fx-liaison" {
		t.Fatalf("herdr focused %q, want the liaison's agent", tt.focused)
	}
	if want := tt.ctx.Config.FactoryHerdr().Socket; tt.sockets[0] != want || want == "" {
		t.Fatalf("herdr focused on socket %q, want the factory session's %q", tt.sockets[0], want)
	}
	calls := tt.fake.CallsWithPrefix("/bin/sh")
	if len(calls) != 1 || !strings.HasSuffix(strings.Join(calls[0].Args, " "), "myterm -e herdr --session factory") {
		t.Fatalf("terminal calls %+v, want one launch on the factory's session", calls)
	}
	if out := tt.stdout.String(); !strings.Contains(out, "the liaison of PR-12345 talks in pane p_9") || !strings.Contains(out, "you're in the pane of the liaison") {
		t.Fatalf("stdout %q, want the answer and that you are in the liaison's pane", out)
	}
	if tt.stderr.Len() != 0 {
		t.Fatalf("stderr %q, want none", tt.stderr)
	}
}

// A talk the daemon refused, or has not answered, steps into nothing. A pane
// that cannot be focused (the liaison is not in the agents store, herdr
// refuses) is said on stderr, the terminal still comes to the front, and the
// command exits 0: the liaison talks all the same.
func TestFactoryTalkStepsIntoNothingWithoutAnAnswerAndSaysWhatFailed(t *testing.T) {
	storetest.Serial(t)
	var got factory.TalkPayload
	refuse := false
	tt := newFactoryTalkTest(t, func(q registry.Request) (string, string) {
		if refuse {
			return registry.RequestFailed, "PR-12345 is stopped: a task that ended has no liaison to talk with"
		}
		return talked(t, &got, "p_9")(q)
	})
	task := tt.task(t, "PR-12345", registry.TaskNeedsYou)

	refuse = true
	if code := tt.run("factory", "talk", "PR-12345"); code != 1 {
		t.Fatalf("a refused talk: exit %d, stderr %s", code, tt.stderr)
	}
	if len(tt.focused) != 0 || len(tt.fake.CallsWithPrefix("/bin/sh")) != 0 {
		t.Fatalf("a refused talk focused %q and launched %d terminals", tt.focused, len(tt.fake.CallsWithPrefix("/bin/sh")))
	}

	refuse = false
	tt.stderr.Reset()
	if code := tt.run("factory", "talk", "PR-12345"); code != 0 {
		t.Fatalf("a talk with no liaison in the agents store: exit %d, stderr %s", code, tt.stderr)
	}
	if len(tt.focused) != 0 || !strings.Contains(tt.stderr.String(), "liaison") || len(tt.fake.CallsWithPrefix("/bin/sh")) != 1 {
		t.Fatalf("no liaison to focus: focused %q, stderr %q, %d terminals; want it said and the terminal brought forward",
			tt.focused, tt.stderr, len(tt.fake.CallsWithPrefix("/bin/sh")))
	}

	tt.liaison(t, task, "factory-liaison/liaison-1-fx-liaison", "p_9")
	tt.focusErr = errors.New("herdr: no such agent")
	tt.stderr.Reset()
	if code := tt.run("factory", "talk", "PR-12345"); code != 0 {
		t.Fatalf("a talk whose pane herdr cannot focus: exit %d, stderr %s", code, tt.stderr)
	}
	if !strings.Contains(tt.stderr.String(), "no such agent") || len(tt.fake.CallsWithPrefix("/bin/sh")) != 2 {
		t.Fatalf("stderr %q, %d terminals; want herdr's refusal said and the terminal brought forward", tt.stderr, len(tt.fake.CallsWithPrefix("/bin/sh")))
	}
}

// The liaison runs `magnum factory answer`, `decide` and `retry` in a pane
// whose env carries the daemon's MAGNUM_HOME but not its MAGNUM_CONFIG, so
// none of them may read a config: with no config loaded and a --config that
// would not load, they all go through. A command that needed the daemon's
// config would break under a non-default one; this test is what says the
// pane needs none (Runtime.liaisonEnv).
func TestTheLiaisonsCommandsLoadNoConfig(t *testing.T) {
	storetest.Serial(t)
	tt := newFactoryTalkTest(t, func(registry.Request) (string, string) { return registry.RequestDone, "ok" })
	asked := seedQuestions(t, tt.daemonGroupTest)
	decided, _ := seedReadyCard(t, tt.daemonGroupTest)
	broken := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(broken, []byte("this is [not toml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	answers := filepath.Join(t.TempDir(), "answers.json")
	if err := os.WriteFile(answers, []byte(`{"revision": 1, "answers": {"1": "Seven days before it expires"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tt.ctx.preset, tt.ctx.cfgPath = nil, broken
	shown := "2026-11-04T09:00:00Z"

	for _, args := range [][]string{
		{"retry", strconv.FormatInt(asked.ID, 10)},
		{"answer", strconv.FormatInt(asked.ID, 10)},
		{"answer", strconv.FormatInt(asked.ID, 10), "--json", answers, "--surface", "liaison", "--shown", shown},
		{"decide", strconv.FormatInt(decided.ID, 10)},
		{"decide", strconv.FormatInt(decided.ID, 10), "drop", "--surface", "liaison", "--shown", shown},
		{"decide", strconv.FormatInt(decided.ID, 10), "push", "--card", "2", "--surface", "liaison", "--shown", shown},
	} {
		tt.stdout.Reset()
		tt.stderr.Reset()
		if code := tt.run("factory", args...); code != 0 {
			t.Errorf("factory %s: exit %d, stderr %s", strings.Join(args, " "), code, tt.stderr)
		}
		if tt.ctx.Config != nil {
			t.Fatalf("factory %s loaded the config", strings.Join(args, " "))
		}
	}
}
