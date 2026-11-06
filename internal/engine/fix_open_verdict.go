package engine

// The verdict autofix = "open" keeps per own PR (KVPROpen), and the seen
// ledger behind it (kvPRFixSeen): what the look (fix_open.go) decided at
// the PR's head, in plain words, and what it remembers of the items it saw,
// the loops it saw start and end and the keys their fixers had. The engine
// is their only writer; the factory's screen reads the verdict.
//
// decideOpen is the decision, a pure function of the facts the look
// gathered (openFacts). The first of these rules that applies wins:
//
//  1. a PR the look does not cover (a draft, a fork, fix off, a watch that
//     leaves own PRs out, a baseline or ineligible PR) is off;
//  2. a PR that needs attention needs you;
//  3. a running fix loop is fixing; a held one waits; an unreadable one
//     needs you (`magnum abort` clears it);
//  4. a stalemate needs you;
//  5. magnum's review of the head waits, without a cap, until
//     magnum_late_after past the time its round was due, when it needs you;
//  6. replies magnum has not re-decided wait;
//  7. without a whole read of GitHub since the PR changed it is unknown, or
//     waits for the GitHub budget; a read GitHub cut short needs you;
//  8. a handed item still open needs you: a person's, magnum's, a conflict,
//     a bot's thread, a finding the fixer skipped without a word, a thread
//     with two fixer replies that the reviewer answered again (a bot's
//     receipt or note is seen and drops out);
//  9. work (an item never handed, magnum's review no loop worked on) starts
//     a loop once settle allows: never on magnum's rebuttals alone with no
//     review work, at most autofix_max_loops a day, never within
//     hands_off_after_push of the operator's own push;
//  10. a head the reviewers have not settled on waits;
//  11. red required CI needs you, pending required CI waits;
//  12. otherwise the PR is idle: nothing open at its head.
//
// Unknown is never idle.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// KVPROpen holds autofix = "open"'s verdict on an own PR (OpenVerdict as
// JSON), written by the engine's look; no row for a PR the look does not
// cover.
func KVPROpen(prID int64) string { return fmt.Sprintf("pr.%d.open", prID) }

// kvPRFixSeen holds the look's ledger of the PR (openLedger as JSON).
func kvPRFixSeen(prID int64) string { return fmt.Sprintf("pr.%d.fix_seen", prID) }

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
	openOffFixOff     = "fix_off"     // `magnum fix --off`
	openOffDraft      = "draft"       // a draft PR
	openOffFork       = "fork"        // its head lives in a fork
	openOffIncludeOwn = "include_own" // its watch reviews no own PRs
	openOffBaseline   = "baseline"    // open before magnum first synced the repository
	openOffIneligible = "ineligible"  // magnum does not review it
)

// OpenVerdict is autofix = "open"'s verdict on a PR at Head (KVPROpen):
// fixing, waits, needs_you, idle, unknown or off, why in plain words
// (counts, kinds, short SHAs and clock times, never PR text), when the
// look decided it (At) and last read GitHub whole (ReadAt), when a wait
// ends where that is known (Until), the open items (work and what needs the
// operator) and what needs the operator (kind and link only), the short SHA
// a person approved ("" = none) and the commits on Head after it (-1 = not
// counted), and why an off verdict is off.
type OpenVerdict struct {
	Verdict       string     `json:"verdict"`
	Why           string     `json:"why"`
	Head          string     `json:"head"`
	At            time.Time  `json:"at"`
	ReadAt        time.Time  `json:"read_at,omitzero"`
	Until         time.Time  `json:"until,omitzero"`
	Open          int        `json:"open"`
	Needs         []OpenNeed `json:"needs,omitempty"`
	Approved      string     `json:"approved,omitempty"`
	AfterApproval int        `json:"after_approval,omitempty"`
	Off           string     `json:"off,omitempty"`
}

// OpenNeed is one thing that needs the operator: its kind (thread, review,
// comment, magnum, finding, conflict, ci, loop or magnum_late) and link.
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
// (the factory's screen) that then shows the PR as looked at: the daemon's
// last tick (lastTick, zero for none) is older than two poll intervals,
// the PR's head moved past v's, or the PR's row changed after v was
// decided (prUpdated).
func (v OpenVerdict) Stale(prHead string, prUpdated, lastTick, now time.Time, poll time.Duration) bool {
	return lastTick.IsZero() || now.Sub(lastTick) > 2*poll || v.Head != prHead || v.At.Before(prUpdated)
}

// sameAs reports whether v and w say the same, whenever each was decided.
func (v OpenVerdict) sameAs(w OpenVerdict) bool {
	v.At, w.At = time.Time{}, time.Time{}
	a, _ := json.Marshal(v)
	b, _ := json.Marshal(w)
	return bytes.Equal(a, b)
}

// openLedger is what the look remembers of a PR (kvPRFixSeen): the keys
// the fixers of finished loops had (Handed, with when the look saw that
// loop end), magnum's reviews a loop worked on (Reviews), the heads the
// fixer pushed (FixerHeads, the last openFixerHeadsMax), the PR's last
// event the look read for the loops that ended (Cursor), the last_error
// its own raise wrote (RaisedMsg) and the keys it raised (Raised), the
// autofix starts of the last 24 hours (Starts), its last read of GitHub
// (Read) and of the base (Conflict), the failed reads in a row
// (ReadFails), the head whose late review it toasted, the commits after a
// person's approval (Approval), and the fingerprint at which prepareFix
// refused the look's start as nothing to fix, with its sentence (Refused,
// RefusedWhy: not tried again until the PR changes).
type openLedger struct {
	Handed      map[string]time.Time `json:"handed,omitempty"`
	Reviews     []string             `json:"reviews,omitempty"`
	FixerHeads  []string             `json:"fixer_heads,omitempty"`
	Cursor      int64                `json:"cursor,omitempty"`
	RaisedMsg   string               `json:"raised_msg,omitempty"`
	Raised      []string             `json:"raised,omitempty"`
	Starts      []time.Time          `json:"starts,omitempty"`
	Read        openRead             `json:"read"`
	Conflict    openConflict         `json:"conflict"`
	ReadFails   int                  `json:"read_fails,omitempty"`
	LateToasted string               `json:"late_toasted,omitempty"`
	Approval    openApprovalCount    `json:"approval,omitzero"`
	Refused     string               `json:"refused,omitempty"`
	RefusedWhy  string               `json:"refused_why,omitempty"`
}

// openApprovalCount is the count of the commits on Head after the approved
// commit SHA (-1: not counted).
type openApprovalCount struct {
	SHA   string `json:"sha,omitempty"`
	Head  string `json:"head,omitempty"`
	After int    `json:"after,omitempty"`
}

// openRead is the look's last read of GitHub: the PR's read fingerprint
// then (openPrint), when it read, at which head, what GitHub cut short, the
// other reviewers' items and magnum's threads it found, and when a read
// failed last (zero: the last read did not fail).
type openRead struct {
	Print    string         `json:"print,omitempty"`
	At       time.Time      `json:"at,omitzero"`
	Head     string         `json:"head,omitempty"`
	Cut      []string       `json:"cut,omitempty"`
	Others   []openItem     `json:"others,omitempty"`
	Mine     []magnumThread `json:"mine,omitempty"`
	FailedAt time.Time      `json:"failed_at,omitzero"`
}

// openConflict is the look's last check of the head against its base: the
// head and the base, the base's tip it fetched, when, and the conflict (no
// paths: a clean merge; Unchecked: the check failed).
type openConflict struct {
	Head      string    `json:"head,omitempty"`
	Base      string    `json:"base,omitempty"`
	Tip       string    `json:"tip,omitempty"`
	At        time.Time `json:"at,omitzero"`
	Paths     []string  `json:"paths,omitempty"`
	Unchecked bool      `json:"unchecked,omitempty"`
}

// handedReview reports whether a loop worked on magnum's review of run.
func (l openLedger) handedReview(run string) bool { return slices.Contains(l.Reviews, reviewKey(run)) }

// startsSince are the autofix starts at or after since.
func (l openLedger) startsSince(since time.Time) int {
	n := 0
	for _, at := range l.Starts {
		if !at.Before(since) {
			n++
		}
	}
	return n
}

// openSettle are the [pipeline] keys of autofix = "open".
type openSettle struct {
	quiet, max, late, handsOff time.Duration
	checks                     []string
	maxLoops                   int
}

// openCI is what the head's required checks say (openCIOf): what failed
// ("required check rspec", "CI" for the rollup; "" for nothing), and
// whether one still runs or has not started.
type openCI struct {
	failed  string
	pending bool
}

// openCIOf is rule 11's CI of pr's head by required, its repository's
// required checks (store.Store.RequiredChecks: the [[repo]] block's, else
// GitHub's for the default branch): a known empty list leaves CI out; the
// head's stored checks decide where they can (requiredCIOf), and GitHub's
// rollup otherwise, a required list with no rollup at all (no check has
// run) waiting for it.
func openCIOf(pr store.PR, required store.RequiredChecks) openCI {
	if required.Source != "" && len(required.Checks) == 0 {
		return openCI{}
	}
	if r, ok := requiredCIOf(pr, required.Checks); ok {
		if len(r.failed) > 0 {
			return openCI{failed: "required check " + r.failed[0].Name}
		}
		return openCI{pending: r.running}
	}
	switch deref(pr.CIState) {
	case "FAILURE", "ERROR":
		return openCI{failed: "CI"}
	case "PENDING", "EXPECTED":
		return openCI{pending: true}
	case "":
		return openCI{pending: len(required.Checks) > 0}
	}
	return openCI{}
}

// openFacts are what decideOpen decides from, all gathered by the look:
// the time, the PR, why it is off ("" = the look covers it), its attention
// kind while it needs attention, its loop's row (fixBad: one that does not
// parse) and a held fix (why it is held, heldFix.Why), the links of its
// stalemate, magnum's last review that stands at the head (nil: none), its
// pending replies, the read of GitHub (whole: one since the PR changed;
// failed: the last read failed; low: the GitHub budget is below the floor;
// cut: what the read lacked), the items, the ledger, the settle's keys,
// CI, the approval and the commits after it.
type openFacts struct {
	now        time.Time
	pr         store.PR
	off        string
	attention  string
	fix        *FixState
	fixBad     bool
	held       string
	stalemate  []string
	sum        *store.ReviewSummary
	pending    int
	whole      bool
	failed     bool
	low        bool
	cut        []string
	items      []openItem
	ledger     openLedger
	settle     openSettle
	ci         openCI
	approved   string
	afterCount int
}

// sortOpen splits items by the ledger: work, never handed (and magnum's
// review no loop worked on), and needs, handed and still open (a person's,
// magnum's, a conflict, a bot's thread), every finding a fixer skipped
// without a word, and every thread a fixer answered twice whose reviewer
// answered again (never work again). A handed bot's review body or comment
// is seen: a receipt, a summary or a note, it drops out.
func sortOpen(items []openItem, l openLedger) (work, needs []openItem) {
	for _, it := range items {
		_, handed := l.Handed[it.Key]
		switch {
		case it.Kind == itemFindings:
			if !l.handedReview(strings.TrimPrefix(it.Key, "review:")) {
				work = append(work, it)
			}
		case it.Kind == itemFinding:
			needs = append(needs, it)
		case it.Kind == itemThread && it.FixerReplies >= 2:
			needs = append(needs, it)
		case handed && it.Class == classBot && it.Kind != itemThread:
		case handed:
			needs = append(needs, it)
		default:
			work = append(work, it)
		}
	}
	return work, needs
}

// decideOpen decides the verdict of f's PR by the rules of this file's
// comment, and returns it with the work a loop would start on (only for
// the fixing verdict of rule 9, a start) and what needs the operator.
func decideOpen(f openFacts) (OpenVerdict, []openItem, []openItem) {
	head := f.pr.HeadSHA
	short := textx.ShortSHA(head)
	v := OpenVerdict{Head: head, At: f.now, ReadAt: f.ledger.Read.At, Approved: f.approved, AfterApproval: f.afterCount}
	if v.Approved == "" {
		v.AfterApproval = 0
	}
	say := func(verdict, why string) OpenVerdict {
		v.Verdict, v.Why = verdict, why
		return v
	}
	if f.off != "" {
		v.Off = f.off
		return say(OpenOff, offWords(f.off)), nil, nil
	}
	work, needs := sortOpen(f.items, f.ledger)
	if !f.whole || f.failed || len(f.cut) > 0 { // the items are not known: none counts
		work, needs = nil, nil
	}
	v.Open = len(work) + len(needs)
	v.Needs = openNeeds(needs)
	switch {
	case f.attention != "":
		return say(OpenNeedsYou, "needs your attention ("+f.attention+")"), nil, needs
	case f.fixBad:
		return say(OpenNeedsYou, fmt.Sprintf("the fix row of #%d cannot be read: `magnum abort` clears it", f.pr.Number)), nil, needs
	case f.fix != nil && f.fix.Phase == FixPhaseReviewing:
		return say(OpenFixing, "fixing, re-review of "+textx.ShortSHA(f.fix.Head)), nil, needs
	case f.fix != nil:
		return say(OpenFixing, fmt.Sprintf("fixing, cycle %d of %d", f.fix.Cycle, f.fix.Cap)), nil, needs
	case f.held == heldBudget:
		return say(OpenWaits, "a fix waits for the GitHub budget"), nil, needs
	case f.held == heldPause:
		return say(OpenWaits, "a fix waits for the fixer's pause to end"), nil, needs
	case f.held != "":
		return say(OpenWaits, "a fix waits for the restart"), nil, needs
	}
	if len(f.stalemate) > 0 {
		v.Needs = append(v.Needs, linkNeeds("thread", f.stalemate)...)
		return say(OpenNeedsYou, fmt.Sprintf("magnum stopped arguing in %s", textx.Count(len(f.stalemate), "thread", "threads"))), nil, needs
	}
	if why, late, until := magnumDue(f); why != "" {
		if late {
			v.Needs = append(v.Needs, OpenNeed{Kind: "magnum_late", URL: f.pr.URL})
			return say(OpenNeedsYou, why), nil, needs
		}
		v.Until = until
		return say(OpenWaits, why), nil, needs
	}
	if f.sum == nil {
		v.Needs = append(v.Needs, OpenNeed{Kind: "magnum", URL: f.pr.URL})
		return say(OpenNeedsYou, "no review of magnum's stands at "+short+": `magnum review` reviews it"), nil, needs
	}
	if f.pending > 0 {
		return say(OpenWaits, "magnum re-decides "+textx.Count(f.pending, "reply", "replies")), nil, needs
	}
	switch {
	case len(f.cut) > 0:
		return say(OpenNeedsYou, "GitHub cut the read short ("+strings.Join(f.cut, ", ")+"): answer the open items by hand"), nil, needs
	case f.low && (!f.whole || f.failed):
		return say(OpenWaits, "GitHub budget: reading the PR waits for it"), nil, needs
	case f.failed:
		return say(OpenUnknown, "reading GitHub failed; trying again shortly"), nil, needs
	case !f.whole:
		return say(OpenUnknown, "reading GitHub"), nil, needs
	}
	if len(needs) > 0 {
		return say(OpenNeedsYou, needsWords(needs)), nil, needs
	}
	settled, until := settleDone(f)
	if len(work) > 0 {
		onlyRebuttals := !slices.ContainsFunc(work, func(it openItem) bool { return it.Kind != itemMagnum }) && !reviewWork(*f.sum)
		handsOff := f.pr.HeadChangedAt.Add(f.settle.handsOff)
		switch {
		case onlyRebuttals:
			v.Needs = append(v.Needs, openNeeds(work)...)
			return say(OpenNeedsYou, "magnum's reply waits for your answer: "+work[0].URL), nil, work
		case f.ledger.startsSince(f.now.Add(-24*time.Hour)) >= f.settle.maxLoops:
			v.Needs = append(v.Needs, OpenNeed{Kind: "loop", URL: f.pr.URL})
			return say(OpenNeedsYou, fmt.Sprintf("%s today: f starts another", textx.Count(f.settle.maxLoops, "loop", "loops"))), nil, needs
		case !slices.Contains(f.ledger.FixerHeads, head) && f.now.Before(handsOff):
			v.Until = handsOff
			return say(OpenWaits, fmt.Sprintf("you pushed %s at %s; hands off until %s", short, waitClock(f.pr.HeadChangedAt, f.now),
				waitClock(handsOff, f.now))), nil, needs
		case !settled:
			v.Until = until
			return say(OpenWaits, "settling: "+workWords(work)+" waits until "+waitClock(until, f.now)), nil, needs
		}
		return say(OpenFixing, "starts a fix loop on "+workWords(work)), work, needs
	}
	if !settled {
		v.Until = until
		return say(OpenWaits, "settling until "+waitClock(until, f.now)+": a reviewer can still be at work"), nil, needs
	}
	switch {
	case f.ci.failed != "":
		v.Needs = append(v.Needs, OpenNeed{Kind: "ci", URL: f.pr.URL})
		return say(OpenNeedsYou, fmt.Sprintf("%s failed at %s", f.ci.failed, short)), nil, needs
	case f.ci.pending:
		return say(OpenWaits, "CI at "+short), nil, needs
	}
	why := "nothing open at " + short
	if f.pr.ReviewGate != nil && f.pr.ReviewGate.Decision == "APPROVED" {
		why += " · approved, merge is yours"
	} else {
		why += " · no approval yet"
	}
	if f.approved != "" && f.afterCount > 0 {
		why += " · " + textx.Count(f.afterCount, "commit", "commits") + " after the approval"
	}
	return say(OpenIdle, why), nil, nil
}

// heldRestart is a drain's held fix as openFacts.held carries it (its
// heldFix.Why is "").
const heldRestart = "restart"

// offWords is why the look leaves a PR alone, by its off reason.
func offWords(off string) string {
	switch off {
	case openOffFixOff:
		return "fix is off (F)"
	case openOffDraft:
		return "a draft"
	case openOffFork:
		return "the head lives in a fork"
	case openOffIncludeOwn:
		return "its watch leaves your own PRs out (include_own)"
	case openOffBaseline:
		return "open before magnum first synced the repository"
	}
	return "magnum does not review it"
}

// magnumDue is rule 5: magnum has not reviewed the head (its reviewed head
// is another, or a round is due or running): the wait's words, whether it
// is late (needs_you) and when it ends where known. "" when magnum is done
// at the head.
func magnumDue(f openFacts) (why string, late bool, until time.Time) {
	pr := f.pr
	due, running := slices.Contains(store.DueStates, pr.State), slices.Contains(store.InFlightStates, pr.State)
	if deref(pr.ReviewedSHA) == pr.HeadSHA && !due && !running {
		return "", false, time.Time{}
	}
	short := textx.ShortSHA(pr.HeadSHA)
	since := pr.HeadChangedAt
	if pr.NextEligibleAt != nil && pr.NextEligibleAt.After(since) {
		since = *pr.NextEligibleAt
	}
	if due && f.settle.late > 0 && !f.now.Before(since.Add(f.settle.late)) {
		return fmt.Sprintf("magnum has not reviewed %s since %s", short, waitClock(since, f.now)), true, time.Time{}
	}
	if pr.NextEligibleAt != nil && pr.NextEligibleAt.After(f.now) {
		until = *pr.NextEligibleAt
	}
	return "magnum reviews " + short, false, until
}

// settleDone is rule 2 of the settle: the head is settled once the PR saw
// no activity for settle_quiet and every settle_checks check that exists
// at the head is done (one with checks of another head is not known yet,
// and waited for), both waited for at most settle_max after the head moved.
// until is when the wait ends at the latest.
func settleDone(f openFacts) (bool, time.Time) {
	pr := f.pr
	capAt := pr.HeadChangedAt.Add(f.settle.max)
	if !f.now.Before(capAt) {
		return true, time.Time{}
	}
	activity := pr.HeadChangedAt
	if pr.ActivityAt != nil && pr.ActivityAt.After(activity) {
		activity = *pr.ActivityAt
	}
	quietAt := activity.Add(f.settle.quiet)
	if f.now.Before(quietAt) {
		if capAt.Before(quietAt) {
			return false, capAt
		}
		return false, quietAt
	}
	if checksPending(pr, f.settle.checks) {
		return false, capAt
	}
	return true, time.Time{}
}

// checksPending reports whether a check names lists (by check run or
// workflow name, any case) is still running at pr's head, or whether the
// head's checks are not known yet (stored for another commit, or none).
func checksPending(pr store.PR, names []string) bool {
	if len(names) == 0 {
		return false
	}
	if pr.CI == nil || pr.CI.SHA != pr.HeadSHA {
		return true
	}
	for _, c := range pr.CI.Checks {
		if c.State == store.CheckPending && slices.ContainsFunc(names, func(n string) bool {
			return strings.EqualFold(n, c.Name) || c.Workflow != "" && strings.EqualFold(n, c.Workflow)
		}) {
			return true
		}
	}
	return false
}

// openNeeds are items as needs: kind and link only.
func openNeeds(items []openItem) []OpenNeed {
	var out []OpenNeed
	for _, it := range items {
		out = append(out, OpenNeed{Kind: it.Kind, URL: it.URL})
	}
	return out
}

// linkNeeds are urls as needs of kind.
func linkNeeds(kind string, urls []string) []OpenNeed {
	var out []OpenNeed
	for _, u := range urls {
		out = append(out, OpenNeed{Kind: kind, URL: u})
	}
	return out
}

// itemNouns names each kind of item in the verdict's words.
var itemNouns = map[string][2]string{
	itemThread:   {"thread", "threads"},
	itemReview:   {"review body", "review bodies"},
	itemComment:  {"comment", "comments"},
	itemMagnum:   {"reply of magnum's", "replies of magnum's"},
	itemFinding:  {"skipped finding", "skipped findings"},
	itemFindings: {"review of magnum's", "reviews of magnum's"},
	itemConflict: {"conflict with the base", "conflicts with the base"},
}

// workWords counts items by kind and class: "1 review of magnum's and 1
// thread of a person".
func workWords(items []openItem) string {
	type group struct{ kind, class string }
	counts := map[group]int{}
	var order []group
	for _, it := range items {
		g := group{it.Kind, it.Class}
		if counts[g] == 0 {
			order = append(order, g)
		}
		counts[g]++
	}
	var parts []string
	for _, g := range order {
		noun := itemNouns[g.kind]
		s := textx.Count(counts[g], noun[0], noun[1])
		switch g.class {
		case classPerson:
			s += " of " + textx.Plural(counts[g], "a person", "people")
		case classBot:
			s += " of " + textx.Plural(counts[g], "a bot", "bots")
		}
		parts = append(parts, s)
	}
	return joinWords(parts)
}

// needsWords is the needs_you verdict of items left to the operator.
func needsWords(items []openItem) string {
	return textx.Count(len(items), "item needs you", "items need you") + ": " + workWords(items)
}

// joinWords joins parts as a sentence does: "a", "a and b", "a, b and c".
func joinWords(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}
