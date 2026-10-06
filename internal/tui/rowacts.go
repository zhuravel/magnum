package tui

// The row actions the PR board, the status dashboard and the picker share:
// one table of what each key does to the selected row, one predicate that
// says, before anything is asked, why an action cannot run on a row, the y/N
// question that says what y will do, and the menu entries, the card's
// actions and the key hints, all built from that table. A screen only says
// what it knows of its row (actRow).

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/zhuravel/magnum/internal/textx"
)

// rowAct is an action on the selected row's PR (or, for pin, unpin and
// release, on an empty slot).
type rowAct int

const (
	actReview rowAct = iota
	actFresh
	actSimplify
	actAbort
	actIgnore
	actApprove
	actRequestChanges
	actOpen
	actBrowser
	actTracker
	actPin
	actUnpin
	actRelease
	actMute
	actUnmute
	actUnapprove
)

// rowActDef is a row action's key and words.
type rowActDef struct {
	key   string // the key on the board and the dashboard
	label string // its entry in the menu and on the card
	hint  string // its word in the key hints
	what  string // its name while it runs, in its flash and in "… cancelled"
}

var rowActDefs = [...]rowActDef{
	actReview:         {"r", "review", "review", "review"},
	actFresh:          {"R", "fresh review", "fresh", "fresh review"},
	actSimplify:       {"i", "simplify review", "simplify", "simplify review"},
	actAbort:          {"K", "kill review", "kill", "abort"},
	actIgnore:         {"I", "ignore", "ignore", "ignore"},
	actApprove:        {"A", "approve", "approve", "approve"},
	actRequestChanges: {"C", "request changes", "changes", "request changes"},
	actOpen:           {"o", "open pane", "open", "open"},
	actBrowser:        {"b", "browser", "browser", "browser"},
	actTracker:        {"t", "tracker", "tracker", "tracker"},
	actPin:            {"p", "pin", "pin", "pin"},
	actUnpin:          {"u", "unpin", "unpin", "unpin"},
	actRelease:        {"x", "release", "release", "release"},
	actMute:           {"M", "mute", "mute", "mute"},
	actUnmute:         {"U", "unmute", "unmute", "unmute"},
	actUnapprove:      {"D", "withdraw approval", "withdraw", "unapprove"},
}

// The actions each screen offers, in the order its menu (and the card) lists
// them.
var (
	boardActs = []rowAct{actReview, actFresh, actSimplify, actAbort, actIgnore, actApprove, actRequestChanges, actUnapprove,
		actOpen, actBrowser, actTracker, actPin, actUnpin, actRelease, actMute, actUnmute}
	dashActs = []rowAct{actReview, actFresh, actSimplify, actAbort, actIgnore,
		actOpen, actBrowser, actPin, actUnpin, actRelease, actMute, actUnmute}
)

// rowActFor is the action key k runs among acts.
func rowActFor(acts []rowAct, k string) (rowAct, bool) {
	for _, a := range acts {
		if rowActDefs[a].key == k {
			return a, true
		}
	}
	return 0, false
}

// The PR states a round is in: running, waiting in line, paused mid-way, and
// the states of a PR magnum is done with.
var (
	runningStates = []string{"claiming", "reviewing", "verifying"}
	queuedStates  = []string{"queued", "rereview_pending"}
	endedStates   = []string{"closed", "releasing", "released"}
)

// actRow is what the row actions know of the selected row, whichever screen
// lists it. A fact a screen does not know stays at its zero value with its
// known flag false: the predicate refuses only on what it knows, and the
// daemon decides the rest.
type actRow struct {
	ok    bool   // a row is selected
	ref   string // the PR actions receive; "" when the row names none
	slot  string // an empty slot's name: pin, unpin and release act on it
	label string // the PR (or "slot <name>") as a question names it
	none  string // why a row naming no PR offers no PR action

	state   string // magnum's state (normState, flags cut); "" = unknown
	ghState string // GitHub's state, upper case: OPEN, CLOSED, MERGED; "" = unknown (open)
	head    string // the head commit; "" = unknown
	// reviewed is the head magnum reviewed last; "" when never or unknown.
	reviewed string
	facts    string // what the review question says about the PR
	// replies counts the replies on magnum's review its judge has not
	// re-decided yet; 0 when there are none or the screen does not know
	// (only the board does).
	replies int
	// findings is what magnum's latest review concluded (nil: magnum has not
	// reviewed the PR); verdicts says the screen knows it, so A and C apply.
	findings *FindingsInfo
	verdicts bool
	// auto is the approval magnum posted as the operator that stands on the
	// PR (nil: none); autoKnown says the screen knows it, so D applies.
	auto                 *AutoApproval
	autoKnown            bool
	url, issue, issueURL string

	pinned, pinKnown                bool
	muted, mutedKnown               bool
	inSlot, slotKnown               bool // the PR holds a slot
	mergedUnreviewed, flagDismissed bool

	// keys are the screen's keys where they differ from rowActDefs (the
	// picker's); an action missing from it there has no key.
	keys map[rowAct]string
}

// key is the key that runs a on the row's screen; "" when it has none.
func (r actRow) key(a rowAct) string {
	if r.keys != nil {
		return r.keys[a]
	}
	return rowActDefs[a].key
}

// repliesNow reports that r (the review key's row) has replies waiting for
// the judge on a head magnum reviewed, with no round in flight or over: the
// review key then has the judge alone re-decide them (a reply round) instead
// of an ordinary forced review. The daemon decides the same from the
// registry, so a head that moved meanwhile gets an ordinary round.
func (r actRow) repliesNow() bool {
	return r.replies > 0 && sameSHA(r.head, r.reviewed) && !r.merged() && !r.closed() && !r.running() && !r.ended()
}

// target is what the action receives: the PR, else the empty slot.
func (r actRow) target() string {
	if r.ref != "" {
		return r.ref
	}
	return r.slot
}

func (r actRow) merged() bool { return r.ghState == "MERGED" }
func (r actRow) closed() bool { return r.ghState == "CLOSED" }

// ghWord is "merged" or "closed" for a PR GitHub no longer lists as open.
func (r actRow) ghWord() string { return strings.ToLower(r.ghState) }

func (r actRow) running() bool { return slices.Contains(runningStates, r.state) }
func (r actRow) queued() bool  { return slices.Contains(queuedStates, r.state) }
func (r actRow) paused() bool  { return r.state == "paused" }
func (r actRow) ended() bool   { return slices.Contains(endedStates, r.state) }
func (r actRow) ignored() bool { return r.state == "ignored" }

// orKey is note with the key that runs a on the row's screen, or "" when the
// screen has no key for it ("; K kills it").
func (r actRow) orKey(a rowAct, note string) string {
	k := r.key(a)
	if k == "" {
		return ""
	}
	return strings.ReplaceAll(note, "%k", k)
}

// missing says why a cannot run on the row before anything else does:
// nothing selected, or a row that names no PR for an action that needs one.
func (r actRow) missing(a rowAct) string {
	switch {
	case !r.ok:
		return "nothing selected"
	case r.ref != "":
		return ""
	case a == actBrowser:
		return "" // the URL decides
	case r.slot != "" && (a == actPin || a == actUnpin || a == actRelease):
		return ""
	}
	return cmp.Or(r.none, "this row names no PR")
}

// actionRefusal says why a cannot run on the row r, "" when it can. The
// screens check it before asking anything, so a y/N question is only ever
// put for an action that will run; the menus dim and the card leaves out
// what it refuses.
func actionRefusal(a rowAct, r actRow) string {
	if why := r.missing(a); why != "" {
		return why
	}
	l := r.label
	switch a {
	case actReview, actFresh, actSimplify:
		switch {
		case r.closed():
			return l + " was closed without merging: only open or merged PRs are reviewed"
		case r.merged() && sameSHA(r.head, r.reviewed):
			return l + ": its merged head " + textx.ShortSHA(r.head) + " was already reviewed"
		case !r.merged() && r.ended():
			return l + " is " + r.state + ": only open or merged PRs are reviewed"
		case r.running():
			return l + ": round in progress (" + stateLabel(r.state) + ")" + r.orKey(actAbort, "; %k kills it")
		}
	case actAbort:
		if r.state != "" && !r.running() && !r.queued() && !r.paused() {
			return "no review of " + l + " is running or queued (" + stateLabel(r.state) + ")"
		}
	case actIgnore:
		switch {
		case r.ignored():
			return l + " is already ignored" + r.orKey(actUnmute, " (%k stops ignoring it)")
		case r.merged() && !r.running() && !r.queued() && !r.paused():
			return l + " is merged: nothing to ignore"
		}
	case actApprove, actRequestChanges:
		verb := map[bool]string{true: "approve on", false: "request changes on"}[a == actApprove]
		f := r.findings
		switch {
		case !r.verdicts:
		case f == nil:
			return "magnum has not reviewed this PR: nothing to " + verb
		case r.merged() || r.closed() || r.ended():
			return l + " is " + cmp.Or(r.ghWord(), r.state) + ": nothing to " + verb
		case f.SHA != "" && r.head != "" && !sameSHA(r.head, f.SHA):
			return l + ": the head moved since magnum reviewed " + textx.ShortSHA(f.SHA) + r.orKey(actReview, ": review again first (%k)")
		}
	case actUnapprove:
		if r.autoKnown && r.auto == nil {
			return "magnum has no approval standing as you on " + l
		}
	case actBrowser:
		if r.url == "" {
			return "no PR URL for " + strings.TrimPrefix(l, "slot ")
		}
	case actTracker:
		if r.issueURL == "" {
			return "the title names no issue: [board] trackers lists the issue keys and their URLs"
		}
	case actPin:
		if r.pinKnown && r.pinned {
			return l + " is already pinned"
		}
	case actUnpin:
		if r.pinKnown && !r.pinned {
			return l + " is not pinned"
		}
	case actRelease:
		switch {
		case r.pinKnown && r.pinned:
			return l + " is pinned: unpin first" + r.orKey(actUnpin, " (%k)")
		case r.ref != "" && r.slotKnown && !r.inSlot:
			return l + " holds no slot: nothing to release"
		}
	case actMute:
		switch act, state := muteActFor(r.ghState, r.mergedUnreviewed, r.flagDismissed); {
		case act == muteNothing:
			return nothingToMute(l, state)
		case act == muteAsk && r.mutedKnown && r.muted:
			return l + " is already muted" + r.orKey(actUnmute, " (%k unmutes it)")
		}
	case actUnmute:
		switch {
		case (r.merged() || r.closed()) && !r.flagDismissed:
			return l + " is " + r.ghWord() + ": nothing to unmute"
		case !r.merged() && !r.closed() && r.mutedKnown && !r.muted && !r.ignored():
			return l + " is not muted"
		}
	}
	return ""
}

// actionQuestion is the y/N question before a runs on r, saying what y
// will do; "" for the actions that run at once (open, browser, tracker, pin,
// unpin).
func actionQuestion(a rowAct, r actRow) string {
	l := r.label
	switch a {
	case actReview, actFresh, actSimplify:
		o := reviewOptsFor(a, r)
		if o.Replies {
			return repliesQuestion(l, r.replies, r.facts)
		}
		if r.merged() {
			return postMergeQuestion(l, o)
		}
		facts := r.facts
		if r.pinKnown && r.pinned { // the daemon unpins it (its slot's guards still hold a person's changes)
			facts = joinFacts(facts, "pinned: the review unpins it")
		}
		return reviewQuestion(l, o, facts)
	case actAbort:
		return abortQuestion(r)
	case actIgnore:
		return ignoreQuestion(r)
	case actApprove, actRequestChanges:
		return verdictQuestion(l, a == actApprove, r.findings)
	case actUnapprove:
		return unapproveQuestion(l, r.auto)
	case actRelease:
		return releaseQuestion(strings.TrimPrefix(l, "slot "))
	case actMute:
		switch act, _ := muteActFor(r.ghState, r.mergedUnreviewed, r.flagDismissed); act {
		case muteDismiss:
			return dismissFlagQuestion(l)
		case muteRestore:
			return restoreFlagQuestion(l)
		}
		return muteQuestion(l, true)
	case actUnmute:
		switch {
		case r.merged() || r.closed():
			return restoreFlagQuestion(l)
		case r.ignored():
			return unmuteIgnoredQuestion(l)
		}
		return muteQuestion(l, false)
	}
	return ""
}

// reviewOptsOf is the review variant a starts.
func reviewOptsOf(a rowAct) ReviewOpts {
	return ReviewOpts{Fresh: a == actFresh, Simplify: a == actSimplify}
}

// reviewOptsFor is the review variant a starts on the row r: the review key
// of a row with replies waiting for the judge has it alone re-decide them.
func reviewOptsFor(a rowAct, r actRow) ReviewOpts {
	if a == actReview && r.repliesNow() {
		return ReviewOpts{Replies: true}
	}
	return reviewOptsOf(a)
}

// actionLabel is a's entry in the menu and on the card for the row r: the
// review variants of a merged PR are post-merge reviews, M on a merged PR
// dismisses or restores its merged-unreviewed flag, U on an ignored PR
// stops ignoring it, and r on a row with replies waiting for the judge
// re-decides them.
func actionLabel(a rowAct, r actRow) string {
	switch a {
	case actReview, actFresh:
		if r.merged() {
			return map[rowAct]string{actReview: "post-merge review", actFresh: "fresh post-merge review"}[a]
		}
		if a == actReview && r.repliesNow() {
			return "re-decide replies"
		}
	case actMute:
		switch act, _ := muteActFor(r.ghState, r.mergedUnreviewed, r.flagDismissed); act {
		case muteDismiss:
			return "dismiss merged flag"
		case muteRestore:
			return "restore merged flag"
		}
	case actUnmute:
		if r.ignored() {
			return "unmute (stop ignoring)"
		}
	}
	return rowActDefs[a].label
}

// actionRun is what runs a on r: its name ("review talkable#7") and the call.
// M on a merged PR whose flag a mute dismissed restores it with an unmute.
func actionRun(a rowAct, r actRow) (what string, fn actionFunc) {
	target := r.target()
	what = rowActDefs[a].what + " " + target
	// on calls a DashboardActions method on target.
	on := func(call func(DashboardActions, context.Context, string) (ActionResult, error)) actionFunc {
		return func(ctx context.Context, act DashboardActions) (ActionResult, error) { return call(act, ctx, target) }
	}
	switch a {
	case actReview, actFresh, actSimplify:
		o := reviewOptsFor(a, r)
		if o.Replies {
			what = "re-decide replies " + target
		}
		return what, func(ctx context.Context, act DashboardActions) (ActionResult, error) {
			return act.Review(ctx, target, o)
		}
	case actAbort:
		return what, on(DashboardActions.Abort)
	case actIgnore:
		return what, on(DashboardActions.Ignore)
	case actApprove:
		return what, on(DashboardActions.Approve)
	case actRequestChanges:
		return what, on(DashboardActions.RequestChanges)
	case actUnapprove:
		return what, on(DashboardActions.Unapprove)
	case actOpen:
		return what, on(DashboardActions.Open)
	case actBrowser, actTracker:
		url, name := r.url, r.url
		if a == actTracker {
			url, name = r.issueURL, r.issue+": "+r.issueURL
		}
		return rowActDefs[a].what, func(ctx context.Context, act DashboardActions) (ActionResult, error) {
			if err := act.OpenBrowser(ctx, url); err != nil {
				return ActionResult{}, err
			}
			return ActionResult{Text: "opened " + name}, nil
		}
	case actPin:
		return what, on(DashboardActions.Pin)
	case actUnpin:
		return what, on(DashboardActions.Unpin)
	case actRelease:
		return what, on(DashboardActions.Release)
	case actMute:
		if act, _ := muteActFor(r.ghState, r.mergedUnreviewed, r.flagDismissed); act == muteRestore {
			return rowActDefs[actUnmute].what + " " + target, on(DashboardActions.Unmute)
		}
		return what, on(DashboardActions.Mute)
	}
	return what, on(DashboardActions.Unmute)
}

// rowAction runs a on the row r: a refusal flashes at once with its reason
// (nothing selected, actions unavailable or busy, then actionRefusal), an
// action with a question asks it, any other starts; started reports that.
func (b *actionBar) rowAction(a rowAct, r actRow) (cmd tea.Cmd, started bool) {
	if why := r.missing(a); why != "" {
		return b.setFlash(why, true), false
	}
	what, fn := actionRun(a, r)
	if why := b.refusal(what); why != "" {
		return b.setFlash(why, true), false
	}
	if why := actionRefusal(a, r); why != "" {
		return b.setFlash(why, true), false
	}
	if q := actionQuestion(a, r); q != "" {
		b.confirm = &pendingAction{question: q, what: what, target: r.target(), fn: fn}
		return nil, false
	}
	return b.start(what, r.target(), fn)
}

// actionMenu is the menu entries of acts for the row r, dimmed where the
// action cannot run (no actions at all when avail is false).
func actionMenu(acts []rowAct, r actRow, avail bool, display map[rowAct]string) []menuItem {
	items := make([]menuItem, 0, len(acts))
	for _, a := range acts {
		k := rowActDefs[a].key
		items = append(items, menuItem{label: actionLabel(a, r), hint: cmp.Or(display[a], k), key: k,
			ok: avail && actionRefusal(a, r) == ""})
	}
	return items
}

// actionCard is the card's actions: those of acts that can run on r.
func actionCard(acts []rowAct, r actRow) []hint {
	var hs []hint
	for _, a := range acts {
		if actionRefusal(a, r) == "" {
			hs = append(hs, hint{rowActDefs[a].key, actionLabel(a, r)})
		}
	}
	return hs
}

// actionHints are the key hints of acts, in their order: pin and unpin
// listed together read "p/u pin", mute and unmute "M/U mute".
func actionHints(acts ...rowAct) []hint {
	pairs := map[rowAct]rowAct{actPin: actUnpin, actMute: actUnmute}
	var hs []hint
	for i := 0; i < len(acts); i++ {
		a := acts[i]
		d := rowActDefs[a]
		if b, ok := pairs[a]; ok && i+1 < len(acts) && acts[i+1] == b {
			hs = append(hs, hint{d.key + "/" + rowActDefs[b].key, d.hint})
			i++
			continue
		}
		hs = append(hs, hint{d.key, d.hint})
	}
	return hs
}

// withHints is hs with more appended, in a new slice.
func withHints(hs []hint, more ...[]hint) []hint {
	out := slices.Clone(hs)
	for _, m := range more {
		out = append(out, m...)
	}
	return out
}

// abortQuestion asks before K stops the row's review: one that runs or is
// paused is killed, one that waits in line is dropped before it starts.
func abortQuestion(r actRow) string {
	what := "review"
	if r.merged() {
		what = "post-merge review"
	}
	switch {
	case r.queued():
		return "Drop the queued " + what + " of " + r.label + " before it starts?"
	case r.paused():
		return "Kill the paused " + what + " of " + r.label + "?"
	}
	return "Kill the running " + what + " of " + r.label + "?"
}

// ignoreQuestion asks before I ignores the row's PR, naming what will
// happen: its review killed or dropped when one runs or waits, the mute, and
// its slot freed while it holds one (a closed PR's slot is cleanup's); a
// PR closed without merging is only muted, which keeps it ignored if
// reopened.
func ignoreQuestion(r actRow) string {
	var does []string
	switch {
	case r.running() || r.paused():
		does = append(does, "kill its review")
	case r.queued():
		does = append(does, "drop its queued review")
	}
	does = append(does, "mute it")
	if !r.ended() && !r.closed() && (r.inSlot || !r.slotKnown) {
		does = append(does, "free its slot")
	}
	q := "Ignore " + r.label + ": " + andList(does)
	if r.closed() {
		q += " (closed: it stays ignored if reopened)"
	}
	return q + "?"
}

// unapproveQuestion asks before D withdraws the approval magnum posted as
// the operator on the PR ref (a; nil when the screen does not know it).
func unapproveQuestion(ref string, a *AutoApproval) string {
	what := "the approval magnum posted as you on " + ref
	if a != nil {
		what += " (review " + strconv.FormatInt(a.ReviewID, 10) + " on " + textx.ShortSHA(a.Head) + ")"
	}
	return "Withdraw " + what + " and stop it approving " + ref + "?"
}

// andList joins words as "a, b and c".
func andList(words []string) string {
	switch n := len(words); n {
	case 0:
		return ""
	case 1:
		return words[0]
	default:
		return strings.Join(words[:n-1], ", ") + " and " + words[n-1]
	}
}

// boardActRow is what the row actions know of a board row: everything the
// registry says of the PR. label names it in questions ("" = its ref).
func boardActRow(r PRBoardRow, label string, now time.Time) actRow {
	ref := prRef(r)
	row := actRow{ok: true, ref: ref, label: cmp.Or(label, ref), none: "this row names no PR",
		state: normState(r.State), ghState: strings.ToUpper(strings.TrimSpace(r.GHState)), head: r.HeadSHA,
		findings: r.Findings, verdicts: true, url: prURL(r), issue: r.Issue, issueURL: r.IssueURL,
		pinned: r.Pinned, pinKnown: true, muted: r.Muted || normState(r.State) == "ignored", mutedKnown: true,
		inSlot: r.Slot != "", slotKnown: true, mergedUnreviewed: r.MergedUnreviewed, flagDismissed: r.FlagDismissed,
		auto: r.AutoApproved, autoKnown: true}
	if r.LastReview != nil {
		row.reviewed = r.LastReview.CommitSHA
	}
	row.facts = boardReviewFacts(r, now)
	row.replies = r.Replies
	return row
}
