package cli

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/store"
)

// On a terminal release asks y/N before it parks the sessions and resets
// the worktree, but --json skipped the question and reset it. Like cleanup's
// --json, release --json applies only with --yes: without it nothing is
// released, handed to the daemon or asked. Off a terminal (scripts, the
// herdr plugin) nothing changes, and --json --yes releases.
func TestReleaseJSONOnATerminalNeedsYes(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.cleaner.plan = cleanup.Plan{Actions: []cleanup.Action{{Kind: "release", Slot: "review3"}}}
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.stdin("y\n")
	if code := h.cmd("release", "5", "--json"); code != 2 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "--json needs --yes to apply", "nothing released")
	if h.cleaner.applied != 0 || len(h.cleaner.opts) != 0 || len(h.requests()) != 0 || h.out.Len() != 0 {
		t.Fatalf("applied %d planned %d requests %d out %s", h.cleaner.applied, len(h.cleaner.opts), len(h.requests()), h.out.String())
	}
	if strings.Contains(h.errb.String(), "release talkable#5?") {
		t.Fatalf("asked: %s", h.errb.String())
	}

	if code := h.cmd("release", "5", "--json", "--yes"); code != 0 || h.cleaner.applied != 1 {
		t.Fatalf("--json --yes: exit %d applied %d: %s", code, h.cleaner.applied, h.errb.String())
	}
	h.d.StdinTTY = false // a script: no question, so --json alone applies as before
	if code := h.cmd("release", "5", "--json"); code != 0 || h.cleaner.applied != 2 {
		t.Fatalf("script --json: exit %d applied %d: %s", code, h.cleaner.applied, h.errb.String())
	}
}

// unapprove's y/N on a terminal was skipped by --json too: it withdrew the
// operator's approval unasked. --json there needs --yes.
func TestUnapproveJSONOnATerminalNeedsYes(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.standingAutoApproval(pr)
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.stdin("y\n")
	if code := h.cmd("unapprove", "5", "--json"); code != 2 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "--json needs --yes to apply", "nothing withdrawn")
	if reqs := h.requests(); len(reqs) != 0 || h.out.Len() != 0 {
		t.Fatalf("queued %+v, out %s", reqs, h.out.String())
	}
	if code := h.cmd("unapprove", "5", "--json", "--yes"); code != 0 || len(h.requests()) != 1 {
		t.Fatalf("--json --yes: exit %d requests %d: %s", code, len(h.requests()), h.errb.String())
	}
}
