package tui

import "testing"

// A PR GitHub merged gets the post-merge question (comment only), from its
// queue row and from the slot row that still holds it; an open one keeps the
// generic question. The question is the only difference: y runs the same
// Review action.
func TestDashboardAsksPostMergeQuestionForMergedPR(t *testing.T) {
	data := dashData()
	data.Queue[0].State, data.Queue[0].GHState = "closed", "MERGED" // talkable#7
	data.Queue[1].State, data.Queue[1].GHState = "queued", "OPEN"   // talkable#1, in line (a running one refuses r)
	data.Queue = append(data.Queue, PRRow{Ref: "talkable#9", State: "queued", GHState: "merged"})
	m, _, _ := newDash(t, 220, 50)
	m, _ = send(t, m, dashDataMsg{data: data})
	for _, c := range []struct {
		keys []string
		want string
	}{
		{[]string{"j", "j", "r"}, "Post-merge review talkable#7 (comment only)?"},
		{[]string{"j", "j", "R"}, "Fresh post-merge review of talkable#7 in new agent sessions (comment only)?"},
		{[]string{"j", "j", "i"}, "Post-merge review of talkable#7, also running the simplify role (comment only)?"},
		{[]string{"j", "j", "j", "j", "r"}, "Post-merge review talkable#9 (comment only)?"}, // the state is read without regard to case
		{[]string{"j", "j", "j", "r"}, "Review talkable#1 now (not reviewed yet, queued, next: judge)?"},
	} {
		got, _ := send(t, m, keys(c.keys...)...)
		if got.confirm == nil || got.confirm.question != c.want {
			t.Errorf("keys %v asked %+v, want %q", c.keys, got.confirm, c.want)
			continue
		}
		mustContain(t, viewOf(got), c.want+" y/N")
	}

	// y runs the same Review action as for any other PR.
	for key, want := range map[string]string{
		"r": "review talkable#7 fresh=false simplify=false",
		"R": "review talkable#7 fresh=true simplify=false",
		"i": "review talkable#7 fresh=false simplify=true",
	} {
		m, src, act := newDash(t, 220, 50)
		m, _ = send(t, m, dashDataMsg{data: data})
		before := src.count()
		m, _ = send(t, m, keys("j", "j", key)...)
		if len(act.calls) != 0 {
			t.Fatalf("%s alone acted: %v", key, act.calls)
		}
		if _, follow := dashAct(t, m, "y"); act.last() != want || len(act.calls) != 1 || !hasMsg[dashDataMsg](follow) || src.count() != before+1 {
			t.Errorf("%s then y called %v, want %q and a refresh", key, act.calls, want)
		}
	}
}

// A slot row reads its PR's GitHub state from the row itself, else from the
// PR's queue row.
func TestDashboardSlotRowAsksPostMergeQuestionForMergedPR(t *testing.T) {
	const post = "Post-merge review talkable#1 (comment only)?"
	const generic = "Review talkable#1 now (not reviewed yet, queued, next: judge)?"
	for name, c := range map[string]struct {
		slot, queue string
		want        string
	}{
		"slot says merged":                      {slot: "MERGED", want: post},
		"queue row says merged":                 {queue: "MERGED", want: post},
		"slot says merged, queue row is behind": {slot: "MERGED", queue: "OPEN", want: post},
		"slot says open":                        {slot: "OPEN", want: generic},
		"slot says open, queue row is behind":   {slot: "OPEN", queue: "MERGED", want: generic},
		"neither knows":                         {want: generic},
	} {
		data := dashData()
		data.Slots[0].PRGHState, data.Slots[0].PRState = c.slot, "queued"
		data.Queue[1].GHState, data.Queue[1].State = c.queue, "queued" // talkable#1, in line (a running one refuses r)
		m, _, act := newDash(t, 220, 50)
		m, _ = send(t, m, dashDataMsg{data: data})
		m, _ = send(t, m, keyMsg("r")) // the cursor starts on slot review1
		if m.confirm == nil || m.confirm.question != c.want {
			t.Errorf("%s: asked %+v, want %q", name, m.confirm, c.want)
			continue
		}
		if _, follow := dashAct(t, m, "y"); act.last() != "review talkable#1 fresh=false simplify=false" || !hasMsg[dashDataMsg](follow) {
			t.Errorf("%s: y called %v", name, act.calls)
		}
	}
}
