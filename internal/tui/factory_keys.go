package tui

// The factory screen's keys (spec 9.D) and what they do on each view: home,
// the question card and its recap, the ready card, the task card and new
// task. See factory.go for the model. The handlers change the model in
// place and return the command to run: a mutating call is never an operand
// beside the model in a return.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/zhuravel/magnum/internal/textx"
)

// factoryReason is a reason a key picks by its digit: the key the factory
// keeps (a word: lowercase letters, digits, - and _) and its words on the
// card.
type factoryReason struct{ key, words string }

// The reasons of a send back, and of a question flagged needless.
var (
	factorySendBackReasons = []factoryReason{
		{"wrong-cause", "wrong cause or fix"}, {"too-much", "changed too much"},
		{"missed-words", "missed what I said"}, {"thin-tests", "tests too thin"},
	}
	factoryNeedlessReasons = []factoryReason{
		{"ticket-answers", "the ticket answers it"}, {"code-answers", "the code answers it"},
		{"any-answer", "any answer works"}, {"asked-before", "you asked it before"},
	}
	// factoryRatingReasons are the reasons of a rating, one set for + and −:
	// what the verdict is about, not a fault, so a + reads "the tests were
	// right" as a − reads "the tests were not". The send back's reasons
	// (wrong cause or fix, changed too much, ...) name only faults, which a +
	// cannot give.
	factoryRatingReasons = []factoryReason{
		{"cause", "the cause and the fix"}, {"scope", "how much it changed"}, {"words", "how it followed my words"},
		{"tests", "the tests"}, {"time", "the time it took me"},
	}
)

// RatingReasonKeys are the keys of the reasons the task card's + and −
// offer, in the order of their digits: the keys the factory takes for a
// rating (factory.RatingReasons), which a test in the cli pins them to.
func RatingReasonKeys() []string {
	out := make([]string, len(factoryRatingReasons))
	for i, r := range factoryRatingReasons {
		out[i] = r.key
	}
	return out
}

func reasonWords(rs []factoryReason) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.words
	}
	return out
}

// factoryKinds are the kinds tab cycles on new task.
var factoryKinds = []string{"bug", "feature", "chore"}

// jiraWords is a text that names a Jira key or a Jira link anywhere (a key
// at its start, a key found in it, a link): its preview reads Jira, and the
// form shows the spinner meanwhile. A key-shaped word Jira does not know
// (UTF-8) shows it too, for as long as the preview takes.
var jiraWords = regexp.MustCompile(`(^|[^A-Za-z0-9_-])[A-Z][A-Z0-9]+-[0-9]+($|[^A-Za-z0-9_-])|https?://\S*/browse/[A-Z]`)

const noActions = "this screen runs no actions: it shows the factory only"

func (m *factoryModel) updateKey(msg tea.KeyPressMsg) tea.Cmd {
	k := msg.String()
	if k == "ctrl+c" {
		return m.quit()
	}
	seen := m.seeHome()
	var cmd tea.Cmd
	switch {
	case m.menu.open:
		cmd = m.menuKey(k)
	case m.bar.asking != nil:
		cmd = m.answerKey(msg)
	case m.bar.choose != nil: // E's chooser takes every key, as a question does
		cmd = m.chooseKey(k)
	default:
		m.bar.key(msg) // a failure kept in the footer ends with any key
		cmd = m.viewKey(msg)
	}
	return tea.Batch(seen, cmd)
}

// quit drops a question up, so a y queued behind the quit starts nothing,
// and ends the screen.
func (m *factoryModel) quit() tea.Cmd {
	m.bar.drop()
	m.asked = nil
	return tea.Quit
}

// seenEvery is how often your input tells the factory you saw home: the
// digest counts your absence from your last input, to the minute.
const seenEvery = time.Minute

// seeHome is a key or a click: the operator is at the screen. The first
// one after the digest showed on home makes it go away; the factory learns
// the operator saw home then, and at any input at most once a minute
// after, with the input's time.
func (m *factoryModel) seeHome() tea.Cmd { return m.seeHomeOf(m.view == fvHome) }

// seeHomeOf is seeHome for an input on home or not (a click that opened a
// card was on home, the digest drawn).
func (m *factoryModel) seeHomeOf(home bool) tea.Cmd {
	at := m.now()
	digest := false
	if d := m.digest(); d != nil && home {
		m.digestSeen, m.digestGone, digest = d.Since, true, true
	}
	if m.act == nil || (!digest && !m.seenSent.IsZero() && at.Sub(m.seenSent) < seenEvery) {
		return nil
	}
	m.seenSent = at
	ctx, act := m.ctx, m.act
	return func() tea.Msg {
		_ = act.Seen(ctx, at) // best effort: an unsaved read shows the digest again later
		return nil
	}
}

// answerKey is a key while a question waits: y runs it, any other key
// cancels it. On the recap the key then does what it does there (a digit
// opens its question, esc goes back), and the recap asks again.
func (m *factoryModel) answerKey(msg tea.KeyPressMsg) tea.Cmd {
	cmd, _ := m.bar.key(msg)
	if m.bar.busy != "" && m.asked != nil { // y: it runs
		m.inflight, m.asked = m.asked, nil
		return cmd
	}
	m.asked = nil
	if m.view != fvRecap {
		return cmd
	}
	more, handled := m.recapKey(msg.String())
	if !handled {
		more = m.commonKey(msg.String())
	}
	return tea.Batch(cmd, more)
}

// ask puts sentence on the bar for run, and remembers what follows it.
func (m *factoryModel) ask(f *factoryFollow, sentence string, run factoryAction) tea.Cmd {
	if m.act == nil {
		return m.note(noActions)
	}
	was := m.bar.asking
	cmd := m.bar.ask(sentence, run)
	if was == nil && m.bar.asking != nil {
		m.asked = f
	}
	return cmd
}

func (m *factoryModel) note(text string) tea.Cmd { return m.bar.show(text, factoryFlashNote) }

func (m *factoryModel) viewKey(msg tea.KeyPressMsg) tea.Cmd {
	k := msg.String()
	switch {
	case m.view == fvNew:
		return m.newTaskKey(msg)
	case m.view == fvHelp || m.view == fvLog:
		return m.boxKey(k)
	case m.view == fvQuestion && m.q.typing, m.view == fvReady && m.readyState().noting:
		return m.inputKey(msg)
	case m.view == fvHome && m.filtering:
		return m.filterKey(msg)
	}
	var cmd tea.Cmd
	var handled bool
	switch m.view {
	case fvQuestion:
		cmd, handled = m.questionKey(k)
	case fvRecap:
		cmd, handled = m.recapKey(k)
	case fvReady:
		cmd, handled = m.readyKey(k)
	case fvTask:
		cmd, handled = m.taskCardKey(k)
	default:
		cmd, handled = m.homeKey(k)
	}
	if handled {
		return cmd
	}
	return m.commonKey(k)
}

// commonKey is a key every view but new task takes.
func (m *factoryModel) commonKey(k string) tea.Cmd {
	switch k {
	case "q":
		return m.quit()
	case logKey:
		m.under, m.view, m.scroll = m.view, fvLog, 0
	case "?":
		m.under, m.view, m.scroll = m.view, fvHelp, 0
	case settingsKey:
		if m.opts.Settings == nil {
			return m.note("settings: not available on this screen")
		}
		return openSettings
	case "m":
		m.toggleMouse()
		return m.note(mouseNote(m.mouseOn))
	case "ctrl+r", "f5":
		return m.reload()
	case reportKey:
		return m.openReport()
	case "o":
		r, ok := m.row(m.task)
		if m.view == fvHome {
			if r, ok = m.selectedRow(); !ok {
				return nil
			}
		}
		if ok && magnumsPane(r) {
			return m.openMagnum(r)
		}
		id := cmp.Or(r.ID, m.task)
		if file := m.followOf(id); file != "" {
			return m.followOutput(id, file)
		}
		return m.stepIn(id)
	}
	return nil
}

// followOf is the file o follows on task id: what the factory itself runs
// for it while no agent of it works (TaskCard.Follow); "" for none, and o
// steps into its pane.
func (m factoryModel) followOf(id int64) string {
	if f := m.data.Cards[id].Task.Follow; filepath.IsAbs(f) {
		return f
	}
	return ""
}

// followOutput follows file live in less (+F, as tail -f), asking nothing:
// ctrl-c stops following, q quits, and the screen comes back.
func (m *factoryModel) followOutput(id int64, file string) tea.Cmd {
	return tea.ExecProcess(m.follow(m.ctx, file), func(err error) tea.Msg { return factoryPagerMsg{task: id, err: err} })
}

// openMagnum steps into magnum's pane of row r's PR (the judge's, or the
// fixer's while its loop works), as the board's o: at once for a live
// pane; a parked one is restored in a slot, which claims it and holds the
// PR, so o asks the board's y/N first.
func (m *factoryModel) openMagnum(r FactoryRow) tea.Cmd {
	if m.act == nil {
		return m.note(noActions)
	}
	pr, key, act := *r.PR, prLabel(r), m.act
	run := func(ctx context.Context) (ActionResult, error) { return act.OpenPR(ctx, pr.Ref) }
	if pr.Restores {
		return m.ask(nil, OpenRestoreQuestion(key, pr.Hold), run)
	}
	return m.bar.start("Open magnum's pane of "+key, run)
}

// prKey is f, F, K, x or P on row r (home's, or the card's): on a row whose
// open PR magnum's registry has (FactoryRow.PR: your PR's, or a task's),
// magnum's fix of what is open (f), its switch out of autofix or back in
// (F; a PR magnum leaves alone for another reason says why and runs
// nothing), the stop of its fix loop (K; with no loop it says so) and the
// merge of its parent (P, home's rows: the daemon's plan first, a dry run,
// then its sentence as the y/N), each behind y/N and through magnum's own
// request paths; on a row without one nothing but a note of the keys that
// act on one (home's P among them). x is no key of home's: it drops a
// task, on its card, and says that K stops magnum's loop.
func (m *factoryModel) prKey(k string, r FactoryRow) tea.Cmd {
	switch {
	case k == "x":
		return m.note("K stops magnum's loop on a row with a PR; x drops a task, on its card")
	case r.PR == nil && m.view == fvHome:
		return m.note("f, F, K and P act on a PR in magnum's registry: " + cleanText(r.Key) + " has none there")
	case r.PR == nil:
		return m.note("f, F and K act on a PR in magnum's registry: " + cleanText(r.Key) + " has none there")
	}
	pr, key, act := *r.PR, prLabel(r), m.act
	f := &factoryFollow{kind: followPR, task: r.ID} // on a walk, its card leaves for the next
	switch {
	case k == "f":
		return m.ask(f, "Fix "+key+": magnum's fixer takes what is open now?", func(ctx context.Context) (ActionResult, error) {
			return act.Fix(ctx, pr.Ref)
		})
	case k == "P": // the daemon's plan first (a dry run changes nothing); its question puts the merge behind y
		why := actionRefusal(actMergeParent, m.prActRow(r, true))
		if cmd, held := m.raiseHeldPlan(r.ID, "P", why); held { // its plan answered away from the row
			return cmd
		}
		if why != "" {
			return m.note(why)
		}
		if m.act == nil {
			return m.note(noActions)
		}
		view := m.view
		return m.bar.start("Plan the merge of the parent of "+key, func(ctx context.Context) (ActionResult, error) {
			res, err := act.MergeParentPlan(ctx, pr.Ref)
			if err == nil && res.Ask != "" && res.Parent != nil { // y sends the merge bound to what the plan asked about
				asked := *res.Parent
				res.nextFactory = &factoryQuestion{sentence: res.Ask, label: "Merge the parent of " + key, wrap: true, // "with no conflict" shows before y
					plan: &planOf{row: r.ID, view: view, key: "P", name: key, ref: pr.Ref},
					run:  func(ctx context.Context) (ActionResult, error) { return act.MergeParent(ctx, pr.Ref, asked) }}
			}
			return res, err
		})
	case k == "F" && pr.FixOff:
		return m.ask(f, "Switch fix on again for "+key+"?", func(ctx context.Context) (ActionResult, error) {
			return act.FixOff(ctx, pr.Ref, false)
		})
	case k == "F" && pr.Off != "":
		return m.note(cleanText(pr.Off))
	case k == "F":
		return m.ask(f, "Switch fix off for "+key+": autofix leaves it to you?", func(ctx context.Context) (ActionResult, error) {
			return act.FixOff(ctx, pr.Ref, true)
		})
	case !pr.Fixing:
		return m.note("no fix loop runs on " + key)
	}
	return m.ask(f, "Stop what magnum runs on "+key+" (its fix loop and the re-review the loop forced)?",
		func(ctx context.Context) (ActionResult, error) { return act.Abort(ctx, pr.Ref) })
}

// fixCIKey is c on row r (home's, or the card's): magnum's CI fix of the
// failed required check of r's PR, yours or a published task's (the factory
// opens it as you), as the board's c (ci_fix.go): the daemon's plan first (a
// dry run, which also reads why the check failed), its sentence as the y/N,
// wrapped over the footer so that all it names shows, and y starts the CI
// fix bound to what the plan asked about (CIFixAsked), which the daemon
// checks again. The board's refusals come before anything is asked (a PR
// merged, in a round or in a fix loop); the daemon decides the rest, whose
// PR it is, what failed, the base and the cap, and its refusal is the
// footer's note. The plan is held, like E's and P's, when it answers away
// from the row it was asked on. A row without a PR in magnum's registry
// says so.
func (m *factoryModel) fixCIKey(r FactoryRow) tea.Cmd {
	if r.PR == nil {
		return m.note("c acts on a PR in magnum's registry: " + cleanText(r.Key) + " has none there")
	}
	pr, key, act := *r.PR, prLabel(r), m.act
	why := actionRefusal(actFixCI, m.prActRow(r, true))
	if cmd, held := m.raiseHeldPlan(r.ID, "c", why); held { // its plan answered away from the row
		return cmd
	}
	switch {
	case why != "":
		return m.note(why)
	case act == nil:
		return m.note(noActions)
	}
	view := m.view
	return m.bar.start("Plan the CI fix of "+key, func(ctx context.Context) (ActionResult, error) {
		res, err := act.FixCIPlan(ctx, pr.Ref)
		if err == nil && res.Ask != "" && res.CIFix != nil { // y starts the CI fix bound to what the plan asked about
			asked := *res.CIFix
			res.nextFactory = &factoryQuestion{sentence: res.Ask, label: "Fix the CI of " + key, wrap: true,
				plan: &planOf{row: r.ID, view: view, key: "c", name: key, ref: pr.Ref},
				run:  func(ctx context.Context) (ActionResult, error) { return act.FixCI(ctx, pr.Ref, asked) }}
		}
		return res, err
	})
}

// factoryPRActs are the board's row actions the factory screen offers on a
// row with a PR (rowacts.go): review (r), fresh review (R), snooze or its
// lift (z), mute (M) and unmute (U).
var factoryPRActs = []rowAct{actReview, actFresh, actSnooze, actMute, actUnmute}

// prActKey is r, R, z, M or U on row r (home's, or the card's), as the
// board's: on a row whose open PR magnum's registry has (FactoryRow.PR),
// the board's refusal when its predicate refuses the PR, else its y/N
// question (on a task's PR with what y does to the factory's own loop,
// factoryLoopEffect) and, on y, the board's call through magnum's own
// request paths; on a row without one it says why. reviewOnR is false on a
// task card that offers a retry, whose r is the retry: the questions then
// name no key for the review.
func (m *factoryModel) prActKey(k string, r FactoryRow, reviewOnR bool) tea.Cmd {
	a, ok := rowActFor(factoryPRActs, k)
	switch {
	case !ok:
		return nil
	case r.PR == nil:
		return m.note("r, R, z, M and U act on a PR in magnum's registry: " + cleanText(r.Key) + " has none there")
	}
	row := m.prActRow(r, reviewOnR)
	if why := actionRefusal(a, row); why != "" {
		return m.note(why)
	}
	q := actionQuestion(a, row)
	if r.ID > 0 { // a task's PR: what y does to the factory's own loop
		if e := factoryLoopEffect(a, row, *r.PR); e != "" {
			q = strings.TrimSuffix(q, "?") + "; " + e + "?"
		}
	}
	return m.ask(&factoryFollow{kind: followPR, task: r.ID}, q, m.prActRun(a, row))
}

// prActRow is what the board's row actions know of row r's PR: its
// reference, the row's name for it, its fix loop and fix off, and the facts
// magnum's registry has of it (FactoryPR.Facts; none read: each unknown, so
// the predicate refuses only on what it knows). keys are the screen's own
// (factoryActKeys), which the board's refusals and questions name.
func (m factoryModel) prActRow(r FactoryRow, reviewOnR bool) actRow {
	pr := *r.PR
	row := actRow{ok: true, ref: pr.Ref, label: prLabel(r), now: m.now(), fixOff: pr.FixOff, fixLoop: pr.Fixing,
		keys: factoryActKeys(reviewOnR, pr.Fixing)}
	f := pr.Facts
	if f == nil {
		return row
	}
	row.state, row.ghState = normState(cleanText(f.State)), strings.ToUpper(strings.TrimSpace(cleanText(f.GHState)))
	row.head, row.reviewed = cleanText(f.Head), cleanText(f.Reviewed)
	row.muted, row.mutedKnown = f.Muted || row.state == "ignored", true
	row.snoozedUntil, row.flag = f.SnoozedUntil, cleanText(f.CodexFlag)
	row.mergedUnreviewed, row.flagDismissed, row.replies = f.MergedUnreviewed, f.FlagDismissed, f.Replies
	row.facts = reviewFacts(row.state, "", &ReviewFacts{HeadSHA: row.head, ReviewedSHA: row.reviewed}, row.now)
	return row
}

// factoryActKeys are the factory screen's keys of the board's row actions
// its refusals and questions name ("U unmutes it", "K kills it"): r only
// where r is the review (reviewOnR), and K only while a fix loop works on
// the PR (fixing): the screen's K stops a fix loop and the re-review it
// forced (prKey), never a review alone.
func factoryActKeys(reviewOnR, fixing bool) map[rowAct]string {
	keys := map[rowAct]string{actFresh: "R", actSnooze: "z", actMute: "M", actUnmute: "U",
		actFix: "f", actFixOff: "F", actBrowser: "b", actMergeParent: "P"}
	if reviewOnR {
		keys[actReview] = "r"
	}
	if fixing {
		keys[actAbort] = "K"
	}
	return keys
}

// prActRun is the board's call of a on row's PR, through FactoryActions
// (magnum's own request paths, as the board's actionRun): the review's
// variant (reviewOptsFor), the snooze of boardSnooze or its lift, the mute,
// or the unmute (M too, on a merged PR whose flag a mute dismissed).
func (m factoryModel) prActRun(a rowAct, row actRow) factoryAction {
	act, ref := m.act, row.ref
	switch a {
	case actReview, actFresh:
		o := reviewOptsFor(a, row)
		return func(ctx context.Context) (ActionResult, error) { return act.Review(ctx, ref, o) }
	case actSnooze:
		if row.snoozed() {
			return func(ctx context.Context) (ActionResult, error) { return act.Unsnooze(ctx, ref) }
		}
		return func(ctx context.Context) (ActionResult, error) { return act.Snooze(ctx, ref, boardSnooze) }
	case actMute:
		if what, _ := muteActFor(row.ghState, row.mergedUnreviewed, row.flagDismissed); what != muteRestore {
			return func(ctx context.Context) (ActionResult, error) { return act.Mute(ctx, ref) }
		}
		fallthrough
	case actUnmute:
		return func(ctx context.Context) (ActionResult, error) { return act.Unmute(ctx, ref) }
	}
	return func(context.Context) (ActionResult, error) {
		return ActionResult{}, fmt.Errorf("row action %d has no call here", a)
	}
}

// undoHints are the hints of what undoes the state M and z set on row r's
// PR: U while it is muted, z while its snooze holds (the board's z lifts
// it), each where the board's predicate lets it run.
func (m factoryModel) undoHints(r FactoryRow, reviewOnR bool) []hint {
	if r.PR == nil || r.PR.Facts == nil {
		return nil
	}
	row := m.prActRow(r, reviewOnR)
	var hs []hint
	if row.muted && actionRefusal(actUnmute, row) == "" {
		hs = append(hs, hint{"U", "unmute"})
	}
	if row.snoozed() && actionRefusal(actSnooze, row) == "" {
		hs = append(hs, hint{"z", "lift snooze"})
	}
	return hs
}

// factoryLoopEffect is what a does to the factory's own loop on a task's
// PR (pr, row), in magnum's engine's terms, for its question; "" when it
// changes nothing there. The factory runs no fix loop of its own on a
// published task: magnum's autofix and magnum's fix loops work on its PR.
// A mute holds only the rounds nobody forced (a fix loop's re-review is
// forced) and stops autofix's new loops, never a loop at work's next
// cycle; a snooze holds every fix start, a loop at work's next cycle
// included, though not the re-review it forced: a re-review that still
// finds work then ends the loop for your attention (continueFixLoop); a
// review's findings go to autofix, unless fix is off, and while a fix loop
// works magnum refuses the review (requestReview). autofix is the watch's
// (FactoryPRFacts), with fix on.
func factoryLoopEffect(a rowAct, row actRow, pr FactoryPR) string {
	autofix := pr.Facts != nil && pr.Facts.Autofix && !pr.FixOff
	const nextCycle = "the fix loop at work on the factory's PR runs no next cycle: if its re-review still finds work, the loop ends and the PR needs you"
	switch a {
	case actReview, actFresh:
		switch {
		case row.merged():
		case pr.Fixing:
			return "magnum refuses a review while its fix loop works on the factory's PR (K stops the loop)"
		case pr.FixOff:
			return "fix is off for the factory's PR: what it finds stays yours"
		case autofix && pr.Off == "":
			return "autofix may take what it finds on the factory's PR"
		}
	case actMute:
		if what, _ := muteActFor(row.ghState, row.mergedUnreviewed, row.flagDismissed); what != muteAsk {
			return ""
		}
		switch {
		case pr.Fixing && autofix:
			return "the fix loop at work on the factory's PR goes on, its re-reviews too, but autofix starts no other until U"
		case pr.Fixing:
			return "the fix loop at work on the factory's PR goes on, its re-reviews too"
		case autofix:
			return "autofix starts no fix loop on the factory's PR until U"
		}
	case actUnmute, actSnooze:
		switch {
		case row.merged() || row.closed():
		case a == actUnmute || row.snoozed():
			if autofix {
				return "autofix may start fix loops on the factory's PR again"
			}
		case pr.Fixing && autofix:
			return nextCycle + ", and autofix starts no other until then"
		case pr.Fixing:
			return nextCycle
		case autofix:
			return "autofix starts no fix loop on the factory's PR until then"
		}
	}
	return ""
}

// prLabel names row r's PR in a question or a note: your PR's key
// ("#4602", "widget#12"), a task's key with its PR's number ("PR-12320's
// #4530").
func prLabel(r FactoryRow) string {
	key := cleanText(r.Key)
	if r.yourPR() || r.GitHub == "" {
		return key
	}
	return key + "'s " + cleanText(r.GitHub)
}

// prFixOffVerb is what F does on your PR's row: fix off, or on again.
func prFixOffVerb(pr FactoryPR) string {
	if pr.FixOff {
		return "fix on"
	}
	return "fix off"
}

// stepIn steps into the pane of the task (asks nothing, spec 9.D).
func (m *factoryModel) stepIn(task int64) tea.Cmd {
	if m.act == nil {
		return m.note(noActions)
	}
	act := m.act
	return m.bar.start("Open "+m.taskKey(task)+"'s pane", func(ctx context.Context) (ActionResult, error) { return act.Open(ctx, task) })
}

// home

func (m *factoryModel) homeKey(k string) (tea.Cmd, bool) {
	page := max(m.viewHeight()-factoryTop-factoryBottom-1, 1)
	switch k {
	case "j", "down":
		m.moveTo(m.selIdx + 1)
	case "k", "up":
		m.moveTo(m.selIdx - 1)
	case "g", "home":
		m.moveTo(0)
	case "G", "end":
		m.moveTo(len(m.order()) - 1)
	case "pgdown", "space", "ctrl+d":
		m.moveTo(m.selIdx + page)
	case "pgup", "ctrl+u":
		m.moveTo(m.selIdx - page)
	case "enter":
		if r, ok := m.selectedRow(); ok {
			m.openCard(r.ID)
		}
	case "a":
		m.walked = map[int64]bool{}
		id, ok := m.nextDecision()
		if !ok {
			return m.note("Nothing waits on you."), true
		}
		m.walk = true
		m.openCard(id)
	case "n":
		if m.act == nil {
			return m.note(noActions), true
		}
		m.view = fvNew
		return m.textChanged(), true
	case "b", "t":
		if r, ok := m.selectedRow(); ok {
			return m.openPage(k, r.ID), true
		}
	case "f", "F", "K", "x", "P":
		if r, ok := m.selectedRow(); ok {
			return m.prKey(k, r), true
		}
	case "c":
		if r, ok := m.selectedRow(); ok {
			return m.fixCIKey(r), true
		}
	case "E":
		if r, ok := m.selectedRow(); ok {
			return m.stagingKey(r), true
		}
	case "r", "R", "z", "M", "U":
		if r, ok := m.selectedRow(); ok {
			return m.prActKey(k, r, true), true
		}
	case "/":
		m.filtering = true
	case "O":
		return m.nextOwner(), true
	case "esc": // a filter kept is cleared, as on the board
		if m.filter.Value() == "" {
			return nil, false
		}
		m.filter.SetValue("")
		m.fixCursor()
	default:
		return nil, false
	}
	return nil, true
}

// filterKey is a key while home's filter has the keyboard: it edits the
// text, which narrows home at once; enter keeps the filter and gives the
// keys back, esc clears it; the arrows still move the cursor, as on the
// board. It asks nothing.
func (m *factoryModel) filterKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.filtering = false
		m.filter.SetValue("")
	case "enter":
		m.filtering = false
	case "down", "ctrl+n":
		m.moveTo(m.selIdx + 1)
		return nil
	case "up", "ctrl+p":
		m.moveTo(m.selIdx - 1)
		return nil
	default:
		m.filter = editLine(m.filter, msg)
	}
	m.fixCursor()
	return nil
}

// openPage opens task's PR (b) or its Jira issue (t) in the browser, asking
// nothing, as the board's b and t (spec 9.D): a task's, or your PR's (its
// id below zero, the issue its title names); one with nothing to open says
// why in the footer.
func (m *factoryModel) openPage(k string, task int64) tea.Cmd {
	if m.act == nil {
		return m.note(noActions)
	}
	c, key := m.data.Cards[task].Task, m.taskKey(task)
	name, url := cmp.Or(cleanText(c.PR), key), c.PRURL
	switch {
	case k == "t" && c.Issue == "" && task < 0: // your PR's row
		return m.note(key + " has no Jira issue: its title names no key of the trackers")
	case k == "t" && c.Issue == "":
		return m.note(key + " has no Jira issue: it started from your words")
	case k == "t" && c.IssueURL == "":
		return m.note("no link for " + cleanText(c.Issue) + ": no [board] trackers template names its key")
	case k == "t":
		name, url = cleanText(c.Issue), c.IssueURL
	case url == "":
		return m.note("no PR yet for " + key)
	}
	act := m.act
	return m.bar.start("Open "+name+" in the browser", func(ctx context.Context) (ActionResult, error) {
		if err := act.OpenBrowser(ctx, url); err != nil {
			return ActionResult{}, err
		}
		return ActionResult{Text: "opened " + name + ": " + url}, nil
	})
}

// openCard opens a row's card: the decision card a task that needs the
// operator waits on, else its task card; your PR's row (its id below zero)
// opens its PR's card, the task card's view with its PR's lines alone.
func (m *factoryModel) openCard(id int64) {
	if id == 0 {
		return
	}
	m.cardName = m.taskKey(id)
	m.rating = factoryRating{}
	cards := m.data.Cards[id]
	r, _ := m.row(id)
	m.selID = id
	m.fixCursor()
	switch {
	case r.Group == GroupNeedsYou && cards.Question != nil:
		m.view, m.task, m.q = fvQuestion, id, factoryQuestions{}
		m.startQuestions(id)
	case r.Group == GroupNeedsYou && cards.Ready != nil:
		m.view, m.task = fvReady, id
		if m.readies[id] == nil {
			m.freshReady(id, cards.Bind)
		}
	default:
		m.view, m.task = fvTask, id
	}
}

// nextDecision is the card of the oldest row in NEEDS YOU not walked yet,
// as the chip counts them: a task's decision card (its questions, its ready
// card), else its task card (a PR magnum never picked up, one closed
// without a merge, a task that stopped), and the card of a PR that needs
// you, yours or a published task's.
func (m factoryModel) nextDecision() (int64, bool) {
	var rows []FactoryRow
	for _, r := range m.data.Rows {
		if r.Group == GroupNeedsYou && r.ID != 0 && !m.walked[r.ID] {
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return 0, false
	}
	slices.SortStableFunc(rows, func(a, b FactoryRow) int {
		if a.Since.IsZero() != b.Since.IsZero() { // a task without a time comes last
			if a.Since.IsZero() {
				return 1
			}
			return -1
		}
		return a.Since.Compare(b.Since)
	})
	return rows[0].ID, true
}

// goHome goes back to home; a walk of the decisions ends.
func (m *factoryModel) goHome() {
	m.view, m.walk = fvHome, false
	m.q.typing, m.q.reasons = false, false
	m.rating = factoryRating{}
	if rs := m.readies[m.task]; rs != nil {
		rs.reasons, rs.noting = false, false
	}
}

// leaveDecision leaves a card decided on: for the next one on a walk, else
// for home with the cursor on the next row; a PR's card (the task card's
// view) is left so on a walk only. A card the operator left while the
// decision ran stays left.
func (m *factoryModel) leaveDecision(task int64) tea.Cmd {
	walkedPR := m.view == fvTask && m.walk
	if m.task != task || (m.view != fvQuestion && m.view != fvRecap && m.view != fvReady && !walkedPR) {
		return nil
	}
	if m.walk {
		return m.walkOn(task)
	}
	m.goHome()
	ids := m.order()
	if i := slices.Index(ids, task); i >= 0 && i+1 < len(ids) {
		m.selID, m.selIdx = ids[i+1], i+1
	}
	return nil
}

// walkOn ends the walk's visit of task's card: the next card that needs
// you, else home and the footer says nothing else waits.
func (m *factoryModel) walkOn(task int64) tea.Cmd {
	m.walked[task] = true
	if next, ok := m.nextDecision(); ok {
		m.openCard(next)
		return nil
	}
	m.goHome()
	return m.bar.show("Nothing else waits on you.", factoryFlashDone)
}

// dropSent takes out of task f.task's batch what answer f sent: an answer
// or a needless flag changed while it ran is still yours, and stays.
func (m *factoryModel) dropSent(f *factoryFollow) {
	b := m.batches[f.task]
	if b == nil {
		return
	}
	for q, text := range f.sent {
		if a, ok := b.answers[q]; ok && a.text == text {
			delete(b.answers, q)
		}
	}
	for q, why := range f.sentNeedless {
		if b.needless[q] == why {
			delete(b.needless, q)
		}
	}
	if len(b.answers) == 0 && len(b.needless) == 0 {
		delete(m.batches, f.task)
	}
}

// followed is the end of an action the screen asked: an answered batch or a
// decision leaves its card, answers to questions that changed show the
// recap's variant, a start goes home onto its new row. The factory is read
// again either way.
func (m *factoryModel) followed(f *factoryFollow, err error) tea.Cmd {
	cmds := []tea.Cmd{m.reload()}
	if f == nil {
		return tea.Batch(cmds...)
	}
	switch f.kind {
	case followAnswer:
		switch {
		case err == nil:
			m.dropSent(f)
			cmds = append(cmds, m.leaveDecision(f.task))
		case errors.Is(err, ErrQuestionsChanged) && m.task == f.task && (m.view == fvRecap || m.view == fvQuestion):
			ch := &recapChange{}
			if e, ok := errors.AsType[*QuestionsChangedError](err); ok {
				ch.at, ch.why = e.At, e.Why
			}
			// The new revision may be read already (rebind then rebound the
			// batch): name its question that is open again now; otherwise the
			// reload names it.
			if c := m.data.Cards[f.task].Question; c != nil && c.Revision != f.rev {
				if i := m.nextOpen(c, -1); i >= 0 {
					ch.again = i + 1
				}
			}
			m.view, m.q.changed = fvRecap, ch
		}
	case followDecide:
		if err == nil {
			delete(m.readies, f.task)
			cmds = append(cmds, m.leaveDecision(f.task))
		}
	case followStart:
		if err == nil {
			m.form = factoryDraft{area: m.form.area}
			m.form.area.SetValue("")
			if m.view == fvNew {
				m.view = fvHome
			}
			m.selectKey = f.key
		}
	case followStop:
		if err == nil && m.view == fvTask && m.task == f.task {
			m.goHome()
		}
	case followPR:
		if err == nil {
			cmds = append(cmds, m.leaveDecision(f.task))
		}
	}
	return tea.Batch(cmds...)
}

// the question card and the recap

// batch is the task's batch for the revision of its card.
func (m *factoryModel) batch(task int64, c *QuestionCard) *factoryBatch {
	b := m.batches[task]
	if b == nil || b.rev != c.Revision {
		b = &factoryBatch{rev: c.Revision, answers: map[string]factoryAnswer{}, needless: map[string]string{}}
		m.batches[task] = b
	}
	return b
}

func (b *factoryBatch) answerOf(text string) (factoryAnswer, bool) {
	if b == nil {
		return factoryAnswer{}, false
	}
	a, ok := b.answers[text]
	return a, ok
}

// answered reports whether the question has an answer: the factory's, or
// one of the batch.
func (m factoryModel) answered(task int64, q CardQuestion) bool {
	if q.Answer != "" {
		return true
	}
	_, ok := m.batches[task].answerOf(q.Text)
	return ok
}

// startQuestions puts the first question without an answer on the card, the
// mark on its recommendation; with every one answered, the recap.
func (m *factoryModel) startQuestions(task int64) {
	c := m.data.Cards[task].Question
	if c == nil {
		return
	}
	m.batch(task, c)
	m.q.changed = nil
	cur := m.nextOpen(c, -1)
	if cur < 0 {
		m.view = fvRecap
		return
	}
	m.view = fvQuestion
	m.setQuestion(c, cur)
}

// nextOpen is the first question after after (round the batch) without an
// answer; -1 when every one has one.
func (m factoryModel) nextOpen(c *QuestionCard, after int) int {
	n := len(c.Questions)
	for k := 1; k <= n; k++ {
		if i := (after + k + n) % n; !m.answered(m.task, c.Questions[i]) {
			return i
		}
	}
	return -1
}

// setQuestion puts question cur on the card: the mark on the operator's
// answer, else on the recommendation, else on none.
func (m *factoryModel) setQuestion(c *QuestionCard, cur int) {
	m.q.cur, m.q.cursor, m.q.reasons, m.q.typing = cur, -1, false, false
	q := c.Questions[cur]
	if a, ok := m.batches[m.task].answerOf(q.Text); ok && a.option >= 0 {
		m.q.cursor = a.option
	} else if q.Recommended >= 0 && q.Recommended < len(q.Options) {
		m.q.cursor = q.Recommended
	}
}

// questionCard is the open task's question card, the batch's answers in it.
func (m factoryModel) questionCard() (QuestionCard, bool) {
	src := m.data.Cards[m.task].Question
	if src == nil {
		return QuestionCard{}, false
	}
	c := *src
	c.Questions = slices.Clone(src.Questions)
	b := m.batches[m.task]
	if b != nil && b.rev != c.Revision {
		b = nil
	}
	for i, q := range c.Questions {
		if q.Answer != "" {
			c.Questions[i].sent = true
			continue
		}
		a, ok := b.answerOf(q.Text)
		if !ok {
			continue
		}
		q.AnsweredAt, q.Answer = a.at, a.text
		if a.option >= 0 && a.option < len(q.Options) {
			o := q.Options[a.option]
			q.Answer = lowerFirst(o.Label)
			q.TookRecommendation = a.option == q.Recommended
			if login, ok := strings.CutPrefix(optionSends(o), "ask:"); ok {
				q.Asks = login
				q.Comment = strings.TrimRight(AskComment(login, c.TaskKey, q.Text, q.Context, q.Example, q.Options), "\n")
			}
		}
		c.Questions[i] = q
	}
	c.Current = min(max(m.q.cur, 0), max(len(c.Questions)-1, 0))
	c.Cursor = m.q.cursor
	if m.q.reasons {
		c.NeedlessReasons = reasonWords(factoryNeedlessReasons)
	}
	if m.q.typing {
		t := lineTyped(m.input)
		c.input = &cardInput{label: "Your answer · enter takes it · esc drops it", text: t.text, after: t.after}
	}
	c.changed = m.q.changed
	c.sending = m.bar.busy != "" && m.inflight != nil && m.inflight.kind == followAnswer
	c.rule = m.rule()
	if m.view == fvQuestion { // the recap draws its own question
		c.Ask = m.bar.question()
	}
	if len(c.Questions) > 0 && m.view == fvQuestion {
		p := newCardPainter(m.viewWidth(), m.st, m.g, m.now())
		c.Hints = dropHints(p.questionHints(c, c.Questions[c.Current], c.Current), "z")
	}
	return c, true
}

// batchOf is the batch to send for card c: the answers and the needless
// flags of its questions the factory has no answer to, by index from 0.
func (m factoryModel) batchOf(c QuestionCard) (answers, needless map[int]string) {
	answers, needless = map[int]string{}, map[int]string{}
	b := m.batches[m.task]
	if b == nil || b.rev != c.Revision {
		return answers, needless
	}
	for i, q := range c.Questions {
		if a, ok := b.answers[q.Text]; ok && !q.sent {
			answers[i] = a.text
		}
		if r, ok := b.needless[q.Text]; ok {
			needless[i] = r
		}
	}
	return answers, needless
}

func optionSends(o CardOption) string { return cmp.Or(strings.TrimSpace(o.Send), o.Label) }

// lowerFirst puts the first letter of s in lower case.
func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 || r == utf8.RuneError {
		return s
	}
	return string(unicode.ToLower(r)) + s[n:]
}

func (m *factoryModel) questionKey(k string) (tea.Cmd, bool) {
	c := m.data.Cards[m.task].Question
	if c == nil || len(c.Questions) == 0 {
		if k == "esc" {
			m.goHome()
			return nil, true
		}
		return nil, false
	}
	cur := min(max(m.q.cur, 0), len(c.Questions)-1)
	q := c.Questions[cur]
	if m.q.reasons { // − asked for a reason's digit
		switch i, err := strconv.Atoi(k); {
		case k == "esc":
			m.q.reasons = false
		case err == nil && i >= 1 && i <= len(factoryNeedlessReasons):
			m.q.reasons = false
			r := factoryNeedlessReasons[i-1]
			m.batch(m.task, c).needless[q.Text] = r.key
			return m.note(fmt.Sprintf("question %d flagged needless: %s", cur+1, r.words)), true
		}
		return nil, true
	}
	switch k {
	case "j", "down":
		m.q.cursor = min(max(m.q.cursor+1, 0), len(q.Options)-1)
	case "k", "up":
		m.q.cursor = max(m.q.cursor-1, 0)
	case "enter":
		if m.q.cursor < 0 || m.q.cursor >= len(q.Options) {
			return m.note("no recommendation: pick one (j k, or its digit)"), true
		}
		return m.take(c, cur, m.q.cursor), true
	case "-", "−":
		if c.Needless {
			m.q.reasons = true
		}
	case "backspace":
		if cur > 0 {
			m.setQuestion(c, cur-1)
		}
	case "esc":
		m.goHome()
	default:
		if i, err := strconv.Atoi(k); err == nil && i >= 1 && i <= min(len(q.Options), 9) {
			return m.take(c, cur, i-1), true
		}
		return nil, false
	}
	return nil, true
}

// take answers question cur with option i and moves on (no y/N: nothing is
// sent before the batch's y); your own words are typed first.
func (m *factoryModel) take(c *QuestionCard, cur, i int) tea.Cmd {
	q := c.Questions[cur]
	if q.Answer != "" {
		return m.note(fmt.Sprintf("the factory has the answer to question %d already", cur+1))
	}
	o := q.Options[i]
	m.q.cursor = i
	if o.Own {
		m.q.typing = true
		m.input.SetValue("")
		return nil
	}
	m.batch(m.task, c).answers[q.Text] = factoryAnswer{option: i, text: optionSends(o), at: m.now()}
	m.advance(c)
	return nil
}

// advance moves to the next question without an answer, else to the recap.
func (m *factoryModel) advance(c *QuestionCard) {
	next := m.nextOpen(c, m.q.cur)
	if next < 0 {
		m.view = fvRecap
		return
	}
	m.setQuestion(c, next)
}

// recapKey is a key on the recap: a digit changes its question's answer,
// esc goes back to the questions; on "the questions changed", enter shows
// them again.
func (m *factoryModel) recapKey(k string) (tea.Cmd, bool) {
	c := m.data.Cards[m.task].Question
	if c == nil || len(c.Questions) == 0 {
		if k == "esc" {
			m.goHome()
			return nil, true
		}
		return nil, false
	}
	if ch := m.q.changed; ch != nil {
		switch k {
		case "enter":
			m.q.changed = nil
			m.view = fvQuestion
			m.setQuestion(c, min(max(ch.again-1, 0), len(c.Questions)-1))
			return m.reload(), true
		case "esc":
			m.q.changed = nil
			m.goHome()
			return nil, true
		}
		return nil, false
	}
	switch i, err := strconv.Atoi(k); {
	case k == "esc":
		m.view = fvQuestion
		m.setQuestion(c, len(c.Questions)-1)
	case err == nil && i >= 1 && i <= len(c.Questions):
		m.view = fvQuestion
		m.setQuestion(c, i-1)
	default:
		return nil, false
	}
	return nil, true
}

// inputKey types the line a card asks for: your own answer, the note of a
// send back. enter takes it, esc drops it.
func (m *factoryModel) inputKey(msg tea.KeyPressMsg) tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	switch msg.String() {
	case "esc":
		m.q.typing = false
		if rs := m.readies[m.task]; rs != nil {
			rs.noting = false
		}
		return nil
	case "enter":
		if text == "" {
			return m.note("type it first, or esc")
		}
		if m.view == fvReady {
			return m.askSendBack(text)
		}
		m.q.typing = false
		c := m.data.Cards[m.task].Question
		if c == nil || m.q.cur >= len(c.Questions) {
			return nil
		}
		m.batch(m.task, c).answers[c.Questions[m.q.cur].Text] = factoryAnswer{option: -1, text: text, at: m.now()}
		m.advance(c)
		return nil
	}
	m.input = editLine(m.input, msg)
	return nil
}

// editLine and editText hand a key or a paste to the line or the text being
// typed. Their commands are dropped: the painter draws the text and its
// cursor itself, which never blinks, and a paste comes from the terminal
// (tea.PasteMsg), never from a read of the clipboard (the shared keys have
// no ctrl+v).
func editLine(in textinput.Model, msg tea.Msg) textinput.Model {
	in, _ = in.Update(msg)
	return in
}

func editText(area textarea.Model, msg tea.Msg) textarea.Model {
	area, _ = area.Update(msg)
	return area
}

// the ready card

func (m factoryModel) readyState() *factoryReady {
	if rs := m.readies[m.task]; rs != nil {
		return rs
	}
	return &factoryReady{cursor: -1}
}

// readyCard is the open task's ready card, the screen's mark and times in it.
func (m factoryModel) readyCard() (ReadyCard, bool) {
	cards := m.data.Cards[m.task]
	if cards.Ready == nil {
		return ReadyCard{}, false
	}
	c := *cards.Ready
	rs := m.readyState()
	c.Cursor = rs.cursor
	if !rs.diffAt.IsZero() {
		c.DiffOpened = rs.diffAt
		c.Unread = nil // 1a's pager shows the whole diff: every file was opened
	}
	c.Ask = m.bar.question()
	c.askOther = c.Ask != "" && (m.asked == nil || m.asked.kind != followDecide) // not the card's own push or drop
	if rs.reasons {
		c.Reasons = reasonWords(factorySendBackReasons)
	}
	if rs.noting {
		t := lineTyped(m.input)
		c.input = &cardInput{label: "Your note to the writer · enter asks y/N · esc drops it", text: t.text, after: t.after}
	}
	c.rule = m.rule()
	c.Hints = newCardPainter(m.viewWidth(), m.st, m.g, m.now()).readyHints(c)
	return c, true
}

func (m *factoryModel) readyKey(k string) (tea.Cmd, bool) {
	c, ok := m.readyCard()
	if !ok {
		if k == "esc" {
			m.goHome()
			return nil, true
		}
		return nil, false
	}
	rs := m.readies[m.task]
	if rs == nil {
		rs = m.freshReady(m.task, m.data.Cards[m.task].Bind)
	}
	if rs.reasons { // send back asked for a reason's digit
		switch i, err := strconv.Atoi(k); {
		case k == "esc":
			rs.reasons = false
		case err == nil && i >= 1 && i <= len(factorySendBackReasons):
			rs.reasons, rs.reason, rs.noting = false, i-1, true
			m.input.SetValue(rs.note) // a note whose y/N was answered no comes back
		}
		return nil, true
	}
	// On a card that offers no push the mark moves over send back and drop.
	first := readyPush
	if c.NoPush != "" {
		first = readySendBack
	}
	switch k {
	case "j", "down":
		rs.cursor = min(max(rs.cursor+1, first), len(readyOptions)-1)
	case "k", "up":
		rs.cursor = max(rs.cursor-1, first)
	case "1", "2", "3":
		rs.cursor = int(k[0] - '1')
		return m.doReady(c, rs.cursor), true
	case "enter":
		if rs.cursor < 0 {
			return m.note("pick one: j k, or 1-3"), true
		}
		return m.doReady(c, rs.cursor), true
	case "d":
		return m.openDiff(m.task), true
	case knownKey:
		if c.Known <= 0 { // the card offers no such key
			return nil, false
		}
		return m.askKnown(c), true
	case "esc":
		m.goHome()
	default:
		return nil, false
	}
	return nil, true
}

// knownKey sends the known defects the local review left back to the
// writer, on a ready card that counts some.
const knownKey = "w"

// askKnown asks y/N for the send back of the card's known defects, naming
// how many go back; y sends it with the reason FactoryReasonKnown and no
// note (the factory writes the note from the card's review).
func (m *factoryModel) askKnown(c ReadyCard) tea.Cmd {
	return m.decide("Send "+factoryPlural(c.Known, "known defect", "known defects")+" back to the writer?", FactorySendBack, FactoryReasonKnown, "")
}

// doReady does option i of the ready card: a push or a drop asks y/N, a send
// back asks for its reason's digit first.
func (m *factoryModel) doReady(c ReadyCard, i int) tea.Cmd {
	key := cmp.Or(c.TaskKey, m.taskKey(m.task))
	switch i {
	case readyPush:
		if c.NoPush != "" {
			return m.note("push is not offered: " + c.NoPush)
		}
		sentence := "Push without reading the diff?"
		if !c.DiffOpened.IsZero() {
			sentence = "Push " + cmp.Or(c.Branch, key) + " and open a PR?"
		}
		return m.decide(sentence, FactoryPush, "", "")
	case readySendBack:
		m.readies[m.task].reasons = true
	case readyDrop:
		return m.decide("Drop "+key+": free "+cmp.Or(c.Copy, "its copy")+", keep the branch?", FactoryDrop, "", "")
	}
	return nil
}

// askSendBack asks y/N for the send back with its reason and note.
func (m *factoryModel) askSendBack(note string) tea.Cmd {
	rs := m.readyState()
	r := factorySendBackReasons[min(max(rs.reason, 0), len(factorySendBackReasons)-1)]
	rs.noting, rs.note = false, note
	key := m.taskKey(m.task)
	if c := m.data.Cards[m.task].Ready; c != nil && c.TaskKey != "" {
		key = cleanText(c.TaskKey)
	}
	return m.decide("Send "+key+" back to the writer ("+r.words+")?", FactorySendBack, r.key, note)
}

// decide asks sentence for a decision on the ready card, bound to what the
// card showed and to the screen's own times.
func (m *factoryModel) decide(sentence, choice, reason, note string) tea.Cmd {
	task, act := m.task, m.act
	bound := m.data.Cards[task].Bind
	c, _ := m.readyCard()
	rs := m.readyState()
	bound.Shown, bound.DiffOpened, bound.DiffDwell = rs.shown, c.DiffOpened, rs.dwell
	bound.Recommended = ""
	if rec := c.shownRecommendation(); rec >= 0 {
		bound.Recommended = readyChoices[rec]
	}
	return m.ask(&factoryFollow{kind: followDecide, task: task}, sentence, func(ctx context.Context) (ActionResult, error) {
		return act.Decide(ctx, task, choice, reason, note, bound)
	})
}

// openDiff runs the pager on the task's diff (Diff only builds the command,
// on the screen's turn); on the ready card the time it was read counts.
func (m *factoryModel) openDiff(task int64) tea.Cmd {
	if m.act == nil {
		return m.note(noActions)
	}
	cmd := m.act.Diff(task)
	if cmd == nil {
		return m.note("no diff to show for " + m.taskKey(task))
	}
	if rs := m.readies[task]; rs != nil && m.view == fvReady {
		rs.opening = m.now()
	}
	if m.diffs[task] != diffRead {
		m.diffs[task] = diffOpening
	}
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return factoryPagerMsg{task: task, err: err} })
}

// pagerDone is the pager's end: on the ready card the diff counts as read
// (when it was first opened, and for how long in all), and the mark goes
// to the recommendation unless the operator put it somewhere.
func (m *factoryModel) pagerDone(msg factoryPagerMsg) tea.Cmd {
	if m.diffs[msg.task] == diffOpening { // the pager of the diff (d), not of a report: the diff was opened if the pager ran
		if msg.err == nil {
			m.diffs[msg.task] = diffRead
		} else {
			delete(m.diffs, msg.task)
		}
	}
	rs := m.readies[msg.task]
	var opening time.Time
	if rs != nil {
		opening, rs.opening = rs.opening, time.Time{}
	}
	if msg.err != nil {
		return m.bar.show("the pager failed: "+errLine(msg.err), factoryFlashFailed)
	}
	if rs == nil || opening.IsZero() {
		return m.reload()
	}
	if rs.diffAt.IsZero() {
		rs.diffAt = opening
	}
	rs.dwell += max(m.now().Sub(opening), 0)
	if c := m.data.Cards[msg.task].Ready; c != nil && rs.cursor < 0 && c.Recommended >= 0 && c.Recommended < len(readyOptions) &&
		(c.Recommended != readyPush || c.NoPush == "") {
		rs.cursor = c.Recommended
	}
	return m.reload()
}

// the task card

// taskCard is the open task's card.
func (m factoryModel) taskCard() TaskCard {
	c := m.data.Cards[m.task].Task
	if c.Key == "" {
		c.Key = m.taskKey(m.task)
	}
	if r, ok := m.row(m.task); ok && c.Title == "" {
		c.Title = r.Title
	}
	c.Cursor = m.taskCursor(c)
	c.Spinner = m.anim
	c.Ask, c.askWrap = m.bar.question(), m.bar.asksWrapped()
	c.rule = m.rule()
	p := newCardPainter(m.viewWidth(), m.st, m.g, m.now())
	if v := m.pendingRating(); v != 0 && c.Ask == "" {
		c.rateAsk, c.rateReasons = "Why "+p.ratingSign(v)+"?", reasonWords(factoryRatingReasons)
	}
	r, _ := m.row(m.task)
	if r.PR != nil {
		c.StagingDeploys = r.PR.StagingDeploys
	}
	e, deployFirst := m.deployFirstHint(r) // the row's move: first, after j k on a task's card
	if r.yourPR() {
		c.Hints = m.walkHint(m.reportHints(m.prCardHints(r, c), m.task, "b", "t"))
		if deployFirst {
			c.Hints = slices.Insert(c.Hints, 0, e)
		}
		return c
	}
	unoffered := []string{"enter", "l"} // the painter's own hints that the screen draws otherwise or has no key for
	if !c.Rate {
		unoffered = append(unoffered, "+ −", "+ -") // + and − rate a published or a done task only
	}
	c.Hints = m.reportHints(pageHints(dropHints(p.taskHints(c), unoffered...), c), m.task, "d", "b", "t")
	if what := m.waitingDecision(m.task); what != "" && len(c.Hints) > 0 {
		c.Hints = slices.Insert(c.Hints, 1, hint{"enter", what}) // after j k
	}
	if r.PR != nil && r.PR.Fixing { // magnum's loop on the task's PR
		c.Hints = slices.Insert(c.Hints, max(len(c.Hints)-2, 0), hint{"K", "stop magnum"})
	}
	c.Hints = slices.Insert(c.Hints, max(len(c.Hints)-2, 0), m.undoHints(r, !c.Retry)...) // before esc and ?
	if deployFirst {
		c.Hints = slices.Insert(c.Hints, min(1, len(c.Hints)), e)
	}
	c.Hints = m.walkHint(c.Hints)
	return c
}

// walkHint is hs with a's "next" before esc while a walk visits the card.
func (m factoryModel) walkHint(hs []hint) []hint {
	i := slices.IndexFunc(hs, func(h hint) bool { return h.key == "esc" })
	if !m.walk || i < 0 {
		return hs
	}
	return slices.Insert(slices.Clone(hs), i, hint{"a", "next"})
}

// prCardHints are the hints of your PR's card c, of row r: the fix keys
// (c, the CI fix, where it acts, as the move of a failed check names it), the
// review where r acts (the move of a PR whose review magnum owes names it),
// magnum's pane, and the pages it has.
func (m factoryModel) prCardHints(r FactoryRow, c TaskCard) []hint {
	hs := []hint{{"f", "fix"}}
	if m.act != nil && actionRefusal(actFixCI, m.prActRow(r, true)) == "" {
		hs = append(hs, hint{"c", rowActDefs[actFixCI].label})
	}
	hs = append(hs, hint{"F", prFixOffVerb(*r.PR)})
	if r.PR.Fixing {
		hs = append(hs, hint{"K", "stop"})
	}
	if m.act != nil && actionRefusal(actReview, m.prActRow(r, true)) == "" {
		hs = append(hs, hint{"r", "review"})
	}
	hs = append(hs, m.undoHints(r, true)...)
	if m.act != nil {
		hs = append(hs, hint{"o", "magnum's pane"})
	}
	if c.PRURL != "" {
		hs = append(hs, hint{"b", "PR"})
	}
	if c.IssueURL != "" {
		hs = append(hs, hint{"t", "Jira"})
	}
	return append(hs, hint{"esc", "back"}, hint{"?", "more"})
}

// waitingDecision is what enter opens on task id's card: "answer" for the
// questions it waits on you for, "ready card" for its ready card, "" when
// it waits on no decision card (enter then stays, as no stage card exists
// yet). A task card left open while its task came to need you opens the
// decision as enter on home does.
func (m factoryModel) waitingDecision(id int64) string {
	r, ok := m.row(id)
	if !ok || r.Group != GroupNeedsYou {
		return ""
	}
	switch cards := m.data.Cards[id]; {
	case cards.Question != nil:
		return "answer"
	case cards.Ready != nil:
		return "ready card"
	}
	return ""
}

// pageHints are the task card's hints hs with b and t after d, each when
// the card has its page to open: the painter's own hints are the approved
// frames', which leave them to the help.
func pageHints(hs []hint, c TaskCard) []hint {
	var pages []hint
	if c.PRURL != "" {
		pages = append(pages, hint{"b", "PR"})
	}
	if c.IssueURL != "" {
		pages = append(pages, hint{"t", "Jira"})
	}
	i := slices.IndexFunc(hs, func(h hint) bool { return h.key == "d" })
	return slices.Insert(slices.Clone(hs), i+1, pages...)
}

// taskStops counts the lines the task card's mark stands on.
func taskStops(c TaskCard) int {
	n := len(c.Stages) + len(c.Agents)
	if len(c.Report) > 0 {
		n++
	}
	return n
}

// taskCursor is the task card's mark: where the operator put it, else on the
// stage at work (or the one that stopped), else the first.
func (m factoryModel) taskCursor(c TaskCard) int {
	if i, ok := m.cursors[m.task]; ok {
		return min(i, max(taskStops(c)-1, 0))
	}
	if i := slices.IndexFunc(c.Stages, func(s CardStage) bool {
		return s.State == "working" || s.State == "failed" || s.State == "waiting"
	}); i >= 0 {
		return i
	}
	return 0
}

func (m *factoryModel) taskCardKey(k string) (tea.Cmd, bool) {
	c := m.taskCard()
	n, cur := taskStops(c), c.Cursor
	task, act := m.task, m.act
	r, _ := m.row(task)
	if v := m.pendingRating(); v != 0 { // + or − asked for a reason's digit
		return m.rateKey(v, k), true
	}
	switch {
	case k == "f" || k == "F" || k == "K":
		if r.ID == 0 {
			return m.note("f, F and K act on a PR in magnum's registry: " + m.taskKey(task) + " has none there"), true
		}
		return m.prKey(k, r), true
	case k == "c":
		if r.ID == 0 {
			return m.note("c acts on a PR in magnum's registry: " + m.taskKey(task) + " has none there"), true
		}
		return m.fixCIKey(r), true
	case k == "E":
		if r.ID == 0 {
			return m.note("E acts on a PR in magnum's registry: " + m.taskKey(task) + " has none there"), true
		}
		return m.stagingKey(r), true
	case r.yourPR() && k == "x":
		return m.note("x drops a task: " + m.taskKey(task) + " is your PR (K stops magnum's loop)"), true
	case r.yourPR() && k == "d":
		return m.note("no diff of the factory's for " + m.taskKey(task) + ": b opens the PR"), true
	}
	switch k {
	case "a": // the walk's next card; off a walk, its first
		if !m.walk {
			m.walk, m.walked = true, map[int64]bool{}
		}
		return m.walkOn(task), true
	case "j", "down":
		m.cursors[task] = min(cur+1, max(n-1, 0))
	case "k", "up":
		m.cursors[task] = max(cur-1, 0)
	case "g", "home":
		m.cursors[task] = 0
	case "G", "end":
		m.cursors[task] = max(n-1, 0)
	case "d":
		return m.openDiff(task), true
	case "b", "t":
		return m.openPage(k, task), true
	case "enter":
		if m.waitingDecision(task) != "" {
			m.openCard(task)
		}
	case "r":
		if !c.Retry { // the review of its PR, as the board's r
			return m.cardPRActKey(k, r, false), true
		}
		return m.ask(&factoryFollow{kind: followTask, task: task},
			"Retry "+c.Key+": it goes on at its stage, which checks again what stopped it?",
			func(ctx context.Context) (ActionResult, error) { return act.Retry(ctx, task) }), true
	case "R", "z", "M", "U":
		return m.cardPRActKey(k, r, c.Retry), true
	case "x":
		return m.ask(&factoryFollow{kind: followStop, task: task}, "Drop "+c.Key+": stop its agents, free its copy, keep the branch?",
			func(ctx context.Context) (ActionResult, error) { return act.Stop(ctx, task) }), true
	case acceptQAKey:
		return m.askAcceptQA(c), true
	case "+", "-", "−":
		switch {
		case !c.Rate: // a task at work is not rated: nothing happens
		case act == nil:
			return m.note(noActions), true
		case m.bar.busy != "": // refused at the key: the digit after it would find the bar busy and lose the rating
			return m.bar.show("wait: "+m.bar.busy+" runs", factoryFlashWait), true
		case k == "+":
			m.rating = factoryRating{task: task, value: 1}
		default:
			m.rating = factoryRating{task: task, value: -1}
		}
	case "esc":
		m.goHome()
	default:
		return nil, false
	}
	return nil, true
}

// pendingRating is the value (1 or -1) of the + or − the open task card
// waits for a reason's digit of; 0 when none, and for one asked on another
// card.
func (m factoryModel) pendingRating() int {
	if m.view == fvTask && m.rating.task == m.task {
		return m.rating.value
	}
	return 0
}

// rateKey is a key while a rating of value (1 or -1) waits for its reason: a
// digit of factoryRatingReasons gives the rating with that reason's key,
// esc gives it with none; any other key waits.
func (m *factoryModel) rateKey(value int, k string) tea.Cmd {
	task := m.task
	switch i, err := strconv.Atoi(k); {
	case k == "esc":
		m.rating = factoryRating{}
		return m.rate(task, value, "")
	case err == nil && i >= 1 && i <= len(factoryRatingReasons):
		m.rating = factoryRating{}
		return m.rate(task, value, factoryRatingReasons[i-1].key)
	}
	return nil
}

// rate gives task the rating value with the key of a reason ("" for none),
// asking nothing: a rating is undoable (rating again stores a new one). The
// factory is told whether this run of the screen opened the task's diff
// first; what it answers goes to the footer.
func (m *factoryModel) rate(task int64, value int, reason string) tea.Cmd {
	act := m.act
	if act == nil {
		return m.note(noActions)
	}
	diff := m.diffs[task] == diffRead
	sign := newCardPainter(m.viewWidth(), m.st, m.g, m.now()).ratingSign(value)
	ascii := m.g.mode == IconsASCII
	return m.bar.start("Rate "+m.taskKey(task)+" "+sign, func(ctx context.Context) (ActionResult, error) {
		res, err := act.Rate(ctx, task, value, reason, diff)
		if ascii { // the factory writes the minus sign; these glyphs draw a hyphen
			res.Text = strings.ReplaceAll(res.Text, "−", "-")
		}
		return res, err
	})
}

// cardPRActKey is r, R, z, M or U on the task card of row r (prActKey):
// r is the review where the card offers no retry (retry false), which r
// is otherwise, and the questions then name no key for the review.
func (m *factoryModel) cardPRActKey(k string, r FactoryRow, retry bool) tea.Cmd {
	if r.ID == 0 {
		return m.note("r, R, z, M and U act on a PR in magnum's registry: " + m.taskKey(m.task) + " has none there")
	}
	return m.prActKey(k, r, !retry)
}

// new task

// newTaskForm is the new task's form: the text, what the factory made of it
// when the text is not blank, the kind the operator picked and the question.
func (m factoryModel) newTaskForm() NewTaskForm {
	text := m.form.area.Value()
	f := NewTaskForm{}
	if strings.TrimSpace(text) != "" {
		f = m.form.preview
	}
	if f.Jira != nil { // "pgdn reads more" names a key this screen does not have: the excerpt is all it shows
		j := *f.Jira
		j.More = false
		f.Jira = &j
	}
	if m.form.pickFor != m.form.repo && severalRepos(f) {
		// Every row but REPO is the preview of another repository than your
		// pick's: none is drawn until the pick's preview is read.
		f = NewTaskForm{Repos: f.Repos}
	}
	f.Text, f.after = text, utf8.RuneCountInString(text)-textCursor(m.form.area)
	if m.form.repo != "" && severalRepos(f) {
		f.Repo, f.RepoWhy = m.form.repo, "your pick"
	}
	if m.form.kind != "" && f.Kind != "" {
		f.Kind, f.KindWhy = m.form.kind, "your pick"
		if agents, ok := f.AgentsByKind[f.Kind]; ok { // the agents the start launches for the pick, not the guess's
			f.Agents = agents
		}
	}
	f.Reading, f.Spinner = m.reading(), m.anim
	f.Ask = m.bar.question()
	f.rule = m.rule()
	if strings.TrimSpace(text) != "" && f.Ask == "" {
		f.Hints = dropHints(newCardPainter(m.viewWidth(), m.st, m.g, m.now()).newTaskHints(f), "pgdn")
	}
	return f
}

// reading reports whether the factory reads Jira for the text: a preview
// runs for a text that starts with a key or a Jira link, and not with the
// key the last preview said is not Jira's (NotJira), bare or in a link.
func (m factoryModel) reading() bool {
	text := m.form.area.Value()
	if k := m.form.preview.NotJira; k != "" {
		if first, _, _ := strings.Cut(strings.TrimSpace(text)+" ", " "); strings.TrimRight(first, ":,;") == k || strings.Contains(first, "/"+k) {
			return false
		}
	}
	return m.form.inFlight && !m.form.fresh() && jiraWords.MatchString(text)
}

func (m *factoryModel) newTaskKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc": // the draft stays for the next n; enter's question is not asked off the form
		m.view, m.form.askOnce = fvHome, false
		return nil
	case "enter":
		return m.submit()
	case "tab":
		if kind := cmp.Or(m.form.kind, m.form.preview.Kind); kind != "" {
			m.form.kind = factoryKinds[(slices.Index(factoryKinds, kind)+1)%len(factoryKinds)]
		}
		return nil
	case repoKey: // the next repository: the form only, whose preview is read again for it
		m.form.askOnce = false // enter's question is asked only for the repository it was pressed on
		if len(m.form.preview.Repos) < 2 {
			if strings.TrimSpace(m.form.area.Value()) == "" || m.form.fresh() {
				return nil // one repository, or no text to preview
			}
			m.form.cycles++ // before the text's first preview: kept, and applied when it lands
			return m.previewCmd()
		}
		m.nextRepo()
		return m.previewCmd()
	case "ctrl+g":
		return m.editDraft()
	}
	switch {
	case key.Matches(msg, editKeys.lineUp):
		m.moveRow(-1)
		return nil
	case key.Matches(msg, editKeys.lineDown):
		m.moveRow(1)
		return nil
	}
	m.form.goal = 0
	before := m.form.area.Value()
	if m.form.area = editText(m.form.area, msg); m.form.area.Value() != before {
		return m.textChanged()
	}
	return nil
}

// moveRow moves the new task's cursor d rows down (up when d is below
// zero) through the rows the painter draws, a long line's too, on the cell
// column it had when the moves began, or as near as the row allows; on the
// first row up and on the last one down it stays.
func (m *factoryModel) moveRow(d int) {
	rows := typedRows(m.form.area.Value(), inputTextW(m.viewWidth()))
	row, col := cursorRowAt(rows, textCursor(m.form.area))
	if m.form.goal == 0 {
		m.form.goal = col + 1
	}
	if to := row + d; to >= 0 && to < len(rows) {
		setTextCursor(&m.form.area, rows[to].offsetAt(m.form.goal-1))
	}
}

// nextRepo picks the repository after the one the form shows, in the
// preview's order.
func (m *factoryModel) nextRepo() {
	repos := m.form.preview.Repos
	m.form.repo = repos[(slices.Index(repos, cmp.Or(m.form.repo, m.form.preview.Repo))+1)%len(repos)]
}

// textChanged reads the text's preview after a pause in typing.
//
// The text changed (typed, pasted, back from the editor, or the form opened
// on its draft): enter's question waits no more for its preview, since it
// is asked only for the text enter was pressed on; a y typed meanwhile is
// text, never an answer. Up and down forget the column they kept.
func (m *factoryModel) textChanged() tea.Cmd {
	m.form.seq++
	m.form.kind, m.form.askOnce, m.form.goal = "", false, 0
	seq := m.form.seq
	return m.opts.Tick(factoryPreviewWait, func(time.Time) tea.Msg { return factoryPreviewDueMsg{seq: seq} })
}

// previewCmd reads what the factory makes of the text, once the preview in
// flight (for an older text) ends.
func (m *factoryModel) previewCmd() tea.Cmd {
	text := m.form.area.Value()
	if m.act == nil || strings.TrimSpace(text) == "" {
		return nil
	}
	if m.form.inFlight {
		m.form.again = true
		return nil
	}
	m.form.inFlight, m.form.again = true, false
	ctx, act, repo := m.ctx, m.act, m.form.repo
	return func() tea.Msg {
		ctx, cancel := loadContext(ctx)
		defer cancel()
		f, err := act.Preview(ctx, text, repo)
		return factoryPreviewMsg{text: text, repo: repo, form: f, err: err}
	}
}

// previewed takes a preview: the current text's and pick's is kept (and
// enter's question asked, when it waited for it and the form is still open: a
// question asked off the form would be answered by what you type there);
// an older one's is read again.
func (m *factoryModel) previewed(msg factoryPreviewMsg) tea.Cmd {
	m.form.inFlight = false
	if msg.text != m.form.area.Value() || msg.repo != m.form.repo {
		if m.form.again || m.form.askOnce {
			return m.previewCmd()
		}
		return nil
	}
	m.form.preview, m.form.previewFor, m.form.pickFor = msg.form, msg.text, msg.repo
	if msg.err != nil {
		m.form.preview = NewTaskForm{Text: msg.text, Err: "the factory could not read your text: " + errLine(msg.err)}
	}
	if n := m.form.cycles; n > 0 { // shift+tab pressed before this preview: the pick, read next
		m.form.cycles = 0
		if severalRepos(m.form.preview) {
			for range n {
				m.nextRepo()
			}
			return m.previewCmd()
		}
	}
	if m.form.askOnce {
		m.form.askOnce = false
		if m.view == fvNew {
			return m.askStart()
		}
	}
	return nil
}

// submit is enter on new task: with the text's preview read, the question
// that starts the task; else the preview first.
func (m *factoryModel) submit() tea.Cmd {
	text := m.form.area.Value()
	switch {
	case strings.TrimSpace(text) == "":
		return nil
	case m.act == nil:
		return m.note(noActions)
	case m.form.fresh() && !m.form.inFlight:
		return m.askStart()
	}
	m.form.askOnce = true
	return m.previewCmd()
}

// askStart asks y/N before the task starts agents (spec 9.D); a text the
// factory makes no task of, or one with no repository of several chosen, is
// refused with its reason. The start carries the repository the form shows.
func (m *factoryModel) askStart() tea.Cmd {
	f := m.form.preview
	text, act := m.form.area.Value(), m.act
	kind := cmp.Or(m.form.kind, f.Kind)
	repo := cmp.Or(m.form.repo, f.Repo)
	if agents, ok := f.AgentsByKind[kind]; ok { // the y/N counts the agents the start launches for the kind it names
		f.Agents = agents
	}
	switch {
	case severalRepos(f) && repo == "":
		return m.note("cannot start: which repository? " + repoKey + " picks one")
	case kind == "" && f.WordsOnly: // a key Jira does not know: your words, the key kept as words (spec 9.C)
		return m.ask(&factoryFollow{kind: followStart}, "Start from your words only"+repoIn(f)+"?", func(ctx context.Context) (ActionResult, error) {
			return act.Start(ctx, text, "", repo, true)
		})
	case kind == "":
		return m.note("cannot start: " + cmp.Or(cleanText(f.Err), "the factory makes no task of this text"))
	}
	key := f.Title
	switch {
	case f.Jira != nil && !severalKeys(f) && f.Jira.Key != "":
		key = f.Jira.Key
	case f.NotJira != "":
		key = f.NotJira
	}
	return m.ask(&factoryFollow{kind: followStart, key: key}, startSentence(f, kind), func(ctx context.Context) (ActionResult, error) {
		return act.Start(ctx, text, kind, repo, false)
	})
}

// repoIn names the repository in a start's y/N when there are several: " in
// example/site".
func repoIn(f NewTaskForm) string {
	if !severalRepos(f) || f.Repo == "" {
		return ""
	}
	return " in " + cleanText(f.Repo)
}

// startSentence is the y/N of a start: the kind, the task (its key, or the
// title of a prompt task: a text naming several keys is one, never a key's
// task), and its agents; or, when it would be queued, when it starts.
func startSentence(f NewTaskForm, kind string) string {
	name := "the task"
	switch {
	case f.Jira != nil && !severalKeys(f) && f.Jira.Key != "":
		name = cleanText(f.Jira.Key)
	case f.NotJira != "":
		name = cleanText(f.NotJira)
	case strings.TrimSpace(f.Title) != "":
		name = `"` + textx.Clip(cleanText(f.Title), factoryTitleMax) + `"`
	}
	name += repoIn(f)
	if strings.HasPrefix(f.Starts, "queued") {
		when := "a copy or memory frees"
		if _, after, ok := strings.Cut(f.Starts, "starts when "); ok && strings.TrimSpace(after) != "" {
			when = strings.TrimSpace(after)
		}
		return fmt.Sprintf("Queue %s %s? It starts when %s.", kind, name, strings.TrimSuffix(when, "."))
	}
	agents := ""
	if n := len(f.Agents); n > 0 {
		agents = " with " + factoryPlural(n, "agent", "agents")
	}
	return fmt.Sprintf("Start %s %s%s?", kind, name, agents)
}

// editDraft opens the editor on the new task's text (ctrl+g).
func (m *factoryModel) editDraft() tea.Cmd {
	f, err := os.CreateTemp(m.draftDir, "magnum-factory-task-*.md")
	if err != nil {
		return m.bar.show("the editor: "+errLine(err), factoryFlashFailed)
	}
	_, werr := f.WriteString(m.form.area.Value())
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(f.Name())
		return m.bar.show("the editor: "+errLine(err), factoryFlashFailed)
	}
	path := f.Name()
	return tea.ExecProcess(m.editor(m.ctx, path), func(err error) tea.Msg {
		b, rerr := os.ReadFile(path)
		_ = os.Remove(path)
		return factoryEditorMsg{text: string(b), err: errors.Join(err, rerr)}
	})
}

// edited takes the text the editor wrote.
func (m *factoryModel) edited(msg factoryEditorMsg) tea.Cmd {
	if msg.err != nil {
		return m.bar.show("the editor failed: "+errLine(msg.err)+"; your text is kept", factoryFlashFailed)
	}
	m.form.area.SetValue(strings.TrimRight(msg.text, "\n"))
	return m.textChanged()
}

// paste goes to the line being typed.
func (m *factoryModel) paste(msg tea.PasteMsg) tea.Cmd {
	switch {
	case m.view == fvNew && m.bar.asking == nil:
		before := m.form.area.Value()
		if m.form.area = editText(m.form.area, msg); m.form.area.Value() != before {
			return m.textChanged()
		}
	case m.view == fvQuestion && m.q.typing, m.view == fvReady && m.readyState().noting:
		m.input = editLine(m.input, msg)
	case m.view == fvHome && m.filtering:
		m.filter = editLine(m.filter, msg)
		m.fixCursor()
	}
	return nil
}

// the help and the log

func (m *factoryModel) boxKey(k string) tea.Cmd {
	switch k {
	case "q":
		return m.quit()
	case "esc", "enter", "backspace":
		m.view, m.scroll = m.under, 0
	case "?", logKey: // its own key closes a box; the other's opens that one
		next := fvHelp
		if k == logKey {
			next = fvLog
		}
		if m.view == next {
			next = m.under
		}
		m.view, m.scroll = next, 0
	default:
		m.scroll = scrollKey(k, m.scroll, max(m.viewHeight()-6, 1))
		m.clampScroll()
	}
	return nil
}

// clampScroll keeps the help's and the log's scroll within their lines.
func (m *factoryModel) clampScroll() {
	content := m.helpContent(m.viewWidth())
	if m.view == fvLog {
		content = m.logContent(m.viewWidth())
	}
	m.scroll = min(max(m.scroll, 0), max(len(content)-max(m.viewHeight()-5, 1), 0))
}
