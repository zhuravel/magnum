package cli

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// finishTerminal puts the command on a terminal whose keys are input, or
// off one (tty false), for the test's span.
func finishTerminal(t *testing.T, tty bool, input string) {
	t.Helper()
	prevStdin, prevTTY, prevErrTTY := inspStdin, inspIsTTY, inspStderrTTY
	t.Cleanup(func() { inspStdin, inspIsTTY, inspStderrTTY = prevStdin, prevTTY, prevErrTTY })
	inspStdin = strings.NewReader(input)
	inspIsTTY = func() bool { return tty }
	inspStderrTTY = func(io.Writer) bool { return tty }
}

// finishTask is a factory task of key in state at stage, its round 1 of
// the local review in flight when round is set.
func finishTask(t *testing.T, dt *daemonGroupTest, key, state, stage string, round bool) registry.Task {
	t.Helper()
	reg, err := registry.Open(dt.ctx.Layout.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	task, err := reg.CreateTask(context.Background(), registry.Task{JiraKey: key, Kind: "bug", Repo: "talkable/talkable", State: state, Stage: stage,
		WaitsWho: map[bool]string{true: "you"}[state == registry.TaskNeedsYou], WaitsWhat: map[bool]string{true: "review unavailable"}[state == registry.TaskNeedsYou]})
	if err != nil {
		t.Fatal(err)
	}
	if round {
		if _, err := reg.BeginCycle(context.Background(), task.ID, "abc1234", "skill"); err != nil {
			t.Fatal(err)
		}
	}
	return task
}

// `magnum factory finish-review <task>` says what it does and asks y/N on
// a terminal: only y sends your request (by its id or its Jira key), and
// the daemon's answer is printed; any other key sends nothing. --dry-run
// says it and sends nothing; off a terminal it asks nothing and sends
// nothing.
func TestFactoryFinishReviewAsksYNThenSendsTheRequest(t *testing.T) {
	storetest.Serial(t)
	var got factory.FinishReviewPayload
	sent := 0
	dt := newFactoryStartTest(t, 250, func(q registry.Request) (string, string) {
		sent++
		if q.Kind != factory.ReqFinishReview {
			t.Errorf("request kind %q", q.Kind)
		}
		if err := json.Unmarshal(q.Payload, &got); err != nil {
			t.Error(err)
		}
		return registry.RequestDone, "PR-12530: round 1 of its local review runs: it finishes, then no round starts after it"
	})
	task := finishTask(t, dt, "PR-12530", registry.TaskActive, "local_review", true)
	run := func(input string, tty bool, args ...string) int {
		dt.stdout.Reset()
		dt.stderr.Reset()
		finishTerminal(t, tty, input)
		return dt.run("factory", append([]string{"finish-review"}, args...)...)
	}

	if code := run("n\n", true, "PR-12530"); code != 1 || sent != 0 {
		t.Fatalf("n: exit %d, %d requests sent", code, sent)
	}
	for _, want := range []string{"End PR-12530's local review: round 1 of its local review runs: it finishes, then no round starts after it",
		"waits on you?", "the local review goes on"} {
		if !strings.Contains(dt.stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, dt.stderr)
		}
	}
	if code := run("y\n", true, "PR-12530"); code != 0 || sent != 1 || got != (factory.FinishReviewPayload{TaskID: task.ID, Surface: "cli"}) ||
		!strings.Contains(dt.stdout.String(), "PR-12530: round 1 of its local review runs") {
		t.Fatalf("y: exit %d, %d sent, payload %+v, stdout %q, stderr %q", code, sent, got, dt.stdout, dt.stderr)
	}
	if code := run("", false, "1", "--dry-run"); code != 0 || sent != 1 ||
		!strings.Contains(dt.stdout.String(), "dry-run: PR-12530: round 1 of its local review runs: it finishes, then no round starts after it") {
		t.Fatalf("--dry-run: exit %d, %d sent, stdout %q", code, sent, dt.stdout)
	}
	if code := run("y\n", false, "PR-12530"); code != 1 || sent != 1 || !strings.Contains(dt.stderr.String(), "asks y/N on a terminal") {
		t.Fatalf("off a terminal: exit %d, %d sent, stderr %q", code, sent, dt.stderr)
	}
	for _, bad := range [][]string{{}, {"PR-12530", "PR-12531"}} {
		if code := run("y\n", true, bad...); code == 0 {
			t.Errorf("finish-review %q went through", bad)
		}
	}
}

// finish-review refuses a task outside its local review at work with the
// reason, before any question or request, --dry-run too.
func TestFactoryFinishReviewRefusesATaskOutsideItsLocalReview(t *testing.T) {
	storetest.Serial(t)
	sent := 0
	dt := newFactoryStartTest(t, 250, func(registry.Request) (string, string) {
		sent++
		return registry.RequestDone, ""
	})
	finishTask(t, dt, "PR-12532", registry.TaskActive, "coding", false)
	finishTask(t, dt, "PR-12533", registry.TaskNeedsYou, "local_review", false)
	for key, want := range map[string]string{"PR-12532": "PR-12532 is at its coding stage, not in its local review",
		"PR-12533": "PR-12533 waits on you (review unavailable)"} {
		for _, args := range [][]string{{key}, {key, "--dry-run"}} {
			dt.stderr.Reset()
			finishTerminal(t, true, "y\n")
			if code := dt.run("factory", append([]string{"finish-review"}, args...)...); code != 1 || sent != 0 || !strings.Contains(dt.stderr.String(), want) {
				t.Errorf("%v: exit %d, %d sent, stderr %q; want refused with %q", args, code, sent, dt.stderr, want)
			}
		}
	}
}

// A finish-review typed in a terminal starts with no config loaded (the
// CLI loads it per command), so the command loads it before it judges the
// task's stage: without it every task "is not in its local review". On
// 2026-11-13 the operator's `magnum factory finish-review 9` on a task at
// its local review was refused so.
func TestFactoryFinishReviewLoadsTheConfigBeforeItJudgesTheStage(t *testing.T) {
	storetest.Serial(t)
	dt := newFactoryStartTest(t, 250, func(registry.Request) (string, string) { return registry.RequestDone, "" })
	finishTask(t, dt, "PR-12536", registry.TaskActive, "local_review", true)
	dt.ctx.Config = nil // as Main leaves it before a command loads it
	finishTerminal(t, true, "")
	if code := dt.run("factory", "finish-review", "PR-12536", "--dry-run"); code != 0 || strings.Contains(dt.stderr.String(), "not in its local review") {
		t.Errorf("exit %d, stderr %q; want the plan of a task at its local review", code, dt.stderr)
	}
}
