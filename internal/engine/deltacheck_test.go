package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// withoutDeltaCheck turns [daemon] delta_check off: a small delta waits for
// the threshold and a full round, as before delta checks.
func withoutDeltaCheck(h *harness) { h.cfg.Daemon.DeltaCheck = false }

// liveDelta is the live push: two GIFs swapped (GitHub sends no patch for
// them) and 4 changed lines in two mail templates.
var liveDelta = []github.FileDelta{
	{Path: "app/assets/images/mailer/hero.gif", Status: "modified", Truncated: true},
	{Path: "app/assets/images/mailer/badge.gif", Status: "modified", Truncated: true},
	{Path: "app/views/mailer/welcome.html.erb", Status: "modified",
		Patch: "@@ -3,3 +3,3 @@\n <table>\n-  <td>Welcome!</td>\n+  <td>Welcome back!</td>\n </table>"},
	{Path: "app/views/mailer/reminder.html.erb", Status: "modified",
		Patch: "@@ -8,3 +8,3 @@\n <p>\n-  <%= link_to \"Open\", url %>\n+  <%= link_to \"Open now\", url %>\n </p>"},
}

// verdictRounds makes round n post verdicts[n-1] (the last one for later
// rounds) as review 700+n; "timeout" and "needs_attention" end the round
// with that outcome instead. inFlight records, for each round, the delta
// check KVPRDeltaCheck says it runs ("" = none).
func verdictRounds(h *harness, verdicts ...string) *[]string {
	inFlight := &[]string{}
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		n := len(h.rd.all())
		v, _ := h.e.getKV(h.ctx, KVPRDeltaCheck(in.PR.ID))
		h.rd.mu.Lock()
		*inFlight = append(*inFlight, v)
		h.rd.mu.Unlock()
		switch verdict := verdicts[min(n, len(verdicts))-1]; verdict {
		case pipeline.OutcomeTimeout, pipeline.OutcomeNeedsAttention:
			return pipeline.RoundResult{Outcome: verdict, Round: n, Error: "the judge did not finish"}, nil
		default:
			res, err := h.rd.posted(h.ctx, in, n)
			res.Event, res.ReviewID = verdict, int64(700+n)
			return res, err
		}
	}
	return inFlight
}

// deltaCheckHarness is the App harness with [triage] on (its model keeps
// every role) and the round's diff for it.
func deltaCheckHarness(t *testing.T, mods ...func(*harness)) (*harness, *approvalGH, *execx.Fake) {
	t.Helper()
	model := &execx.Fake{Rules: []execx.Rule{modelAnswers(`{"run": ["codex-judge", "claude-review", "codex-review", "claude-simplify"]}`)}}
	on := func(h *harness) {
		h.cfg.Triage.Enabled = true
		h.d.Runner = model
	}
	h, app := newApprovalHarness(t, append([]func(*harness){on}, mods...)...)
	reqSetFiles(h, "master...b1", codePatch(5))
	return h, app, model
}

// keptForCheck runs the live sequence up to the kept approval: round 1
// approves b1 while the commit b2 (files) arrives during its judge's turn,
// and the next poll sees the head. It returns the notes the round added.
func keptForCheck(t *testing.T, h *harness, files []github.FileDelta) []string {
	t.Helper()
	notes := pushDuringReview(t, h, files, nil) // b2 pushed at 10:06
	pollPR(h, 30*time.Second, 2, "b2")
	return notes
}

func dismissCalls(h *harness) []string { return callsWith(h.gh, "dismiss:") }

// The live case: the approval of b1 stands, and after the quiet period
// (not rereview_max_wait) only the judge checks the 4 lines and two images,
// without triage; its approval supersedes the first one.
func TestSmallDeltaGetsAJudgeOnlyDeltaCheckAndKeepsTheApproval(t *testing.T) {
	h, app, model := deltaCheckHarness(t)
	inFlight := verdictRounds(h, "APPROVED")
	notes := keptForCheck(t, h, liveDelta)
	pushedAt := h.pr(2).HeadChangedAt

	if want := "701:_Reviewed b1; 1 commit arrived during the review, a short check of that commit follows after the quiet period._"; !slices.Equal(notes, []string{want}) {
		t.Fatalf("notes = %q, want %q", notes, want)
	}
	pr := h.wantState(2, store.PRRereviewPending)
	rec, _ := h.e.deltaRecord(h.ctx, pr.ID)
	if rec.Lines != 4 || rec.Complete || !slices.Equal(rec.Binaries, []string{liveDelta[0].Path, liveDelta[1].Path}) || rec.Unread != 0 {
		t.Fatalf("delta record = %+v, want 4 lines and the two GIFs", rec)
	}
	// The approval stands, without even a compare by the App.
	if got := dismissCalls(h); len(got) != 0 || deref(pr.LastReviewEvent) != "APPROVED" || len(app.comparesMade()) != 0 {
		t.Fatalf("dismissals %q, event %q, App compares %q", got, deref(pr.LastReviewEvent), app.comparesMade())
	}
	kept := approvalEvents(t, h, 2, "review.approval_kept_for_check")
	if len(kept) != 1 || !strings.Contains(kept[0].Message, "approval 701 by talkable[bot] on b1 stands for a delta check of 4 lines up to b2") {
		t.Fatalf("review.approval_kept_for_check events: %+v", kept)
	}
	w := waitOf(t, h, 2)
	quietEnd := pushedAt.Add(5 * time.Minute)
	if !w.DeltaCheck || w.Reason != WaitQuiet || !w.Until.Equal(quietEnd) {
		t.Fatalf("wait = %+v, want the delta check after the quiet period", w)
	}
	if got, want := w.Short(h.clock.Now()), "delta check · quiet → "+clockText(quietEnd); got != want {
		t.Errorf("short = %q, want %q", got, want)
	}
	if got := w.Sentence("talkable#2", h.clock.Now()); !strings.HasPrefix(got, "delta check waits for the push quiet period (5m) until ") {
		t.Errorf("sentence = %q", got)
	}

	h.advance(quietEnd.Sub(h.clock.Now()) - time.Second)
	h.tick()
	reqWantRounds(t, h, 1)
	pollPR(h, time.Second, 2, "b2") // the quiet period ends: the delta check runs
	ins := h.rd.all()
	if len(ins) != 2 {
		t.Fatalf("rounds = %d, want the delta check", len(ins))
	}
	in := ins[1]
	if in.Kind != pipeline.KindRereview || in.TargetSHA != "b2" || !slices.Equal(roleNames(in.Roles), []string{"codex-judge"}) || in.MaxRestarts != 0 {
		t.Fatalf("delta check input: kind %s target %s roles %v restarts %d", in.Kind, in.TargetSHA, roleNames(in.Roles), in.MaxRestarts)
	}
	want := &pipeline.DeltaCheck{Lines: 4, Files: []pipeline.DeltaFile{
		{Path: liveDelta[0].Path, Status: "modified", Binary: true}, {Path: liveDelta[1].Path, Status: "modified", Binary: true},
		{Path: liveDelta[2].Path, Status: "modified"}, {Path: liveDelta[3].Path, Status: "modified"},
	}}
	if got, _ := json.Marshal(in.DeltaCheck); string(got) != mustJSON(t, want) {
		t.Fatalf("delta check = %s, want %s", got, mustJSON(t, want))
	}
	if got := h.workspaceRoles(pr.ID); !slices.Equal(got, []string{"codex-judge"}) {
		t.Fatalf("workspace roles = %v, want the judge alone", got)
	}
	if n := len(model.Calls); n != 1 {
		t.Fatalf("triage calls = %d, want round 1's only", n)
	}
	if evs := approvalEvents(t, h, 2, "round.triage"); len(evs) != 1 {
		t.Fatalf("round.triage events = %d, want round 1's only", len(evs))
	}
	if evs := approvalEvents(t, h, 2, "round.rerun_role"); len(evs) != 0 {
		t.Fatalf("round.rerun_role events: %+v", evs)
	}
	starts := approvalEvents(t, h, 2, "engine.round_start")
	if len(starts) != 2 || starts[1].Message != "delta check (4 lines) in review1 at b2: codex-judge" {
		t.Fatalf("round starts: %+v", starts)
	}
	var data struct {
		DeltaCheck bool `json:"delta_check"`
		DeltaLines int  `json:"delta_lines"`
	}
	if err := json.Unmarshal(starts[1].Data, &data); err != nil || !data.DeltaCheck || data.DeltaLines != 4 {
		t.Fatalf("round start data %s (%v)", starts[1].Data, err)
	}
	if got := *inFlight; len(got) != 2 || got[0] != "" || !strings.Contains(got[1], `"lines":4`) {
		t.Fatalf("%s during the rounds = %q, want the delta check in the second", KVPRDeltaCheck(pr.ID), got)
	}

	// Its approval supersedes the first one: nothing is dismissed.
	pr = h.wantState(2, store.PRReviewed)
	if deref(pr.ReviewedSHA) != "b2" || deref(pr.LastReviewID) != 702 || deref(pr.LastReviewEvent) != "APPROVED" {
		t.Fatalf("after the check: reviewed %q review %v %q", deref(pr.ReviewedSHA), pr.LastReviewID, deref(pr.LastReviewEvent))
	}
	if got := dismissCalls(h); len(got) != 0 {
		t.Fatalf("dismissals %q", got)
	}
	if evs := approvalEvents(t, h, 2, "review.approval_superseded"); len(evs) != 1 ||
		evs[0].Message != "approval 701 on b1 superseded by approval 702 on b2" {
		t.Fatalf("review.approval_superseded events: %+v", evs)
	}
	for _, key := range []string{kvApprovalPending(pr.ID), KVPRDeltaCheck(pr.ID)} {
		if v, ok := kvValue(h, key); ok {
			t.Fatalf("%s = %q after the check", key, v)
		}
	}
	if h.clock.Now().After(pushedAt.Add(10 * time.Minute)) {
		t.Fatal("the check ran late")
	}
}

// A modified image is a known binary: it leaves the delta incomplete for the
// threshold but readable for a delta check, at 0 lines. A text file without
// a patch (too large), a removed binary and any file of a listing at
// GitHub's cap are unread.
func TestModifiedBinaryFilesKeepADeltaReadable(t *testing.T) {
	s := MeasureDelta(liveDelta)
	if s.Lines != 4 || s.Complete || !slices.Equal(s.Binaries, []string{liveDelta[0].Path, liveDelta[1].Path}) || s.Unread != 0 || !s.Readable() {
		t.Fatalf("live delta = %+v", s)
	}
	for name, extra := range map[string]github.FileDelta{
		"a large text file": {Path: "app/models/schema_dump.rb", Status: "modified", Truncated: true},
		"a removed image":   {Path: "app/assets/images/old.png", Status: "removed", Truncated: true},
	} {
		if s := MeasureDelta(append(slices.Clone(liveDelta), extra)); s.Unread != 1 || s.Readable() {
			t.Errorf("%s: %+v, want it unread", name, s)
		}
	}
	capped := make([]github.FileDelta, github.CompareFileLimit)
	for i := range capped {
		capped[i] = github.FileDelta{Path: fmt.Sprintf("img/%d.png", i), Status: "modified", Truncated: true}
	}
	if s := MeasureDelta(capped); len(s.Binaries) != 0 || s.Readable() {
		t.Errorf("a listing at the cap: %d binaries, readable %v", len(s.Binaries), s.Readable())
	}
	if s := MeasureDelta(rubyMixed); !s.Complete || !s.Readable() || s.Binaries != nil {
		t.Errorf("a complete delta: %+v", s)
	}
}

// GitHub's client lists a binary or empty file without a patch as complete
// (not Truncated, with its blob): the size of a push still takes it as one
// without a patch, never as 0 lines read in full, and the push is never
// trivial for it.
func TestAPatchlessFileListedCompleteIsStillUnread(t *testing.T) {
	asListed := slices.Clone(liveDelta)
	for i := range 2 {
		asListed[i].Truncated, asListed[i].BlobSHA = false, fmt.Sprintf("b%d", i)
	}
	if s := MeasureDelta(asListed); s.Lines != 4 || s.Complete || !slices.Equal(s.Binaries, []string{liveDelta[0].Path, liveDelta[1].Path}) || !s.Readable() {
		t.Fatalf("live delta as listed = %+v, want the two images as binaries", s)
	}
	for name, f := range map[string]github.FileDelta{
		"an image":         {Path: "app/assets/logo.png", Status: "modified", BlobSHA: "b1"},
		"an empty file":    {Path: "app/assets/.keep", Status: "modified", BlobSHA: "b2"},
		"a removed binary": {Path: "app/assets/old.png", Status: "removed", BlobSHA: "b3"},
	} {
		if s := MeasureDelta([]github.FileDelta{f}); s.Complete {
			t.Errorf("%s: %+v, want it incomplete", name, s)
		}
		if _, trivial := TrivialDelta([]github.FileDelta{yamlComments[0], f}, DeltaClasses); trivial {
			t.Errorf("%s: a push with it is trivial", name)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The approval kept for a delta check goes when the check does not approve,
// fails or needs attention, with the reason in the dismissal.
func TestKeptApprovalIsDismissedWhenTheCheckDoesNotApprove(t *testing.T) {
	for _, tc := range []struct {
		name, verdict, why string
		event              string // last_review_event after it
	}{
		{"a comment", "COMMENTED", "their check posted commented", "COMMENTED"},
		{"changes requested", "CHANGES_REQUESTED", "their check posted changes_requested", "CHANGES_REQUESTED"},
		{"a failure", pipeline.OutcomeTimeout, "their check failed: its round ended timeout", "DISMISSED"},
		{"needs attention", pipeline.OutcomeNeedsAttention, "their check failed: its round ended needs_attention", "DISMISSED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := deltaCheckHarness(t)
			verdictRounds(h, "APPROVED", tc.verdict)
			keptForCheck(t, h, liveDelta)
			if got := dismissCalls(h); len(got) != 0 {
				t.Fatalf("dismissed before the check: %q", got)
			}
			pollPR(h, 5*time.Minute, 2, "b2") // the delta check
			reqWantRounds(t, h, 2)
			wantCall := fmt.Sprintf("dismiss:talkable/talkable#2:%d:magnum: new commits since this approval; %s", approvalID, tc.why)
			if got := dismissCalls(h); !slices.Equal(got, []string{wantCall}) {
				t.Fatalf("dismiss calls = %q, want %q", got, wantCall)
			}
			evs := approvalEvents(t, h, 2, "review.approval_dismissed")
			if len(evs) != 1 || !strings.HasSuffix(evs[0].Message, "; "+tc.why) || !strings.Contains(string(evs[0].Data), tc.why) {
				t.Fatalf("review.approval_dismissed events: %+v", evs)
			}
			if ev := deref(h.pr(2).LastReviewEvent); ev != tc.event {
				t.Fatalf("last_review_event = %q, want %q", ev, tc.event)
			}
			if v, ok := kvValue(h, kvApprovalPending(h.pr(2).ID)); ok {
				t.Fatalf("pending approval %q after the dismissal", v)
			}
			pollPR(h, time.Minute, 2, "b2")
			if got := dismissCalls(h); len(got) != 1 {
				t.Fatalf("dismissed again: %q", got)
			}
		})
	}
}

// A dismissal GitHub did not answer at the check's end is tried again at the
// next poll, not forgotten while the delta still qualifies.
func TestKeptApprovalDismissalIsRetriedAtTheNextPoll(t *testing.T) {
	h, _, _ := deltaCheckHarness(t)
	verdictRounds(h, "APPROVED", pipeline.OutcomeTimeout)
	keptForCheck(t, h, liveDelta)
	setDismissErr(h.gh, &github.APIError{Status: 502, Message: "Bad Gateway"})
	pollPR(h, 5*time.Minute, 2, "b2") // the check times out; the dismissal fails
	if got := dismissCalls(h); len(got) != 1 {
		t.Fatalf("dismiss calls = %q, want the failed one", got)
	}
	setDismissErr(h.gh, nil)
	pollPR(h, time.Second, 2, "b2")
	if got := dismissCalls(h); len(got) != 2 || got[0] != got[1] {
		t.Fatalf("dismiss calls = %q, want the same one again", got)
	}
	if ev := deref(h.pr(2).LastReviewEvent); ev != "DISMISSED" {
		t.Fatalf("last_review_event = %q", ev)
	}
	if v, ok := kvValue(h, kvApprovalPending(h.pr(2).ID)); ok {
		t.Fatalf("pending approval %q after the dismissal", v)
	}
}

// No check within an hour of the time it was due, after the push quiet
// period (a pause holds it, and the hour runs during a pause): the approval
// goes, so it never covers unreviewed code for long.
func TestKeptApprovalIsDismissedAnHourAfterItsCheckWasDue(t *testing.T) {
	h, _, _ := deltaCheckHarness(t)
	verdictRounds(h, "APPROVED")
	keptForCheck(t, h, liveDelta)
	pushedAt := h.pr(2).HeadChangedAt
	if err := h.st.SetKV(h.ctx, KVDaemonPaused, "1"); err != nil {
		t.Fatal(err)
	}
	due := pushedAt.Add(5 * time.Minute) // push_quiet_period
	pollPR(h, due.Add(time.Hour-time.Second).Sub(h.clock.Now()), 2, "b2")
	if got := dismissCalls(h); len(got) != 0 {
		t.Fatalf("dismissed before the hour: %q", got)
	}
	pollPR(h, time.Second, 2, "b2")
	why := "no check of them posted within 1h of becoming due; a re-review follows"
	wantCall := fmt.Sprintf("dismiss:talkable/talkable#2:%d:magnum: new commits since this approval; %s", approvalID, why)
	if got := dismissCalls(h); !slices.Equal(got, []string{wantCall}) {
		t.Fatalf("dismiss calls = %q, want %q", got, wantCall)
	}
	if ev := deref(h.pr(2).LastReviewEvent); ev != "DISMISSED" {
		t.Fatalf("last_review_event = %q", ev)
	}
	reqWantRounds(t, h, 1)
}

// The live case (talkable#11920): an approval kept at 10:10 for a check
// that quiet_hours (03:00-12:00) held was dismissed at 11:11, an hour of
// wall time after the push, and the check approved again at 12:10: a dismiss
// and a re-approval the author saw for nothing. Quiet hours no longer hold a
// delta check: it runs once the push quiet period ends, inside them, and its
// approval supersedes the kept one; the hour a kept approval stands is wall
// time again.
func TestKeptApprovalsCheckRunsInsideQuietHours(t *testing.T) {
	h, _, _ := deltaCheckHarness(t)
	verdictRounds(h, "APPROVED")
	keptForCheck(t, h, liveDelta) // b2 pushed at 10:06
	h.cfg.Daemon.QuietHours = "03:00-12:00"
	since := h.pr(2).HeadChangedAt
	pollPR(h, since.Add(5*time.Minute).Sub(h.clock.Now()), 2, "b2")
	reqWantRounds(t, h, 2)
	if got := dismissCalls(h); len(got) != 0 {
		t.Fatalf("dismissed: %q", got)
	}
	if evs := approvalEvents(t, h, 2, "review.approval_superseded"); len(evs) != 1 {
		t.Fatalf("approval_superseded events = %+v", evs)
	}
	if got, want := h.e.keptApprovalDeadline(h.cfg.WatchFor("talkable/talkable"), since), since.Add(5*time.Minute+time.Hour); !got.Equal(want) {
		t.Fatalf("deadline inside quiet hours = %v, want an hour of wall time after the push quiet period (%v)", got, want)
	}
}

// A push that takes the delta past the threshold ends the wait for a check:
// the kept approval goes at once.
func TestKeptApprovalIsDismissedWhenTheDeltaGrows(t *testing.T) {
	h, _, _ := deltaCheckHarness(t)
	verdictRounds(h, "APPROVED")
	keptForCheck(t, h, liveDelta)
	reqSetFiles(h, "b1...b3", codeDelta(30))
	pollPR(h, time.Minute, 2, "b3")
	why := "they are no longer a small delta; a re-review follows"
	wantCall := fmt.Sprintf("dismiss:talkable/talkable#2:%d:magnum: new commits since this approval; %s", approvalID, why)
	if got := dismissCalls(h); !slices.Equal(got, []string{wantCall}) {
		t.Fatalf("dismiss calls = %q, want %q", got, wantCall)
	}
	if w := waitOf(t, h, 2); w.DeltaCheck {
		t.Fatalf("wait = %+v, want a full re-review", w)
	}
}

// What is not a delta check keeps today's behaviour: the approval goes at
// the push and every reviewer runs: a delta at the threshold, a delta that
// adds a file (added code is never a short check) and any delta with
// delta_check off.
func TestDeltasThatAreNoDeltaCheckGetAFullRoundAndTheApprovalGoes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mod   func(*harness)
		files []github.FileDelta
	}{
		{"30 lines", nil, codeDelta(30)},
		{"an added code file", nil, append(slices.Clone(rubyMixed), addedFile...)},
		{"delta_check off", withoutDeltaCheck, liveDelta},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mods []func(*harness)
			if tc.mod != nil {
				mods = append(mods, tc.mod)
			}
			h, _, _ := deltaCheckHarness(t, mods...)
			verdictRounds(h, "APPROVED")
			approvedPR(h, 2, "b1")
			reqSetFiles(h, "b1...b2", tc.files)
			pollPR(h, time.Minute, 2, "b2")
			wantCall := fmt.Sprintf("dismiss:talkable/talkable#2:%d:%s", approvalID, ApprovalDismissMessage)
			if got := dismissCalls(h); !slices.Equal(got, []string{wantCall}) {
				t.Fatalf("dismiss calls = %q, want %q at the push", got, wantCall)
			}
			if evs := approvalEvents(t, h, 2, "review.approval_kept_for_check"); len(evs) != 0 {
				t.Fatalf("kept for a check: %+v", evs)
			}
			if w := waitOf(t, h, 2); w.DeltaCheck {
				t.Fatalf("wait = %+v, want a full re-review", w)
			}
			pollPR(h, 30*time.Minute, 2, "b2") // past the quiet period and the interval
			ins := h.rd.all()
			if len(ins) != 2 || ins[1].DeltaCheck != nil || len(ins[1].Roles) < 3 {
				t.Fatalf("rounds = %d, second %+v: want a full re-review", len(ins), ins[len(ins)-1].DeltaCheck)
			}
		})
	}
}

// A forced or requested round, or one that names roles, runs in full even
// when the delta is small.
func TestAForcedReviewOfASmallDeltaRunsInFull(t *testing.T) {
	h, _, _ := deltaCheckHarness(t)
	verdictRounds(h, "APPROVED")
	keptForCheck(t, h, liveDelta)
	if err := h.st.UpdatePR(h.ctx, h.pr(2).ID, func(u *store.PRUpdate) { u.Set("forced", true) }); err != nil {
		t.Fatal(err)
	}
	pollPR(h, time.Second, 2, "b2") // forced: due at once
	ins := h.rd.all()
	if len(ins) != 2 || ins[1].DeltaCheck != nil || len(ins[1].Roles) < 3 {
		t.Fatalf("rounds = %d: want a full forced re-review", len(ins))
	}
	// Its approval supersedes the kept one all the same.
	if got := dismissCalls(h); len(got) != 0 {
		t.Fatalf("dismissals %q", got)
	}
	if evs := approvalEvents(t, h, 2, "review.approval_superseded"); len(evs) != 1 {
		t.Fatalf("review.approval_superseded events: %+v", evs)
	}
}

// freshCheckWant is what a delta check with a fresh judge session looks
// like: a recovery of the judge alone on the one-line delta, at its
// rereview effort, with the previous review to rebuild its context from and
// round.delta_check_fresh naming why (reason) instead of
// round.delta_check_dropped.
func freshCheckWant(t *testing.T, h *harness, reason string) pipeline.RoundInput {
	t.Helper()
	ins := h.rd.all()
	if len(ins) != 2 {
		t.Fatalf("rounds = %d, want the delta check", len(ins))
	}
	in := ins[1]
	if in.Kind != pipeline.KindRecovery || in.TargetSHA != "b2" || !slices.Equal(roleNames(in.Roles), []string{"codex-judge"}) || in.MaxRestarts != 0 {
		t.Fatalf("delta check input: kind %s target %s roles %v restarts %d", in.Kind, in.TargetSHA, roleNames(in.Roles), in.MaxRestarts)
	}
	if in.DeltaCheck == nil || in.DeltaCheck.Lines != 1 || len(in.DeltaCheck.Files) != 1 {
		t.Fatalf("delta check = %+v, want the one line", in.DeltaCheck)
	}
	if in.Previous == nil || in.Previous.ID != 701 || in.Previous.SHA != "b1" {
		t.Fatalf("Previous = %+v, want review 701 on b1", in.Previous)
	}
	pr := h.pr(2)
	if got := h.workspaceRoles(pr.ID); !slices.Equal(got, []string{"codex-judge"}) {
		t.Fatalf("workspace roles = %v, want the judge alone", got)
	}
	if n := h.ag.count(fmt.Sprintf("pane:%d:", pr.ID)); n != 0 {
		t.Fatalf("panes added for other roles: %v", h.ag.all())
	}
	h.ag.mu.Lock()
	starts := slices.Clone(h.ag.efforts)
	h.ag.mu.Unlock()
	if last := starts[len(starts)-1]; last != "codex-judge:high:" {
		t.Fatalf("judge started as %q (starts %v), want a fresh session at its rereview effort", last, starts)
	}
	fresh := approvalEvents(t, h, 2, "round.delta_check_fresh")
	if len(fresh) != 1 || fresh[0].Message != "delta check with a fresh judge session: "+reason {
		t.Fatalf("round.delta_check_fresh events: %+v", fresh)
	}
	if evs := approvalEvents(t, h, 2, "round.delta_check_dropped"); len(evs) != 0 {
		t.Fatalf("round.delta_check_dropped events: %+v", evs)
	}
	roundStarts := approvalEvents(t, h, 2, "engine.round_start")
	if len(roundStarts) != 2 || roundStarts[1].Message != "delta check (1 line) in review1 at b2: codex-judge" {
		t.Fatalf("round starts: %+v", roundStarts)
	}
	return in
}

// The live case: a one-line push qualified for a delta check, and the PR's
// posting identity had just migrated to another App, so the judge's session
// was parked. The check runs with a fresh judge session that rebuilds its
// context from the former App's review (the recovery prompt with
// delta_check), not as a full round of every reviewer; the approval kept for
// it is superseded as with any delta check.
func TestAnIdentityMigrationGivesADeltaCheckAFreshJudgeSession(t *testing.T) {
	h, _, _ := deltaCheckHarness(t, migWithApp("zhuravel-app", "zhuravel[bot]"))
	verdictRounds(h, "APPROVED")
	keptForCheck(t, h, codePatch(1))
	migRun(h, h.pr(2), "run-a", 1, "talkable-app", "talkable[bot]", 701, "APPROVED")
	h.cfg.Watches[0].Identity = "zhuravel-app"

	pollPR(h, 5*time.Minute, 2, "b2")
	if evs := approvalEvents(t, h, 2, "pr.identity_migrated"); len(evs) != 1 {
		t.Fatalf("pr.identity_migrated events: %+v", evs)
	}
	in := freshCheckWant(t, h, "identity talkable-app → zhuravel-app")
	if in.PR.Identity != "zhuravel-app" || !in.Previous.Former {
		t.Fatalf("identity %q, previous review former %v: want zhuravel-app building on talkable[bot]'s review", in.PR.Identity, in.Previous.Former)
	}
	wantStrings(t, "FormerLogins", in.FormerLogins, []string{"talkable[bot]"})
	if evs := approvalEvents(t, h, 2, "review.approval_superseded"); len(evs) != 1 {
		t.Fatalf("review.approval_superseded events: %+v", evs)
	}
	if pr := h.wantState(2, store.PRReviewed); deref(pr.ReviewedSHA) != "b2" {
		t.Fatalf("reviewed_sha %q after the check", deref(pr.ReviewedSHA))
	}
}

// A judge whose session is gone (parked with no conversation to resume, a
// resume that fails, fresh sessions requested) checks the delta in a fresh
// session too.
func TestADeltaCheckWhoseJudgeLostItsSessionRunsWithAFreshOne(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		lose         func(h *harness)
	}{
		{"no conversation to resume", "the judge's session is gone", func(h *harness) { parkSessions(h, 2) }},
		{"a resume that fails", "the judge's session is gone", func(h *harness) {
			parkSessions(h, 2)
			h.ag.mu.Lock()
			h.ag.resumeIDs, h.ag.failResume = map[agents.Role]string{agents.RoleJudge: "uuid-judge"}, true
			h.ag.mu.Unlock()
		}},
		{"fresh sessions requested", "fresh sessions were requested", func(h *harness) { h.e.setKV(h.ctx, kvPRFresh(h.pr(2).ID), "1") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := deltaCheckHarness(t)
			verdictRounds(h, "APPROVED")
			keptForCheck(t, h, codePatch(1))
			tc.lose(h)
			pollPR(h, 5*time.Minute, 2, "b2")
			freshCheckWant(t, h, tc.reason)
			if evs := approvalEvents(t, h, 2, "review.approval_superseded"); len(evs) != 1 {
				t.Fatalf("review.approval_superseded events: %+v", evs)
			}
		})
	}
}

// A fresh judge session needs a review of its own to build on: when no
// review by the PR's current or former identities is on record (the last
// one was posted as another login), the delta check becomes a full
// recovery round of every role at the judge's full effort.
func TestAFreshJudgeWithoutAnEarlierReviewRunsTheDeltaInFull(t *testing.T) {
	h, _, _ := deltaCheckHarness(t)
	verdictRounds(h, "APPROVED")
	keptForCheck(t, h, codePatch(1))
	migRun(h, h.pr(2), "run-z", 1, "zhuravel", "zhuravel", 701, "APPROVED") // posted as another login (magnum review --as)
	parkSessions(h, 2)
	pollPR(h, 5*time.Minute, 2, "b2")
	ins := h.rd.all()
	if len(ins) != 2 || ins[1].Kind != pipeline.KindRecovery || ins[1].DeltaCheck != nil || len(ins[1].Roles) < 3 {
		t.Fatalf("rounds = %d, second kind %s check %v roles %v: want a full recovery", len(ins), ins[1].Kind, ins[1].DeltaCheck, roleNames(ins[1].Roles))
	}
	dropped := approvalEvents(t, h, 2, "round.delta_check_dropped")
	if len(dropped) != 1 || dropped[0].Message != "a full round instead of the delta check: the judge starts in a fresh session (the judge's session is gone), with no review of this PR's identities on record to build on" {
		t.Fatalf("round.delta_check_dropped events: %+v", dropped)
	}
	if evs := approvalEvents(t, h, 2, "round.delta_check_fresh"); len(evs) != 0 {
		t.Fatalf("round.delta_check_fresh events: %+v", evs)
	}
	h.ag.mu.Lock()
	starts := slices.Clone(h.ag.efforts)
	h.ag.mu.Unlock()
	if !slices.Contains(starts, "codex-judge:xhigh:") {
		t.Fatalf("starts %v: want the judge at its full effort", starts)
	}
}

// parkSessions parks PR n's live sessions, as a lost pane or a daemon
// restart leaves them: nothing to resume unless the agents know an id.
func parkSessions(h *harness, n int) {
	h.t.Helper()
	sessions, err := h.st.SessionsByPR(h.ctx, h.pr(n).ID)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, s := range sessions {
		if s.State != store.SessionLive {
			continue
		}
		if err := h.st.TransitionSession(h.ctx, s.ID, []string{store.SessionLive}, store.SessionParked, nil); err != nil {
			h.t.Fatal(err)
		}
	}
}
