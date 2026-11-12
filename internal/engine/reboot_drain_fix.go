package engine

// The fix loops' part of the drain for a reboot (reboot_drain.go): the push
// of a held merge, which runs off the loop with the fixer App's Workflows
// token (fix_held_merge.go).

import "context"

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
