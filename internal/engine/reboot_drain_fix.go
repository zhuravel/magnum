package engine

// The fix loops' part of the drain for a reboot (reboot_drain.go): the push
// of a held merge, which runs off the loop with the fixer App's Workflows
// token (fix_held_merge.go), and a fixer's push that the settle confirms
// while GitHub does not answer (fix_confirm.go). A reboot cuts the latter
// as a restart does: the start fails the loop as unconfirmed, and the poll
// settles it late once GitHub shows the push. The drain still waits for it,
// up to the cap of its confirmation, so that GitHub's answer settles the fix
// as pushed (no failure, no attention, no late reopen); past the cap the
// loop fails as unconfirmed anyway (capPushes), so the drain leaves it.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// reservedWords names reservation h of PR id for the drain's lines: "round
// of talkable/talkable#12", "open of …", "fix of …", "whiteboard of …", and
// the push of a held merge, a fix reservation whose loop row its y started
// (heldPushBy): "push of the held merge of talkable/talkable#12 (Workflows
// token)".
func (e *Engine) reservedWords(ctx context.Context, id int64, h *roundHandle) string {
	pr := e.prWords(ctx, id)
	switch {
	case h.open:
		return "open of " + pr
	case h.fix:
		if st, ok, err := e.fixState(ctx, id); err == nil && ok && st.By == heldPushBy {
			return "push of the held merge of " + pr + " (Workflows token)"
		}
		return "fix of " + pr
	case h.whiteboard:
		return "whiteboard of " + pr
	}
	return "round of " + pr
}

// rebootFixWork is the fix loops' work a reboot would cut that no
// reservation holds (held), and what the drain leaves to the start or the
// poll after the reboot (left):
//   - the settle of a confirmed push that began and stopped between its
//     steps (the next poll resumes it) is work for up to fixConfirmCap from
//     when this drain first saw it (st.settles), then left;
//   - a loop whose settle confirms its fixer's push while GitHub does not
//     answer (FixState.Confirm) is work for up to fixConfirmCap from when the
//     settle began to confirm it (its push record's Since, else the mark's),
//     then left: the late settle recovers it after the reboot;
//   - a fix that failed only because GitHub did not confirm its push
//     (fixPushUnconfirmed) is left: the late settle recovers it;
//   - a held merge's push whose job is gone (a stop cut it, and the start's
//     recovery did not end its row) is left: the start ends its loop
//     (recoverHeldPush), and P asks the push again.
func (e *Engine) rebootFixWork(ctx context.Context, held map[int64]*roundHandle, st *rebootState) (work, left []string) {
	vals, err := e.st.KVValues(ctx, "pr.*.fix")
	if err != nil {
		return []string{"the fix loops (unreadable)"}, nil
	}
	pushes := e.fixPushes(ctx, store.Repo{})
	now := e.now()
	skip := map[int64]bool{}
	for _, id := range slices.Sorted(maps.Keys(pushes)) {
		rec := pushes[id].rec
		if _, ok := held[id]; ok {
			skip[id] = true
			continue
		}
		push := textx.ShortSHA(rec.Pushed) + " on " + e.prWords(ctx, id)
		switch {
		case e.pushStarted(ctx, rec):
			skip[id] = true
			what := "the settle of the confirmed push of " + push
			if now.Sub(st.firstSeen(rec.Run, now)) < fixConfirmCap {
				work = append(work, what+" (its next steps run at the next poll)")
			} else {
				left = append(left, what+", stopped between its steps: the poll after the reboot resumes it")
			}
		case rec.Kind == fixPushUnconfirmed:
			left = append(left, "the push of "+push+", which GitHub did not confirm: the late settle recovers it after the reboot")
		}
	}
	for _, id := range fixRowIDs(vals) {
		if _, ok := held[id]; ok || skip[id] {
			continue
		}
		fs, ok := ParseFixState(vals[KVPRFix(id)])
		if !ok || fs.Phase != FixPhaseFixing {
			continue
		}
		pr := e.prWords(ctx, id)
		switch {
		case fs.By == heldPushBy:
			left = append(left, "the push of the held merge of "+pr+", cut by a stop: the start after the reboot ends its loop, and P asks the push again")
		case fs.Confirm != nil:
			since := fs.Confirm.Since
			if row, ok := pushes[id]; ok && row.rec.Kind == fixPushConfirming && row.rec.Job.State.Report == fs.Report {
				since = row.rec.Since
			}
			what := "confirming the push of " + textx.ShortSHA(fs.Confirm.Head) + " on " + pr
			if until := since.Add(fixConfirmCap); now.Before(until) {
				work = append(work, fmt.Sprintf("%s (GitHub does not answer; at most until %s)", what, until.Local().Format("15:04")))
			} else {
				left = append(left, fmt.Sprintf("%s: GitHub did not answer for %s; the late settle recovers it after the reboot", what, humanDuration(fixConfirmCap)))
			}
		}
	}
	return work, left
}

// fixRowIDs are the PR ids of the fix loops' rows (KVPRFix keys of vals),
// in order.
func fixRowIDs(vals map[string]string) []int64 {
	var ids []int64
	for key := range vals {
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.fix", &id); err == nil && KVPRFix(id) == key {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// firstSeen is when this drain first saw the settle of run's confirmed push
// stopped between its steps (now, the first time).
func (st *rebootState) firstSeen(run string, now time.Time) time.Time {
	if st.settles == nil {
		st.settles = map[string]time.Time{}
	}
	if at, ok := st.settles[run]; ok {
		return at
	}
	st.settles[run] = now
	return now
}
