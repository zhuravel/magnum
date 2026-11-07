package factory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// The other NEEDS YOU lines whose cause is the machine's, the
// configuration's or yours in the copy name what to do, then the command
// that lets the task go on: a claim's switch git refused over your file
// said "then let the task go on", a stage that keeps failing named only its
// cause, and a copy a base run left detached named `git switch` and no
// retry.
func TestTheOtherEnvironmentNeedsYouLinesNameTheRetry(t *testing.T) {
	retry := func(t *testing.T, what string, id int64, more ...string) {
		t.Helper()
		for _, w := range append(more, fmt.Sprintf("then `magnum factory retry %d`", id)) {
			if !strings.Contains(what, w) {
				t.Errorf("the wait %q does not name %q", what, w)
			}
		}
		if strings.Contains(what, "let the task go on") {
			t.Errorf("the wait %q still says only to let the task go on", what)
		}
	}

	t.Run("a claim's switch git refused", func(t *testing.T) {
		r := newCodingRig(t)
		task := r.codingTask("PR-12385", "bug")
		refused := &execx.ExitError{Code: 1, Stderr: "error: The following untracked working tree files would be overwritten by checkout:\n" +
			"\ttmp/seed.txt\nPlease move or remove them before you switch branches.\nAborting"}
		r.copies.mu.Lock()
		r.copies.claimErr = fmt.Errorf("claim factory1 for PR-12385: step switch_branch: gitx: switch %s: %w", task.Branch, refused)
		r.copies.mu.Unlock()
		r.startStage(task)
		got := r.reload(task.ID)
		if got.State != registry.TaskNeedsYou {
			t.Fatalf("task %s waits %q; want needs_you", got.State, got.WaitsWhat)
		}
		retry(t, got.WaitsWhat, got.ID, "tmp/seed.txt", "move or remove it")
	})

	t.Run("a stage that keeps failing", func(t *testing.T) {
		h := newHarness(t)
		task := h.task("PR-12386", registry.TaskActive, "investigate")
		h.stage(config.FactoryStageAgents, func(context.Context, registry.Task, config.FactoryStage) (bool, error) {
			return false, fmt.Errorf("gh pr create: HTTP 403: Resource not accessible by token")
		})
		for range stageFailLimit {
			h.clock.Add(45 * time.Second)
			h.tick()
		}
		got := h.reload(task.ID)
		if got.State != registry.TaskNeedsYou {
			t.Fatalf("task %s waits %q; want needs_you", got.State, got.WaitsWhat)
		}
		retry(t, got.WaitsWhat, got.ID, "HTTP 403", "`magnum factory install`")
	})

	t.Run("a copy a base run left detached", func(t *testing.T) {
		r := newCodingRig(t)
		task := r.codingTask("PR-12387", "bug")
		base := r.git(r.copyDir, "rev-parse", "HEAD")
		head := r.fix(task)
		r.copies.reserve(task.ID, "factory1")
		if err := r.e.detachForBase(r.ctx, task, r.cp, base, head, []string{pointsSpec}); err != nil {
			t.Fatal(err)
		}
		r.write(r.copyDir, pointsSpec, "your edit of the laid spec\n")
		r.restart()
		if err := r.e.startup(r.ctx); err != nil {
			t.Fatal(err)
		}
		got := r.reload(task.ID)
		if got.State != registry.TaskNeedsYou {
			t.Fatalf("task %s waits %q; want needs_you", got.State, got.WaitsWhat)
		}
		retry(t, got.WaitsWhat, got.ID, "git switch "+task.Branch)
	})
}
