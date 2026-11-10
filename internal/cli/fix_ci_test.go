package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	// ciFixPlanText is the daemon's answer to a dry run of a CI fix, and
	// ciFixAnswer to the start.
	ciFixPlanText = engine.CIFixDry + "talkable/talkable#5: claude-fix fixes required check Completion at a1a1a1a (2 failed jobs) in review1 as fixer-app[bot], CI fix 1 of 3"
	ciFixAnswer   = "fixing the failed CI of talkable/talkable#5: claude-fix fixes the failed CI at a1a1a1a (2 failed jobs) in review1, CI fix 1 of 3; " +
		"`magnum abort talkable/talkable#5` stops it"
)

// ciFixAsked is what the daemon's plan asks about.
var ciFixAsked = engine.CIFixAsked{Head: "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", Entries: []string{"Completion"}, Count: 0,
	At: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)}

// ciFixDaemon answers each pending request as the daemon does: a dry run
// with the plan (its JSON line, then its words), a start with ciFixAnswer.
func (h *actHarness) ciFixDaemon() {
	h.onSleep = func(h *actHarness) {
		for _, r := range h.requests() {
			if r.State != store.RequestPending {
				continue
			}
			res := ciFixAnswer
			if actDecode[engine.FixPayload](h.t, r.Payload).DryRun {
				b, err := json.Marshal(engine.CIFixPlan{Ref: "talkable/talkable#5", Asked: ciFixAsked, Text: ciFixPlanText})
				if err != nil {
					h.t.Fatal(err)
				}
				res = string(b) + "\n" + ciFixPlanText
			}
			if err := h.st.CompleteRequest(h.ctx, r.ID, store.RequestDone, res); err != nil {
				h.t.Fatal(err)
			}
		}
	}
}

// `magnum fix --ci` sends a fix request in CI mode. On a terminal its y/N
// names the daemon's plan, asked in a dry run first, and y sends the start
// bound to it; --ci goes alone, and without a terminal it needs --yes.
func TestFixCIFlagSendsModeCIAndGoesAlone(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.ciFixDaemon()
	h.stdin("y\n")
	if code := h.cmd("fix", "5", "--ci"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	actContains(t, h.out.String(), "Fix the failed CI of talkable/talkable#5: claude-fix fixes required check Completion at a1a1a1a (2 failed jobs) in review1 as "+
		"fixer-app[bot], CI fix 1 of 3? [y/N]", ciFixAnswer)
	reqs := h.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %+v, want the plan and the start", reqs)
	}
	if p := actDecode[engine.FixPayload](t, reqs[0].Payload); !p.DryRun || p.Mode != engine.FixModeCI {
		t.Errorf("the plan's payload = %+v", p)
	}
	p := actDecode[engine.FixPayload](t, reqs[1].Payload)
	if p.DryRun || p.Mode != engine.FixModeCI || p.CIAsked == nil || p.CIAsked.Head != ciFixAsked.Head || !slices.Equal(p.CIAsked.Entries, ciFixAsked.Entries) ||
		!p.CIAsked.At.Equal(ciFixAsked.At) {
		t.Errorf("the start's payload = %+v, want CI mode bound to the plan", p)
	}
	for _, args := range [][]string{{"5", "--ci", "--off"}, {"5", "--ci", "--done"}, {"5", "--ci", "--decide", "4100=yes"}} {
		h.errb.Reset()
		if code := h.cmd("fix", args...); code != 2 || !strings.Contains(h.errb.String(), "--ci goes alone") {
			t.Errorf("%v: exit %d: %s", args, code, h.errb.String())
		}
	}
	h.d.StdinTTY, h.d.StdoutTTY = false, false
	h.errb.Reset()
	if code := h.cmd("fix", "5", "--ci"); code != 2 || !strings.Contains(h.errb.String(), "pass --yes to start it without asking") {
		t.Errorf("no terminal: exit %d: %s", code, h.errb.String())
	}
	h.out.Reset()
	if code := h.cmd("fix", "5", "--ci", "--dry-run"); code != 0 || !strings.Contains(h.out.String(), ciFixPlanText) || strings.Contains(h.out.String(), `{"ref"`) {
		t.Errorf("--dry-run: exit %d: %s", code, h.out.String())
	}
	if n := len(h.requests()); n != 3 {
		t.Errorf("requests = %d, want one more dry run", n)
	}
}

// A CI fix's plan is no read-only question: the daemon reads why the check
// failed for it (the base's CI, the failed jobs and the end of their logs,
// once per head) and keeps that for the start. --dry-run says so, and
// says nothing starts, instead of "nothing is changed".
func TestFixCIDryRunSaysThePlanReadsTheFailedCI(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.ciFixDaemon()
	if code := h.cmd("fix", "5", "--ci", "--dry-run"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	out := h.out.String() + h.errb.String()
	if !strings.Contains(out, "; nothing starts: the daemon reads why the CI failed, once for the head, and keeps it for the start") || strings.Contains(out, "nothing is changed") {
		t.Errorf("--ci --dry-run says:\n%s", out)
	}
	h.out.Reset()
	if code := h.cmd("fix", "--help"); code != 0 || !strings.Contains(h.out.String(), "its plan reads why the check failed") {
		t.Errorf("help: exit %d:\n%s", code, h.out.String())
	}
}
