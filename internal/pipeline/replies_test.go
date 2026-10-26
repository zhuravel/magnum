package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
)

func TestClassifyReply(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"Fixed in 1a2b3c4.", ReplyFixed},
		{"(Claude) Fixed in 1a2b3c4: the retry now checks the session first.", ReplyFixed},
		{"(claude): done", ReplyFixed},
		{"**Done.** Moved the check before the catch.", ReplyFixed},
		{"Addressed in 9be04f2", ReplyFixed},
		{"✅ fixed", ReplyFixed},
		{"> **[P2] Retry sends the email twice**\n>\n> details\n\n(Claude) Fixed in 4c1d2e3.", ReplyFixed},
		{"Not a bug: the job is idempotent.", ReplyNotABug},
		{"(Claude) NOT A BUG — the caller holds the lock.", ReplyNotABug},
		{"By design; see the ADR.", ReplyNotABug},
		{"Intended. The admin page needs it.", ReplyNotABug},
		{"As intended", ReplyNotABug},
		{"Intentional: we keep the old column one release.", ReplyNotABug},
		{"Won't fix, the feature is going away.", ReplyWontFix},
		{"(Claude) Won’t fix: out of scope for this PR.", ReplyWontFix},
		{"wont fix", ReplyWontFix},
		{"Out of scope for this PR.", ReplyWontFix},
		{"Follow-up: TKBL-123", ReplyWontFix},
		{"follow up in the next PR", ReplyWontFix},
		{"Not fixed yet, working on it.", ReplyOther},
		{"Fixedness is not a word", ReplyOther},
		{"Donezo", ReplyOther},
		{"Why is this a problem?", ReplyOther},
		{"> only a quote", ReplyOther},
		{"", ReplyOther},
		{"(Claude)", ReplyOther},
	} {
		if got := classifyReply(tc.body); got != tc.want {
			t.Errorf("classifyReply(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// Replies as authors and their agents write them: the verdict is the first
// clause, or the clause after an acknowledgement ("Good catch", "Valid",
// "Analyzed", "Noted", "Low priority"), once a leading "(Claude)" or a like
// tag is gone. An acknowledgement alone, or one followed by anything but a
// verdict, claims nothing.
func TestClassifyReplyReadsTheFirstClause(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"(Claude) Good catch, fixed in 84c0b1e", ReplyFixed},
		{"(Claude) Good catch — fixed in 84c0b1e; verified by `bundle exec rspec spec/models/user_spec.rb` (3 examples, 0 failures).", ReplyFixed},
		{"Good catch. Fixed in 84c0b1e.", ReplyFixed},
		{"(Claude) Valid — fixed in d64bcae", ReplyFixed},
		{"Applied in 3f9e2a1.", ReplyFixed},
		{"Already addressed in 7c1e0b2: the guard runs first.", ReplyFixed},
		{"(Claude) This was already addressed in 7c1e0b2.", ReplyFixed},
		{"[Codex] Done.", ReplyFixed},
		{"(Claude Code) Fixed in 1a2b3c4.", ReplyFixed},
		{"Incorrect — the caller holds the lock.", ReplyNotABug},
		{"That's incorrect: the caller holds the lock.", ReplyNotABug},
		{"(Claude) Analyzed — this is intentional.", ReplyNotABug},
		{"(Claude) Analyzed — this concern does not apply.", ReplyNotABug},
		{"Moot: the method is gone in 5d2c9e1.", ReplyNotABug},
		{"(Claude) Low priority — the import runs once a night. Kept as is.", ReplyWontFix},
		{"Low priority, kept.", ReplyWontFix},
		{"Declined: the helper would hide the retry.", ReplyWontFix},
		{"(Claude) Noted — left as is until the importer is rewritten.", ReplyWontFix},
		{"(Claude) Noted — deprioritized for now.", ReplyWontFix},
		{"(Claude) Valid, but out of scope for this PR — the PR only moves the job. Left for a separate change.", ReplyWontFix},
		{"Good catch, not fixed yet.", ReplyOther},
		{"Good catch. I'll look into it.", ReplyOther},
		{"Noted.", ReplyOther},
		{"Incorrect handling of nil is fixed in 1a2b3c4.", ReplyOther},
		{"This is not intentional.", ReplyOther},
		{"Thanks!\n\nFixed in the next PR, maybe.", ReplyOther},
	} {
		if got := classifyReply(tc.body); got != tc.want {
			t.Errorf("classifyReply(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// A reply that weighs the proposed fix and declines it is won't fix, though
// it names no verdict keyword and calls the thread open: a judge kept a
// finding open because the reply began "Confirmed on every premise", said
// "still open" and "undecided", and scored the fix at −8. A negative score
// for the fix ("score that fix at −8", "Net: −3", "net -2.5", either minus)
// anywhere in the first paragraph, or a clause "we accept the risk" or "not
// worth it", declines the fix unless an earlier clause says fixed or not a
// bug.
func TestClassifyReplyTakesADeclinedFixAsWontFix(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"(Claude) Confirmed on every premise. No change in this push — this one is still open. A revert leaves the queued jobs " +
			"failing, but they retry for about seven weeks and complete on the next deploy, so the impact is bounded. Staging a " +
			"compatible consumer release first costs a second PR and deploy for a window that only opens on a revert, so I score " +
			"that fix at −8. How to handle it is still undecided.", ReplyWontFix},
		{"Checked. Scored the fix at -3: it doubles the queries on every page.", ReplyWontFix},
		{"I score this fix at − 2 against the risk.", ReplyWontFix},
		{"Net: −3. The helper would hide the retry.", ReplyWontFix},
		{"(Claude) Valid concern; net -2.5 once the second deploy is counted.", ReplyWontFix},
		{"Confirmed. We accept the risk: the import runs once a night.", ReplyWontFix},
		{"Reproduced it, but not worth it for a nightly import.", ReplyWontFix},
		{"It's not worth the second deploy.", ReplyWontFix},
		{"(Claude) Applied in 3f9e2a1. Net: +3, the guard costs one query.", ReplyFixed},
		{"Fixed in 3f9e2a1; the larger rewrite I score that fix at −4.", ReplyFixed},
		{"Confirmed. Not a bug here: the caller retries, so I score that fix at −8.", ReplyNotABug},
		{"Reproduced on staging, already addressed in 3f9e2a1. Net: −1 for the second guard.", ReplyOther},
		{"Reproduced on staging, not a bug for the nightly path. Not worth it elsewhere.", ReplyOther},
		{"Still open, will look later.", ReplyOther},
		{"Confirmed. This one is still open; how to handle it is undecided.", ReplyOther},
		{"Net: 0 either way, still open.", ReplyOther},
		{"Is it worth it? The internet -3 dB loss is unrelated.", ReplyOther},
		{"Noted.\n\nI score that fix at −8.", ReplyOther},
	} {
		if got := classifyReply(tc.body); got != tc.want {
			t.Errorf("classifyReply(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// The forms the authors' coding agents write that the classifier read as
// other: 24 of 57 declines were "Low priority (Net 0)" and the like with no
// verdict keyword, others deferred ("Deferred.", "No guard for now"), left
// the finding open ("left open — not fixed in this PR") or argued it "by
// design" or "by decision" mid-sentence, and three fixes named the commit
// in another way ("Re <id>: right, fixed in <sha>", "covered in <sha>",
// "<sha> adds …"). A low priority acknowledgement with no fixed clause
// declines the fix; one followed by "fixed in <sha>" stays fixed.
func TestClassifyReplyReadsLowPriorityDeferralsAndCommitFixes(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		// A low priority acknowledgement, with or without a score, and no fix.
		{"(Claude) Low priority (Net 0): one parse instead of two is real, but the shared one would run on every save, for a five-line saving.", ReplyWontFix},
		{"(Claude) Low priority (Net +0.5): one three-line writer instead of two; small next to re-testing both callers.", ReplyWontFix},
		{"(Claude) Low priority (Net +1): same output, one call instead of two; not worth a change on its own.", ReplyWontFix},
		{"(Claude) Low priority — net 0. Correct, but it saves three calls to a one-line function; the churn costs as much as it saves.", ReplyWontFix},
		{"(Claude) Low priority — goes with the helper suggestion above, which stays as is (net +1, under the +5 bar).", ReplyWontFix},
		{"low priority (Net +0.7): one duplicated number is cheap.", ReplyWontFix},
		{"(Claude) Low priority — the import runs once a night.", ReplyWontFix},
		{"(Claude) Low priority — not changed in this PR. The path is real, but only a console session reaches it.", ReplyWontFix},
		// Deferred, left open, kept for now.
		{"(Claude) Noted. The report has shown the first reading since before this change; a fix means rewriting the stored readings. Deferred.", ReplyWontFix},
		{"(Claude) Noted. Real, but it needs a partial outage and then a later change. Deferred.", ReplyWontFix},
		{"(Claude) Noted. After the earlier fix only generated names reach this path, and such a name would need 230 characters. No guard for now.", ReplyWontFix},
		{"(Claude) Valid, left open — not fixed in this PR. Confirmed: the parser raises on a 2048-byte header.", ReplyWontFix},
		{"(Claude) Valid — left open, not fixed in this push: showing the code needs a design call on its placement.", ReplyWontFix},
		{"Not changed in this round: the caller retries.", ReplyWontFix},
		{"The method stays as it is.", ReplyWontFix},
		// "by design" or "by decision" after the start of a clause, no other verdict.
		{"The new issue type is not carried over from the old checker, so such a behaviour is by design.", ReplyNotABug},
		{"(Claude) The old path is gone, so there is nothing to copy. Stale entries are retired without a replacement by decision: the page shows the state they led to.", ReplyNotABug},
		{"Declined by design: fewer notices is the intent.", ReplyWontFix},
		{"Reproduced, already addressed in 3f9e2a1; the order is by design.", ReplyOther},
		// The commit named another way.
		{"(Claude) Re 4100000001: right, fixed in 0e1f2a3b4c. The import now checks the row before it writes.", ReplyFixed},
		{"Re 42: not a bug, the lock is held.", ReplyNotABug},
		{"(Claude) The empty-list case is covered in 5052817: the loader returns before it reads the cache.", ReplyFixed},
		{"(Claude) 84c0b1e adds a note on large inputs to the FAQ.", ReplyFixed},
		{"9be04f2 fixes the guard: it now runs first.", ReplyFixed},
		{"That case is not covered in 1a2b3c4 yet.", ReplyOther},
		{"84c0b1e is unrelated to this thread.", ReplyOther},
		{"2026100 requests per day is the peak.", ReplyOther},
		// Controls: a plain fix, a low priority fix, a won't fix, a negative score.
		{"Fixed in 1a2b3c4.", ReplyFixed},
		{"Fixed for now; the rewrite replaces it.", ReplyFixed},
		{"(Claude) Low priority (Net +1), but fixed in 1a2b3c4 since it was one line.", ReplyFixed},
		{"Low priority — applied in 3f9e2a1.", ReplyFixed},
		{"Won't fix: the feature is going away.", ReplyWontFix},
		{"(Claude) Low priority (Net -2): naming the check once is tidier, but it edits a check that waits on a separate review.", ReplyWontFix},
		{"Net: −3. The helper would hide the retry.", ReplyWontFix},
	} {
		if got := classifyReply(tc.body); got != tc.want {
			t.Errorf("classifyReply(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// Two fixes the classifier read as other: "Already resolved by <sha> together
// with …" (a commit that closes the finding, named with "resolved" instead of
// "covered" or "fixed"), and a correction that opens with "Correction: you're
// right", says where the first commit landed, and names the fix on this branch
// ("The same fix is now on this branch as <sha>"). A bare "resolved" or a
// resolved question claims nothing, and "Correction" alone is only an
// acknowledgement.
func TestClassifyReplyReadsResolvedByCommitAndFixOnThisBranch(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		// Resolved by or in a commit.
		{"(Claude) Already resolved by 64b00a2 together with example/app#7. The page body carries the log-in note, " +
			"and the dashboard campaign renders only for a known customer.", ReplyFixed},
		{"Resolved by 64b00a2.", ReplyFixed},
		{"(Claude) Resolved in `64b00a2`: the guard runs first.", ReplyFixed},
		{"Good catch, already resolved by 64b00a2e5d.", ReplyFixed},
		{"(Claude) This was resolved by 64b00a2.", ReplyFixed},
		{"The empty-list case is already resolved in 5052817: the loader returns before it reads the cache.", ReplyFixed},
		// Resolved without a commit, or not resolved.
		{"Resolved? Not sure this applies.", ReplyOther},
		{"(Claude) Resolved.", ReplyOther},
		{"Resolved by the next PR.", ReplyOther},
		{"Resolved by hand, no commit.", ReplyOther},
		{"Not resolved by 64b00a2 yet.", ReplyOther},
		{"The empty-list case is not resolved by 64b00a2.", ReplyOther},
		{"This will be resolved by 64b00a2 after the rebase.", ReplyOther},
		// A correction that ends in the fix on this branch.
		{"(Claude) Correction: you're right. 0374ea3 landed on #27's branch, which stacks on this one, not here. " +
			"The same fix is now on this branch as 64ecdcf: before saving, the fork asks git for the branch's tip. " +
			"Your test fails on 17a6e44 and passes on 64ecdcf.", ReplyFixed},
		{"(Claude) Correction: you’re right. The fix is on this branch as `64ecdcf`.", ReplyFixed},
		{"You are right. It is on this branch as 64ecdcf.", ReplyFixed},
		{"Correction — the same fix landed on this branch as 64ecdcf.", ReplyFixed},
		{"Correction, on this branch as 64ecdcf.", ReplyFixed},
		// A correction that fixes nothing.
		{"Correction: you're right.", ReplyOther},
		{"(Claude) Correction: you're right. 0374ea3 landed on #27's branch, which stacks on this one, not here.", ReplyOther},
		{"Correction: the fix is not on this branch as 64ecdcf.", ReplyOther},
		{"Correction: the same fix is on this branch as 64ecdc.", ReplyOther},
		{"Correction: the fix is on the branch as 64ecdcf.", ReplyOther},
		{"The same fix is on this branch as the one in #27.", ReplyOther},
		// Controls: the acknowledgement passes any other verdict on.
		{"Correction: not a bug, the caller holds the lock.", ReplyNotABug},
		{"You're right, fixed in 64ecdcf.", ReplyFixed},
		{"You're right, left as is for now.", ReplyWontFix},
	} {
		if got := classifyReply(tc.body); got != tc.want {
			t.Errorf("classifyReply(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// Three more fixes and one decline the classifier read as other: the authors'
// agents named the commit with "added in <sha>", "removed in <sha>" and "closed
// by <what> in <sha>" after "Good catch", "Agreed" or "Valid" (nine replies of
// a window), and answered an optional finding with "Noted, not adding it" (a
// decline). A commit is needed: "added in the next PR" fixes nothing, and
// "closed as won't fix" names no commit.
func TestClassifyReplyReadsAddedRemovedClosedByCommitAndNotAdding(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		// Added or removed in a commit.
		{"(Claude) Good catch, added in 3eda937271: `order_spec.rb` 'renders a saved preview whose variants still exist'. " +
			"With the guard cut down the example fails with 404.", ReplyFixed},
		{"Good catch, added in 8e48fe1b5b.", ReplyFixed},
		{"(Claude) Agreed, removed in d803e5f4. `tcp_keepalive_timer` only checks the probe count when the user timeout is 0.", ReplyFixed},
		{"Added in 3f9e2a1.", ReplyFixed},
		{"Removed in `3f9e2a1`: the helper had no other caller.", ReplyFixed},
		{"Valid, now added in 3f9e2a1.", ReplyFixed},
		{"The guard was added in 3f9e2a1.", ReplyFixed},
		{"It was removed in 3f9e2a1.", ReplyFixed},
		{"Good catch — the test is already added in 3f9e2a1.", ReplyFixed},
		// Closed or resolved by something, in a commit.
		{"(Claude) Valid — closed by the Approve & apply route in 2397323958: the launch tool is now approval-gated.", ReplyFixed},
		{"(Claude) Valid — closed by the approval route in 2397323958 (see the line-16 thread): a deferred request " +
			"can only produce a held proposal.", ReplyFixed},
		{"Agreed, resolved by the redesign in 64b00a2.", ReplyFixed},
		{"This is closed by the retry rewrite in 64b00a2.", ReplyFixed},
		{"Resolved in 64b00a2.", ReplyFixed},
		// No commit, not done, or not this thread's.
		{"Added in the next PR.", ReplyOther},
		{"Good catch, added in a follow-up.", ReplyOther},
		{"Not added yet.", ReplyOther},
		{"Added in 12 places.", ReplyOther},
		{"Removed in a later PR.", ReplyOther},
		{"Not removed in 3f9e2a1.", ReplyOther},
		{"That is not added in 3f9e2a1 yet.", ReplyOther},
		{"Closed by the redesign in the next PR.", ReplyOther},
		{"Not closed by the redesign in 2397323958.", ReplyOther},
		{"Closed as won't fix.", ReplyOther},
		{"Resolved by the next PR.", ReplyOther},
		// Not adding it, won't add: a decline.
		{"(Claude) Noted, not adding it. The example would repeat four config values; the existing spec covers the naming logic.", ReplyWontFix},
		{"Not adding it: the spec covers the naming logic.", ReplyWontFix},
		{"Not adding.", ReplyWontFix},
		{"Good catch, but not adding it for a nightly import.", ReplyWontFix},
		{"Valid. Not adding a guard here: the caller checks.", ReplyWontFix},
		{"I'm not adding the helper; it would hide the retry.", ReplyWontFix},
		{"Won't add it, the feature is going away.", ReplyWontFix},
		{"(Claude) Valid, but won't add: the helper would hide the retry.", ReplyWontFix},
		{"I won't add a test for this; the integration spec covers it.", ReplyWontFix},
		{"We will not add the guard.", ReplyWontFix},
		{"Not adding up: the totals differ.", ReplyOther},
		{"That won't add up.", ReplyOther},
		{"I'll add a test.", ReplyOther},
		{"Good catch, I will add it.", ReplyOther},
		// Controls: a fix still wins over a later "not adding", and the older forms hold.
		{"Fixed in 1a2b3c4; not adding the larger rewrite.", ReplyFixed},
		{"Good catch, added in 3f9e2a1. Not adding the second test.", ReplyFixed},
		{"Won't fix: the feature is going away.", ReplyWontFix},
	} {
		if got := classifyReply(tc.body); got != tc.want {
			t.Errorf("classifyReply(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// A reply that names the commit of its follow-up ("Follow-up in 9aed21f: the pin is gone", talkable-proxies#250,
// 2026-10-14) is a fix, as "added in <sha>" is; a follow-up without a commit stays a deferral.
func TestClassifyReplyReadsFollowUpInACommitAsFixed(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		// A follow-up in a commit: fixed.
		{"Follow-up in 9aed21f: the pin is gone", ReplyFixed},
		{"Follow-up in 9aed21f: the `< 1.9` pin is gone, and the probe from this comment prints `200`.", ReplyFixed},
		{"Follow-up in `9aed21f`.", ReplyFixed},
		{"Follow up 9aed21f", ReplyFixed},
		{"Follow-up 9aed21f", ReplyFixed},
		{"Follow up in 9aed21f.", ReplyFixed},
		{"Followed up in 9aed21f.", ReplyFixed},
		{"followed up in `9aed21fb04`: the pin is gone", ReplyFixed},
		{"FOLLOW-UP IN 9AED21F", ReplyFixed},
		{"(Claude) Good catch, follow-up in 9aed21f.", ReplyFixed},
		{"Valid. Follow-up in 9aed21f: the pin is gone.", ReplyFixed},
		{"Follow-up in 9aed21f; not adding the larger rewrite.", ReplyFixed},
		// A follow-up without a commit: a deferral, as before.
		{"Follow-up.", ReplyWontFix},
		{"Follow-up PR", ReplyWontFix},
		{"Follow-up later.", ReplyWontFix},
		{"Follow-up ticket: ENG-12", ReplyWontFix},
		{"Follow up in a later PR.", ReplyWontFix},
		{"Follow-up in 12 places.", ReplyWontFix},
		{"Follow-up in 9aed21 (six digits are no commit)", ReplyWontFix},
		{"Deferred to a follow-up", ReplyWontFix},
		{"Good catch, out of scope: follow-up in 9aed21f", ReplyWontFix},
		// Followed up without a commit is no verdict, as "added in the next PR" is none.
		{"Followed up in the next PR.", ReplyOther},
		{"Followed up with the author.", ReplyOther},
	} {
		if got := classifyReply(tc.body); got != tc.want {
			t.Errorf("classifyReply(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// Three replies of 2026-10-17 declined a finding with "I did not make this
// change", which no pattern read: the classifier filed them `other`. A clause
// "I (or we) did not (or didn't) make, apply or take this (that, the, the
// suggested) change", alone or before "here", "in this PR" or "because", is a
// decline; one that goes on ("yet", "myself") claims nothing.
func TestAReplyThatDidNotMakeTheChangeIsAWontFix(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		// The three phrasings, paraphrased.
		{"(Claude) I did not make this change. The result stays the same, so the gain is shorter code only. " +
			"A copy of this file in #42 needs the same edit.", ReplyWontFix},
		{"(Claude) I did not make this change. This comment is on an older version of the file; in the current one " +
			"the gain is one loop fewer.", ReplyWontFix},
		{"(Claude) Correct: the provider declares the field as a set, so `distinct()` has no effect. I did not make " +
			"this change, because the result stays the same.", ReplyWontFix},
		// The forms around them.
		{"We didn't apply the suggested change: the caller already checks.", ReplyWontFix},
		{"I did not take that change here.", ReplyWontFix},
		{"Valid, but I did not make the change in this PR.", ReplyWontFix},
		{"I did not make this change because the plan stays the same.", ReplyWontFix},
		// Not a decline, or a verdict first.
		{"I did not make this change yet; I'll push it tomorrow.", ReplyOther},
		{"Good catch, I did not make this change myself, it came from the rebase. Fixed in 1a2b3c4.", ReplyFixed},
		{"Fixed in 1a2b3c4. I did not make this change in the old path.", ReplyFixed},
		{"Not a bug: I did not make this change because the lock is held.", ReplyNotABug},
		{"Why did you not make this change?", ReplyOther},
	} {
		if got := classifyReply(tc.body); got != tc.want {
			t.Errorf("classifyReply(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// Two replies of 2026-10-20 accepted a finding and sent its fix elsewhere,
// which the classifier filed `other`: "Confirmed. This is a real gap …",
// whose deferral ("Backend fix as a separate follow-up") comes paragraphs
// after the first, which alone was read, and "Thanks — all four are real …
// we are not patching them here. We'll raise them as one templates
// follow-up", whose clauses name the follow-up at their end, where the
// deferral pattern, which reads a clause's start, never looks. A clause "we
// are not patching" (or "not patching", "I'm not patching") declines; a
// clause that ends in "as a (separate, …) follow-up" (or "follow-up PR")
// with no commit defers, in the first paragraph and, after an
// acknowledgement, in a later one, unless a fixed or not-a-bug clause comes
// before it. A follow-up in a commit stays fixed (spec 60).
func TestAnAcceptedGapDeferredToAFollowUpIsAWontFix(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		// The two phrasings, paraphrased.
		{"Confirmed. This is a real gap, and it sits in the backend, not in this config.\n\n" +
			"- The callback creates only one hook.\n- The uninstall leaves the flag as it was.\n\n" +
			"Resolution in this PR: the manual step is written down in the changelog (1a2b3c4). " +
			"Backend fix as a separate follow-up: the manager also turns the flag on.", ReplyWontFix},
		{"Thanks — all four are real, and all four come from the shared template. This copy is that template " +
			"verbatim, so per repo convention we are not patching them here. We'll raise them as one templates " +
			"follow-up, fix them in the template and copy the fix to the clients:\n\n1. The renderer drops a space.", ReplyWontFix},
		// Each signal on its own.
		{"Thanks — all four are real. We'll raise them as one templates follow-up.", ReplyWontFix},
		{"This copy is the template verbatim, so per repo convention we are not patching it here.", ReplyWontFix},
		{"Not patching it here: the template owns it.", ReplyWontFix},
		{"I'm not patching the copy.", ReplyWontFix},
		{"Valid; tracking it as a follow-up PR.", ReplyWontFix},
		{"Thanks!\n\nWe'll handle it as a follow-up.", ReplyWontFix},
		{"Confirmed.\n\nThe path is real. Backend fix as a separate follow-up.", ReplyWontFix},
		// A follow-up in a commit stays fixed; a commit or a fix said first is no deferral.
		{"Follow-up in 9aed21f: the pin is gone.", ReplyFixed},
		{"Confirmed, a real gap. Follow-up in 9aed21f.", ReplyFixed},
		{"Confirmed.\n\nShipped as a follow-up in 9aed21f.", ReplyOther},
		{"Pushed as a follow-up.", ReplyOther},
		{"Confirmed.\n\nFixed in 1a2b3c4; the docs as a separate follow-up.", ReplyOther},
		// Not a deferral, or not read past the first paragraph.
		{"Good catch, added in a follow-up.", ReplyOther},
		{"The job runs as a follow-up step after the import.", ReplyOther},
		{"The guard runs first.\n\nThe cleanup goes in as a separate follow-up.", ReplyOther},
		{"Why are we not patching this?", ReplyOther},
		{"The migration is not patching the schema.", ReplyOther},
	} {
		if got := classifyReply(tc.body); got != tc.want {
			t.Errorf("classifyReply(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// 12 of the 196 replies of the 2026-10-23 window were `other` where a person
// reads a fix or a decline: three "(Claude) (Claude) Added in <sha>" and
// "Applied in <sha>" on one PR (the tag was stripped once), and bare
// declines, "Not changed here." four times on one PR, "but not changing it
// here", "No change." and "Still open, with no fix in this PR". An agent
// tag, however often it repeats, is dropped before the first word. "Not
// added" and "Not changed" (alone), "No change" (alone), "not changed here"
// ("not changing it here", after "I'm" or "we are" too) and "no fix in this
// PR" (push, round) decline the fix; "Not added yet." and "Still open, will
// look later." claim nothing, as before.
func TestClassifyReplyReadsADoubledAgentTagAndBareDeclines(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		// The tag more than once, in any mix.
		{"(Claude) (Claude) Added in 3f9e2a1: the guard now runs first.", ReplyFixed},
		{"(Claude) (Claude) Applied in 3f9e2a1.", ReplyFixed},
		{"(Claude) (Claude) Good catch, added in 3f9e2a1.", ReplyFixed},
		{"[Codex] (Claude): Fixed in 3f9e2a1.", ReplyFixed},
		{"(Claude) (Claude) (Claude) Not changed here.", ReplyWontFix},
		{"(Claude) (Claude) The tag twice and no verdict.", ReplyOther},
		// Bare declines.
		{"Not changed here.", ReplyWontFix},
		{"(Claude) Not changed here. None of the 11 notification entries contains an HTML entity.", ReplyWontFix},
		{"Not changed.", ReplyWontFix},
		{"Not added.", ReplyWontFix},
		{"Not added", ReplyWontFix},
		{"No change.", ReplyWontFix},
		{"(Claude) No change.", ReplyWontFix},
		{"Valid, but not changing it here.", ReplyWontFix},
		{"Good point, but I'm not changing it here: the helper is going away.", ReplyWontFix},
		{"We are not changing it here.", ReplyWontFix},
		{"Still open, with no fix in this PR.", ReplyWontFix},
		{"Confirmed. No fix in this push.", ReplyWontFix},
		{"Valid; there is no fix in this round.", ReplyWontFix},
		// Claims nothing: pinned before, and the near misses.
		{"Not added yet.", ReplyOther},
		{"Still open, will look later.", ReplyOther},
		{"Not changed yet.", ReplyOther},
		{"No change needed in the helper.", ReplyOther},
		{"Why is this not changed here?", ReplyOther},
		{"The fix is in this PR.", ReplyOther},
		{"Nothing changed here.", ReplyOther},
		// A fix or a not-a-bug said first still wins.
		{"Fixed in 3f9e2a1. Not changed here: the second path.", ReplyFixed},
		{"(Claude) (Claude) Fixed in 3f9e2a1; no fix in this PR for the docs.", ReplyFixed},
		{"Not a bug: not changing it here, the caller retries.", ReplyNotABug},
	} {
		if got := classifyReply(tc.body); got != tc.want {
			t.Errorf("classifyReply(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// The fixer fixes the same defect in the PR's other lines too and names each
// extra site after its Fixed-in opening (skills/magnum-fix/SKILL.md, section
// 6, 2026-10-26): the reply is still fixed, with one extra site or several,
// with a guard's spec after them, and with the fixer's marker after a blank
// line.
func TestClassifyReplyReadsAFixedInReplyThatNamesTheSameDefectsOtherSites(t *testing.T) {
	for _, body := range []string{
		"Fixed in 1a2b3c4: the session cookie now sets SameSite, also at app/models/session.rb:57, " +
			"`bundle exec rspec spec/models/session_spec.rb` passed (8 examples, 0 failures)",
		"Fixed in 1a2b3c4: the total now skips discounted items, also at app/models/order.rb:88, also at app/models/cart.rb:41, " +
			"`bundle exec rspec spec/models/order_spec.rb` passed (12 examples, 0 failures); `spec/models/order_spec.rb:42` fails without it",
		"Fixed in 1a2b3c4: the guard now runs first, also at lib/a.rb:3, `rake test` passed\n\n" + postreview.FixReplyMarker,
	} {
		if got := classifyReply(body); got != ReplyFixed {
			t.Errorf("classifyReply(%q) = %q, want %q", body, got, ReplyFixed)
		}
	}
}

func TestExcerpt(t *testing.T) {
	if s, cut := excerpt("  short  ", 600); s != "short" || cut {
		t.Fatalf("short = %q, %v", s, cut)
	}
	long := strings.Repeat("é", 700)
	s, cut := excerpt(long, 600)
	if !cut || len([]rune(s)) != 600 || !strings.HasSuffix(s, "…") {
		t.Fatalf("long: %d runes, cut %v, suffix %q", len([]rune(s)), cut, s[len(s)-3:])
	}
	if s, cut := excerpt(strings.Repeat("a", 600), 600); cut || len(s) != 600 {
		t.Fatalf("exactly 600: %d, %v", len(s), cut)
	}
}

func TestThreadSummary(t *testing.T) {
	ts := []agents.ReviewThread{
		{Resolved: true, Replies: []agents.ThreadReply{{Class: ReplyFixed}}},
		{Outdated: true, Replies: []agents.ThreadReply{{Class: ReplyNotABug}, {Own: true}, {Class: ReplyOther}}},
		{Replies: []agents.ThreadReply{{Own: true}}},
	}
	if got, want := threadSummary(ts), "3 threads (1 resolved, 1 outdated); replies: 1 fixed, 1 not a bug, 1 other; 1 thread without a reply"; got != want {
		t.Fatalf("summary = %q\nwant %q", got, want)
	}
	if got := threadSummary(ts[2:]); got != "1 thread; no replies" {
		t.Fatalf("own reply only = %q", got)
	}
	if got := threadSummary(ts[:1]); got != "1 thread (1 resolved); replies: 1 fixed" {
		t.Fatalf("one = %q", got)
	}
	if got := threadSummary(nil); got != "no threads" {
		t.Fatalf("none = %q", got)
	}
}

// The daemon's GitHub client lists threads, so live re-reviews get them.
var _ ThreadLister = (*github.Client)(nil)

// threadsGitHub is the fake GitHub that also lists review threads.
type threadsGitHub struct {
	*fakeGitHub
	threads []github.Thread
	err     error
	calls   int
}

func (g *threadsGitHub) ReviewThreads(ctx context.Context, owner, repo string, number int) ([]github.Thread, error) {
	g.calls++
	return g.threads, g.err
}

// prThreads are the PR's threads: two started by the App (one with an
// author reply, the App's own rebuttal and a long reply; one outdated
// without a line), one by a human and one by a user account that shares
// the App's login.
func prThreads() []github.Thread {
	at := t0.Add(-2 * time.Hour)
	bot := func(id int64, body string) github.ThreadComment {
		return github.ThreadComment{ID: id, AuthorLogin: "talkable", AuthorType: "Bot", Body: body, URL: "https://example.test/c", CreatedAt: at}
	}
	author := func(id int64, body string) github.ThreadComment {
		return github.ThreadComment{ID: id, AuthorLogin: "octocat", AuthorType: "User", Body: body, CreatedAt: at}
	}
	return []github.Thread{
		{ID: "PRRT_1", Path: "app/models/order.rb", Line: 42, OriginalLine: 40, Resolved: true, Comments: []github.ThreadComment{
			bot(101, "\n**[P2] Retry sends the email twice**\n\nIf the first attempt times out, ..."),
			author(102, "(Claude) Not a bug: the mailer is idempotent."),
			bot(103, "The mailer has no idempotency key at d4e5f6a; the second send goes out."),
			author(104, "Fixed in 1a2b3c4. "+strings.Repeat("x", 700)),
		}},
		{ID: "PRRT_2", Path: "app/x.rb", OriginalLine: 7, Outdated: true, Comments: []github.ThreadComment{
			bot(105, "**[P3] Wrong field name**"),
			{ID: 106, Body: "by design"}, // a ghost
		}},
		{ID: "PRRT_3", Path: "lib/y.rb", Line: 3, Comments: []github.ThreadComment{author(107, "a question"), bot(108, "an answer")}},
		{ID: "PRRT_4", Path: "lib/z.rb", Line: 9, Comments: []github.ThreadComment{
			{ID: 109, AuthorLogin: "talkable", AuthorType: "User", Body: "a user named like the App"}}},
		{ID: "PRRT_5"}, // no comments
	}
}

// rereviewInput is a second round on a new head after review 900.
func (e *env) rereviewInput() RoundInput {
	in := e.input(KindRereview)
	in.Round = 2
	in.Previous = &PreviousReview{ID: 900, Event: "CHANGES_REQUESTED", SHA: prevSHA, SubmittedAt: t0.Add(-3 * time.Hour)}
	in.Since = t0.Add(-3 * time.Hour)
	return in
}

// A re-review hands the judge the reviewer's own threads as a file next to
// the reports, every author reply classified, and names the file and the
// counts in the prompt (the .next prompt until the restart swaps it in).
func TestRereviewHandsTheJudgeItsThreads(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	text, err := os.ReadFile(filepath.Join("..", "..", "prompts", "judge-rereview.md.next"))
	if errors.Is(err, os.ErrNotExist) {
		text, err = os.ReadFile(filepath.Join("..", "..", "prompts", "judge-rereview.md"))
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "judge-rereview.md"), text, 0o600); err != nil {
		t.Fatal(err)
	}
	e.cfg.Pipeline.PromptsDir = dir
	gh := &threadsGitHub{fakeGitHub: e.gh, threads: prThreads()}
	e.r.GitHub = gh
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(601, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.rereviewInput())
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	path := filepath.Join(res.ReportDir, ThreadsFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []agents.ReviewThread
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("threads file: %v\n%s", err, b)
	}
	if len(got) != 2 || got[0].ID != "PRRT_1" || got[1].ID != "PRRT_2" {
		t.Fatalf("threads = %+v", got)
	}
	first := got[0]
	if first.CommentID != 101 || first.Finding != "**[P2] Retry sends the email twice**" || first.Location != "app/models/order.rb:42" ||
		!first.Resolved || first.Outdated || len(first.Replies) != 3 {
		t.Fatalf("first thread = %+v", first)
	}
	r := first.Replies
	if r[0].Author != "octocat" || r[0].Class != ReplyNotABug || r[0].Own || r[0].Body != "(Claude) Not a bug: the mailer is idempotent." {
		t.Errorf("author reply = %+v", r[0])
	}
	if r[1].Author != "talkable" || !r[1].Own || r[1].Class != "" {
		t.Errorf("own reply = %+v", r[1])
	}
	if r[2].Class != ReplyFixed || !r[2].Truncated || len([]rune(r[2].Body)) != replyExcerptMax {
		t.Errorf("long reply: class %q truncated %v, %d runes", r[2].Class, r[2].Truncated, len([]rune(r[2].Body)))
	}
	if second := got[1]; second.Location != "app/x.rb:7" || !second.Outdated || second.Replies[0].Author != "ghost" || second.Replies[0].Class != ReplyNotABug {
		t.Errorf("outdated thread = %+v", second)
	}

	judge := e.ag.submitsFor(agents.RoleJudge)[0].Text
	summary := "2 threads (1 resolved, 1 outdated); replies: 1 fixed, 2 not a bug"
	if strings.Contains(string(text), "ThreadsFile") {
		mustContain(t, "judge prompt", judge, "are in "+path+": "+summary+".", "threads_file: "+path+"\n")
	}
	for _, reply := range []string{"the mailer is idempotent", "Retry sends the email", "by design"} {
		if strings.Contains(judge, reply) {
			t.Errorf("the judge prompt carries thread text %q", reply)
		}
	}
	evs := eventsOfKind(e.events(), "round.threads")
	if len(evs) != 1 || evs[0].Level != "info" || !strings.Contains(evs[0].Message, summary) || strings.Contains(evs[0].Message, "idempotent") {
		t.Fatalf("threads events = %+v", evs)
	}
}

// graphQLThreadsGitHub lists the review threads through the daemon's client
// from a scripted `gh api graphql` response.
type graphQLThreadsGitHub struct {
	*fakeGitHub
	client *github.Client
}

func (g *graphQLThreadsGitHub) ReviewThreads(ctx context.Context, owner, repo string, number int) ([]github.Thread, error) {
	return g.client.ReviewThreads(ctx, owner, repo, number)
}

// threadsGraphQL is a reviewThreads page: a thread the App started in review
// 9001 on commit 1111…, with an author reply in another review, and one whose
// first comment GitHub reports without a review or a commit (it is gone).
const threadsGraphQL = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[
 {"id":"PRRT_1","isResolved":false,"isOutdated":false,"path":"app/models/order.rb","line":42,"originalLine":40,"diffSide":"RIGHT",
  "root":{"nodes":[{"originalCommit":{"oid":"1111111111111111111111111111111111111111"},"diffHunk":"@@ -40 +40 @@\n-a\n+b"}]},
  "comments":{"nodes":[
   {"databaseId":101,"body":"**[P2] Retry sends the email twice**","url":"https://github.com/talkable/talkable/pull/5#discussion_r101","createdAt":"2026-10-03T09:00:00Z","author":{"login":"talkable","__typename":"Bot"},"pullRequestReview":{"databaseId":9001}},
   {"databaseId":102,"body":"Low priority (Net 0): the mailer retries once a night.","url":"u102","createdAt":"2026-10-03T10:00:00Z","author":{"login":"alice","__typename":"User"},"pullRequestReview":{"databaseId":9002}}]}},
 {"id":"PRRT_2","isResolved":false,"isOutdated":true,"path":"app/x.rb","line":null,"originalLine":7,"diffSide":"RIGHT",
  "root":{"nodes":[{"originalCommit":null,"diffHunk":"@@ -7 +7 @@\n-a\n+b"}]},
  "comments":{"nodes":[
   {"databaseId":103,"body":"**[P3] Wrong field name**","url":"u103","createdAt":"2026-10-03T11:30:00+02:00","author":{"login":"talkable","__typename":"Bot"},"pullRequestReview":null}]}}]}}}}}`

// The threads file says when and where magnum posted each finding: the
// review of the thread's first comment, its time and the commit it was made
// on, as GitHub's GraphQL reports them, and leaves out what GitHub does not
// report.
func TestThreadsFileSaysWhenAndWhereEachFindingWasPosted(t *testing.T) {
	e := newEnv(t)
	var page bytes.Buffer
	if err := json.Compact(&page, []byte(threadsGraphQL)); err != nil {
		t.Fatal(err)
	}
	run := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "graphql"}, Result: execx.Result{Stdout: page.Bytes()}}}}
	e.r.GitHub = &graphQLThreadsGitHub{fakeGitHub: e.gh, client: &github.Client{Run: run}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(601, "COMMENTED", "COMMENT").behavior(t)}

	res, err := e.r.RunRound(e.ctx, e.rereviewInput())
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	b, err := os.ReadFile(filepath.Join(res.ReportDir, ThreadsFile))
	if err != nil {
		t.Fatal(err)
	}
	var raw []map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("threads file: %v\n%s", err, b)
	}
	if len(raw) != 2 {
		t.Fatalf("threads = %s", b)
	}
	if raw[0]["review_id"] != float64(9001) || raw[0]["created_at"] != "2026-10-03T09:00:00Z" ||
		raw[0]["commit"] != "1111111111111111111111111111111111111111" {
		t.Errorf("first thread: review_id %v, created_at %v, commit %v", raw[0]["review_id"], raw[0]["created_at"], raw[0]["commit"])
	}
	if replies, _ := raw[0]["replies"].([]any); len(replies) != 1 || replies[0].(map[string]any)["class"] != ReplyWontFix {
		t.Errorf("first thread's replies = %v", raw[0]["replies"])
	}
	if raw[1]["created_at"] != "2026-10-03T09:30:00Z" {
		t.Errorf("second thread's created_at = %v, want it in UTC", raw[1]["created_at"])
	}
	for _, key := range []string{"review_id", "commit"} {
		if v, ok := raw[1][key]; ok {
			t.Errorf("second thread has %s = %v; GitHub reported none", key, v)
		}
	}
}

// A failed read costs the round nothing: no file, a warning event, and the
// judge reads the replies itself. Initial rounds and dry runs never read.
func TestThreadsAreReadOnlyForALiveRereview(t *testing.T) {
	e := newEnv(t)
	gh := &threadsGitHub{fakeGitHub: e.gh, err: errors.New("gh api graphql: exit status 1: connection reset")}
	e.r.GitHub = gh
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(601, "COMMENTED", "COMMENT").behavior(t)}
	res, err := e.r.RunRound(e.ctx, e.rereviewInput())
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(res.ReportDir, ThreadsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("threads file after a failed read: %v", err)
	}
	evs := eventsOfKind(e.events(), "round.threads")
	if gh.calls != 1 || len(evs) != 1 || evs[0].Level != "warn" || !strings.Contains(evs[0].Message, "the judge reads the replies itself") {
		t.Fatalf("calls %d, events %+v", gh.calls, evs)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("a failed read must not warn the round: %v", res.Warnings)
	}

	for name, in := range map[string]RoundInput{"initial": newEnv(t).input(KindInitial), "dry run": func() RoundInput {
		in := newEnv(t).rereviewInput()
		in.DryRun = true
		return in
	}()} {
		e := newEnv(t)
		gh := &threadsGitHub{fakeGitHub: e.gh, threads: prThreads()}
		e.r.GitHub = gh
		post := e.judgePosts(601, "COMMENTED", "COMMENT")
		if in.DryRun {
			post.gh, post.status = nil, statusDryRun
		}
		e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}
		in.PR, in.Repo = e.pr, e.repo
		if _, err := e.r.RunRound(e.ctx, in); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if gh.calls != 0 {
			t.Errorf("%s read the threads", name)
		}
	}
}
