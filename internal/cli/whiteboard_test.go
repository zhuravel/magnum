package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// whiteboardAnswer is what the daemon answers a request that starts the job.
const whiteboardAnswer = "writing the whiteboard of abc1234 in review1"

// whiteboardRunOf records the writer's run of the job.
func (h *actHarness) whiteboardRunOf(pr store.PR, round int, state string) store.Run {
	h.t.Helper()
	r, err := h.st.CreateRun(h.ctx, store.Run{PRID: pr.ID, Round: round, Role: "claude-whiteboard", Kind: store.RunWhiteboard, TargetSHA: pr.HeadSHA, State: state})
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// setWhiteboardRecord records the PR's whiteboard as the job's settle does.
func (h *actHarness) setWhiteboardRecord(pr store.PR, session, head string) {
	h.t.Helper()
	b, err := json.Marshal(engine.WhiteboardRecord{Session: session, Head: head, At: h.now, Role: "claude-whiteboard"})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, engine.KVPRWhiteboard(pr.ID), string(b)); err != nil {
		h.t.Fatal(err)
	}
}

// whiteboardDaemon plays the daemon around a job: the first poll answers the
// request and starts the writer's run, and at step 3 end plays the job's end.
func (h *actHarness) whiteboardDaemon(pr store.PR, end func(h *actHarness, run store.Run)) {
	step := 0
	var run store.Run
	h.onSleep = func(h *actHarness) {
		step++
		switch step {
		case 1:
			h.completePending(store.RequestDone, whiteboardAnswer)
			run = h.whiteboardRunOf(pr, 1, store.RunWorking)
		case 3:
			end(h, run)
		}
	}
}

// whiteboardDone records the job's end as the settle does: the run verified
// and whiteboard.done with the session.
func (h *actHarness) whiteboardDone(run store.Run, session string) {
	h.t.Helper()
	if err := h.st.TransitionRun(h.ctx, run.ID, nil, store.RunVerified, nil); err != nil {
		h.t.Fatal(err)
	}
	h.fixEvent("whiteboard.done", "claude-whiteboard wrote the whiteboard "+session+" of abc1234 in 4m12s",
		map[string]any{"run": run.ID, "role": "claude-whiteboard", "head": "abc1234def5678", "session": session, "seconds": 252})
}

// whiteboard asks y/N on a terminal before it hands the daemon the request
// (only y confirms), names the PR, and queues a request of kind whiteboard
// for it; --yes and a script do not ask.
func TestWhiteboardAsksAndQueuesTheRequest(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, whiteboardAnswer) }

	for _, in := range []string{"n\n", "\n", ""} {
		h.stdin(in)
		h.out.Reset()
		h.errb.Reset()
		if code := h.cmd("whiteboard", "talkable#5"); code != 1 {
			t.Fatalf("exit %d after %q: %s", code, in, h.errb.String())
		}
		actContains(t, h.out.String(), "Write the whiteboard of talkable#5? [y/N]")
		actContains(t, h.errb.String(), "nothing queued")
		if reqs := h.requests(); len(reqs) != 0 {
			t.Fatalf("queued without a y: %+v", reqs)
		}
	}

	h.stdin("y\n")
	h.out.Reset()
	if code := h.cmd("whiteboard", "5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqWhiteboard {
		t.Fatalf("requests = %+v", reqs)
	}
	if p := actDecode[engine.WhiteboardPayload](t, reqs[0].Payload); p.Repo != "talkable/talkable" || p.Number != 5 {
		t.Fatalf("payload = %+v", p)
	}
	if h.kicks != 1 {
		t.Errorf("kicks = %d", h.kicks)
	}
	// The answer is the daemon's, as it came.
	actContains(t, h.out.String(), "talkable#5  Fix coupon export (@alice)", whiteboardAnswer)

	// --yes skips the question on a terminal; so does a script.
	h.stdin("")
	h.out.Reset()
	if code := h.cmd("whiteboard", "5", "--yes"); code != 0 || strings.Contains(h.out.String(), "[y/N]") {
		t.Fatalf("--yes: exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	h.d.StdinTTY = false
	if code := h.cmd("whiteboard", "5"); code != 0 || strings.Contains(h.out.String(), "[y/N]") {
		t.Fatalf("script: exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	if n := len(h.requests()); n != 3 {
		t.Fatalf("requests = %d, want 3", n)
	}
}

// --json prints the request's outcome as one JSON document; on a terminal
// the question would land in it, so --json there needs --yes and asks and
// queues nothing without it.
func TestWhiteboardJSONOnATerminalNeedsYes(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, whiteboardAnswer) }
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.stdin("y\n")
	if code := h.cmd("whiteboard", "5", "--json"); code != 2 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "--json needs --yes to apply", "nothing queued", "usage: magnum whiteboard ")
	if reqs := h.requests(); len(reqs) != 0 || h.out.Len() != 0 {
		t.Fatalf("queued %+v, out %s", reqs, h.out.String())
	}

	if code := h.cmd("whiteboard", "5", "--json", "--yes"); code != 0 {
		t.Fatalf("--json --yes: exit %d: %s", code, h.errb.String())
	}
	var doc whiteboardJSON
	if err := json.Unmarshal(h.out.Bytes(), &doc); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if doc.PR != "talkable#5" || doc.URL != "https://github.com/talkable/talkable/pull/5" || doc.Request == nil ||
		doc.Request.Kind != engine.ReqWhiteboard || doc.Request.Result != whiteboardAnswer || doc.State != store.PRReviewed || doc.Error != "" {
		t.Fatalf("json = %+v", doc)
	}
}

// The daemon's refusal is the command's: its sentence on stderr, exit 1,
// and as JSON the error field.
func TestWhiteboardPrintsTheDaemonsRefusal(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestFailed, "no review on abc1234") }
	if code := h.cmd("whiteboard", "5"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "magnum whiteboard: no review on abc1234")
	h.out.Reset()
	if code := h.cmd("whiteboard", "5", "--json"); code != 1 {
		t.Fatalf("--json exit %d", code)
	}
	var doc whiteboardJSON
	if err := json.Unmarshal(h.out.Bytes(), &doc); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if doc.Error != "no review on abc1234" || doc.Request == nil || doc.Request.State != store.RequestFailed {
		t.Fatalf("json = %+v", doc)
	}
}

// A job that starts must not start later, when nobody waits for it: with no
// daemon nothing is queued.
func TestWhiteboardWithoutADaemonQueuesNothing(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	if code := h.cmd("whiteboard", "5"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "talkable#5: nothing was queued: no daemon is running")
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("queued %+v", reqs)
	}
}

// A review agent's shell cannot start the job or open the whiteboard for the
// operator: the writer works in the PR's slot, and the open takes the focus.
func TestWhiteboardRefusesInAnAgentPane(t *testing.T) {
	for _, env := range []string{"MAGNUM_REPORT_DIR", "MAGNUM_PR_URL"} {
		t.Run(env, func(t *testing.T) {
			storetest.Serial(t)
			t.Setenv(env, "/x/reviews/talkable/talkable/5")
			h := newActHarness(t)
			h.pid = 4242
			pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
			h.setWhiteboardRecord(pr, "wb-123", pr.HeadSHA)
			for _, args := range [][]string{{"5", "--yes"}, {"5", "--open"}} {
				h.errb.Reset()
				if code := h.cmd("whiteboard", args...); code != 1 {
					t.Errorf("whiteboard %v: exit %d, want 1: %s", args, code, h.errb.String())
				}
				actContains(t, h.errb.String(), "magnum whiteboard: "+operatorOnlyRefusal)
			}
			if reqs, ran := h.requests(), h.run.CallsWithPrefix("whiteboard"); len(reqs) != 0 || len(ran) != 0 {
				t.Fatalf("queued %+v, ran %+v", reqs, ran)
			}
		})
	}
}

// A PR argument is required, and only one; --open opens what magnum wrote
// and starts nothing, so --wait has nothing to follow.
func TestWhiteboardUsageErrors(t *testing.T) {
	h := newActHarness(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "which PR?"},
		{[]string{"5", "6"}, "one PR at a time"},
		{[]string{"5", "--open", "--wait"}, "--open opens the whiteboard magnum wrote and starts nothing, so there is nothing for --wait to follow"},
	} {
		h.errb.Reset()
		if code := h.cmd("whiteboard", tc.args...); code != 2 {
			t.Errorf("whiteboard %v: exit %d, want 2", tc.args, code)
		}
		actContains(t, h.errb.String(), "magnum whiteboard: "+tc.want, "usage: magnum whiteboard <")
	}
}

// --wait follows the job the request started, printing the writer's run,
// until whiteboard.done ends it: exit 0 with the event's sentence, and as
// JSON the end with the whiteboard's session and head.
func TestWhiteboardWaitFollowsToTheJobsEnd(t *testing.T) {
	h := newActHarness(t)
	h.pid = 1
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	// An earlier job's end must not be taken for this one's.
	h.now = h.now.Add(-time.Hour)
	h.fixEvent("whiteboard.failed", "whiteboard run failed: an earlier job", nil)
	h.now = h.now.Add(time.Hour)
	h.whiteboardDaemon(pr, func(h *actHarness, run store.Run) { h.whiteboardDone(run, "wb-123") })
	if code := h.cmd("whiteboard", "5", "--wait", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	out := h.out.String()
	actContains(t, out, whiteboardAnswer, "state reviewed", "claude-whiteboard whiteboard round 1: working", "claude-whiteboard whiteboard round 1: verified",
		"talkable#5: claude-whiteboard wrote the whiteboard wb-123 of abc1234 in 4m12s")
	if strings.Contains(out, "an earlier job") || strings.Contains(h.errb.String(), "an earlier job") {
		t.Errorf("an earlier job's end was read:\n%s%s", out, h.errb.String())
	}

	h.out.Reset()
	h.sleeps = 0
	h.whiteboardDaemon(pr, func(h *actHarness, run store.Run) { h.whiteboardDone(run, "wb-123") })
	if code := h.cmd("whiteboard", "5", "--wait", "--yes", "--json"); code != 0 {
		t.Fatalf("--json exit %d: %s", code, h.errb.String())
	}
	var doc whiteboardJSON
	if err := json.Unmarshal(h.out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, h.out.String())
	}
	if doc.End == nil || doc.End.Kind != whiteboardEndDone || !strings.Contains(doc.End.Text, "wrote the whiteboard wb-123") ||
		doc.Session != "wb-123" || doc.Head != "abc1234def5678" || doc.State != store.PRReviewed || doc.Error != "" {
		t.Fatalf("json = %+v end %+v", doc, doc.End)
	}
	actContains(t, h.errb.String(), "claude-whiteboard whiteboard round 1: working")
	if strings.Contains(h.out.String(), "claude-whiteboard whiteboard round") {
		t.Errorf("progress on stdout:\n%s", h.out.String())
	}
}

// A job that failed or was stopped ends --wait with the event's sentence on
// stderr and exit 1.
func TestWhiteboardWaitSaysHowTheJobEnded(t *testing.T) {
	for _, tc := range []struct {
		name, kind, msg, want string
		code                  int
	}{
		{name: "failed", kind: "whiteboard.failed", msg: "whiteboard run failed: claude-whiteboard wrote no report", code: 1,
			want: "talkable#5: whiteboard run failed: claude-whiteboard wrote no report"},
		{name: "stopped by a push", kind: "whiteboard.stopped", msg: "the whiteboard of talkable/talkable#5 stopped: the head moved to def5678: writer interrupted", code: 1,
			want: "talkable#5: the whiteboard of talkable/talkable#5 stopped: the head moved to def5678"},
		{name: "aborted", kind: "whiteboard.stopped", msg: "aborted the whiteboard of talkable/talkable#5: writer interrupted, run r1 abandoned", code: 1,
			want: "talkable#5: aborted the whiteboard of talkable/talkable#5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newActHarness(t)
			h.pid = 1
			pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
			h.whiteboardDaemon(pr, func(h *actHarness, run store.Run) {
				h.fixEvent(tc.kind, tc.msg, map[string]any{"run": run.ID, "role": "claude-whiteboard"})
			})
			if code := h.cmd("whiteboard", "5", "--wait", "--yes", "--json"); code != tc.code {
				t.Fatalf("exit %d, want %d: %s%s", code, tc.code, h.out.String(), h.errb.String())
			}
			actContains(t, h.errb.String(), tc.want)
			var doc whiteboardJSON
			if err := json.Unmarshal(h.out.Bytes(), &doc); err != nil {
				t.Fatalf("%v\n%s", err, h.out.String())
			}
			if doc.End == nil || doc.End.Kind != strings.TrimPrefix(tc.kind, "whiteboard.") || doc.Error == "" {
				t.Fatalf("json = %+v end %+v", doc, doc.End)
			}
		})
	}
}

// A job that is not over by --timeout is left running: the command says so
// and where to look, and exits 1.
func TestWhiteboardWaitStopsFollowingAfterTheTimeout(t *testing.T) {
	h := newActHarness(t)
	h.pid = 1
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.whiteboardDaemon(pr, func(*actHarness, store.Run) {}) // the writer works on
	if code := h.cmd("whiteboard", "5", "--wait", "--yes", "--timeout", "1m"); code != 1 {
		t.Fatalf("exit %d: %s%s", code, h.out.String(), h.errb.String())
	}
	actContains(t, h.errb.String(), "stopped following after 1m0s; the whiteboard job goes on (`magnum status talkable#5` shows it, `magnum abort talkable#5` stops it)")
}

// --wait on a request the daemon refuses ends with its sentence at once.
func TestWhiteboardWaitOnARefusedRequestEndsAtOnce(t *testing.T) {
	h := newActHarness(t)
	h.pid = 1
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestFailed, "Whiteboard is not running (start Whiteboard Desktop or `whiteboard server start`)")
	}
	if code := h.cmd("whiteboard", "5", "--wait", "--yes"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "magnum whiteboard: Whiteboard is not running")
	if h.sleeps > 3 {
		t.Errorf("slept %d times: it followed a refused request", h.sleeps)
	}
}

// --open asks the daemon nothing: it reads the PR's whiteboard from the
// registry and opens it in Whiteboard Desktop through the whiteboard CLI,
// without a question (as `magnum open` focuses a pane). A whiteboard of an
// earlier head opens with a note; a PR without one is refused.
func TestWhiteboardOpenRunsTheWhiteboardCLI(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.stdin("") // a question would read nothing and decline
	h.run.Rules = []execx.Rule{{Prefix: []string{"whiteboard", "app", "pick"}}}

	if code := h.cmd("whiteboard", "5", "--open"); code != 1 {
		t.Fatalf("no record: exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "magnum whiteboard: no whiteboard of talkable#5 yet")
	if ran := h.run.CallsWithPrefix("whiteboard"); len(ran) != 0 {
		t.Fatalf("ran %+v without a record", ran)
	}

	h.errb.Reset()
	h.setWhiteboardRecord(pr, "wb-123", pr.HeadSHA)
	if code := h.cmd("whiteboard", "5", "--open"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	calls := h.run.CallsWithPrefix("whiteboard")
	if len(calls) != 1 || strings.Join(calls[0].Args, " ") != "app pick --session wb-123 --focus" {
		t.Fatalf("ran %+v", calls)
	}
	actContains(t, h.out.String(), "opened the whiteboard wb-123 of talkable#5 (abc1234)")
	if strings.Contains(h.out.String(), "[y/N]") || len(h.requests()) != 0 {
		t.Fatalf("asked or queued: %s %+v", h.out.String(), h.requests())
	}

	// The whiteboard of an earlier head still opens, with a note.
	h.out.Reset()
	h.setWhiteboardRecord(pr, "wb-123", "0000000aaaaaaa")
	if code := h.cmd("whiteboard", "5", "--open"); code != 0 {
		t.Fatalf("earlier head: exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "opened the whiteboard wb-123 of talkable#5 (0000000; its head is abc1234 now)")

	// --json carries the session and the head it describes.
	h.out.Reset()
	if code := h.cmd("whiteboard", "5", "--open", "--json"); code != 0 {
		t.Fatalf("--json exit %d: %s", code, h.errb.String())
	}
	var doc whiteboardJSON
	if err := json.Unmarshal(h.out.Bytes(), &doc); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if doc.PR != "talkable#5" || doc.Session != "wb-123" || doc.Head != "0000000aaaaaaa" || doc.Request != nil || doc.Error != "" {
		t.Fatalf("json = %+v", doc)
	}

	// The CLI's failure is the command's, its stderr's first line as the reason.
	h.run.Rules = []execx.Rule{{Prefix: []string{"whiteboard", "app", "pick"},
		Err: &execx.ExitError{Code: 1, Stderr: "error: no Whiteboard server is running\nrun `whiteboard server start`"}}}
	h.errb.Reset()
	if code := h.cmd("whiteboard", "5", "--open"); code != 1 {
		t.Fatalf("CLI failure: exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.errb.String(), "magnum whiteboard: could not open the whiteboard wb-123: error: no Whiteboard server is running")
}

// whiteboardOpts has the flags the screens set: a request without a
// question, or the open.
func TestWhiteboardMainTakesTheScreensOptions(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.stdin("")
	h.onSleep = func(h *actHarness) { h.completePending(store.RequestDone, whiteboardAnswer) }
	if code := whiteboardMain(h.ctx, h.c, h.d, "talkable#5", whiteboardOpts{yes: true}); code != 0 {
		t.Fatalf("request: exit %d: %s", code, h.errb.String())
	}
	if reqs := h.requests(); len(reqs) != 1 || reqs[0].Kind != engine.ReqWhiteboard {
		t.Fatalf("requests = %+v", reqs)
	}
	h.setWhiteboardRecord(pr, "wb-123", pr.HeadSHA)
	h.run.Rules = []execx.Rule{{Prefix: []string{"whiteboard", "app", "pick"}}}
	if code := whiteboardMain(h.ctx, h.c, h.d, "talkable#5", whiteboardOpts{open: true}); code != 0 {
		t.Fatalf("open: exit %d: %s", code, h.errb.String())
	}
	if calls := h.run.CallsWithPrefix("whiteboard", "app", "pick"); len(calls) != 1 || len(h.requests()) != 1 {
		t.Fatalf("open ran %+v and left %d requests", calls, len(h.requests()))
	}
}

// The ending of a job is read from the PR's events at or after the request:
// the latest of the job's own end events; events of other kinds are no end.
func TestWhiteboardEndOfReadsTheJobsEndEvents(t *testing.T) {
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	t0 := h.now
	ctx := context.Background()
	target, err := h.d.resolveRef(ctx, "5")
	if err != nil {
		t.Fatal(err)
	}
	h.fixEvent("whiteboard.start", "claude-whiteboard writes the whiteboard of abc1234 in review1 (run r1)", nil)
	h.fixEvent("fix.done", "the fix loop ends clean", nil)
	if end, ok := whiteboardEndOf(ctx, h.d, target, t0); ok {
		t.Fatalf("an end without one: %+v", end)
	}
	h.fixEvent("whiteboard.done", "claude-whiteboard wrote the whiteboard wb-1 of abc1234 in 3m", map[string]any{"session": "wb-1", "head": pr.HeadSHA})
	end, ok := whiteboardEndOf(ctx, h.d, target, t0)
	if !ok || end.Kind != whiteboardEndDone || end.Session != "wb-1" || end.Head != pr.HeadSHA {
		t.Fatalf("end = %+v, %v", end, ok)
	}
	h.now = h.now.Add(time.Minute)
	h.fixEvent("whiteboard.failed", "whiteboard run failed: x", nil)
	if end, ok := whiteboardEndOf(ctx, h.d, target, t0); !ok || end.Kind != whiteboardEndFailed || end.code() != 1 {
		t.Fatalf("the latest end = %+v, %v", end, ok)
	}
	if _, ok := whiteboardEndOf(ctx, h.d, target, h.now.Add(time.Hour)); ok {
		t.Fatal("an end before the start counted")
	}
}
