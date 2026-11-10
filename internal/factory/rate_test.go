package factory

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

// rate enqueues a rating request as the screen sends it, runs a tick and
// returns the request as the daemon answered it.
func (h *harness) rate(p RatePayload) registry.Request {
	h.t.Helper()
	id, err := h.reg.EnqueueRequest(h.ctx, ReqRate, p)
	if err != nil {
		h.t.Fatal(err)
	}
	h.tick()
	return h.request(id)
}

func (h *harness) ratings(id int64) []registry.Rating {
	h.t.Helper()
	rs, err := h.reg.Ratings(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return rs
}

// A rating of a published task is stored with its value, the key of its
// reason and whether the screen showed the operator the diff first, and it
// is an input of the operator's (kind rating, from the screen); the
// request's answer says what was stored.
func TestARatingOfAPublishedTaskIsStoredWithItsReasonAndAnInput(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12310", registry.TaskPublished, "publish")

	q := h.rate(RatePayload{TaskID: task.ID, Value: 1, Reason: "tests", DiffOpened: true, Surface: "screen"})

	if q.State != registry.RequestDone || q.Result != "PR-12310: rated + (tests), the diff opened first" {
		t.Fatalf("the rating request is %s with %q", q.State, q.Result)
	}
	rs := h.ratings(task.ID)
	if len(rs) != 1 || rs[0].Value != 1 || rs[0].ReasonKey != "tests" || !rs[0].DiffOpenedFirst || rs[0].TaskID != task.ID {
		t.Fatalf("ratings %+v, want one + for tests with the diff opened first", rs)
	}
	ins, err := h.reg.Inputs(h.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 || ins[0].Kind != registry.InputRating || ins[0].Surface != "screen" || ins[0].Text != "+1: tests" || ins[0].Class != "" {
		t.Fatalf("inputs %+v, want one rating input from the screen, +1: tests, with no class", ins)
	}
}

// A rating without a reason stores none, a − is -1, and rating again stores a
// new rating: every one is kept.
func TestRatingAgainStoresANewRatingAndEveryOneIsKept(t *testing.T) {
	h := newHarness(t)
	task := h.task("PR-12311", registry.TaskDone, "")

	h.rate(RatePayload{TaskID: task.ID, Value: -1, Surface: "screen"})
	h.rate(RatePayload{TaskID: task.ID, Value: 1, Reason: "scope", Surface: "screen"})

	rs := h.ratings(task.ID)
	if len(rs) != 2 || rs[0].Value != -1 || rs[0].ReasonKey != "" || rs[1].Value != 1 || rs[1].ReasonKey != "scope" {
		t.Fatalf("ratings %+v, want - with no reason, then + for scope", rs)
	}
}

// The diff counts as opened first when the screen showed it, or when the
// registry already records it: the task's newest push was taken with the
// diff opened (a diff read for a card that was sent back is another diff).
// With neither the rating is noise and its answer says so.
func TestARatingKnowsWhetherTheDiffWasOpenedFromTheScreenOrTheRegistry(t *testing.T) {
	h := newHarness(t)
	opened := h.task("PR-12312", registry.TaskPublished, "publish")
	unread := h.task("PR-12313", registry.TaskPublished, "publish")
	sentBack := h.task("PR-12316", registry.TaskPublished, "publish")
	adopted := h.task("PR-12317", registry.TaskPublished, "publish")
	read := h.clock.Now().Add(-time.Hour)
	for _, d := range []registry.Decision{
		{TaskID: opened.ID, Kind: "ready_push", Choice: "push", DiffOpened: read},
		{TaskID: unread.ID, Kind: "ready_push", Choice: "push"},
		{TaskID: sentBack.ID, Kind: "ready_push", Choice: "send_back", Note: "scope", DiffOpened: read},
		{TaskID: sentBack.ID, Kind: "ready_push", Choice: "push"},
	} {
		if err := h.reg.RecordDecision(h.ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	fromRegistry := h.rate(RatePayload{TaskID: opened.ID, Value: 1, Surface: "screen"})
	noise := h.rate(RatePayload{TaskID: unread.ID, Value: 1, Surface: "screen"})
	h.rate(RatePayload{TaskID: sentBack.ID, Value: 1, Surface: "screen"})
	h.rate(RatePayload{TaskID: adopted.ID, Value: -1, Surface: "screen"})
	fromScreen := h.rate(RatePayload{TaskID: adopted.ID, Value: -1, DiffOpened: true, Surface: "screen"})

	for _, tc := range []struct {
		name string
		task registry.Task
		want []bool
	}{
		{"a push taken with the diff opened", opened, []bool{true}},
		{"a push taken without it", unread, []bool{false}},
		{"a push after a send back whose diff was read, the new card's not", sentBack, []bool{false}},
		{"a task with no push, rated once without the screen's word and once with it", adopted, []bool{false, true}},
	} {
		var got []bool
		for _, r := range h.ratings(tc.task.ID) {
			got = append(got, r.DiffOpenedFirst)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: the diff opened first %v, want %v", tc.name, got, tc.want)
		}
	}
	if !strings.Contains(fromRegistry.Result, "the diff opened first") || !strings.Contains(fromScreen.Result, "the diff opened first") {
		t.Errorf("the answers %q and %q do not say the diff was opened first", fromRegistry.Result, fromScreen.Result)
	}
	if noise.Result != "PR-12313: rated +, the diff not opened first: noise" {
		t.Errorf("the answer %q does not call the rating noise", noise.Result)
	}
}

// Only a published or a done task is rated, only by + or −, and a reason is
// one of the rating's five keys (RatingReasons); a refused rating stores nothing.
func TestARatingIsRefusedForATaskNotPublishedOrDoneAndForABadValueOrReason(t *testing.T) {
	h := newHarness(t)
	active := h.task("PR-12314", registry.TaskActive, "coding")
	published := h.task("PR-12315", registry.TaskPublished, "publish")
	for _, tc := range []struct {
		name string
		p    RatePayload
		want string
	}{
		{"an active task", RatePayload{TaskID: active.ID, Value: 1}, "PR-12314 is active: a task is rated once it is published or done"},
		{"a value of 0", RatePayload{TaskID: published.ID, Value: 0}, "a rating is + (1) or − (-1), not 0"},
		{"a value of 2", RatePayload{TaskID: published.ID, Value: 2}, "a rating is + (1) or − (-1), not 2"},
		{"a reason with spaces", RatePayload{TaskID: published.ID, Value: 1, Reason: "too much"}, "a rating takes one of the keys cause, scope, words, tests, time"},
		{"a send back's key", RatePayload{TaskID: published.ID, Value: -1, Reason: "wrong-cause"}, "a rating takes one of the keys cause, scope, words, tests, time"},
		{"an unknown task", RatePayload{TaskID: 9999, Value: 1}, "not found"},
	} {
		q := h.rate(tc.p)
		if q.State != registry.RequestFailed || !strings.Contains(q.Result, tc.want) {
			t.Errorf("%s: the request is %s with %q, want a failure naming %q", tc.name, q.State, q.Result, tc.want)
		}
	}
	for _, id := range []int64{active.ID, published.ID} {
		if rs := h.ratings(id); len(rs) != 0 {
			t.Errorf("task %d has ratings %+v after the refusals", id, rs)
		}
	}
}

// The task card offers + and − (Rate) on a published task's and on a task
// done today, and on no task at work, waiting, queued or needing you.
func TestTheTaskCardOffersRatingOnAPublishedOrDoneTaskOnly(t *testing.T) {
	r := newScreenRig(t)
	r.at(9 * time.Minute)
	active := r.task("PR-12320", "bug", "At work", registry.TaskActive, "coding", "", "")
	needs := r.task("PR-12321", "bug", "Needs you", registry.TaskNeedsYou, "report", "you", "3 questions")
	queued := r.task("PR-12322", "bug", "Queued", registry.TaskQueued, "", "", "")
	pushed := r.task("PR-12323", "bug", "Published", registry.TaskActive, "publish", "", "")
	pushed = r.published(pushed, 4520)
	merged := r.task("PR-12324", "bug", "Merged", registry.TaskActive, "publish", "", "")
	merged = r.move(merged, registry.TaskDone, "", "")

	d := r.data(screenNow.Add(-time.Minute))

	for _, tc := range []struct {
		task registry.Task
		want bool
	}{{active, false}, {needs, false}, {queued, false}, {pushed, true}, {merged, true}} {
		if got := d.Cards[tc.task.ID].Task.Rate; got != tc.want {
			t.Errorf("%s (%s): the card's Rate is %v, want %v", tc.task.JiraKey, tc.task.State, got, tc.want)
		}
	}
}
