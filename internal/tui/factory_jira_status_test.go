package tui

import (
	"testing"
	"time"
)

// A task's Jira status, as the factory's screen data carries it on its row
// and its task card, reaches the screen's model whole with each load (the
// painters draw it from there), and a row without one keeps none.
func TestATasksJiraStatusReachesTheFactoryScreensModel(t *testing.T) {
	st := &FactoryJiraStatus{Name: "In Review", Category: "indeterminate", Since: factoryAgo(factoryNow, 26*time.Hour),
		Assigned: true, AssignedToYou: true, ReadAt: factoryAgo(factoryNow, 2*time.Minute), LastError: "Jira did not answer within 20s"}
	d := FactoryData{
		Repos: []string{"example/shop"},
		Rows: []FactoryRow{
			{ID: 1, Key: "PR-12360", Title: "Coupon rounding", Group: GroupWaits, Mark: MarkWaits, JiraStatus: st,
				WaitsOn: []WaitsOn{{Who: "reviewers", What: "no approval yet"}}, Since: factoryAgo(factoryNow, time.Hour)},
			{ID: 2, Key: "PR-12361", Title: "Offer email locale", Group: GroupWaits, Mark: MarkWaits,
				WaitsOn: []WaitsOn{{Who: "agent", What: "finding the cause"}}, Since: factoryAgo(factoryNow, time.Hour)},
		},
		Cards: map[int64]FactoryCards{1: {Task: TaskCard{Key: "PR-12360", Title: "Coupon rounding", JiraStatus: st}}},
	}
	h := testFactoryWith(t, 160, 40, FactoryOptions{}, d)
	if !h.m.haveData {
		t.Fatal("the screen did not load its data")
	}
	for _, row := range h.m.data.Rows {
		switch row.ID {
		case 1:
			if row.JiraStatus == nil || *row.JiraStatus != *st {
				t.Errorf("row 1's Jira status in the model: %+v, want %+v", row.JiraStatus, st)
			}
		case 2:
			if row.JiraStatus != nil {
				t.Errorf("row 2's Jira status in the model: %+v, want none", row.JiraStatus)
			}
		}
	}
	if c := h.m.data.Cards[1].Task.JiraStatus; c == nil || *c != *st {
		t.Errorf("the task card's Jira status in the model: %+v, want %+v", c, st)
	}
	_ = h.view() // the painters take a row that carries one
}
