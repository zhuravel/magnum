package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// `magnum factory retry <task>` sends your retry of a task that waits on
// you, by its id or its Jira key, and prints the daemon's answer; the
// daemon's refusal is the command's error. It takes exactly one task.
func TestFactoryRetrySendsYourRetry(t *testing.T) {
	storetest.Serial(t)
	var got factory.RetryPayload
	refuse := false
	dt := newFactoryStartTest(t, 250, func(q registry.Request) (string, string) {
		if q.Kind != factory.ReqRetry {
			t.Errorf("request kind %q", q.Kind)
		}
		if err := json.Unmarshal(q.Payload, &got); err != nil {
			t.Error(err)
		}
		if refuse {
			return registry.RequestFailed, "PR-12360 is active, not waiting on you: there is nothing to retry"
		}
		return registry.RequestDone, "PR-12360 goes on at investigate"
	})
	reg, err := registry.Open(dt.ctx.Layout.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	task, err := reg.CreateTask(context.Background(), registry.Task{JiraKey: "PR-12360", Kind: "bug", Repo: "talkable/talkable",
		State: registry.TaskNeedsYou, Stage: "investigate", WaitsWho: "you", WaitsWhat: "the fx-cause agent wrote no valid findings-fx-cause.md"})
	reg.Close()
	if err != nil {
		t.Fatal(err)
	}

	if code := dt.run("factory", "retry", "PR-12360"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	if got != (factory.RetryPayload{TaskID: task.ID, Surface: "cli"}) || !strings.Contains(dt.stdout.String(), "PR-12360 goes on at investigate") {
		t.Fatalf("payload %+v, stdout %q", got, dt.stdout)
	}

	refuse = true
	if code := dt.run("factory", "retry", "1"); code != 1 || !strings.Contains(dt.stderr.String(), "nothing to retry") {
		t.Fatalf("a refused retry: exit %d, stderr %s", code, dt.stderr)
	}
	for _, bad := range [][]string{{}, {"PR-12360", "PR-12361"}} {
		if code := dt.run("factory", append([]string{"retry"}, bad...)...); code == 0 {
			t.Errorf("retry %q went through", bad)
		}
	}
}

// `magnum factory retry <task> --accept-qa` sends your retry with your
// acceptance of the QA defects the writer declined, which only the daemon
// can check; a plain retry sends none.
func TestFactoryRetryAcceptQASendsYourAcceptance(t *testing.T) {
	storetest.Serial(t)
	var got factory.RetryPayload
	dt := newFactoryStartTest(t, 250, func(q registry.Request) (string, string) {
		if err := json.Unmarshal(q.Payload, &got); err != nil {
			t.Error(err)
		}
		return registry.RequestDone, "PR-12370 goes on at qa; QA accepts 1 defect the writer declined, accepted by you"
	})
	reg, err := registry.Open(dt.ctx.Layout.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	task, err := reg.CreateTask(context.Background(), registry.Task{JiraKey: "PR-12370", Kind: "bug", Repo: "example/shop",
		State: registry.TaskNeedsYou, Stage: "qa", WaitsWho: "you", WaitsWhat: "the writer declined 1 defect that blocks"})
	reg.Close()
	if err != nil {
		t.Fatal(err)
	}
	if code := dt.run("factory", "retry", "PR-12370", "--accept-qa"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	if got != (factory.RetryPayload{TaskID: task.ID, Surface: "cli", AcceptQA: true}) || !strings.Contains(dt.stdout.String(), "accepted by you") {
		t.Fatalf("payload %+v, stdout %q", got, dt.stdout)
	}
	if code := dt.run("factory", "retry", "PR-12370"); code != 0 || got.AcceptQA {
		t.Fatalf("a plain retry: exit %d, payload %+v", code, got)
	}
}
