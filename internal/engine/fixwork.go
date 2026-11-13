package engine

// The fix loop's records and their vocabulary live in internal/fixwork,
// which the factory shares (DECISIONS "The fix loop moves into the
// factory"): the engine reads and writes the same rows under the names it
// had, through these aliases, until its fix code goes.

import (
	"context"

	"github.com/zhuravel/magnum/internal/fixwork"
	"github.com/zhuravel/magnum/internal/store"
)

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
	pushedBack  = fixwork.PushedBack
	pushbacksOf = fixwork.PushbacksOf
	sortOpen    = fixwork.SortOpen
	threadKey   = fixwork.ThreadKey
	reviewKey   = fixwork.ReviewKey
)

// FixModeCI is the mode of a fix request and loop that fixes the PR's
// failed required CI (FixPayload.Mode, FixState.Mode).
const FixModeCI = fixwork.FixModeCI

// FixReport is what the fixer reports: fixwork.FixReport.
type FixReport = fixwork.FixReport

// FixCommit is a commit the fixer says it pushed: fixwork.FixCommit.
type FixCommit = fixwork.FixCommit

// The errors of ParseFixReport: fixwork's.
var (
	// ErrFixReportMarker is the error for a report whose first line is not
	// the marker of the run asked for.
	ErrFixReportMarker = fixwork.ErrFixReportMarker
	// ErrFixReportPartial is the error for a report with a section missing,
	// empty or out of order.
	ErrFixReportPartial = fixwork.ErrFixReportPartial
)

// ParseFixReport reads the fixer's report of the run runID
// (fixwork.ParseFixReport).
func ParseFixReport(text string, runID string) (FixReport, error) {
	return fixwork.ParseFixReport(text, runID)
}

// FixNeedsAsked keeps of entries those that msg still asks the author for
// (fixwork.FixNeedsAsked).
func FixNeedsAsked(msg string, entries []string) []string { return fixwork.FixNeedsAsked(msg, entries) }

// OpenMergeIsYours reports whether GitHub's review gate g leaves the merge
// of an own PR with nothing open to the operator (fixwork.OpenMergeIsYours).
func OpenMergeIsYours(g *store.ReviewGate) bool { return fixwork.OpenMergeIsYours(g) }

// OptionalCallsWords names the optional calls whose links are links
// (fixwork.OptionalCallsWords).
func OptionalCallsWords(links []string) string { return fixwork.OptionalCallsWords(links) }

// The verdict's facts, the Needs you entry and the CI fix's facts, under
// the engine's names.
type (
	openFacts     = fixwork.Facts
	openSettle    = fixwork.Settle
	openCI        = fixwork.OpenCI
	openHold      = fixwork.Hold
	fixConflict   = fixwork.Conflict
	p3Calls       = fixwork.P3Calls
	needsYouEntry = fixwork.NeedsYouEntry
	ciFailedCheck = fixwork.CIFailedCheck
	ciFixFacts    = fixwork.CIFixFacts
)

const (
	conflictPlace = fixwork.ConflictPlace
	ciPlace       = fixwork.CIPlace
	itemCI        = fixwork.ItemCI
	ciFixMax      = fixwork.CIFixMax
	heldRestart   = fixwork.HeldRestart
	openHoldsBack = fixwork.OpenHoldsBack
)

var (
	decideOpen        = fixwork.DecideOpen
	magnumDue         = fixwork.MagnumDue
	magnumItemsOf     = fixwork.MagnumItemsOf
	decisionOf        = fixwork.DecisionOf
	reviewWork        = fixwork.ReviewWork
	openP3            = fixwork.OpenP3
	optionalSplit     = fixwork.OptionalSplit
	optionalLinks     = fixwork.OptionalLinks
	workWords         = fixwork.WorkWords
	missingWhat       = fixwork.MissingWhat
	runEnd            = fixwork.RunEnd
	shownCheckName    = fixwork.ShownCheckName
	readNeedsYouEntry = fixwork.ReadNeedsYouEntry
	ciWorkOf          = fixwork.CIWorkOf
	ciFixWords        = fixwork.CIFixWords
	ciCannotOf        = fixwork.CICannotOf
	waitClock         = fixwork.WaitClock
	quietHoursHold    = fixwork.QuietHoursHold
	quietHoursEnd     = fixwork.QuietHoursEnd
)

// The CI reader's files and steps, under the engine's names.
const (
	ciFailuresFile = fixwork.CIFailuresFile
	ciFailuresMD   = fixwork.CIFailuresMD
)

type ciFailuresJSON = fixwork.CIFailures

var (
	ciReadKey      = fixwork.CIReadKey
	ciRunNow       = fixwork.CIRunNow
	runFailed      = fixwork.RunFailed
	writeCIFiles   = fixwork.WriteCIFiles
	copyCIFiles    = fixwork.CopyCIFiles
	ciFailuresText = fixwork.CIFailuresText
)

// The checks of a push of a merge of the parent, under the engine's names.
const workflowsPath = fixwork.WorkflowsPath

type (
	isolatedGit = fixwork.IsolatedGit
	pushRefusal = fixwork.PushRefusal
)

var (
	forgedWords         = fixwork.ForgedWords
	ownWorkflowsRefusal = fixwork.OwnWorkflowsRefusal
	refusedHeadBranch   = fixwork.RefusedHeadBranch
)

// workflowRead is fixwork.WorkflowRead: read of a merge's workflow files
// in dir's objects, in a repository of the daemon's own, after every
// object of them at commits is hashed again.
func workflowRead[T any](ctx context.Context, g isolatedGit, dir string, commits []string, read func(iso string) (T, error)) (T, error) {
	return fixwork.WorkflowRead(ctx, g, dir, commits, read)
}

// HeldMergeFile is the file in the PR's report directory that shows what
// a held merge does to the workflow files (fixwork.HeldMergeFile).
const HeldMergeFile = fixwork.HeldMergeFile

// The held merge's diff, under the engine's names.
var (
	heldDiff       = fixwork.HeldDiff
	writeHeldDiff  = fixwork.WriteHeldDiff
	escapeControls = fixwork.EscapeControls
)

// The settle's confirmation of the fixer's push, under the engine's names.
const fixConfirmWindow = fixwork.ConfirmWindow

type (
	pushNotConfirmed = fixwork.PushNotConfirmed
	pushUnanswered   = fixwork.PushUnanswered
)

// The settle's filters of what a fixer left the author, under the engine's
// names.
type (
	laterWork = fixwork.LaterWork
	threadNow = fixwork.ThreadNow
)

var (
	answeredOf   = fixwork.AnsweredOf
	optionalBody = fixwork.OptionalBody
)
