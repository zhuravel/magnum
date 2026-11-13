package fixwork

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/textx"
)

// Phases of a fix loop (FixState.Phase).
const (
	// FixPhaseFixing: the fixer works on the head the cycle fixes.
	FixPhaseFixing = "fixing"
	// FixPhaseReviewing: the fixer pushed, and a review round runs on the new head.
	FixPhaseReviewing = "reviewing"
)

// FixState is where a PR's fix loop is: cycle Cycle of at most Cap, in Phase.
// Since is the head the loop started from and Head the head the current cycle
// fixes (in phase reviewing, the head the fixer pushed). By says who started
// it ("magnum fix", "autofix"), Role is the fix role whose fixer runs the
// current cycle, Report is the path of the report the fixer writes, and
// StartedAt is when the loop started. NeedsYou are the findings a fixer that
// pushed left to the author (its report's "## Needs you" entries, in its own
// words, so never put in an event or a log; FixCalls reads what each asks):
// the push is re-reviewed first, and the loop ends after that round as
// needs attention, unless magnum's threads show every call settled by then
// (freshNeedsYou), when the round decides the loop as for a push that left
// nothing to the author. Answered are the comment ids of the threads of
// magnum's among them that showed their answer at the settle, whose calls
// ask only for the resolution; fixReReview writes it with NeedsYou, and an
// id whose entry is gone asks nothing. ForcedAt is when
// the loop forced the re-review of its push (phase reviewing): its
// re-review is the forced round dispatched at or after it, and every other
// round that ends while the loop waits is ignored (fixAfterRound; zero in a
// row written before it was recorded, where any forced round is). Held says
// that re-review ended and called for a next cycle a drain for a restart
// refused: no round that ends afterwards is the loop's re-review, and the
// restart's recovery, or the drain's end without one, starts that cycle
// (recoverReviewingLoop). Context is what the current cycle did to the
// fixer's conversation it resumed before its prompt, in a few words
// (fixCompact: "compacted 82% → 31%", "compacted from 82%" when the pane
// shows no figure after it, "fresh session (82% after /compact)", "fresh
// session (/compact failed)"); "" when it resumed it as it was or started a
// fresh fixer anyway. OthersUnread says that the current cycle's fixer did
// not get all the other reviewers' items (writeExternal): "not read (HTTP
// 502)", "not all read (over 1000 review threads)", a short cause and never
// PR text; the loop's end says so (othersUnread). "" when it got them.
// Carried are the calls the loop's earlier cycles left the author, which it
// carries to its end, each cycle's under its own report (FixCarry,
// fix_carry.go): a re-review that posts new findings runs the next cycle
// over them while cycles are left. Waits are the keys of other reviewers'
// items (fix_open_items.go) that an earlier loop was handed and that wait
// for the author, a person's, beside which autofix = "open" started this
// loop on work never handed (rule 9), or a CI fix, the look's or by hand,
// whose next cycles read the other reviewers: every cycle's external file
// marks them waiting, and the fixer leaves them alone (writeExternal); a
// pushback key among them (pushbackKey) names a reviewer one of whose
// items waited, and marks their change requests made since. Mode is
// what the loop does: "" fixes findings, FixModeMerge merges the PR's
// parent alone (fix_parent.go), FixModeCI fixes its failed required CI on
// the loop's first cycle (ci_fix.go; the next cycles fix the re-review's
// findings, with Mode ""), the required entries that failed in CIChecks
// (the [[repo]] list's or GitHub's rulesets', never a job's name; the
// registry's alone). ParentRefused is why the daemon did not
// push its merge of the parent on the loop's first cycle (mergeParent's
// refusal: "needs Workflows on the App", a branch it never pushes to),
// which the cycle's fixer goes on without and the loop's end puts under
// Needs you (parentRefusedNote); "" when it merged or had nothing to merge.
// Confirm is the push the cycle's settle confirms on GitHub while GitHub
// does not answer (fix_confirm.go): the loop is still at work, in phase
// fixing, and the screens say so (Confirming); nil otherwise. Late says the
// cycle's push was settled late, after its fix failed for want of it: the
// toasts of that settle and of the loop's end after it have keys of their
// own, apart from the failure's (fixAttentionKey).
type FixState struct {
	Cycle         int         `json:"cycle"`
	Cap           int         `json:"cap"`
	Phase         string      `json:"phase"`
	Since         string      `json:"since"`
	Head          string      `json:"head"`
	By            string      `json:"by"`
	Report        string      `json:"report"`
	StartedAt     time.Time   `json:"started_at"`
	NeedsYou      []string    `json:"needs_you,omitempty"`
	Answered      []string    `json:"answered,omitempty"`
	Role          string      `json:"role,omitempty"`
	ForcedAt      time.Time   `json:"forced_at,omitzero"`
	Held          bool        `json:"held,omitempty"`
	Context       string      `json:"context,omitempty"`
	OthersUnread  string      `json:"others_unread,omitempty"`
	Carried       []FixCarry  `json:"carried,omitempty"`
	Waits         []string    `json:"waits,omitempty"`
	Mode          string      `json:"mode,omitempty"`
	CIChecks      []string    `json:"ci_checks,omitempty"`
	ParentRefused string      `json:"parent_refused,omitempty"`
	Confirm       *FixConfirm `json:"confirm,omitempty"`
	Late          bool        `json:"late,omitempty"`
}

// FixConfirm is the fixer's push a fix loop's settle confirms on GitHub
// while GitHub does not answer: the pushed head and when GitHub first did
// not answer.
type FixConfirm struct {
	Head  string    `json:"head"`
	Since time.Time `json:"since"`
}

// Confirming says that the loop's settle confirms its fixer's push while
// GitHub does not answer: "confirming the push of e6c1f37 (GitHub does not
// answer)"; "" when it does not.
func (s FixState) Confirming() string {
	if s.Confirm == nil {
		return ""
	}
	return "confirming the push of " + textx.ShortSHA(s.Confirm.Head) + " (GitHub does not answer)"
}

// AnsweredSet is Answered as a set (nil for none), as needsYouCalls reads it.
func (s FixState) AnsweredSet() map[string]bool {
	if len(s.Answered) == 0 {
		return nil
	}
	set := make(map[string]bool, len(s.Answered))
	for _, id := range s.Answered {
		set[id] = true
	}
	return set
}

// FixOff is a PR's opt-out of the fix job: when it was set and by whom
// ("magnum fix --off").
type FixOff struct {
	Since time.Time `json:"since"`
	By    string    `json:"by"`
}

// ParseFixState reads a KVPRFix value; ok is false for one that is not a
// fix loop.
func ParseFixState(s string) (FixState, bool) {
	var st FixState
	if s == "" || json.Unmarshal([]byte(s), &st) != nil || st.Cycle < 1 || st.Phase == "" {
		return FixState{}, false
	}
	return st, true
}

// Short is the loop while its fixer works, as the board's state cell shows
// it: "fixing 1/3 · claude-fix" ("fixing 1/3" when the role is not known),
// "fixing 1/3 · confirming e6c1f37" while its settle confirms the push
// (Confirm).
func (s FixState) Short() string {
	short := fmt.Sprintf("fixing %d/%d", s.Cycle, s.Cap)
	if s.Confirm != nil {
		return short + " · confirming " + textx.ShortSHA(s.Confirm.Head)
	}
	if s.Role == "" {
		return short
	}
	return short + " · " + s.Role
}

// ParseFixOff reads a KVPRFixOff value; ok is false for one that is not an
// opt-out.
func ParseFixOff(s string) (FixOff, bool) {
	var o FixOff
	if s == "" || json.Unmarshal([]byte(s), &o) != nil {
		return FixOff{}, false
	}
	return o, true
}

// FixCarry is the calls one earlier cycle of a fix loop left the author
// (FixState.Carried): the cycle's report, beside which its files place the
// threads, the entries of its "## Needs you" still asked (in the fixer's
// own words, so never put in an event or a log), and the comment ids among
// them whose threads showed their answer and ask only for the resolution
// (as FixState.Answered).
type FixCarry struct {
	Report   string   `json:"report"`
	NeedsYou []string `json:"needs_you"`
	Answered []string `json:"answered,omitempty"`
}

// AnsweredSet is Answered as a set (nil for none).
func (c FixCarry) AnsweredSet() map[string]bool {
	return FixState{Answered: c.Answered}.AnsweredSet()
}

// Kinds of a fix's push record (Push.Kind).
const (
	// PushConfirming: the settle confirms the push (its own retries,
	// then the ticks', up to fixConfirmCap).
	PushConfirming = "confirming"
	// PushUnconfirmed: the fix failed only because its push was not
	// confirmed on GitHub; the poll settles it late when GitHub shows the
	// pushed head (settlePushes).
	PushUnconfirmed = "push_unconfirmed"
)

// Push is a fixer's push the settle confirms (Kind PushConfirming)
// or could not confirm (PushUnconfirmed): the run, the pushed head (the
// whole SHA), the short SHAs of the commits the fixer's report lists as
// pushed (Commits: an earlier one GitHub shows is no other head, only one
// not there yet), when the settle began to confirm it (Since, which
// fixConfirmCap counts from), when the fix failed (Failed), whether the
// operator typed into the fixer's pane during the run, whether the settle
// looked for the author's replies without the fix marker in the threads
// (RepliesNamed) and on the conversation (ConversationNamed): a later
// settle looks where it could not, and only there, and the job as the
// settle had it, which a later settle rebuilds (pushJob).
type Push struct {
	Kind              string    `json:"kind"`
	Run               string    `json:"run"`
	Pushed            string    `json:"pushed"`
	Commits           []string  `json:"commits,omitempty"`
	Since             time.Time `json:"since"`
	Failed            time.Time `json:"failed,omitzero"`
	Typed             bool      `json:"typed,omitempty"`
	RepliesNamed      bool      `json:"replies_named,omitempty"`
	ConversationNamed bool      `json:"conversation_named,omitempty"`
	Job               PushJob   `json:"job"`
}

// PushJob is what a settle reads of its fix job beyond the registry's
// rows (fixJob's fields of the same names).
type PushJob struct {
	State       FixState   `json:"state"`
	Role        string     `json:"role"`
	Head        string     `json:"head"`
	Started     time.Time  `json:"started"`
	Login       string     `json:"login,omitempty"`
	Mode        string     `json:"mode,omitempty"`
	Conflict    *PushMerge `json:"conflict,omitempty"`
	Merged      *PushMerge `json:"merged,omitempty"`
	ForMerge    bool       `json:"for_merge,omitempty"`
	Alone       bool       `json:"alone,omitempty"`
	Described   bool       `json:"described,omitempty"`
	BaseChecked bool       `json:"base_checked,omitempty"`
}

// PushMerge is a conflict of the head with its base (the engine's
// fixConflict) as a record keeps it.
type PushMerge struct {
	Base  string   `json:"base"`
	SHA   string   `json:"sha"`
	Paths []string `json:"paths,omitempty"`
}

// EarlierPush reports whether head, the PR's head GitHub shows, is one of
// the commits rec's fixer pushed before its last (Push.Commits): GitHub
// has not got to the last yet.
func (rec Push) EarlierPush(head string) bool {
	return head != rec.Pushed && slices.ContainsFunc(rec.Commits, func(c string) bool { return len(c) >= 7 && strings.HasPrefix(head, c) })
}

// HeldMerge is a merge of a PR's parent that magnum holds for your y: the
// PR it was held for (Repo, owner/name, and Number: checked against the
// registry's rows, never a path's source), the merge commit (Merge), the
// PR's head on GitHub it merges into (Head, its first parent), the base
// branch and the commit of it it merges (Base, Tip: its second parent, the
// base's tip on GitHub when it was held), the workflow files its push
// changes against the head, how many files the fixer resolved conflicts in
// (Conflicts; 0 when not known), the fix role whose fixer resolved it, the
// identity that pushes it and its login, the clone that holds its ref, its
// held-merge.diff (absolute), whether a fix run's findings wait for the
// next fix after the push (Findings), the words the PR's attention starts
// with while it is held (Attention) and when it was held.
type HeldMerge struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Merge     string    `json:"merge"`
	Head      string    `json:"head"`
	Base      string    `json:"base"`
	Tip       string    `json:"tip"`
	Workflows []string  `json:"workflows"`
	Conflicts int       `json:"conflicts,omitempty"`
	Role      string    `json:"role"`
	Identity  string    `json:"identity"`
	Login     string    `json:"login"`
	Clone     string    `json:"clone"`
	Diff      string    `json:"diff"`
	Findings  bool      `json:"findings,omitempty"`
	Attention string    `json:"attention"`
	At        time.Time `json:"at"`
}

// ParseHeldMerge reads a KVPRHeldMerge row; false for one that names no
// merge, head, base or tip.
func ParseHeldMerge(raw string) (HeldMerge, bool) {
	var h HeldMerge
	if json.Unmarshal([]byte(raw), &h) != nil || h.Merge == "" || h.Head == "" || h.Tip == "" || h.Base == "" {
		return HeldMerge{}, false
	}
	return h, true
}

// CIPush is a head magnum pushed to a PR, and when.
type CIPush struct {
	Head string    `json:"head"`
	At   time.Time `json:"at"`
}

// HeldFix is a held fix: when it was asked, the PR's head then (the head
// the review it would fix stood at), who asked (fixBy, whose loop unpins a
// pinned PR as the request would have; autofixBy for an autofix the GitHub
// floor held) and why it is held ("" for a drain, HeldBudget for the GitHub
// budget, HeldPause for the fixer's paused agent kind, Kind).
type HeldFix struct {
	At   time.Time `json:"at"`
	Head string    `json:"head"`
	By   string    `json:"by"`
	Why  string    `json:"why,omitempty"`
	Kind string    `json:"kind,omitempty"`
}

const (
	// HeldBudget is the HeldFix.Why of a fix the GitHub budget held.
	HeldBudget = "github_budget"
	// HeldPause is the HeldFix.Why of a fix the pause of its fixer's agent
	// kind (HeldFix.Kind) held.
	HeldPause = "kind_pause"
)

// OptionalCall is a P3 of magnum's that a fixer left the author as an
// optional call: the id of its thread's first comment, the thread's link
// on the PR's page, and when the settle took the call (At): the author's
// own word in the thread after it is a decision on it, as on any call the
// fixer left (fixerCalls), which `magnum fix --decide` posts.
type OptionalCall struct {
	Thread int64     `json:"thread"`
	URL    string    `json:"url"`
	At     time.Time `json:"at,omitzero"`
}
