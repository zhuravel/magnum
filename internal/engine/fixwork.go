package engine

// The fix loop's records and their vocabulary live in internal/fixwork,
// which the factory shares (DECISIONS "The fix loop moves into the
// factory"): the engine reads and writes the same rows under the names it
// had, through these aliases, until its fix code goes.

import "github.com/zhuravel/magnum/internal/fixwork"

// Phases of a fix loop (FixState.Phase).
const (
	// FixPhaseFixing: the fixer works on the head the cycle fixes.
	FixPhaseFixing = fixwork.FixPhaseFixing
	// FixPhaseReviewing: the fixer pushed, and a review round runs on the new head.
	FixPhaseReviewing = fixwork.FixPhaseReviewing
)

// Verdicts of autofix = "open" (OpenVerdict.Verdict).
const (
	OpenFixing   = fixwork.OpenFixing
	OpenWaits    = fixwork.OpenWaits
	OpenNeedsYou = fixwork.OpenNeedsYou
	OpenIdle     = fixwork.OpenIdle
	OpenUnknown  = fixwork.OpenUnknown
	OpenOff      = fixwork.OpenOff
)

// Why an off verdict is off (OpenVerdict.Off).
const (
	OpenOffFixOff     = fixwork.OpenOffFixOff
	OpenOffDraft      = fixwork.OpenOffDraft
	OpenOffFork       = fixwork.OpenOffFork
	OpenOffIncludeOwn = fixwork.OpenOffIncludeOwn
	OpenOffMuted      = fixwork.OpenOffMuted
	OpenOffBaseline   = fixwork.OpenOffBaseline
	OpenOffIneligible = fixwork.OpenOffIneligible
)

type (
	// FixState is where a PR's fix loop is (KVPRFix): fixwork.FixState.
	FixState = fixwork.FixState
	// FixConfirm is the push a loop's settle confirms: fixwork.FixConfirm.
	FixConfirm = fixwork.FixConfirm
	// FixCarry is the calls an earlier cycle left the author: fixwork.FixCarry.
	FixCarry = fixwork.FixCarry
	// FixOff is a PR's opt-out of the fix job (KVPRFixOff): fixwork.FixOff.
	FixOff = fixwork.FixOff
	// HeldMerge is a merge of a PR's parent held for your y (KVPRHeldMerge):
	// fixwork.HeldMerge.
	HeldMerge = fixwork.HeldMerge
	// OpenVerdict is autofix = "open"'s verdict on a PR (KVPROpen):
	// fixwork.OpenVerdict.
	OpenVerdict = fixwork.OpenVerdict
	// OpenNeed is one thing that needs the operator: fixwork.OpenNeed.
	OpenNeed = fixwork.OpenNeed
)

// ParseFixState reads a KVPRFix value; ok is false for one that is not a
// fix loop.
func ParseFixState(s string) (FixState, bool) { return fixwork.ParseFixState(s) }

// ParseFixOff reads a KVPRFixOff value; ok is false for one that is not an
// opt-out.
func ParseFixOff(s string) (FixOff, bool) { return fixwork.ParseFixOff(s) }

// ParseHeldMerge reads a KVPRHeldMerge row; false for one that names no
// merge, head, base or tip.
func ParseHeldMerge(raw string) (HeldMerge, bool) { return fixwork.ParseHeldMerge(raw) }

// ParseOpenVerdict reads a KVPROpen value; ok is false for one that is not a
// verdict.
func ParseOpenVerdict(s string) (OpenVerdict, bool) { return fixwork.ParseOpenVerdict(s) }

// The records the engine alone names, under its names.
type (
	fixPush           = fixwork.Push
	fixPushJob        = fixwork.PushJob
	fixPushMerge      = fixwork.PushMerge
	ciPush            = fixwork.CIPush
	heldFix           = fixwork.HeldFix
	optionalCall      = fixwork.OptionalCall
	openLedger        = fixwork.Ledger
	openRead          = fixwork.LedgerRead
	openConflict      = fixwork.LedgerConflict
	openApprovalCount = fixwork.ApprovalCount
	openSpell         = fixwork.Spell
	openItem          = fixwork.Item
	magnumThread      = fixwork.MagnumThread
	ciFixRecord       = fixwork.CIFixRecord
	ciFixStart        = fixwork.CIFixStart
	ciFailRead        = fixwork.CIFailRead
	ciReadRun         = fixwork.CIReadRun
	ciBaseRead        = fixwork.CIBaseRead
	ciBaseCheck       = fixwork.CIBaseCheck
	ciStart           = fixwork.CIStart
	ciRun             = fixwork.CIRun
	ciRerun           = fixwork.CIRerun
	ciNoJob           = fixwork.CINoJob
	ciAttempt         = fixwork.CIAttempt
)

const (
	fixPushConfirming  = fixwork.PushConfirming
	fixPushUnconfirmed = fixwork.PushUnconfirmed
	heldBudget         = fixwork.HeldBudget
	heldPause          = fixwork.HeldPause
	openStalePolls     = fixwork.OpenStalePolls
	classMagnum        = fixwork.ClassMagnum
	classPerson        = fixwork.ClassPerson
	classBot           = fixwork.ClassBot
	classConflict      = fixwork.ClassConflict
	itemThread         = fixwork.ItemThread
	itemReview         = fixwork.ItemReview
	itemComment        = fixwork.ItemComment
	itemMagnum         = fixwork.ItemMagnum
	itemFinding        = fixwork.ItemFinding
	itemFindings       = fixwork.ItemFindings
	itemConflict       = fixwork.ItemConflict
	ciOutcomeUnstarted = fixwork.CIOutcomeUnstarted
	ciOutcomePushed    = fixwork.CIOutcomePushed
	ciOutcomeMerged    = fixwork.CIOutcomeMerged
	ciOutcomeCannot    = fixwork.CIOutcomeCannot
	ciOutcomeFailed    = fixwork.CIOutcomeFailed
	ciOutcomeStopped   = fixwork.CIOutcomeStopped
	ciOutcomeLost      = fixwork.CIOutcomeLost
	ciBasePassed       = fixwork.CIBasePassed
	ciBaseFailed       = fixwork.CIBaseFailed
	ciBaseRunning      = fixwork.CIBaseRunning
	ciBaseNever        = fixwork.CIBaseNever
)

var (
	lastActivity  = fixwork.LastActivity
	pushedBack    = fixwork.PushedBack
	pushbacksOf   = fixwork.PushbacksOf
	acceptedWaits = fixwork.AcceptedWaits
	sortOpen      = fixwork.SortOpen
	waitsOf       = fixwork.WaitsOf
	threadKey     = fixwork.ThreadKey
	reviewKey     = fixwork.ReviewKey
)
