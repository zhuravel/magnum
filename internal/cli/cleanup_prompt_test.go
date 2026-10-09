package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/cleanup"
)

// `magnum cleanup > plan.txt` on a terminal asked its y/N and the typed slug
// on stdout, into the file, and waited for an answer nobody could see. The
// questions go to stderr; the plan and the report stay on stdout.
func TestCleanupAsksOnStderrWhenStdoutIsRedirected(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	f.tty("y\npr27087fix\n")
	fp := &fakePlanner{plan: cleanupPlanFixture(), report: cleanup.Report{Done: 2}}
	cf, _ := cleanupParse(f.Ctx, nil)
	if code := cleanupExec(context.Background(), f.Ctx, fp, st, cf, "talkable/talkable"); code != 0 || !fp.applied || !fp.confirmed {
		t.Fatalf("code %d applied %v confirmed %v err %s", code, fp.applied, fp.confirmed, f.Err.String())
	}
	for _, q := range []string{"Apply 2 action(s)?", `Dropping the databases of slug "pr27087fix" cannot be undone.`} {
		if strings.Contains(f.Out.String(), q) {
			t.Errorf("stdout carries the question %q:\n%s", q, f.Out.String())
		}
		actContains(t, f.Err.String(), q)
	}
	actContains(t, f.Out.String(), "release   talkable#11700", "2 done")
}

// A terminal on stdin is not enough to ask: with stderr redirected
// (`magnum cleanup 2>log`) the question would be invisible, so cleanup
// applies nothing, reads nothing and says to pass --yes.
func TestCleanupDoesNotAskWhenStderrIsNotATerminal(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	f.tty("y\npr27087fix\n")
	inspStderrTTY = func(io.Writer) bool { return false }
	fp := &fakePlanner{plan: cleanupPlanFixture()}
	cf, _ := cleanupParse(f.Ctx, nil)
	if code := cleanupExec(context.Background(), f.Ctx, fp, st, cf, "talkable/talkable"); code != 1 || fp.applied {
		t.Fatalf("code %d applied %v", code, fp.applied)
	}
	actContains(t, f.Err.String(), "not applied", "--yes")
	if strings.Contains(f.Err.String()+f.Out.String(), "Apply 2 action(s)?") {
		t.Fatalf("asked anyway: out %s err %s", f.Out.String(), f.Err.String())
	}
	if inspConfirm(context.Background(), f.Ctx, "go?") || inspConfirmTyped(context.Background(), f.Ctx, "drop?", "pr27087fix") {
		t.Fatal("a confirmation with stderr off a terminal confirmed")
	}
}
