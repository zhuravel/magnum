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

// `magnum factory let-go <task> --role <role>` sends your let-go of the
// role's prompt of unknown delivery, by the task's id or its Jira key, and
// prints the daemon's answer; the daemon's refusal is the command's error.
// It takes exactly one task and needs the role.
func TestFactoryLetGoSendsYourLetGo(t *testing.T) {
	storetest.Serial(t)
	var got factory.LetGoPayload
	refuse := false
	dt := newFactoryStartTest(t, 250, func(q registry.Request) (string, string) {
		if q.Kind != factory.ReqLetGo {
			t.Errorf("request kind %q", q.Kind)
		}
		if err := json.Unmarshal(q.Payload, &got); err != nil {
			t.Error(err)
		}
		if refuse {
			return registry.RequestFailed, "PR-12361 has no prompt of unknown delivery to fx-cause: there is nothing to let go"
		}
		return registry.RequestDone, "let 1 prompt(s) to fx-repro go"
	})
	reg, err := registry.Open(dt.ctx.Layout.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	task, err := reg.CreateTask(context.Background(), registry.Task{JiraKey: "PR-12361", Kind: "bug", Repo: "talkable/talkable",
		State: registry.TaskNeedsYou, Stage: "investigate", WaitsWho: "you", WaitsWhat: "a prompt to fx-repro may or may not have arrived"})
	reg.Close()
	if err != nil {
		t.Fatal(err)
	}

	if code := dt.run("factory", "let-go", "PR-12361", "--role", "fx-repro"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	if got != (factory.LetGoPayload{TaskID: task.ID, Role: "fx-repro", Surface: "cli"}) || !strings.Contains(dt.stdout.String(), "let 1 prompt(s) to fx-repro go") {
		t.Fatalf("payload %+v, stdout %q", got, dt.stdout)
	}

	refuse = true
	if code := dt.run("factory", "let-go", "1", "--role", "fx-cause"); code != 1 || !strings.Contains(dt.stderr.String(), "nothing to let go") {
		t.Fatalf("a refused let-go: exit %d, stderr %s", code, dt.stderr)
	}
	for _, bad := range [][]string{{}, {"PR-12361"}, {"PR-12361", "PR-12362", "--role", "fx-repro"}, {"--role", "fx-repro"}} {
		if code := dt.run("factory", append([]string{"let-go"}, bad...)...); code == 0 {
			t.Errorf("let-go %q went through", bad)
		}
	}
}
