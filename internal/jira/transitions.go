package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

// maxTransitions is the most transitions Transitions keeps of one answer.
const maxTransitions = 50

// Transition is one move an issue's workflow offers it now, as Transitions
// reads it: its id (digits) and name, the status it goes to (its name, id
// and Jira's category of it: new, indeterminate or done; "" for a word
// that is not a lowercase one), and whether its screen has a required
// field without a default, which a move through it would have to fill.
// Every name is on one line, without control or format characters, cut at
// MaxStatusName bytes.
type Transition struct {
	ID, Name             string
	To, ToID, ToCategory string
	NeedsFields          bool
}

// transitionID is a transition's id as Transitions keeps it.
var transitionID = regexp.MustCompile(`^[0-9]{1,18}$`)

// Transitions reads the moves issue key's workflow offers it now, to your
// account (Jira lists none to an account without the Transition issues
// permission): GET /rest/api/2/issue/<KEY>/transitions?expand=transitions.fields,
// whose fields say whether a transition's screen requires one. It only
// reads, as Ticket and Status do, with the credentials on curl's stdin; a
// transition without an id of digits or a status it goes to is left out,
// and at most maxTransitions are kept. A failure is an *Error of a class.
func (c *Client) Transitions(ctx context.Context, key string) ([]Transition, error) {
	if !keyPattern.MatchString(key) {
		return nil, fmt.Errorf("%q is not a Jira issue key", key)
	}
	body, err := c.get(ctx, "/rest/api/2/issue/"+key+"/transitions?expand=transitions.fields", "jira transitions")
	if err != nil {
		return nil, err
	}
	var ans struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   *struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				Category struct {
					Key string `json:"key"`
				} `json:"statusCategory"`
			} `json:"to"`
			Fields map[string]struct {
				Required        bool `json:"required"`
				HasDefaultValue bool `json:"hasDefaultValue"`
			} `json:"fields"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(body, &ans); err != nil || ans.Transitions == nil {
		return nil, failed(ClassMalformed, 0, "Jira's answer is malformed: not a list of transitions")
	}
	var out []Transition
	for _, tr := range ans.Transitions {
		if len(out) == maxTransitions {
			break
		}
		if !transitionID.MatchString(tr.ID) || tr.To == nil || statusName(tr.To.Name) == "" {
			continue
		}
		t := Transition{ID: tr.ID, Name: statusName(tr.Name), To: statusName(tr.To.Name), ToID: oneLine(tr.To.ID, 64)}
		if categoryPattern.MatchString(tr.To.Category.Key) {
			t.ToCategory = tr.To.Category.Key
		}
		for _, f := range tr.Fields {
			t.NeedsFields = t.NeedsFields || f.Required && !f.HasDefaultValue
		}
		out = append(out, t)
	}
	return out, nil
}
