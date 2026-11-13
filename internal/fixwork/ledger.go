package fixwork

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/store"
)

// Verdicts of autofix = "open" (OpenVerdict.Verdict).
const (
	OpenFixing   = "fixing"    // a fix loop runs, or the look starts one
	OpenWaits    = "waits"     // something will move on its own: magnum's review, the settle, CI, the budget
	OpenNeedsYou = "needs_you" // only the operator can move it
	OpenIdle     = "idle"      // nothing open at the head
	OpenUnknown  = "unknown"   // the look has not read GitHub since the PR changed: never idle
	OpenOff      = "off"       // the look does not cover the PR (OpenVerdict.Off says why)
)

// Why an off verdict is off (OpenVerdict.Off).
const (
	OpenOffFixOff     = "fix_off"     // `magnum fix --off`
	OpenOffDraft      = "draft"       // a draft PR
	OpenOffFork       = "fork"        // its head lives in a fork
	OpenOffIncludeOwn = "include_own" // its watch reviews no own PRs
	OpenOffMuted      = "muted"       // `magnum mute`: a mute stops every automatic round
	OpenOffBaseline   = "baseline"    // open before magnum first synced the repository
	OpenOffIneligible = "ineligible"  // magnum does not review it
)

// Classes of an open item (Item.Class): whose word it is.
const (
	ClassMagnum   = "magnum"   // magnum's review, thread or finding
	ClassPerson   = "person"   // another reviewer who is not a bot
	ClassBot      = "bot"      // another reviewer that is a bot (github.IsBot)
	ClassConflict = "conflict" // a conflict of the head with its base
)

// Kinds of an open item (Item.Kind).
const (
	ItemThread   = "thread"   // another reviewer's review thread
	ItemReview   = "review"   // another reviewer's review body
	ItemComment  = "comment"  // another reviewer's conversation comment
	ItemMagnum   = "magnum"   // magnum's thread whose rebuttal waits for an answer
	ItemFinding  = "finding"  // a finding of a handed review the fixer skipped without a word
	ItemFindings = "findings" // magnum's review with work for the fixer
	ItemConflict = "conflict"
)

// OpenVerdict is autofix = "open"'s verdict on a PR at Head (KVPROpen):
// fixing, waits, needs_you, idle, unknown or off, why in plain words
// (counts, kinds, short SHAs and clock times, never PR text), when the
// look decided it (At), since when it has said the same without a break
// (Since: the same verdict, approval and reason to be off, whatever the
// words and however often the PR changed; Since and At are equal when it
// began, and zero in a row of a build before it), when it last read GitHub
// whole (ReadAt), when a wait ends where that is known (Until), the open
// items (work and what needs the
// operator) and what needs the operator (kind and link only), the short SHA
// a person approved ("" = none) and the commits on Head after it (-1 = not
// counted), and why an off verdict is off.
type OpenVerdict struct {
	Verdict       string     `json:"verdict"`
	Why           string     `json:"why"`
	Head          string     `json:"head"`
	At            time.Time  `json:"at"`
	Since         time.Time  `json:"since,omitzero"`
	ReadAt        time.Time  `json:"read_at,omitzero"`
	Until         time.Time  `json:"until,omitzero"`
	Open          int        `json:"open"`
	Needs         []OpenNeed `json:"needs,omitempty"`
	Approved      string     `json:"approved,omitempty"`
	AfterApproval int        `json:"after_approval,omitempty"`
	Off           string     `json:"off,omitempty"`
	// Optional are the links of the operator's optional calls an idle
	// verdict names (fix_optional.go): P3s whose Fix leaves the choice to
	// the operator, which hold nothing and wait for them at the merge.
	Optional []string `json:"optional,omitempty"`
	// Leftovers: the verdict needs you for what loops left (rule 8), which
	// the look raises to needs_attention; Late: for magnum's late review
	// (rule 5), which it toasts once per head; Dispatch: for a required
	// check that did not run, whose workflow the look starts (rule 11,
	// openStartCI). None is stored.
	Leftovers bool `json:"-"`
	Late      bool `json:"-"`
	Dispatch  bool `json:"-"`
	// ReadRuns: rule 11 reads the runs at the head (openReadRuns,
	// ci_rerun.go); Rerun: it re-runs the head's run that started no job
	// (openRerunCI); ReadCIBase: it reads the base's CI of a failed
	// required check (readCIBase, ci_fix.go); RecheckCIRead: it checks a
	// read that listed no failed job of a run it named against the head's
	// runs (ensureCIRead, ci_fix.go). None is stored.
	ReadRuns      bool `json:"-"`
	Rerun         bool `json:"-"`
	ReadCIBase    bool `json:"-"`
	RecheckCIRead bool `json:"-"`
}

// OpenNeed is one thing that needs the operator: its kind (thread, review,
// comment, magnum, finding, conflict, ci for a required check that ran and
// failed, ci_missing for one that did not run at the head, loop or
// magnum_late) and link.
type OpenNeed struct {
	Kind string `json:"kind"`
	URL  string `json:"url,omitempty"`
}

// ParseOpenVerdict reads a KVPROpen value; ok is false for one that is not a
// verdict.
func ParseOpenVerdict(s string) (OpenVerdict, bool) {
	var v OpenVerdict
	if s == "" || json.Unmarshal([]byte(s), &v) != nil || v.Verdict == "" || v.Head == "" {
		return OpenVerdict{}, false
	}
	return v, true
}

// Stale reports whether v no longer tells the PR's state, for a reader
// (the factory's screen) that then shows the PR as looked at: the start of
// the daemon's last tick (lastTick, zero for none) is older than four poll
// intervals (ticks start an interval plus a tick's length apart, so two
// would call a long tick a stop), the PR's head moved past v's, or the
// PR's row changed after v was decided (prUpdated).
func (v OpenVerdict) Stale(prHead string, prUpdated, lastTick, now time.Time, poll time.Duration) bool {
	return lastTick.IsZero() || now.Sub(lastTick) > OpenStalePolls*poll || v.Head != prHead || v.At.Before(prUpdated)
}

// GoesOn reports whether v goes on saying what prev said, so it keeps
// prev's Since: the same verdict, the same approval (a person's or none)
// and the same reason to be off; the words, the counts and the head may
// change.
func (v OpenVerdict) GoesOn(prev OpenVerdict) bool {
	return v.Verdict == prev.Verdict && (v.Approved == "") == (prev.Approved == "") && v.Off == prev.Off
}

// SameAs reports whether v and w say the same, whenever each was decided.
func (v OpenVerdict) SameAs(w OpenVerdict) bool {
	v.At, w.At = time.Time{}, time.Time{}
	a, _ := json.Marshal(v)
	b, _ := json.Marshal(w)
	return bytes.Equal(a, b)
}

// OpenStalePolls is how many poll intervals since the daemon's last
// sign of polling (a watch's last good poll for the look, the last
// tick's start for a verdict's reader) make it behind: the run loop
// waits a whole interval after each tick ends, so ticks start an
// interval plus a tick's length apart, and one failed poll puts the
// last good one two such spans back.
const OpenStalePolls = 4

// Ledger is what the look remembers of a PR (kvPRFixSeen): the keys
// the fixers of finished loops had (Handed, with when the look saw that
// loop end), magnum's reviews a loop worked on (Reviews), the heads the
// fixer pushed (FixerHeads, the last openFixerHeadsMax), the PR's last
// event the look read for the loops that ended (Cursor), when the look
// began to track its loops (Since: its first look at the PR, or the start
// of the loop that ran then; a loop that started before hands nothing), the last_error
// its own raise wrote (RaisedMsg) and the keys it raised (Raised), the
// autofix starts of the last 24 hours (Starts), its last read of GitHub
// (Read) and of the base (Conflict), the failed reads in a row
// (ReadFails), the head whose late review it toasted, the commits after a
// person's approval (Approval), and the fingerprint at which prepareFix
// refused the look's start as nothing to fix, with its sentence (Refused,
// RefusedWhy: not tried again until the PR changes), and when the fix job
// last refused it otherwise, with its sentence (WaitAt, WaitWhy: tried
// again openReadRetry later), and the PR's current spell of activity
// (Spell, the settle's).
type Ledger struct {
	Handed      map[string]time.Time `json:"handed,omitempty"`
	Reviews     []string             `json:"reviews,omitempty"`
	FixerHeads  []string             `json:"fixer_heads,omitempty"`
	Cursor      int64                `json:"cursor,omitempty"`
	Since       time.Time            `json:"since,omitzero"`
	RaisedMsg   string               `json:"raised_msg,omitempty"`
	Raised      []string             `json:"raised,omitempty"`
	Starts      []time.Time          `json:"starts,omitempty"`
	Read        LedgerRead           `json:"read"`
	Conflict    LedgerConflict       `json:"conflict"`
	ReadFails   int                  `json:"read_fails,omitempty"`
	LateToasted string               `json:"late_toasted,omitempty"`
	Approval    ApprovalCount        `json:"approval,omitzero"`
	Refused     string               `json:"refused,omitempty"`
	RefusedWhy  string               `json:"refused_why,omitempty"`
	WaitAt      time.Time            `json:"wait_at,omitzero"`
	WaitWhy     string               `json:"wait_why,omitempty"`
	Spell       Spell                `json:"spell,omitzero"`
	// Waited are the reviewers (Account form, lower case) one of whose
	// items waited for the author, with when the first began to: a loop's
	// hand-off of it (seedWaited), else the look that first saw it wait
	// (noteWaited). A change request of theirs since waits for the author
	// too (pushback).
	Waited map[string]time.Time `json:"waited,omitempty"`
}

// LedgerRead is the look's last read of GitHub: the PR's read fingerprint
// then (openPrint), when it read, at which head, what GitHub cut short, the
// other reviewers' items and magnum's threads it found, and when a read
// failed last (zero: the last read did not fail).
type LedgerRead struct {
	Print    string         `json:"print,omitempty"`
	At       time.Time      `json:"at,omitzero"`
	Head     string         `json:"head,omitempty"`
	Cut      []string       `json:"cut,omitempty"`
	Others   []Item         `json:"others,omitempty"`
	Mine     []MagnumThread `json:"mine,omitempty"`
	FailedAt time.Time      `json:"failed_at,omitzero"`
}

// LedgerConflict is the look's last check of the head against its base: the
// head and the base, the base's tip it fetched (also of a clean merge: the
// base's CI is read there, ci_fix.go), when, and the conflict (no paths: a
// clean merge; Unchecked: the check failed).
type LedgerConflict struct {
	Head      string    `json:"head,omitempty"`
	Base      string    `json:"base,omitempty"`
	Tip       string    `json:"tip,omitempty"`
	At        time.Time `json:"at,omitzero"`
	Paths     []string  `json:"paths,omitempty"`
	Unchecked bool      `json:"unchecked,omitempty"`
}

// ApprovalCount is the count of the commits on Head after the approved
// commit SHA (-1: not counted).
type ApprovalCount struct {
	SHA   string `json:"sha,omitempty"`
	Head  string `json:"head,omitempty"`
	After int    `json:"after,omitempty"`
}

// Spell is the PR's current spell of activity as the look saw it:
// when it began (Start: the activity the look first saw within
// settle_quiet) and when the look last saw it go on (Seen); zero while the
// PR is quiet. The settle's cap counts from its start on a head older than
// it (settleDone), so quiet still holds on an old head.
type Spell struct {
	Start time.Time `json:"start,omitzero"`
	Seen  time.Time `json:"seen,omitzero"`
}

// Next is s at the look at now on pr: none once the PR is quiet (no
// activity, its head's move included, for quiet) or while a loop runs or
// is held on it (its fixer's activity is the loop's; the spell after it
// begins when the look first sees it again without one); a new one at the
// PR's last activity when none was going on, or when the look did not see
// it go on for gap (the daemon slept, and the PR may have been quiet in
// between: the caller's gap is at least quiet, and longer than looks a few
// poll intervals apart); else s, seen now.
func (s Spell) Next(pr store.PR, quiet, gap time.Duration, now time.Time, looping bool) Spell {
	activity := LastActivity(pr)
	if looping || !now.Before(activity.Add(quiet)) {
		return Spell{}
	}
	if s.Start.IsZero() || now.Sub(s.Seen) >= gap {
		s.Start = activity
	}
	s.Seen = now
	return s
}

// LastActivity is pr's last activity: its head's move, or a later one.
func LastActivity(pr store.PR) time.Time {
	if pr.ActivityAt != nil && pr.ActivityAt.After(pr.HeadChangedAt) {
		return *pr.ActivityAt
	}
	return pr.HeadChangedAt
}

// HandedReview reports whether a loop worked on magnum's review of run.
func (l Ledger) HandedReview(run string) bool { return slices.Contains(l.Reviews, ReviewKey(run)) }

// StartsSince are the autofix starts at or after since.
func (l Ledger) StartsSince(since time.Time) int {
	n := 0
	for _, at := range l.Starts {
		if !at.Before(since) {
			n++
		}
	}
	return n
}

// RaisedHere reports whether pr needs attention of kind for what the look
// raised itself: kind fix, last_error the look's message, keys it raised.
// A loop's or a round's Needs you is not.
func (l Ledger) RaisedHere(pr store.PR, kind string) bool {
	return pr.State == store.PRNeedsAttention && kind == attention.KindFix && l.RaisedMsg != "" && store.Deref(pr.LastError) == l.RaisedMsg &&
		len(l.Raised) > 0
}

// leftToAuthor reports whether it, once still open, waits for the author
// rather than a fixer where SortOpen sorts it: a loop was handed it
// (Handed), or it is a reviewer's pushback.
func (l Ledger) leftToAuthor(it Item) bool {
	_, handed := l.Handed[it.Key]
	return handed || l.pushback(it)
}

// pushback reports whether it is a person's review body that requests
// changes made after one of that reviewer's items began to wait for the
// author (PushedBack): they push back on the author's answer, or ask
// again, which only the author can settle. Their plain comment is no
// pushback: it is an item of its own, never handed, so work.
func (l Ledger) pushback(it Item) bool {
	return it.Class == ClassPerson && PushedBack(l.Waited, it.By, it.ChangesAt)
}

// PushedBack reports whether a change request by by whose latest word
// came at at (zero: no change request) is their pushback: made after
// waited's time for them (Ledger.Waited), when one of their items
// began to wait for the author (a loop's hand-off of it, seedWaited). One
// made before is a request of its own, which a fixer works on, though the
// look reads their item only later: it came while the loop that left the
// item ran, or before.
func PushedBack(waited map[string]time.Time, by string, at time.Time) bool {
	since, ok := waited[strings.ToLower(by)]
	return ok && !at.IsZero() && at.After(since)
}

// pushbackPrefix starts a pushbackKey.
const pushbackPrefix = "changes:"

// pushbackKey is the key, among a loop's FixState.Waits, of by's pushback
// since since (Waited): "changes:rev-ann@<unix nanoseconds>". It names no
// item: every cycle's external file marks each change request of theirs
// made after since waiting (externalItems.waiting), so one that comes
// while the loop runs reaches no fixer either.
func pushbackKey(by string, since time.Time) string {
	return pushbackPrefix + strings.ToLower(by) + "@" + strconv.FormatInt(since.UnixNano(), 10)
}

// PushbacksOf are the reviewers and times of the pushback keys among keys
// (pushbackKey), as Waited holds them; nil for none.
func PushbacksOf(keys []string) map[string]time.Time {
	var out map[string]time.Time
	for _, k := range keys {
		rest, ok := strings.CutPrefix(k, pushbackPrefix)
		i := strings.LastIndexByte(rest, '@')
		if !ok || i <= 0 {
			continue
		}
		ns, err := strconv.ParseInt(rest[i+1:], 10, 64)
		if err != nil {
			continue
		}
		if out == nil {
			out = map[string]time.Time{}
		}
		out[rest[:i]] = time.Unix(0, ns)
	}
	return out
}

// LoopWaits are the FixState.Waits of a loop the look starts beside needs:
// the keys of needs and of the items it would wait for you on that their
// reviewer accepted (AcceptedWaits), then a pushback key for each reviewer
// of Waited (pushbackKey), sorted.
func (l Ledger) LoopWaits(items, needs []Item) []string {
	keys := WaitsOf(append(slices.Clone(needs), AcceptedWaits(items, l)...))
	var pushbacks []string
	for by, since := range l.Waited {
		pushbacks = append(pushbacks, pushbackKey(by, since))
	}
	slices.Sort(pushbacks)
	return append(keys, pushbacks...)
}

// LastReadWaits are the FixState.Waits of a loop that starts outside the
// look (a CI fix by hand, fix_ci.go): loopWaits over the other reviewers'
// items of the look's last read (Read.Others), seeded and sorted as the
// look seeds and sorts them (seedWaited, SortOpen) on a copy of l's
// reviewers, so its next cycles leave alone what a loop the look starts
// would. The key of an item that moved since that read marks nothing.
func (l Ledger) LastReadWaits() []string {
	l.Waited = maps.Clone(l.Waited)
	l.SeedWaited(l.Read.Others)
	_, needs := SortOpen(l.Read.Others, l)
	return l.LoopWaits(l.Read.Others, needs)
}

// SeedWaited records in l.Waited, before the look sorts items, the
// reviewer of each person's item of items that a loop was handed, from the
// earliest time a loop handed one of theirs (Handed: when openLoopsEnded
// saw that loop end). Their change requests since are pushback from the
// first look whose read shows the item, though a reply magnum had not
// re-decided, a low budget or failed reads kept the look from seeing it
// wait before; one made while that loop ran stays work.
func (l *Ledger) SeedWaited(items []Item) {
	for _, it := range items {
		if at, ok := l.Handed[it.Key]; ok {
			l.waitedFrom(it, at)
		}
	}
}

// NoteWaited records in l.Waited, at now the first time, the reviewers
// of the person's items of needs, which wait for the author (a thread a
// fixer resolved, never handed, too). A reviewer stays there for the PR's
// life: an approval of theirs and a new change request can come in one
// read.
func (l *Ledger) NoteWaited(needs []Item, now time.Time) {
	for _, it := range needs {
		l.waitedFrom(it, now)
	}
}

// waitedFrom records in l.Waited that the reviewer of it, a person's item,
// waited for the author from at on, unless an earlier time stands.
func (l *Ledger) waitedFrom(it Item, at time.Time) {
	by := strings.ToLower(it.By)
	if it.Class != ClassPerson || by == "" || by == "ghost" {
		return
	}
	if since, ok := l.Waited[by]; ok && !at.Before(since) {
		return
	}
	if l.Waited == nil {
		l.Waited = map[string]time.Time{}
	}
	l.Waited[by] = at
}

// acceptedLeftover reports whether it would wait for you (left to you,
// a thread a fixer resolved, one the fixer answered twice) but its
// reviewer approved the PR since (Item.Accepted): it waits for no one
// (SortOpen). An item never handed is work, accepted or not.
func (l Ledger) acceptedLeftover(it Item) bool {
	return it.Accepted && (l.leftToAuthor(it) || it.Resolved || it.Kind == ItemThread && it.FixerReplies >= 2)
}

// AcceptedWaits are the items of items that would wait for you and whose
// reviewer approved the PR since (acceptedLeftover): no longer work or
// needs, they are still left alone by a loop's fixer, as the ones that wait
// are (WaitsOf), since an earlier fixer left them to you. Its file lists
// them unanswered, and a fixer that answered one would answer for you, or
// name it under Needs you, a loop's own attention the look never lifts.
func AcceptedWaits(items []Item, l Ledger) []Item {
	return slices.DeleteFunc(slices.Clone(items), func(it Item) bool { return !l.acceptedLeftover(it) })
}

// SortOpen splits items by the ledger: work, never handed (and magnum's
// review no loop worked on), and needs, handed and still open (a person's,
// magnum's, a conflict, a bot's thread), every finding a fixer skipped
// without a word, every thread a fixer resolved with no answer (no loop
// can be handed it: the fixer's file leaves resolved threads out) and every
// thread a fixer answered twice whose reviewer answered again (never work
// again). A handed bot's review body or comment is seen: a receipt, a
// summary or a note, it drops out.
func SortOpen(items []Item, l Ledger) (work, needs []Item) {
	for _, it := range items {
		handed := l.leftToAuthor(it) // fix_open_needs.go: handed to a loop, or a reviewer's pushback
		switch {
		case it.Kind == ItemFindings:
			if !l.HandedReview(strings.TrimPrefix(it.Key, "review:")) {
				work = append(work, it)
			}
		case l.acceptedLeftover(it): // fix_open_needs.go: its reviewer approved since, so it waits for no one
		case it.Kind == ItemFinding, it.Resolved:
			needs = append(needs, it)
		case it.Kind == ItemThread && it.FixerReplies >= 2:
			needs = append(needs, it)
		case handed && it.Class == ClassBot && it.Kind != ItemThread:
		case handed:
			needs = append(needs, it)
		default:
			work = append(work, it)
		}
	}
	return work, needs
}

// WaitsOf are the keys of the items of needs that a fixer's external file
// lists (another reviewer's open thread, review body or comment: not one a
// fixer resolved, which the file leaves out), sorted: the loop the look
// starts beside them marks them waiting (FixState.Waits).
func WaitsOf(needs []Item) []string {
	var keys []string
	for _, it := range needs {
		if it.Class == ClassPerson && !it.Resolved && (it.Kind == ItemThread || it.Kind == ItemReview || it.Kind == ItemComment) {
			keys = append(keys, it.Key)
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// Item is one thing open on a PR: its ledger key, whose word it is, its
// kind and link, and, for another reviewer's thread, how many fix-marked
// replies the fixer gave in it (the thread's first and newest comments
// only: a long thread can undercount, which errs toward work), whether
// a fixer login resolved it (Resolved: the fixer's file leaves resolved
// threads out, so it is never work), and whether its last comment is your
// decision on the fixer's call there (Decision, fix_decide.go: its key
// takes your reply, so it is new work).
type Item struct {
	Key          string `json:"key"`
	Class        string `json:"class"`
	Kind         string `json:"kind"`
	URL          string `json:"url,omitempty"`
	FixerReplies int    `json:"fixer_replies,omitempty"`
	Resolved     bool   `json:"resolved,omitempty"`
	Decision     bool   `json:"decision,omitempty"`
	// By is another reviewer's item's reviewer (Account form, "ghost" for
	// none; a thread's first comment's author), and ChangesAt, for a review
	// body that requests changes, when it got its latest word (its last
	// edit): their pushback when one of their items waited for you before
	// it (Ledger.pushback). Zero for every other item.
	By        string    `json:"by,omitempty"`
	ChangesAt time.Time `json:"changes_at,omitzero"`
	// Accepted says that the reviewer of another reviewer's item approved
	// the PR after its latest word (acceptedBy): one that would wait for
	// you waits for no one (SortOpen).
	Accepted bool `json:"accepted,omitempty"`
}

// MagnumThread is an unresolved review thread one of magnum's logins
// started: its first comment's id and link, the review it belongs to, its
// latest comment's id and time, whether that latest comment is magnum's
// rebuttal (a reply of magnum's whose marker says rebuttal or that has
// none; a fixer's reply or a chat session's magnum comment is none), and
// whether it was answered: a reply by the PR author, or a fix-marked reply
// by one of magnum's logins, the latest of them made at AnsweredAt (a
// magnum comment answers none of magnum's own findings).
type MagnumThread struct {
	RootID     int64     `json:"root_id"`
	URL        string    `json:"url,omitempty"`
	ReviewID   int64     `json:"review_id,omitempty"`
	LatestID   int64     `json:"latest_id"`
	LatestAt   time.Time `json:"latest_at"`
	Rebuttal   bool      `json:"rebuttal,omitempty"`
	Answered   bool      `json:"answered,omitempty"`
	AnsweredAt time.Time `json:"answered_at,omitzero"`
}

// AnsweredSince reports whether mt was answered after at (a read recorded
// before AnsweredAt was kept says answered, whenever it was).
func (mt MagnumThread) AnsweredSince(at time.Time) bool {
	return mt.Answered && (mt.AnsweredAt.IsZero() || mt.AnsweredAt.After(at))
}

// ThreadKey is the key of another reviewer's thread: its first comment and
// its latest.
func ThreadKey(root, latest int64) string { return fmt.Sprintf("thread:%d@%d", root, latest) }

// ReviewKey is the key of magnum's review of run.
func ReviewKey(run string) string { return "review:" + run }
