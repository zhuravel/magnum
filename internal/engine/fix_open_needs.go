package engine

// NEEDS YOU from what an autofix = "open" loop left (fix_open.go): a
// person's item, magnum's, a conflict or a bot's thread that a loop was
// handed and that is still open at a later whole read raises the PR to
// needs_attention, kind fix, with the items' links, and your answer to
// every one of them, or a later approval by its reviewer (which accepts
// it), lifts it again. A reviewer whose item waited for you is remembered:
// a change request of theirs since waits for you too (pushback), and their
// plain comment is new work. The look raises only a reviewed PR:
// needsAttentionKeyed replaces last_error, so a raise on a PR in
// needs_attention would wipe the loop's own Needs you (a failed push, a
// declined P1, a description finding), whose entries have no key; there
// the verdict lists the leftovers instead. It lifts only an attention it
// raised itself (its message is last_error), never one a loop or a round
// wrote. A reply magnum has not re-decided holds the look's read of a PR
// (the reply's round goes first), but not of one the look's own raise
// holds: no reply round starts on a PR in needs attention, so the read and
// the lift go first, and the reply's round follows the lift.
// magnum's late review of a head is toasted once per head.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// openKeys are the sorted keys of items.
func openKeys(items []openItem) []string {
	keys := make([]string, 0, len(items))
	for _, it := range items {
		keys = append(keys, it.Key)
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// openLinks are the links of items.
func openLinks(items []openItem) []string {
	var out []string
	for _, it := range items {
		if it.URL != "" && !slices.Contains(out, it.URL) {
			out = append(out, it.URL)
		}
	}
	return out
}

// openNeedsMsg is the attention message of what a loop left: "fix: 2 items
// the fixer left need you: <url>, <url>" (links only, never PR text).
func openNeedsMsg(needs []openItem) string {
	return "fix: " + textx.Count(len(needs), "item the fixer left needs you", "items the fixer left need you") + ": " +
		strings.Join(openLinks(needs), ", ")
}

// openRaise moves pr, reviewed, to needs_attention for needs, what loops
// left open (rule 8), once per set of keys: the attention's kind is fix, its
// message their links, its toast's key the set's own. The ledger keeps the
// keys and the message, the lift's test. It reports whether it raised.
func (e *Engine) openRaise(ctx context.Context, repo store.Repo, pr store.PR, l *openLedger, needs []openItem) bool {
	keys := openKeys(needs)
	if pr.State != store.PRReviewed || len(keys) == 0 || slices.Equal(keys, l.Raised) {
		return false
	}
	msg := openNeedsMsg(needs)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	key := fmt.Sprintf("attention:%d:fix:open:%s", pr.ID, hex.EncodeToString(sum[:6]))
	job := e.loopJob(ctx, repo, pr, FixState{})
	e.needsAttentionKeyed(ctx, job.asRound(), pr, []string{store.PRReviewed}, attention.KindFix, msg, nil, key, fixAttentionWindow)
	cur, err := e.st.PRByID(ctx, pr.ID)
	if err != nil || cur.State != store.PRNeedsAttention || deref(cur.LastError) != msg {
		return false
	}
	l.Raised, l.RaisedMsg = keys, msg
	e.event(ctx, "warn", prSubject(repo, pr.Number), "fix.open_needs_you", msg, map[string]any{"links": openLinks(needs), "items": len(keys)})
	return true
}

// raisedHere reports whether pr needs attention of kind for what the look
// raised itself: kind fix, last_error the look's message, keys it raised.
// A loop's or a round's Needs you is not.
func (l openLedger) raisedHere(pr store.PR, kind string) bool {
	return pr.State == store.PRNeedsAttention && kind == attention.KindFix && l.RaisedMsg != "" && deref(pr.LastError) == l.RaisedMsg &&
		len(l.Raised) > 0
}

// leftToAuthor reports whether it, once still open, waits for the author
// rather than a fixer where sortOpen sorts it: a loop was handed it
// (Handed), or it is a reviewer's pushback.
func (l openLedger) leftToAuthor(it openItem) bool {
	_, handed := l.Handed[it.Key]
	return handed || l.pushback(it)
}

// pushback reports whether it is a person's review body that requests
// changes (openItem.Changes) while or after one of that reviewer's items
// waited for the author (Waited): they push back on the author's answer,
// or ask again, which only the author can settle. Their plain comment is
// no pushback: it is an item of its own, never handed, so work.
func (l openLedger) pushback(it openItem) bool {
	_, waited := l.Waited[strings.ToLower(it.By)]
	return it.Class == classPerson && it.Changes && waited
}

// noteWaited records in l.Waited, at now the first time, the reviewers
// of the person's items of needs, which wait for the author. A reviewer
// stays there for the PR's life: an approval of theirs and a new change
// request can come in one read.
func (l *openLedger) noteWaited(needs []openItem, now time.Time) {
	for _, it := range needs {
		by := strings.ToLower(it.By)
		if it.Class != classPerson || by == "" || by == "ghost" {
			continue
		}
		if _, ok := l.Waited[by]; ok {
			continue
		}
		if l.Waited == nil {
			l.Waited = map[string]time.Time{}
		}
		l.Waited[by] = now
	}
}

// acceptedLeftover reports whether it would wait for you (left to you,
// a thread a fixer resolved, one the fixer answered twice) but its
// reviewer approved the PR since (openItem.Accepted): it waits for no one
// (sortOpen). An item never handed is work, accepted or not.
func (l openLedger) acceptedLeftover(it openItem) bool {
	return it.Accepted && (l.leftToAuthor(it) || it.Resolved || it.Kind == itemThread && it.FixerReplies >= 2)
}

// acceptedWaits are the items of items that would wait for you and whose
// reviewer approved the PR since (acceptedLeftover): no longer work or
// needs, they are still left alone by a loop's fixer, as the ones that wait
// are (waitsOf), since an earlier fixer left them to you. Its file lists
// them unanswered, and a fixer that answered one would answer for you, or
// name it under Needs you, a loop's own attention the look never lifts.
func acceptedWaits(items []openItem, l openLedger) []openItem {
	return slices.DeleteFunc(slices.Clone(items), func(it openItem) bool { return !l.acceptedLeftover(it) })
}

// openLift puts pr back to reviewed when it needs attention for what the
// look raised (raisedHere), no loop runs, and the look's whole, fresh read
// shows none of the raised items open any more: you answered them. A
// loop's or a round's attention, and one whose items the look cannot read,
// are never lifted here. It reports whether it lifted.
func (e *Engine) openLift(ctx context.Context, repo store.Repo, pr store.PR, l *openLedger, f openFacts) bool {
	switch {
	case !l.raisedHere(pr, f.attention), f.fix != nil || f.fixBad, !f.whole || f.failed || len(f.cut) > 0, deref(pr.ReviewedSHA) != pr.HeadSHA:
		return false
	}
	open := map[string]bool{}
	for _, it := range f.items {
		open[it.Key] = !it.Accepted // its reviewer approved since: it waits for no one (sortOpen)
	}
	if slices.ContainsFunc(l.Raised, func(k string) bool { return open[k] }) {
		return false
	}
	err := e.st.TransitionPR(ctx, pr.ID, []string{store.PRNeedsAttention}, store.PRReviewed, func(u *store.PRUpdate) {
		u.Set("last_error", nil)
		u.Where("head_sha", pr.HeadSHA)
		u.Where("reviewed_sha", pr.HeadSHA)
		u.Where("last_error", l.RaisedMsg)
	})
	if err != nil {
		e.log.Info("autofix open: the PR moved on before the lift", "subject", prSubject(repo, pr.Number), "err", err)
		return false
	}
	e.delKV(ctx, KVPRAttention(pr.ID), kvPRAttentionRun(pr.ID))
	e.event(ctx, "info", prSubject(repo, pr.Number), "fix.open_answered", "you answered what the fixer left; autofix goes on",
		map[string]any{"items": len(l.Raised)})
	l.Raised, l.RaisedMsg = nil, ""
	return true
}

// openLate toasts, once per head, that magnum's review of pr's head is late
// (rule 5's needs_you), with a fix.open_magnum_late event.
func (e *Engine) openLate(ctx context.Context, repo store.Repo, pr store.PR, l *openLedger, why string) {
	if l.LateToasted == pr.HeadSHA {
		return
	}
	l.LateToasted = pr.HeadSHA
	label := fmt.Sprintf("%s#%d", repo.Name, pr.Number)
	e.event(ctx, "warn", prSubject(repo, pr.Number), "fix.open_magnum_late", why, map[string]any{"head": pr.HeadSHA})
	e.urgent(fmt.Sprintf("fix-open-late:%d:%s", pr.ID, pr.HeadSHA), "magnum: "+label+": magnum's review of "+textx.ShortSHA(pr.HeadSHA)+" is late",
		why+"\n"+pr.URL, fixAttentionWindow)
}
