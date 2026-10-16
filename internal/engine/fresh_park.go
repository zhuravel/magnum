package engine

// The fresh start's park: a round that starts every role fresh (`magnum
// review --fresh`, a full or audit pass, an identity change) parks the PR's
// sessions first, and an agent still its pane's foreground process after
// two ctrl+c makes that park fail as busy, a wait that charges no attempt. On
// talkable#11932 the fixer's Claude Code stayed in its pane: the setup
// failed every 90 seconds for 40 minutes, the board saying "retry 1/3 agent
// busy" all along. The busy tries are counted (kvPRFreshParkBusy); the
// second parks in the hard mode (agents.Manager.ParkHard), which closes the
// pane of an agent that the kind's exit keys did not quit either.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum/internal/store"
)

// kvPRFreshParkBusy counts the PR's fresh-start parks that failed because
// an agent did not quit (parkQuitTimeout), as a decimal string: the retry
// label counts them (busyTries) and the second try parks hard. A round's
// start, `magnum abort` and the PR leaving the round states delete it.
func kvPRFreshParkBusy(prID int64) string { return fmt.Sprintf("pr.%d.fresh_park_busy", prID) }

// freshParkHardAfter is how many busy tries of a fresh park make the next
// one hard.
const freshParkHardAfter = 1

// freshParkStates are the states of a PR whose round waits, starts or
// runs: a fresh park's busy count lives only while its PR is in one of them.
var freshParkStates = []string{store.PRQueued, store.PRRereviewPending, store.PRClaiming, store.PRReviewing,
	store.PRVerifying, store.PRPaused}

// parkFresh parks the PR's sessions for a fresh start: the plain park
// while fewer than freshParkHardAfter tries failed because an agent did not
// quit, the hard one after. Such a failure counts one more try. Any other
// refusal (a run of the PR in flight, a fixer or a round at work, an agent
// working or blocked) is a plain wait that counts nothing: that agent works,
// it is not stuck. A park that works forgets the count and closes the panes
// of the sessions it retired (closeRetiredPanes): the fresh start lays out
// a new pane per role.
func (e *Engine) parkFresh(ctx context.Context, pr store.PR) error {
	tries := e.freshParkBusy(ctx, pr.ID)
	park := e.d.Agents.Park
	if tries >= freshParkHardAfter {
		park = e.d.Agents.ParkHard
	}
	err := park(ctx, pr)
	switch {
	case err == nil:
		e.delKV(ctx, kvPRFreshParkBusy(pr.ID))
		e.closeRetiredPanes(ctx, pr)
	case parkStuck(err):
		e.setKV(ctx, kvPRFreshParkBusy(pr.ID), strconv.Itoa(tries+1))
	}
	return err
}

// closeRetiredPanes closes, after a fresh start's park or a hard park, the
// panes of the PR's sessions the park retired (agents.Manager.
// CloseRetiredPanes: an idle shell no starting, live or lost session
// records; a shell the operator opened is never one), since every fresh
// start split a new pane per role beside the old ones (17 on talkable#11932).
// A parked session keeps its conversation, resumed later in a new pane. A
// failure is only logged: the park held.
func (e *Engine) closeRetiredPanes(ctx context.Context, pr store.PR) {
	if err := e.d.Agents.CloseRetiredPanes(ctx, pr); err != nil {
		e.warnUnlessStopped(ctx, err, "close the panes of retired sessions", "pr", pr.ID)
	}
}

// parkStuck reports whether a park failed because an agent did not quit:
// its pane was no idle shell after ctrl+c (parkQuitTimeout).
func parkStuck(err error) bool {
	class, _ := parkClass(err)
	return class == parkQuitTimeout
}

// freshParkBusy is the PR's count of fresh-start parks an agent did not
// quit for (0: none, or unreadable).
func (e *Engine) freshParkBusy(ctx context.Context, prID int64) int {
	v, _ := e.getKV(ctx, kvPRFreshParkBusy(prID))
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// busyTries is the attempt a retry of pr waits for, as its label counts
// it: its charged attempts plus one, or, when the retry waits after an
// agent that did not quit, the fresh park's busy tries, which charge none
// (capped at maxAttempts, so the label never reads 4/3).
func (e *Engine) busyTries(ctx context.Context, pr store.PR) int {
	n := pr.Attempts + 1
	if retryCause(deref(pr.LastError)) == "agent busy" {
		n = max(n, e.freshParkBusy(ctx, pr.ID))
	}
	return min(n, maxAttempts)
}

// sweepFreshParkBusy deletes the busy counts of the PRs no longer in a
// round state (closed, ignored, in needs_attention, reviewed) or gone: the
// count belongs to the round that waits, and a later one starts with the
// plain park.
func (e *Engine) sweepFreshParkBusy(ctx context.Context) {
	keys, err := e.st.KVKeys(ctx, "pr.*.fresh_park_busy")
	if err != nil {
		e.warnUnlessStopped(ctx, err, "fresh park counts")
		return
	}
	for _, key := range keys {
		id, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(key, "pr."), ".fresh_park_busy"), 10, 64)
		if err != nil {
			continue
		}
		pr, err := e.st.PRByID(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil, slices.Contains(freshParkStates, pr.State):
			continue
		}
		e.delKV(ctx, key)
	}
}
