package jira

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// transitionsPath is the URL of an issue's transitions read.
func transitionsPath(key string) string {
	return "/rest/api/2/issue/" + key + "/transitions?expand=transitions.fields"
}

// transitionsJSON is what GET .../transitions?expand=transitions.fields
// answers, in the shape of Atlassian's Jira Cloud REST v2 reference ("Get
// transitions"): each transition's id, name, the status it goes to with
// its category, whether it has a screen, and with the expand the screen's
// fields, each saying whether it is required and has a default.
const transitionsJSON = `{
  "expand": "transitions",
  "transitions": [
    {"id": "21", "name": "Start progress", "hasScreen": false, "isAvailable": true, "isConditional": false, "isGlobal": false, "isInitial": false,
     "to": {"self": "https://example.atlassian.net/rest/api/2/status/10001", "description": "The work is under way.", "iconUrl": "https://example.atlassian.net/images/icons/progress.gif",
            "name": "In Development", "id": "10001",
            "statusCategory": {"self": "https://example.atlassian.net/rest/api/2/statuscategory/4", "id": 4, "key": "indeterminate", "colorName": "yellow", "name": "In Progress"}},
     "fields": {}},
    {"id": "31", "name": "Ready for review", "hasScreen": true, "isAvailable": true,
     "to": {"name": "In Code Review", "id": "10002", "statusCategory": {"id": 4, "key": "indeterminate"}},
     "fields": {
       "customfield_10050": {"required": true, "hasDefaultValue": false, "name": "Reviewer notes", "key": "customfield_10050", "operations": ["set"], "schema": {"type": "string"}},
       "summary": {"required": false, "hasDefaultValue": false, "name": "Summary", "key": "summary", "operations": ["set"]}
     }},
    {"id": "41", "name": "Resolve", "hasScreen": true, "isAvailable": true,
     "to": {"name": "Done", "id": "10003", "statusCategory": {"id": 3, "key": "done"}},
     "fields": {"resolution": {"required": true, "hasDefaultValue": true, "name": "Resolution", "key": "resolution", "defaultValue": {"name": "Done"}}}},
    {"id": "51", "name": "Bad\u202e\nname", "to": {"name": "In\u0007 QA", "id": "10004", "statusCategory": {"key": "IN-FLIGHT"}}},
    {"id": "not digits", "name": "Broken", "to": {"name": "Broken", "id": "1"}},
    {"id": "61", "name": "Nowhere"}
  ]
}`

// Transitions reads the moves an issue's workflow offers it now, by one
// GET of .../transitions?expand=transitions.fields: each one's id, name,
// the status it goes to with that status's id and category, and whether
// its screen has a required field without a default (the factory never
// fills a field); every name on one line, cut; a transition without a
// numeric id or a target status is left out.
func TestTransitionsReadsTheMovesTheWorkflowOffers(t *testing.T) {
	c, r := statusSite(t, `{}`, map[string]string{transitionsPath("PS-123"): transitionsJSON + "\n200"})
	got, err := c.Transitions(t.Context(), "PS-123")
	if err != nil {
		t.Fatal(err)
	}
	want := []Transition{
		{ID: "21", Name: "Start progress", To: "In Development", ToID: "10001", ToCategory: "indeterminate"},
		{ID: "31", Name: "Ready for review", To: "In Code Review", ToID: "10002", ToCategory: "indeterminate", NeedsFields: true},
		{ID: "41", Name: "Resolve", To: "Done", ToID: "10003", ToCategory: "done"},
		{ID: "51", Name: "Bad name", To: "In  QA", ToID: "10004"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("transitions:\n%+v\nwant\n%+v", got, want)
	}
	if urls := r.urls(); !slices.Equal(urls, []string{transitionsPath("PS-123")}) {
		t.Errorf("curl was asked for %q, want the transitions alone", urls)
	}
	for _, bad := range []string{"", "PS-1/../../myself", "PS-1?x=1"} {
		if _, err := c.Transitions(t.Context(), bad); err == nil {
			t.Errorf("key %q: no error", bad)
		}
	}
	if n := len(r.urls()); n != 1 {
		t.Errorf("curl ran %d times, want once: a bad key must not reach it", n)
	}
}

// Transitions only reads: its curl config is the read's (no request
// method, no data, no write header), so the factory's plan of a move can
// write nothing to Jira.
func TestTransitionsIsAReadThatCarriesNoWrite(t *testing.T) {
	c, fake := answering(t, "200", transitionsJSON)
	if _, err := c.Transitions(t.Context(), "PS-123"); err != nil {
		t.Fatal(err)
	}
	calls := fake.CallsWithPrefix("curl")
	if len(calls) != 1 {
		t.Fatalf("curl calls = %d, want 1", len(calls))
	}
	want := strings.Join([]string{
		`silent`,
		`show-error`,
		`proto = "=https"`,
		`globoff`,
		`max-time = 20`,
		`url = "https://example.atlassian.net/rest/api/2/issue/PS-123/transitions?expand=transitions.fields"`,
		`header = "Accept: application/json"`,
		`user = "` + testUser + `:` + testToken + `"`,
		`write-out = "\n%{http_code}"`,
	}, "\n") + "\n"
	if stdin := string(calls[0].Stdin); stdin != want {
		t.Errorf("curl's config:\n%s\nwant exactly the read's:\n%s", stdin, want)
	}
	if calls[0].Mutates {
		t.Error("the transitions read is marked as a command that mutates")
	}
}

// An answer that is not a list of transitions is malformed, and a refusal
// is a class with no answer's text, as the other reads' are.
func TestTransitionsFailuresAreAClassWithNoAnswerText(t *testing.T) {
	for name, tc := range map[string]struct {
		status, body, class string
	}{
		"no list":   {"200", `{"expand":"transitions"}`, ClassMalformed},
		"not json":  {"200", `<html>Payouts secret</html>`, ClassMalformed},
		"refused":   {"403", `{"errorMessages":["Payouts secret"]}`, ClassForbidden},
		"not found": {"404", `{"errorMessages":["Payouts secret"]}`, ClassNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := answering(t, tc.status, tc.body)
			_, err := c.Transitions(t.Context(), "PS-123")
			je, ok := errors.AsType[*Error](err)
			if !ok || je.Class != tc.class {
				t.Fatalf("err = %v, want class %s", err, tc.class)
			}
			if strings.Contains(err.Error(), "Payouts") {
				t.Errorf("the error %q quotes the answer", err)
			}
		})
	}
	// An empty list is an answer: Jira lists no transition to an account
	// without the Transition issues permission.
	c, _ := answering(t, "200", `{"expand":"transitions","transitions":[]}`)
	if ts, err := c.Transitions(t.Context(), "PS-123"); err != nil || len(ts) != 0 {
		t.Errorf("an empty list: %+v, %v, want none and no error", ts, err)
	}
	c, _ = withRule(t, execx.Rule{Prefix: []string{"curl"}, Err: &execx.ExitError{Code: 28}})
	if _, err := c.Transitions(t.Context(), "PS-123"); !isClass(err, ClassTimeout) {
		t.Errorf("curl's timeout: %v, want class %s", err, ClassTimeout)
	}
}

func isClass(err error, class string) bool {
	je, ok := errors.AsType[*Error](err)
	return ok && je.Class == class
}
