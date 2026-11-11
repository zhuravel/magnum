package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// fixLeftADescription runs one loop on PR #2 whose fixer pushes nothing and
// leaves you one call, a finding about the PR's description: an entry with
// no thread, which magnum can never read as done. The PR needs attention,
// kind fix, as on a live loop on 2026-11-07, and it returns the PR then.
func fixLeftADescription(t *testing.T) (*harness, store.PR) {
	t.Helper()
	h, git := fixHarness(t)
	fixablePR(h, "zhuravel", map[string]int{"P2": 1})
	fixer(h, git, "", func(run store.Run) string {
		return fixReport(run.ID, "nothing", fixReplied, "- 5808905525 description: the Solution line still says the resend does not redo the failed step")
	})
	h.fix(FixPayload{})
	h.tick()
	pr := h.wantState(2, store.PRNeedsAttention)
	if msg := deref(pr.LastError); !strings.HasPrefix(msg, "fix: 1 finding needs your call: ") {
		t.Fatalf("last_error = %q, want the fixer's description call", msg)
	}
	return h, pr
}

// The loop's Needs you was a change to the PR's description, which you made
// on GitHub by hand. Nothing magnum reads can say so (the entry has no
// thread, and the look lifts only its own raise), and a review of the same
// head only to clear it costs a round: `magnum fix --done` is your word that
// you did what the fixer left. The PR goes back to reviewed at the head
// whose review stands, the attention's rows go, a fix.acknowledged event
// says so, and no round runs, then or at the next tick. A dry run says what
// it would do and changes nothing.
func TestFixDoneLiftsWhatTheFixerLeftWithoutAReview(t *testing.T) {
	h, pr := fixLeftADescription(t)
	msg := deref(pr.LastError)
	submits := h.ag.count("submit:")

	dry := h.fix(FixPayload{Done: true, Seen: msg, DryRun: true})
	h.handle()
	if res := h.wantDone(dry); res != "dry run: would put PR #2 back to reviewed at a1a1a1a, with no review" {
		t.Errorf("dry run answer = %q", res)
	}
	if cur := h.wantState(2, store.PRNeedsAttention); deref(cur.LastError) != msg {
		t.Errorf("the dry run changed last_error to %q", deref(cur.LastError))
	}

	id := h.fix(FixPayload{Done: true, Seen: msg})
	h.handle()
	if res := h.wantDone(id); res != "PR #2 is back to reviewed at a1a1a1a: you did what the fixer left; no review runs" {
		t.Errorf("answer = %q", res)
	}
	cur := h.wantState(2, store.PRReviewed)
	if cur.LastError != nil || cur.HeadSHA != fixHead || deref(cur.ReviewedSHA) != fixHead {
		t.Errorf("after the lift: last_error %q, head %s, reviewed %s", deref(cur.LastError), cur.HeadSHA, deref(cur.ReviewedSHA))
	}
	for _, key := range []string{KVPRAttention(pr.ID), kvPRAttentionRun(pr.ID)} {
		if _, ok := h.e.getKV(h.ctx, key); ok {
			t.Errorf("%s outlived the lift", key)
		}
	}
	ev, ok := h.fixEvent("fix.acknowledged")
	if !ok || ev.Message != "you did what the fixer left: the review of a1a1a1a stands, with no new round" {
		t.Errorf("fix.acknowledged = %q, %v", ev.Message, ok)
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil || data["head"] != fixHead || data["by"] != "magnum fix --done" {
		t.Errorf("fix.acknowledged data = %s", ev.Data)
	}

	h.advance(10 * time.Minute)
	h.tick()
	h.wantState(2, store.PRReviewed)
	if got := h.ag.count("submit:"); got != submits {
		t.Errorf("an agent was prompted after the lift: %d submits, want %d", got, submits)
	}
}

// --done is your word about what the fixer left you, and only about that:
// a PR that needs nothing, an attention of another kind (a round's failure),
// a fix job's that asks for something else (its cap, a failed run), one
// whose fixer did not get the other reviewers' items (what it left may not
// be all: the line under its calls says so), one that changed since you
// looked (Seen), and one you never saw (a command that showed no attention
// sends Seen "") all stay as they are.
func TestFixDoneRefusesWhatIsNotTheFixersCalls(t *testing.T) {
	storetest.Parallel(t)
	const unread = "the other reviewers' threads and comments were not read (HTTP 502): `magnum fix talkable/talkable#2` reads them again"
	// A failed run keeps the decision calls of the attention it replaced
	// after its own words (keepDecisions): still a failure, not calls alone.
	const kept = "fix run failed: the fixer's push failed: /state/claude-fix.md; " + decideCall + "; details: /state/claude-fix.md"
	for _, tc := range []struct {
		name  string
		mod   func(h *harness, pr store.PR) string // returns the Seen to send ("" = the PR's last_error after mod)
		blind bool                                 // send Seen "" whatever mod returns: the command showed no attention
		want  string
	}{
		{name: "reviewed", mod: func(h *harness, pr store.PR) string {
			if err := h.st.TransitionPR(h.ctx, pr.ID, []string{store.PRNeedsAttention}, store.PRReviewed, func(u *store.PRUpdate) {
				u.Set("last_error", nil)
			}); err != nil {
				h.t.Fatal(err)
			}
			return ""
		}, want: "PR #2 needs nothing of you (reviewed): `magnum fix --done` answers what a fix loop left you"},
		{name: "a round's failure", mod: func(h *harness, pr store.PR) string {
			h.e.setKV(h.ctx, KVPRAttention(pr.ID), attention.KindFailed)
			return ""
		}, want: "PR #2 needs attention for failed, not for what the fixer left you: `magnum status talkable/talkable#2` says what"},
		{name: "an unrecorded kind", mod: func(h *harness, pr store.PR) string {
			h.e.delKV(h.ctx, KVPRAttention(pr.ID))
			return ""
		}, want: "PR #2 needs attention for a reason magnum did not record, not for what the fixer left you: `magnum status talkable/talkable#2` says what"},
		{name: "a failed fix run", mod: func(h *harness, pr store.PR) string {
			setLastError(h, pr, "fix run failed: the fixer's push failed: /state/claude-fix.md")
			return ""
		}, want: "PR #2's fix loop asks for something else than calls you can mark done (fix run failed: the fixer's push failed: " +
			"/state/claude-fix.md): `magnum status talkable/talkable#2` says what"},
		{name: "a failed run that kept your decisions", mod: func(h *harness, pr store.PR) string {
			setLastError(h, pr, kept)
			return ""
		}, want: "PR #2's fix loop asks for something else than calls you can mark done (" + oneLine(kept, 200) + "): `magnum status talkable/talkable#2` says what"},
		{name: "the cap", mod: func(h *harness, pr store.PR) string {
			setLastError(h, pr, "3 fix cycles, 1 finding left: 1 new: /state/claude-fix.md")
			return ""
		}, want: "PR #2's fix loop asks for something else than calls you can mark done (3 fix cycles, 1 finding left: 1 new: " +
			"/state/claude-fix.md): `magnum status talkable/talkable#2` says what"},
		{name: "the other reviewers' items unread", mod: func(h *harness, pr store.PR) string {
			setLastError(h, pr, deref(pr.LastError)+"\n"+unread)
			return ""
		}, want: "PR #2's fixer did not get every item it had to read, so what it left may not be all: " + unread + "; nothing changed"},
		{name: "changed since you looked", mod: func(_ *harness, _ store.PR) string {
			return "fix: 2 findings need your call: /state/older/claude-fix.md"
		}, want: "PR #2 needs you for something else since you looked: `magnum status talkable/talkable#2` says what; nothing changed"},
		{name: "never seen", mod: func(_ *harness, _ store.PR) string { return "" }, blind: true,
			want: "PR #2 needs you for something you did not see: `magnum status talkable/talkable#2` says what; nothing changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, pr := fixLeftADescription(t)
			seen := tc.mod(h, pr)
			before := h.pr(2)
			if seen == "" && !tc.blind {
				seen = deref(before.LastError)
			}
			id := h.fix(FixPayload{Done: true, Seen: seen})
			h.handle()
			if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != tc.want {
				t.Errorf("%s %q\nwant failed %q", r.State, deref(r.Result), tc.want)
			}
			if cur := h.pr(2); cur.State != before.State || deref(cur.LastError) != deref(before.LastError) {
				t.Errorf("the refusal moved the PR: %s %q, was %s %q", cur.State, deref(cur.LastError), before.State, deref(before.LastError))
			}
			if _, ok := h.fixEvent("fix.acknowledged"); ok {
				t.Error("a refusal wrote fix.acknowledged")
			}
		})
	}
}

// setLastError replaces pr's last_error, the loop's words, by msg.
func setLastError(h *harness, pr store.PR, msg string) {
	h.t.Helper()
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("last_error", msg) }); err != nil {
		h.t.Fatal(err)
	}
}

// What you mark done must be all there is: a head magnum has not reviewed
// (a push it has not seen through yet), a loop that runs on the PR (whose
// end writes a Needs you of its own) and a fix held for later (a restart,
// the GitHub budget) are refused, and nothing moves.
func TestFixDoneRefusesWhileSomethingIsUnknown(t *testing.T) {
	storetest.Parallel(t)
	for _, tc := range []struct {
		name string
		mod  func(h *harness, pr store.PR)
		want string
	}{
		{name: "an unreviewed head", mod: func(h *harness, pr store.PR) {
			if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("head_sha", fixPushed) }); err != nil {
				h.t.Fatal(err)
			}
		}, want: "PR #2's head b2b2b2b is not the one magnum reviewed (a1a1a1a): its review comes first; nothing changed"},
		{name: "a loop", mod: func(h *harness, pr store.PR) {
			if err := h.e.setFixState(h.ctx, pr.ID, FixState{Cycle: 1, Cap: 3, Phase: FixPhaseFixing}); err != nil {
				h.t.Fatal(err)
			}
		}, want: "fixing, cycle 1 of 3; `magnum abort` stops it"},
		{name: "a held fix", mod: func(h *harness, pr store.PR) {
			h.e.setKV(h.ctx, KVPRFixHeld(pr.ID), `{"at":"2026-11-07T13:00:00Z","head":"`+fixHead+`","by":"magnum fix"}`)
		}, want: "a fix of PR #2 is held to start later: `magnum abort talkable/talkable#2` takes it back first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, pr := fixLeftADescription(t)
			tc.mod(h, pr)
			id := h.fix(FixPayload{Done: true, Seen: deref(h.pr(2).LastError)})
			h.handle()
			if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != tc.want {
				t.Errorf("%s %q\nwant failed %q", r.State, deref(r.Result), tc.want)
			}
			h.wantState(2, store.PRNeedsAttention)
		})
	}
}

// The lift's compare-and-set holds the attention's words it read: a loop's
// end that writes new calls between fixDone's read and its write (the
// settle's fixAttention on the same PR) wins, the lift answers that the PR
// moved on, and nothing of the newer attention changes.
func TestFixDoneLosesToANewerAttention(t *testing.T) {
	h, pr := fixLeftADescription(t)
	msg := deref(pr.LastError)
	const newer = "fix: 2 findings need your call: /state/reviews/talkable/talkable/2/a1a1a1a1a1a1/claude-fix.md"
	setLastError(h, pr, newer)
	_, err := h.e.fixDone(h.ctx, h.repo(), pr, msg, false) // pr is the read before the newer attention
	if err == nil || err.Error() != "PR #2 moved on while you answered: `magnum status talkable/talkable#2` says what; nothing changed" {
		t.Fatalf("a lift that read an older attention: %v", err)
	}
	cur := h.wantState(2, store.PRNeedsAttention)
	if deref(cur.LastError) != newer {
		t.Errorf("last_error = %q, want the newer attention kept", deref(cur.LastError))
	}
	if kind, _ := h.e.getKV(h.ctx, KVPRAttention(pr.ID)); kind != attention.KindFix {
		t.Errorf("the attention's kind = %q, want it kept", kind)
	}
	if _, ok := h.fixEvent("fix.acknowledged"); ok {
		t.Error("the lost lift wrote fix.acknowledged")
	}
}

// --done changes the PR's state and nothing else, so it goes alone.
func TestFixDoneGoesAlone(t *testing.T) {
	h, pr := fixLeftADescription(t)
	for _, p := range []FixPayload{{Done: true, Off: true}, {Done: true, On: true}} {
		id := h.fix(p)
		h.handle()
		if r := h.request(id); r.State != store.RequestFailed || deref(r.Result) != "magnum fix: --done goes alone, without --off or --on" {
			t.Errorf("%+v: %s %q", p, r.State, deref(r.Result))
		}
	}
	if _, ok, _ := h.e.fixOff(h.ctx, pr.ID); ok {
		t.Error("--done with --off switched fix off")
	}
	h.wantState(2, store.PRNeedsAttention)
}
