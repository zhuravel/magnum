package factory

import (
	"strings"
	"testing"
)

// A chat session's post through `magnum comment` is recorded in magnum's
// registry as comment.posted on the PR (the post's kind, its link, the
// App): it is no fix loop, no fixer's push or
// reply, and leaves nothing to you, so the factory's counts of the PR's fix
// loops and its fix record ignore it, while a loop after it still counts.
// A pin: the readers switch on the fix events' kinds.
func TestFixRecordsIgnoreCommentPosted(t *testing.T) {
	l, st := magnumLayout(t)
	magnumPR(t, st, 7, strings.Repeat("ab", 20), nil)
	for _, post := range []string{"conversation", "reply", "title", "review_request"} {
		fixEvent(t, st, 7, "comment.posted", map[string]any{"repo": "example/shop", "number": 7, "identity": "fixer-app",
			"login": "fixer-app[bot]", "post": post, "url": "https://github.com/example/shop/pull/7", "comment_id": 4321, "thread": 11})
	}
	seven := PRRef{"example/shop", 7}
	loops, err := FixLoopsOf(t.Context(), l, []PRRef{seven})
	if err != nil || loops[seven] != (FixLoops{}) {
		t.Errorf("FixLoopsOf = %+v, %v; want none", loops[seven], err)
	}
	records, err := NewMagnum(l, openWatchConfig()).FixRecords(t.Context(), []PRRef{seven})
	if err != nil || records[seven] != (FixRecord{}) {
		t.Errorf("FixRecords = %+v, %v; want the zero record", records[seven], err)
	}
	fixStart(t, st, 7, 1, "magnum fix")
	fixEvent(t, st, 7, "comment.posted", map[string]any{"post": "conversation"})
	if records, err := NewMagnum(l, openWatchConfig()).FixRecords(t.Context(), []PRRef{seven}); err != nil || records[seven] != (FixRecord{Loops: 1, Cycle: 1}) {
		t.Errorf("FixRecords after a loop = %+v, %v; want the loop alone", records[seven], err)
	}
}
