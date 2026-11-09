package registry

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The factory's Jira plans (the factory's jira_plans.go) are effects, no
// table of their own: a plan is the effect of its task and milestone
// (JiraPlanKey, kind EffectJiraPlan), begun when the milestone comes and
// settled ok (a move planned, or nothing to move) or fail (one the factory
// could not plan) with its JiraPlanResult as JSON; what the factory saw of
// your move after it is the effect beside it (JiraSeenKey, kind
// EffectJiraSeen, begun and settled at once with its JiraSeenResult); the
// comment it would keep on the issue is an effect per milestone
// (JiraCommentKey, kind EffectJiraComment). In shadow mode none of them
// wrote anything to Jira.
const (
	EffectJiraPlan    = "jira_plan"
	EffectJiraSeen    = "jira_seen"
	EffectJiraComment = "jira_comment"
)

// JiraPlanKey is the key of task id's plan at milestone.
func JiraPlanKey(id int64, milestone string) string { return fmt.Sprintf("jira:%d:%s", id, milestone) }

// JiraSeenKey is the key of what was seen after task id's plan at
// milestone.
func JiraSeenKey(id int64, milestone string) string { return JiraPlanKey(id, milestone) + ":seen" }

// JiraCommentKey is the key of task id's Jira comment at milestone.
func JiraCommentKey(id int64, milestone string) string {
	return fmt.Sprintf("jira-comment:%d:%s", id, milestone)
}

// The outcomes of a plan (JiraPlanResult.Outcome): a move planned (from a
// status of its from list to its target, through a transition); nothing to
// move (the status is the target, or not one the move goes from); and the
// plans the factory could not make: the issue is assigned to someone else,
// no transition goes to the target, the transition wants fields, the
// target's category is done, or no read of the issue worked before the
// next milestone or the task's end.
const (
	PlanPlanned       = "planned"
	PlanAlready       = "already"
	PlanAssignedOther = "assigned_other"
	PlanNoTransition  = "no_transition"
	PlanNeedsFields   = "needs_fields"
	PlanTargetDone    = "target_done"
	PlanUnread        = "unread"
)

// The moves the factory saw after a planned move (JiraSeenResult.Moved):
// the issue went to the plan's target, to another status, or did not move
// by the task's end.
const (
	SeenTarget    = "target"
	SeenElsewhere = "elsewhere"
	SeenNone      = "none"
)

// JiraPlanResult is what a settled plan holds: its mode (shadow), its
// outcome, the status it read (From) and the target (To), the transition
// it would take (planned), the transitions Jira offers (no transition), the
// read's failure in the jira package's words (unread, never ticket text),
// and when its read was. Names are Jira's: statuses and transitions, never
// a person.
type JiraPlanResult struct {
	Mode        string    `json:"mode"`
	Outcome     string    `json:"outcome"`
	From        string    `json:"from,omitempty"`
	To          string    `json:"to"`
	Transition  string    `json:"transition,omitempty"`
	Transitions []string  `json:"transitions,omitempty"`
	Cause       string    `json:"cause,omitempty"`
	At          time.Time `json:"at,omitzero"`
}

// Encode is the result as its effect keeps it.
func (p JiraPlanResult) Encode() string { return encode(p) }

// JiraSeenResult is what the factory saw after a planned move: where the
// issue moved (SeenTarget, SeenElsewhere, SeenNone), to which status, when
// (the changelog's time of the move when known, else the read that saw
// it; the task's end for none) and how long after the plan's read.
type JiraSeenResult struct {
	Moved   string    `json:"moved"`
	To      string    `json:"to,omitempty"`
	At      time.Time `json:"at,omitzero"`
	LagSecs int64     `json:"lag_secs,omitempty"`
}

// Encode is the result as its effect keeps it.
func (s JiraSeenResult) Encode() string { return encode(s) }

// Lag is how long after the plan the move came.
func (s JiraSeenResult) Lag() time.Duration { return time.Duration(s.LagSecs) * time.Second }

func encode(v any) string {
	b, err := json.Marshal(v)
	if err != nil { // two structs of strings and times: never
		panic(err)
	}
	return string(b)
}

// JiraPlan is one plan of a task's Jira move at a milestone: its effect's
// state (EffectBegin while it waits for its read) and begin, its result
// (zero while begun) and what was seen after it (nil before anything was).
type JiraPlan struct {
	TaskID    int64
	Milestone string
	State     string
	Begun     time.Time
	Result    JiraPlanResult
	Seen      *JiraSeenResult
}

// JiraPlans are every task's Jira plans, by task id and then milestone
// (start before published), each with what was seen after it. A result
// that does not decode is left zero.
func (r *Registry) JiraPlans(ctx context.Context) ([]JiraPlan, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+effectColumns+" FROM effects WHERE kind IN (?, ?)", EffectJiraPlan, EffectJiraSeen)
	if err != nil {
		return nil, fmt.Errorf("jira plans: %w", err)
	}
	es, err := collect(rows, scanEffect)
	if err != nil {
		return nil, fmt.Errorf("jira plans: %w", err)
	}
	seen := map[string]*JiraSeenResult{}
	var plans []JiraPlan
	for _, e := range es {
		id, milestone, ok := parsePlanKey(e.Key)
		if !ok || id != e.TaskID {
			continue
		}
		switch e.Kind {
		case EffectJiraSeen:
			var s JiraSeenResult
			if e.State == EffectOK && json.Unmarshal([]byte(e.Result), &s) == nil {
				seen[JiraPlanKey(id, strings.TrimSuffix(milestone, ":seen"))] = &s
			}
		case EffectJiraPlan:
			p := JiraPlan{TaskID: id, Milestone: milestone, State: e.State, Begun: e.Begun}
			if e.State != EffectBegin && json.Unmarshal([]byte(e.Result), &p.Result) != nil {
				p.Result = JiraPlanResult{}
			}
			plans = append(plans, p)
		}
	}
	for i := range plans {
		plans[i].Seen = seen[JiraPlanKey(plans[i].TaskID, plans[i].Milestone)]
	}
	order := func(m string) int { return cmp.Or(map[string]int{"start": 1, "published": 2}[m], 3) }
	slices.SortFunc(plans, func(a, b JiraPlan) int {
		return cmp.Or(cmp.Compare(a.TaskID, b.TaskID), cmp.Compare(order(a.Milestone), order(b.Milestone)), strings.Compare(a.Milestone, b.Milestone))
	})
	return plans, nil
}

// parsePlanKey reads jira:<id>:<milestone> (the seen's milestone keeps its
// ":seen").
func parsePlanKey(key string) (int64, string, bool) {
	rest, ok := strings.CutPrefix(key, "jira:")
	if !ok {
		return 0, "", false
	}
	num, milestone, ok := strings.Cut(rest, ":")
	id, err := strconv.ParseInt(num, 10, 64)
	return id, milestone, ok && err == nil && milestone != ""
}
