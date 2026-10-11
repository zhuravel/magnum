package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
	"github.com/zhuravel/magnum/internal/tui"
)

// fixAnswer is what the daemon answers a request that starts a loop.
const fixAnswer = "fixing talkable/talkable#5: claude-fix fixes the 2 findings of the review on abc1234 in review1, cycle 1 of 3; " +
	"`magnum abort talkable/talkable#5` stops it"

// setFixRow records the PR's fix loop as the daemon keeps it.
func (h *actHarness) setFixRow(pr store.PR, phase string, cycle int) {
	h.t.Helper()
	b, err := json.Marshal(engine.FixState{Cycle: cycle, Cap: 3, Phase: phase, Since: pr.HeadSHA, Head: pr.HeadSHA, By: "magnum fix",
		Report: "/state/reviews/talkable/talkable/5/abc1234/claude-fix.md", StartedAt: h.now, Role: "claude-fix"})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, engine.KVPRFix(pr.ID), string(b)); err != nil {
		h.t.Fatal(err)
	}
}

// fixRunOf records the fixer's run of a cycle.
func (h *actHarness) fixRunOf(pr store.PR, round int, state string) store.Run {
	h.t.Helper()
	r, err := h.st.CreateRun(h.ctx, store.Run{PRID: pr.ID, Round: round, Role: "claude-fix", Kind: store.RunFix, TargetSHA: pr.HeadSHA, State: state})
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// fixEvent appends an event of the loop's PR (talkable/talkable#5) at the harness clock.
func (h *actHarness) fixEvent(kind, msg string, data map[string]any) {
	h.t.Helper()
	subject := "pr:talkable/talkable#5"
	ev := store.Event{Level: "info", Subject: &subject, Kind: kind, Message: msg}
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			h.t.Fatal(err)
		}
		ev.Data = b
	}
	if _, err := h.st.AppendEvent(h.ctx, ev); err != nil {
		h.t.Fatal(err)
	}
}

// fixDaemon plays the daemon around a loop: the first poll answers the
// request and starts the loop (cycle 1, the fixer working), a few polls
// later the fixer has pushed and the re-review runs, and at step 7 end
// plays the loop's last moment.
func (h *actHarness) fixDaemon(pr store.PR, end func(h *actHarness)) {
	step := 0
	var run store.Run
	h.onSleep = func(h *actHarness) {
		step++
		switch step {
		case 1:
			h.completePending(store.RequestDone, fixAnswer)
			h.setFixRow(pr, engine.FixPhaseFixing, 1)
			run = h.fixRunOf(pr, 1, store.RunWorking)
		case 3:
			if err := h.st.TransitionRun(h.ctx, run.ID, nil, store.RunVerified, nil); err != nil {
				h.t.Errorf("verify the fix run: %v", err)
			}
			h.setFixRow(pr, engine.FixPhaseReviewing, 1)
			h.setPR(pr.ID, store.PRRereviewPending, nil)
		case 5:
			h.setPR(pr.ID, store.PRReviewing, nil)
		case 7:
			h.setPR(pr.ID, store.PRReviewed, nil)
			end(h)
		}
	}
}

// fixRunEnded records how the PR's latest fix run ended, as the settle does
// before the loop's row goes (finishFixRun): the run verified, with outcome
// "pushed", "replied" or "needs_you". It returns the run's id.
func (h *actHarness) fixRunEnded(pr store.PR, outcome string) string {
	h.t.Helper()
	runs, err := h.st.RunsByPR(h.ctx, pr.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	var id string
	for _, r := range runs {
		if r.Kind == store.RunFix {
			id = r.ID
		}
	}
	if id == "" {
		h.t.Fatal("the PR has no fix run")
	}
	if err := h.st.UpdateRun(h.ctx, id, func(u *store.RunUpdate) { u.Set("outcome", outcome) }); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// fixLoopGone is the loop's row going away, which is its end.
func (h *actHarness) fixLoopGone(pr store.PR) {
	h.t.Helper()
	if err := h.st.DeleteKV(h.ctx, engine.KVPRFix(pr.ID)); err != nil {
		h.t.Fatal(err)
	}
}

// fix asks y/N on a terminal before it hands the daemon the request (only y
// confirms), names the PR, and queues a request of kind fix for it; --yes
// and a script do not ask, and --json on a terminal needs --yes.
func TestFixAsksAndQueuesTheRequest(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, fixAnswer) }

	for _, in := range []string{"n\n", "\n", ""} {
		h.stdin(in)
		h.out.Reset()
		h.errb.Reset()
		if code := h.cmd("fix", "talkable#5"); code != 1 {
			t.Fatalf("exit %d after %q: %s", code, in, h.errb.String())
		}
		actContains(t, h.out.String(), "Fix the findings of talkable#5 as you? [y/N]")
		actContains(t, h.errb.String(), "nothing queued")
		if reqs := h.requests(); len(reqs) != 0 {
			t.Fatalf("queued without a y: %+v", reqs)
		}
	}

	h.stdin("y\n")
	h.out.Reset()
	if code := h.cmd("fix", "5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqFix {
		t.Fatalf("requests = %+v", reqs)
	}
	p := actDecode[engine.FixPayload](t, reqs[0].Payload)
	if p.Repo != "talkable/talkable" || p.Number != 5 || p.DryRun || p.Off || p.On {
		t.Fatalf("payload = %+v", p)
	}
	if h.kicks != 1 {
		t.Errorf("kicks = %d", h.kicks)
	}
	// The answer is the daemon's, as it came: it carries the refusals and the notes.
	actContains(t, h.out.String(), "talkable#5  Fix coupon export (@alice)", fixAnswer)

	// --yes skips the question on a terminal; so does a script.
	h.stdin("")
	h.out.Reset()
	if code := h.cmd("fix", "5", "--yes"); code != 0 || strings.Contains(h.out.String(), "[y/N]") {
		t.Fatalf("--yes: exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	h.d.StdinTTY = false
	if code := h.cmd("fix", "5"); code != 0 || strings.Contains(h.out.String(), "[y/N]") {
		t.Fatalf("script: exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	if n := len(h.requests()); n != 3 {
		t.Fatalf("requests = %d, want 3", n)
	}
}

// --json prints the request's outcome as one JSON document; on a terminal
// the question would land in it, so --json there needs --yes and asks and
// queues nothing without it.
func TestFixJSONOnATerminalNeedsYes(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, fixAnswer) }
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.stdin("y\n")
	if code := h.cmd("fix", "5", "--json"); code != 2 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "--json needs --yes to apply", "nothing queued", "usage: magnum fix ")
	if reqs := h.requests(); len(reqs) != 0 || h.out.Len() != 0 {
		t.Fatalf("queued %+v, out %s", reqs, h.out.String())
	}

	if code := h.cmd("fix", "5", "--json", "--yes"); code != 0 {
		t.Fatalf("--json --yes: exit %d: %s", code, h.errb.String())
	}
	var doc fixJSON
	if err := json.Unmarshal(h.out.Bytes(), &doc); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if doc.PR != "talkable#5" || doc.Request == nil || doc.Request.Kind != engine.ReqFix || doc.Request.Result != fixAnswer || doc.Error != "" {
		t.Fatalf("json = %+v", doc)
	}
}

// The daemon's refusal is the command's: its sentence on stderr, exit 1,
// and as JSON the error field.
func TestFixPrintsTheDaemonsRefusal(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestFailed, "nothing to fix: no review on abc1234") }
	if code := h.cmd("fix", "5"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "magnum fix: nothing to fix: no review on abc1234")
	h.out.Reset()
	if code := h.cmd("fix", "5", "--json"); code != 1 {
		t.Fatalf("--json exit %d", code)
	}
	var doc fixJSON
	if err := json.Unmarshal(h.out.Bytes(), &doc); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if doc.Error != "nothing to fix: no review on abc1234" || doc.Request == nil || doc.Request.State != store.RequestFailed {
		t.Fatalf("json = %+v", doc)
	}
}

// A loop that starts must not start later, when nobody waits for it: with no
// daemon nothing is queued.
func TestFixWithoutADaemonQueuesNothing(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	for _, args := range [][]string{{"5"}, {"5", "--off"}, {"5", "--on", "--yes"}} {
		h.errb.Reset()
		if code := h.cmd("fix", args...); code != 1 {
			t.Fatalf("fix %v: exit %d", args, code)
		}
		actContains(t, h.errb.String(), "talkable#5: nothing was queued: no daemon is running")
	}
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("queued %+v", reqs)
	}
}

// A review agent's shell cannot start a fix for the operator: the fixer
// pushes and replies as them.
func TestFixRefusesInAnAgentPane(t *testing.T) {
	for _, env := range []string{"MAGNUM_REPORT_DIR", "MAGNUM_PR_URL"} {
		t.Run(env, func(t *testing.T) {
			storetest.Serial(t)
			t.Setenv(env, "/x/reviews/talkable/talkable/5")
			h := newActHarness(t)
			h.pid = 4242
			h.seedPR("talkable/talkable", 5, store.PRReviewed)
			for _, args := range [][]string{{"5", "--yes"}, {"5", "--off", "--yes"}, {"5", "--dry-run"}} {
				h.errb.Reset()
				if code := h.cmd("fix", args...); code != 1 {
					t.Errorf("fix %v: exit %d, want 1: %s", args, code, h.errb.String())
				}
				actContains(t, h.errb.String(), "magnum fix: "+operatorOnlyRefusal)
			}
			if reqs := h.requests(); len(reqs) != 0 {
				t.Fatalf("queued %+v", reqs)
			}
		})
	}
}

// --dry-run prints the request and sends it with dry_run, so the daemon says
// what would happen; it asks nothing, even on a terminal, and changes nothing.
func TestFixDryRunPrintsTheRequest(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.stdin("") // a question would read nothing and decline
	dry := "dry run: would start fixing talkable/talkable#5: claude-fix fixes the 2 findings of the review on abc1234 in review1, " +
		"cycle 1 of 3, then a forced re-review of what it pushes"
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, dry) }

	if code := h.cmd("fix", "5", "--dry-run"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqFix {
		t.Fatalf("requests = %+v", reqs)
	}
	if p := actDecode[engine.FixPayload](t, reqs[0].Payload); p.Repo != "talkable/talkable" || p.Number != 5 || !p.DryRun || p.Off || p.On {
		t.Fatalf("payload = %+v", p)
	}
	out := h.out.String()
	actContains(t, out, `dry run: the daemon is asked what would happen (request fix {"repo":"talkable/talkable","number":5,"dry_run":true})`, dry)
	if strings.Contains(out, "[y/N]") {
		t.Errorf("a dry run asked:\n%s", out)
	}

	h.out.Reset()
	if code := h.cmd("fix", "5", "--dry-run", "--json"); code != 0 {
		t.Fatalf("--json exit %d: %s", code, h.errb.String())
	}
	var doc fixJSON
	if err := json.Unmarshal(h.out.Bytes(), &doc); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if !doc.DryRun || doc.Payload == nil || !doc.Payload.DryRun || doc.Request == nil || doc.Request.Result != dry {
		t.Fatalf("json = %+v", doc)
	}
}

// --off and --on ask their own y/N, send the payload's Off or On, and exclude
// --wait (they start no loop to follow) and each other.
func TestFixOffAndOnQueueTheirRequests(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestDone, "fix is off for PR #5: autofix leaves it to you; `magnum fix --on` lifts it")
	}

	h.stdin("n\n")
	if code := h.cmd("fix", "5", "--off"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "Leave the findings of talkable#5 to you (no autofix)? [y/N]")
	actContains(t, h.errb.String(), "nothing changed")
	h.stdin("\n")
	h.out.Reset()
	if code := h.cmd("fix", "5", "--on"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "Autofix talkable#5 again? [y/N]")
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("queued without a y: %+v", reqs)
	}

	h.stdin("y\ny\n")
	h.out.Reset()
	if code := h.cmd("fix", "5", "--off"); code != 0 {
		t.Fatalf("--off exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "fix is off for PR #5")
	if code := h.cmd("fix", "5", "--on"); code != 0 {
		t.Fatalf("--on exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %+v", reqs)
	}
	off, on := actDecode[engine.FixPayload](t, reqs[0].Payload), actDecode[engine.FixPayload](t, reqs[1].Payload)
	if !off.Off || off.On || off.Number != 5 || on.Off || !on.On || on.Number != 5 {
		t.Fatalf("payloads %+v / %+v", off, on)
	}

	// --yes and a dry run are as for the loop; --off with --dry-run asks nothing.
	h.stdin("")
	h.d.StdinTTY = false
	if code := h.cmd("fix", "5", "--off", "--dry-run"); code != 0 {
		t.Fatalf("--off --dry-run exit %d: %s", code, h.errb.String())
	}
	if p := actDecode[engine.FixPayload](t, h.requests()[2].Payload); !p.Off || !p.DryRun {
		t.Fatalf("payload %+v", p)
	}

	n := len(h.requests())
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"5", "--off", "--wait"}, "--off and --on start no loop, so there is nothing for --wait to follow"},
		{[]string{"5", "--on", "--wait"}, "--off and --on start no loop, so there is nothing for --wait to follow"},
		{[]string{"5", "--off", "--on"}, "--off and --on exclude each other"},
	} {
		h.errb.Reset()
		if code := h.cmd("fix", tc.args...); code != 2 {
			t.Errorf("fix %v: exit %d, want 2", tc.args, code)
		}
		actContains(t, h.errb.String(), "magnum fix: "+tc.want, "usage: magnum fix ")
	}
	if got := len(h.requests()); got != n {
		t.Errorf("a usage error queued %d requests", got-n)
	}
}

// A PR argument is required, and only one.
func TestFixNeedsOnePR(t *testing.T) {
	h := newActHarness(t)
	for _, tc := range []struct {
		args []string
		want string
	}{{nil, "which PR?"}, {[]string{"5", "6"}, "one PR at a time"}} {
		h.errb.Reset()
		if code := h.cmd("fix", tc.args...); code != 2 {
			t.Errorf("fix %v: exit %d, want 2", tc.args, code)
		}
		actContains(t, h.errb.String(), "magnum fix: "+tc.want, "usage: magnum fix <")
	}
}

// --wait follows the loop the request started until its row is gone, printing
// the PR's states and the fixer's and the re-review's runs, and then says how
// the loop ended. A clean end exits 0 and counts the cycles from the event.
func TestFixWaitFollowsToTheLoopsEnd(t *testing.T) {
	h := newActHarness(t)
	h.pid = 1
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	// An earlier loop's end must not be taken for this one's.
	h.now = h.now.Add(-time.Hour)
	h.fixEvent("fix.failed", "fix run failed: an earlier loop", nil)
	h.now = h.now.Add(time.Hour)
	h.fixDaemon(pr, func(h *actHarness) {
		h.fixLoopGone(pr)
		h.fixEvent("fix.pushed", "claude-fix pushed 1 commit", map[string]any{"cycle": 1})
		h.fixEvent("fix.replied", "claude-fix replied in 2 threads", map[string]any{"cycle": 1})
		h.fixEvent("fix.done", "the fix loop ends clean after 2 fix cycles: the re-review of def5678 found nothing", map[string]any{"cycle": 2, "cap": 3})
	})
	if code := h.cmd("fix", "5", "--wait", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	actContains(t, out, fixAnswer, "state reviewed", "claude-fix fix round 1: working", "claude-fix fix round 1: verified",
		"reviewed → rereview_pending", "rereview_pending → reviewing", "reviewing → reviewed", "talkable#5: clean after 2 fix cycles")
	if strings.Contains(out, "an earlier loop") || strings.Contains(h.errb.String(), "an earlier loop") {
		t.Errorf("an earlier loop's end was read:\n%s%s", out, h.errb.String())
	}
}

// How a loop ended is read from the registry once its row is gone: the latest
// end event of the loop, with the PR's attention. Clean and nothing pushed
// exit 0; everything else exits 1 with the sentence the daemon recorded.
func TestFixWaitSaysHowTheLoopEnded(t *testing.T) {
	report := "/state/reviews/talkable/talkable/5/abc1234/claude-fix.md"
	needsAttention := func(h *actHarness, pr store.PR, msg string) {
		h.setPR(pr.ID, store.PRNeedsAttention, func(u *store.PRUpdate) { u.Set("last_error", msg) })
		if err := h.st.SetKV(h.ctx, engine.KVPRAttention(pr.ID), "fix"); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		end  func(h *actHarness, pr store.PR)
		code int
		out  []string // on stdout
		err  []string // on stderr
		// stale seeds the end event of an earlier loop, an hour before the request.
		stale bool
	}{
		{name: "clean", code: 0, out: []string{"talkable#5: clean after 3 fix cycles"}, end: func(h *actHarness, pr store.PR) {
			h.fixEvent("fix.done", "the fix loop ends clean after 3 fix cycles: the re-review of def5678 found nothing", map[string]any{"cycle": 3, "cap": 3})
		}},
		{name: "a clean end whose event carries no cycle", code: 0, out: []string{"talkable#5: clean"}, end: func(h *actHarness, pr store.PR) {
			h.fixEvent("fix.done", "the fix loop ends clean", nil)
		}},
		{name: "pushed nothing", code: 0, out: []string{"talkable#5: pushed nothing: claude-fix replied in 2 threads"}, end: func(h *actHarness, pr store.PR) {
			run := h.fixRunEnded(pr, "replied")
			h.fixEvent("fix.replied", "claude-fix replied in 2 threads", map[string]any{"cycle": 1, "run": run})
		}},
		// settle records fix.replied, then the run's outcome, then the row goes, then fix.needs_you:
		// a loop whose fixer left findings to the author is no success while that last event is missing.
		{name: "replies and findings left to the author, the needs-you event not written yet", code: 1,
			err: []string{"talkable#5: the fix loop is gone, but magnum recorded no end for it"}, end: func(h *actHarness, pr store.PR) {
				run := h.fixRunEnded(pr, "needs_you")
				h.fixEvent("fix.replied", "claude-fix replied in 2 threads", map[string]any{"cycle": 1, "run": run})
			}},
		{name: "replies of a run that recorded no outcome are no success", code: 1,
			err: []string{"talkable#5: the fix loop is gone, but magnum recorded no end for it"}, end: func(h *actHarness, pr store.PR) {
				run := h.fixRunEnded(pr, "")
				h.fixEvent("fix.replied", "claude-fix replied in 2 threads", map[string]any{"cycle": 1, "run": run})
			}},
		{name: "replies that name no run are no success", code: 1,
			err: []string{"talkable#5: the fix loop is gone, but magnum recorded no end for it"}, end: func(h *actHarness, pr store.PR) {
				h.fixRunEnded(pr, "replied")
				h.fixEvent("fix.replied", "claude-fix replied in 2 threads", map[string]any{"cycle": 1})
			}},
		{name: "the fixer left findings to the author", code: 1,
			err: []string{"talkable#5: needs you: 2 findings need your call (2101 P1 app/models/offer.rb:42): " + report, "fix: read claude-fix's report " + report}, end: func(h *actHarness, pr store.PR) {
				msg := "fix: 2 findings need your call (2101 P1 app/models/offer.rb:42): " + report
				h.fixEvent("fix.needs_you", msg, map[string]any{"cycle": 1})
				needsAttention(h, pr, msg)
			}},
		{name: "the cap", code: 1, err: []string{"talkable#5: needs you: 3 fix cycles, 2 findings left: " + report}, end: func(h *actHarness, pr store.PR) {
			msg := "3 fix cycles, 2 findings left: " + report
			h.fixEvent("fix.capped", msg, map[string]any{"cycle": 3})
			needsAttention(h, pr, msg)
		}},
		{name: "a failed run", code: 1, err: []string{"talkable#5: ended: fix run failed: claude-fix wrote no report at " + report}, end: func(h *actHarness, pr store.PR) {
			msg := "fix run failed: claude-fix wrote no report at " + report
			h.fixEvent("fix.failed", msg, map[string]any{"cycle": 1})
			needsAttention(h, pr, msg)
		}},
		{name: "a re-review that did not post", code: 1, err: []string{"talkable#5: ended: the re-review did not post (error)"}, end: func(h *actHarness, pr store.PR) {
			h.fixEvent("fix.failed", "fix loop ended: the re-review did not post (error)", map[string]any{"cycle": 1})
		}},
		{name: "a PR that closed", code: 1, err: []string{"talkable#5: ended: the PR is closed on GitHub"}, end: func(h *actHarness, pr store.PR) {
			h.fixEvent("fix.ended", "the fix loop ends: the PR is closed on GitHub", map[string]any{"cycle": 1})
		}},
		{name: "an abort", code: 1, err: []string{"talkable#5: aborted"}, end: func(h *actHarness, pr store.PR) {
			h.fixEvent("fix.stopped", "fix loop ended (cycle 1 of 3, reviewing) by magnum abort", map[string]any{"by": "magnum abort"})
		}},
		{name: "a pushed cycle's replies are not an end", code: 1,
			err: []string{"talkable#5: the fix loop is gone, but magnum recorded no end for it (`magnum logs pr:talkable#5` has its events)"},
			end: func(h *actHarness, pr store.PR) {
				h.fixEvent("fix.pushed", "claude-fix pushed 1 commit", map[string]any{"cycle": 1})
				h.fixEvent("fix.replied", "claude-fix replied in 2 threads", map[string]any{"cycle": 1})
			}},
		{name: "no event at all", code: 1,
			err: []string{"talkable#5: the fix loop is gone, but magnum recorded no end for it"}, end: func(h *actHarness, pr store.PR) {}},
		{name: "an earlier loop's end is not this loop's", code: 1, stale: true,
			err: []string{"talkable#5: the fix loop is gone, but magnum recorded no end for it"}, end: func(h *actHarness, pr store.PR) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newActHarness(t)
			h.pid = 1
			pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
			if tc.stale {
				h.now = h.now.Add(-time.Hour)
				h.fixEvent("fix.done", "the fix loop ends clean after 1 fix cycle", map[string]any{"cycle": 1})
				h.now = h.now.Add(time.Hour)
			}
			h.fixDaemon(pr, func(h *actHarness) {
				h.fixLoopGone(pr)
				tc.end(h, pr)
			})
			code := h.cmd("fix", "5", "--wait", "--yes")
			if code != tc.code {
				t.Fatalf("exit %d, want %d: %s%s", code, tc.code, h.out.String(), h.errb.String())
			}
			actContains(t, h.out.String(), tc.out...)
			actContains(t, h.errb.String(), tc.err...)
		})
	}
}

// The fix job's abort while the fixer works (stopFix) records pr.aborted, or
// pr.ignored for an ignore, with fix: true, and no fix.stopped (that one is
// the reviewing phase's): both are the loop's abort. The abort of a review
// round is no end of the loop.
func TestFixWaitSeesAnAbortWhileTheFixerWorks(t *testing.T) {
	for _, tc := range []struct {
		name, kind, msg string
		data            map[string]any
		code            int
		kindOut, text   string
	}{
		{name: "abort", kind: "pr.aborted", msg: "aborted the fix of talkable/talkable#5: fixer interrupted, slot review1 back at abc1234",
			data: map[string]any{"fix": true, "cycle": 1}, code: 1, kindOut: "aborted", text: "aborted"},
		{name: "ignore", kind: "pr.ignored", msg: "ignored the fix of talkable/talkable#5: fixer interrupted; muted until `magnum unmute talkable#5`",
			data: map[string]any{"fix": true, "cycle": 1}, code: 1, kindOut: "aborted", text: "aborted: the PR was ignored"},
		{name: "the abort of a review round", kind: "pr.aborted", msg: "aborted talkable/talkable#5: round abandoned",
			data: map[string]any{"queued": true}, code: 1, kindOut: "gone", text: "the fix loop is gone, but magnum recorded no end for it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newActHarness(t)
			h.pid = 1
			pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
			step := 0
			h.onSleep = func(h *actHarness) {
				step++
				switch step {
				case 1:
					h.completePending(store.RequestDone, fixAnswer)
					h.setFixRow(pr, engine.FixPhaseFixing, 1)
					h.fixRunOf(pr, 1, store.RunWorking)
				case 3: // the fixer still works when the abort ends the loop
					h.fixLoopGone(pr)
					h.fixEvent(tc.kind, tc.msg, tc.data)
				}
			}
			if code := h.cmd("fix", "5", "--wait", "--yes", "--json"); code != tc.code {
				t.Fatalf("exit %d, want %d: %s%s", code, tc.code, h.out.String(), h.errb.String())
			}
			var doc fixJSON
			if err := json.Unmarshal(h.out.Bytes(), &doc); err != nil {
				t.Fatalf("%v\n%s", err, h.out.String())
			}
			if doc.End == nil || doc.End.Kind != tc.kindOut || !strings.Contains(doc.End.Text, tc.text) || doc.Error == "" {
				t.Fatalf("json = %+v end %+v", doc, doc.End)
			}
		})
	}
}

// A fixer that left findings to the author without a push is no success while
// its needs-you event is still to come: the follower waits for it, and the end
// it reports is the needs-you one.
func TestFixWaitDoesNotCallUnpushedRepliesASuccessWhenFindingsAreLeftToYou(t *testing.T) {
	h := newActHarness(t)
	h.pid = 1
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	msg := "fix: 2 findings need your call (2101 P1 app/models/offer.rb:42): /state/claude-fix.md"
	late := false
	h.fixDaemon(pr, func(h *actHarness) {
		h.fixLoopGone(pr)
		run := h.fixRunEnded(pr, "needs_you")
		h.fixEvent("fix.replied", "claude-fix replied in 2 threads", map[string]any{"cycle": 1, "run": run})
		late = true
	})
	daemon := h.onSleep
	h.onSleep = func(h *actHarness) {
		daemon(h)
		if late && !h.hasEvent("fix.needs_you") && h.sleeps > 8 { // a poll later
			h.fixEvent("fix.needs_you", msg, map[string]any{"cycle": 1})
		}
	}
	if code := h.cmd("fix", "5", "--wait", "--yes"); code != 1 {
		t.Fatalf("exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	actContains(t, h.errb.String(), "talkable#5: needs you: 2 findings need your call")
	if strings.Contains(h.out.String(), "pushed nothing") {
		t.Errorf("a loop that needs you was called a success:\n%s", h.out.String())
	}
}

// The row goes before the event that says why, so a follower that finds the
// row gone and no end yet looks again before it calls the end unrecorded.
func TestFixWaitGivesTheDaemonAMomentToRecordTheEnd(t *testing.T) {
	h := newActHarness(t)
	h.pid = 1
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	gone, recorded := false, false
	h.fixDaemon(pr, func(h *actHarness) {
		h.fixLoopGone(pr)
		gone = true
	})
	daemon := h.onSleep
	h.onSleep = func(h *actHarness) {
		daemon(h)
		if gone && !recorded {
			recorded = true
			return // the poll that found the row gone saw no event yet
		}
		if recorded && !h.hasEvent("fix.done") {
			h.fixEvent("fix.done", "the fix loop ends clean after 1 fix cycle", map[string]any{"cycle": 1})
		}
	}
	if code := h.cmd("fix", "5", "--wait", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	actContains(t, h.out.String(), "talkable#5: clean after 1 fix cycle")
}

// hasEvent reports whether the loop's PR has an event of kind.
func (h *actHarness) hasEvent(kind string) bool {
	h.t.Helper()
	evs, err := h.st.EventsBySubject(h.ctx, "pr:talkable/talkable#5", 0)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Kind == kind {
			return true
		}
	}
	return false
}

// The loop is over when its row is gone, not when the PR is reviewed again:
// the forced re-review ends with the row still there, so the follower goes on
// until --timeout, then says the loop goes on and where to look.
func TestFixWaitStopsFollowingAfterTheTimeout(t *testing.T) {
	h := newActHarness(t)
	h.pid = 1
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.fixDaemon(pr, func(h *actHarness) {}) // the re-review ends, the row stays
	code := h.cmd("fix", "5", "--wait", "--yes", "--timeout", "1m")
	if code != 1 {
		t.Fatalf("exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	actContains(t, h.errb.String(), "stopped following after 1m0s; the fix loop goes on (`magnum status talkable#5` shows it, `magnum abort talkable#5` stops it)")
	if _, ok, _ := h.st.GetKV(h.ctx, engine.KVPRFix(pr.ID)); !ok {
		t.Error("the loop's row was removed")
	}
}

// --wait on a request the daemon refuses ends with its sentence at once.
func TestFixWaitOnARefusedRequestEndsAtOnce(t *testing.T) {
	h := newActHarness(t)
	h.pid = 1
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestFailed, "PR #5 is alice's, not yours; the fixer pushes and replies as you")
	}
	if code := h.cmd("fix", "5", "--wait", "--yes"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "magnum fix: PR #5 is alice's, not yours")
	if h.sleeps > 3 {
		t.Errorf("slept %d times: it followed a refused request", h.sleeps)
	}
}

// --wait --json prints one JSON document at the end, with how the loop ended;
// the progress goes to stderr.
func TestFixWaitJSONCarriesTheEnding(t *testing.T) {
	h := newActHarness(t)
	h.pid = 1
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.fixDaemon(pr, func(h *actHarness) {
		h.fixLoopGone(pr)
		h.fixEvent("fix.done", "the fix loop ends clean after 2 fix cycles", map[string]any{"cycle": 2})
	})
	if code := h.cmd("fix", "5", "--wait", "--yes", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	var doc fixJSON
	if err := json.Unmarshal(h.out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, h.out.String())
	}
	if doc.End == nil || doc.End.Kind != "clean" || doc.End.Text != "clean after 2 fix cycles" || doc.State != store.PRReviewed || doc.Error != "" {
		t.Fatalf("json = %+v end %+v", doc, doc.End)
	}
	actContains(t, h.errb.String(), "claude-fix fix round 1: working")
	if strings.Contains(h.out.String(), "claude-fix fix round") {
		t.Errorf("progress on stdout:\n%s", h.out.String())
	}
}

// A dry run starts no loop, so --wait has nothing to follow.
func TestFixDryRunDoesNotFollow(t *testing.T) {
	h := newActHarness(t)
	h.pid = 1
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, "dry run: would start fixing") }
	if code := h.cmd("fix", "5", "--dry-run", "--wait"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if h.sleeps > 3 {
		t.Errorf("slept %d times: a dry run was followed", h.sleeps)
	}
}

// fixOpts has the flags the screens set.
func TestFixMainTakesTheScreensOptions(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.stdin("")
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, fixAnswer) }
	for i, o := range []fixOpts{{yes: true}, {yes: true, off: true}, {yes: true, on: true}} {
		if code := fixMain(h.ctx, h.c, h.d, "talkable#5", o); code != 0 {
			t.Fatalf("%+v: exit %d: %s", o, code, h.errb.String())
		}
		p := actDecode[engine.FixPayload](t, h.requests()[i].Payload)
		if p.Off != o.off || p.On != o.on || p.Number != 5 {
			t.Errorf("%+v: payload %+v", o, p)
		}
	}
}

// The board's f asks the daemon for a fix loop as the command does, without
// asking again and without waiting for the loop: the answer and the queued
// request go back to the screen, and F sends the opt-out or its undoing, the
// daemon's refusal coming back as the error, without the command's prefix.
func TestScreenFixAndFixOff(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, fixAnswer) }
	acts := newScreenActions(h.c)
	defer acts.Close()
	ctx := context.Background()

	res, err := acts.Fix(ctx, "talkable#5")
	if err != nil {
		t.Fatalf("fix: %v (%s)", err, h.errb.String())
	}
	if !strings.Contains(res.Text, fixAnswer) || len(res.Requests) != 1 || res.Requests[0].Kind != "fix" {
		t.Errorf("fix returned %q with requests %+v, want the daemon's answer and the fix request", res.Text, res.Requests)
	}
	if _, err := acts.FixOff(ctx, "talkable#5", true); err != nil {
		t.Fatalf("fix off: %v", err)
	}
	if _, err := acts.FixOff(ctx, "talkable#5", false); err != nil {
		t.Fatalf("fix on: %v", err)
	}
	reqs := h.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests queued, want 3: %+v", len(reqs), reqs)
	}
	for i, want := range []engine.FixPayload{{}, {Off: true}, {On: true}} {
		p := actDecode[engine.FixPayload](t, reqs[i].Payload)
		if reqs[i].Kind != engine.ReqFix || p.Off != want.Off || p.On != want.On || p.DryRun || p.Number != 5 {
			t.Errorf("request %d: %s %+v, want the fix request with off=%v on=%v", i, reqs[i].Kind, p, want.Off, want.On)
		}
	}

	h.onSleep = func(h *actHarness) { h.completePending(store.RequestFailed, "nothing to fix: no review on abc1234") }
	res, err = acts.Fix(ctx, "talkable#5")
	if err == nil || err.Error() != "nothing to fix: no review on abc1234" {
		t.Fatalf("a refused fix returned %v, want the daemon's sentence", err)
	}
	if len(res.Requests) != 1 || res.Requests[0].State != tui.RequestFailed {
		t.Errorf("a refused fix reported %+v, want the failed request", res.Requests)
	}
}
