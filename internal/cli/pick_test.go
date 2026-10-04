package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// pickCall is what the picker screen was handed.
type pickCall struct {
	entries []tui.PickEntry
	opts    tui.PickerOptions
}

// withPicker puts the harness on a terminal and replaces the picker screen:
// choose returns the user's outcome for the entries it is handed.
func (h *actHarness) withPicker(choose func(entries []tui.PickEntry) tui.PickOutcome) *[]pickCall {
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	var calls []pickCall
	old := tuiPicker
	tuiPicker = func(_ context.Context, entries []tui.PickEntry, opts tui.PickerOptions) (tui.PickOutcome, error) {
		calls = append(calls, pickCall{entries: entries, opts: opts})
		return choose(entries), nil
	}
	h.t.Cleanup(func() { tuiPicker = old })
	return &calls
}

// pickEntryAt picks entries[i] with action a.
func pickEntryAt(i int, a tui.PickAction) func([]tui.PickEntry) tui.PickOutcome {
	return func(entries []tui.PickEntry) tui.PickOutcome {
		e := entries[i]
		return tui.PickOutcome{Action: a, Entry: &e}
	}
}

// pickRef picks the entry with Ref ref with action a.
func pickRef(ref string, a tui.PickAction) func([]tui.PickEntry) tui.PickOutcome {
	return func(entries []tui.PickEntry) tui.PickOutcome {
		for _, e := range entries {
			if e.Ref == ref {
				return tui.PickOutcome{Action: a, Entry: &e}
			}
		}
		return tui.PickOutcome{}
	}
}

func TestPickScreenEntriesAndPinToggle(t *testing.T) {
	h := newActHarness(t)
	five := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.now = h.now.Add(3 * time.Hour)
	h.seedPR("zhuravel/widgets", 7, store.PRQueued)
	calls := h.withPicker(pickEntryAt(1, tui.PickActionTogglePin))
	if code := h.cmd("pick", "--query", "talk"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	got := (*calls)[0]
	for i, e := range got.entries { // the y/N question's facts: the head, nothing reviewed yet
		if e.Review == nil || e.Review.HeadSHA != "abc1234def5678" || e.Review.ReviewedSHA != "" || e.Review.SinceReview != nil {
			t.Errorf("entry %s review facts %+v", e.Ref, e.Review)
		}
		got.entries[i].Review = nil
	}
	want := []tui.PickEntry{
		{Ref: "zhuravel/widgets#7", Title: "Fix coupon export", Author: "@alice", State: "queued", Age: "0s",
			URL: "https://github.com/zhuravel/widgets/pull/7"},
		{Ref: "talkable/talkable#5", Title: "Fix coupon export", Author: "@alice", State: "reviewed", Age: "3h",
			URL: "https://github.com/talkable/talkable/pull/5"},
	}
	if len(got.entries) != len(want) || got.entries[0] != want[0] || got.entries[1] != want[1] {
		t.Fatalf("entries (newest first):\n%+v\nwant\n%+v", got.entries, want)
	}
	if got.opts.Query != "talk" {
		t.Errorf("query = %q", got.opts.Query)
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqPin {
		t.Fatalf("requests %+v", reqs)
	}

	// A pinned PR is unpinned by the same key.
	h.setPR(five.ID, store.PRReviewed, func(u *store.PRUpdate) { u.Set("pinned", true) })
	calls = h.withPicker(pickRef("talkable/talkable#5", tui.PickActionTogglePin))
	if code := h.cmd("pick"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if e := pickRef("talkable/talkable#5", 0)((*calls)[0].entries).Entry; e == nil || !e.Pinned || e.State != "reviewed,pinned" {
		t.Fatalf("pinned entry %+v", e)
	}
	if reqs := h.requests(); len(reqs) != 2 || reqs[1].Kind != engine.ReqUnpin {
		t.Fatalf("requests %+v", reqs)
	}
}

func TestPickScreenTypedRefIsReviewed(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.gh.details[11932] = github.PRDetails{NodeID: "PR_11932", Number: 11932, Title: "New", State: "OPEN", HeadRefOid: "ccc"}
	h.withPicker(func([]tui.PickEntry) tui.PickOutcome {
		return tui.PickOutcome{Action: tui.PickActionReview, Query: "11932"}
	})
	if code := h.cmd("pick"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqReview {
		t.Fatalf("requests %+v", reqs)
	}
	if p := actDecode[engine.ReviewPayload](t, reqs[0].Payload); p.Number != 11932 || p.Repo != "talkable/talkable" {
		t.Fatalf("payload %+v", p)
	}

	// Cancelling does nothing.
	h.withPicker(func([]tui.PickEntry) tui.PickOutcome { return tui.PickOutcome{Query: "talk"} })
	if code := h.cmd("pick"); code != 0 || len(h.requests()) != 1 {
		t.Fatalf("cancel: exit %d requests %d", code, len(h.requests()))
	}

	// A query that is no reference fails.
	h.withPicker(func([]tui.PickEntry) tui.PickOutcome {
		return tui.PickOutcome{Action: tui.PickActionReview, Query: "nothing like it"}
	})
	if code := h.cmd("pick"); code != 1 || !strings.Contains(h.errb.String(), `no PR matches "nothing like it"`) {
		t.Fatalf("no match: exit %d: %s", code, h.errb.String())
	}
}

func TestPickLinkQueryOffersAnUnknownPR(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	calls := h.withPicker(pickEntryAt(0, tui.PickActionBrowser))
	h.run.Rules = []execx.Rule{{Prefix: []string{"open"}}}
	h.env["MAGNUM_PICK_QUERY"] = "https://github.com/talkable/talkable/pull/12000/files"
	if code := h.cmd("pick"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	got := (*calls)[0]
	if e := got.entries[0]; e.Ref != "talkable/talkable#12000" || e.State != "new" || !strings.HasPrefix(e.Title, "(not in magnum yet") ||
		got.opts.Query != "talkable#12000" {
		t.Fatalf("offered entry %+v, query %q", e, got.opts.Query)
	}
	opened := h.run.CallsWithPrefix("open")
	if len(opened) != 1 || opened[0].Args[0] != "https://github.com/talkable/talkable/pull/12000" || !opened[0].Mutates {
		t.Fatalf("open calls %+v", opened)
	}
	actContains(t, h.out.String(), "opened https://github.com/talkable/talkable/pull/12000")
}

func TestPickPromptOffATerminal(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	old := tuiPicker
	tuiPicker = func(context.Context, []tui.PickEntry, tui.PickerOptions) (tui.PickOutcome, error) {
		t.Fatal("the picker screen ran off a terminal")
		return tui.PickOutcome{}, nil
	}
	t.Cleanup(func() { tuiPicker = old })
	h.d.StdinTTY = true // stdout is not a terminal
	h.run.Rules = []execx.Rule{{Prefix: []string{"open"}}}
	h.stdin("1 b\n")
	if code := h.cmd("pick"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "1)  talkable#5  reviewed  Fix coupon export  @alice", "opened https://github.com/talkable/talkable/pull/5")

	// A reference query that matches nothing reviews that PR directly.
	h.gh.details[44] = github.PRDetails{NodeID: "PR_44", Number: 44, Title: "x", State: "OPEN", HeadRefOid: "ddd"}
	if code := h.cmd("pick", "--query", "talkable#44"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if reqs := h.requests(); len(reqs) != 1 || reqs[0].Kind != engine.ReqReview {
		t.Fatalf("requests %+v", reqs)
	}
	h.stdin("1 z\n")
	if code := h.cmd("pick"); code != 2 || !strings.Contains(h.errb.String(), `unknown action "z"`) {
		t.Fatalf("bad action exit %d: %s", code, h.errb.String())
	}
}

func TestPickInThePluginPopupKeepsTheResultUp(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.withPicker(pickEntryAt(0, tui.PickActionReview))
	h.tty.Rules = []execx.Rule{{Prefix: []string{"stty", "-g"}, Result: execx.Result{Stdout: []byte("saved")}},
		{Prefix: []string{"stty"}}}
	h.env["HERDR_PLUGIN_ID"] = "zhuravel.magnum"
	h.stdin("k")
	if code := h.cmd("pick"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if reqs := h.requests(); len(reqs) != 1 || reqs[0].Kind != engine.ReqReview {
		t.Fatalf("requests %+v", reqs)
	}
	actContains(t, h.out.String(), "press any key to close")
	if n := len(h.tty.CallsWithPrefix("stty")); n != 3 {
		t.Errorf("stty calls = %d (save, cbreak, restore)", n)
	}
}

// The picker's entries carry what the y/N question needs: the reviewed
// head, who reviewed it when, and the poller's commit count for the head.
func TestPickScreenEntriesCarryReviewFacts(t *testing.T) {
	h := newActHarness(t)
	five := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	at := h.now.Add(-20 * time.Minute)
	h.setPR(five.ID, store.PRRereviewPending, func(u *store.PRUpdate) {
		u.Set("reviewed_sha", "ffa3270aaaa")
		u.Set("reviewed_at", at)
		u.Set("last_review_login", "zhuravel")
		u.Set("since_review_json", store.SinceReview{Source: store.SinceFromReviewed, Base: "ffa3270aaaa", Head: "abc1234def5678", Commits: 3, Files: 2})
	})
	calls := h.withPicker(func([]tui.PickEntry) tui.PickOutcome { return tui.PickOutcome{} })
	if code := h.cmd("pick"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if (*calls)[0].opts.Now == nil {
		t.Error("the picker got no clock")
	}
	e := pickRef("talkable/talkable#5", 0)((*calls)[0].entries).Entry
	if e == nil || e.Review == nil {
		t.Fatalf("entry %+v", e)
	}
	f := e.Review
	if f.HeadSHA != "abc1234def5678" || f.ReviewedSHA != "ffa3270aaaa" || f.ReviewedBy != "zhuravel" || !f.ReviewedAt.Equal(at) ||
		f.SinceReview == nil || f.SinceReview.Commits != 3 {
		t.Errorf("review facts %+v (since %+v)", f, f.SinceReview)
	}
}

// A repo#N query resolves against the registry like `open`: widgets lives
// under another watched owner than the default repository's.
func TestPickQueryUsesTheRegisteredOwner(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("zhuravel/widgets", 7, store.PRReviewed)
	calls := h.withPicker(func([]tui.PickEntry) tui.PickOutcome { return tui.PickOutcome{Action: tui.PickActionCancel} })
	if code := h.cmd("pick", "--query", "widgets#7"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	got := (*calls)[0]
	if len(got.entries) != 1 || got.entries[0].Ref != "zhuravel/widgets#7" || strings.HasPrefix(got.entries[0].Title, "(not in magnum yet") {
		t.Fatalf("entries %+v", got.entries)
	}
	if got.opts.Query != "zhuravel/widgets#7" {
		t.Fatalf("query %q", got.opts.Query)
	}
	if e, ok := pickRefEntry(h.ctx, h.d, "widgets#7"); !ok || !e.Known || e.Repo != "zhuravel/widgets" {
		t.Fatalf("typed ref entry %+v %v", e, ok)
	}
}

func TestPickChoiceRowsAndPRNumbers(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 2, store.PRReviewed)
	shown := []pickEntry{
		{Label: "talkable#2", Repo: "talkable/talkable", Number: 2, Known: true},
		{Label: "talkable#9", Repo: "talkable/talkable", Number: 9, Known: true},
	}
	if e, err := pickChoice(h.ctx, h.d, shown, "1"); err != nil || e.Number != 2 {
		t.Fatalf("row 1: %+v %v", e, err)
	}
	// "2" is row 2 (PR #9) and also PR #2 on row 1.
	if _, err := pickChoice(h.ctx, h.d, shown, "2"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous answer: %v", err)
	}
	if e, err := pickChoice(h.ctx, h.d, shown, "2)"); err != nil || e.Number != 9 {
		t.Fatalf("row 2): %+v %v", e, err)
	}
	if e, err := pickChoice(h.ctx, h.d, shown, "#2"); err != nil || e.Number != 2 || !e.Known {
		t.Fatalf("#2: %+v %v", e, err)
	}
	if _, err := pickChoice(h.ctx, h.d, shown, "nonsense"); err == nil {
		t.Fatal("nonsense accepted")
	}
}
