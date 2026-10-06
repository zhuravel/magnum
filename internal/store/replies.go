package store

import "time"

// Reply is a review or an issue comment that may answer magnum's review of
// a PR (prs.replies_json, migration 0022): the PR author's own, or anyone's
// reply in one of magnum's threads (the engine decides which, from the
// Details' remarks). Who and when only: never what it says, which is PR
// content.
type Reply struct {
	At     time.Time `json:"at"`
	By     string    `json:"by"`               // the author's login, Account form ("" = a ghost)
	Thread bool      `json:"thread,omitempty"` // a reply in one of magnum's threads
}

// Equal reports whether two replies are the same (time.Time compared with
// Equal).
func (r Reply) Equal(o Reply) bool { return r.At.Equal(o.At) && r.By == o.By && r.Thread == o.Thread }

// PendingReplies are the replies (oldest first) magnum's judge has not
// re-decided: those after readAt, when it last read the threads
// (prs.replies_read_at), else after reviewedAt, its last review; every one
// when neither time is known. A PR magnum never reviewed (reviewedSHA "")
// has none.
func PendingReplies(replies []Reply, reviewedSHA string, reviewedAt, readAt *time.Time) []Reply {
	if reviewedSHA == "" {
		return nil
	}
	var after time.Time
	switch {
	case readAt != nil:
		after = *readAt
	case reviewedAt != nil:
		after = *reviewedAt
	}
	var out []Reply
	for _, r := range replies {
		if r.At.After(after) {
			out = append(out, r)
		}
	}
	return out
}

// PendingReplies is PendingReplies for p.
func (p PR) PendingReplies() []Reply {
	return PendingReplies(p.Replies, Deref(p.ReviewedSHA), p.ReviewedAt, p.RepliesReadAt)
}
