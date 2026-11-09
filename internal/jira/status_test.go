package jira

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/execx"
)

// myAccount is the accountId GET /rest/api/2/myself answers in these tests.
const myAccount = "5b10ac8d82e05b22cc7d4ef5"

// recorded is a fake Jira behind a fake curl that answers each URL (path
// and query) with its recorded body and status, and records the URLs curl
// was asked for, in order; an unknown URL answers 404.
type recorded struct {
	mu      sync.Mutex
	answers map[string]string // "body\nstatus" by URL
	asked   []string
}

func (r *recorded) curl(c execx.Cmd) (execx.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var raw string
	for line := range strings.SplitSeq(string(c.Stdin), "\n") {
		if v, ok := strings.CutPrefix(line, "url = "); ok {
			raw = strings.Trim(v, `"`)
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return execx.Result{}, err
	}
	at := u.Path
	if u.RawQuery != "" {
		at += "?" + u.RawQuery
	}
	r.asked = append(r.asked, at)
	if a, ok := r.answers[at]; ok {
		return execx.Result{Stdout: []byte(a)}, nil
	}
	return execx.Result{Stdout: []byte(`{"errorMessages":["Issue does not exist"]}` + "\n404")}, nil
}

func (r *recorded) urls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.asked)
}

// statusSite is a client over a recorded Jira whose myself is myAccount and
// whose issue PS-123 answers issue (fields=status,assignee,created and its
// changelog), plus any other recorded answers.
func statusSite(t *testing.T, issue string, more map[string]string) (*Client, *recorded) {
	t.Helper()
	r := &recorded{answers: map[string]string{
		"/rest/api/2/myself": `{"accountId":"` + myAccount + `","displayName":"Alice Example","emailAddress":"alice@example.com"}` + "\n200",
		statusPath("PS-123"): issue + "\n200",
	}}
	maps.Copy(r.answers, more)
	c, _ := withRule(t, execx.Rule{Prefix: []string{"curl"}, Fn: r.curl})
	return c, r
}

// statusPath is the URL of an issue's status read.
func statusPath(key string) string {
	return "/rest/api/2/issue/" + key + "?fields=status,assignee,created&expand=changelog"
}

// change is one entry of an issue's changelog: when, and the status it
// moved from and to (ids and names), or another field's edit when to is "".
type change struct {
	at               string
	fromID, from     string
	toID, to         string
	field, fieldName string
}

func (ch change) json() map[string]any {
	item := map[string]any{"field": "status", "fieldtype": "jira", "fieldId": "status",
		"from": ch.fromID, "fromString": ch.from, "to": ch.toID, "toString": ch.to}
	if ch.to == "" {
		item = map[string]any{"field": ch.field, "fieldtype": "custom", "fieldId": ch.fieldName, "from": nil, "fromString": "Payouts",
			"to": nil, "toString": "Payouts, again"}
	}
	return map[string]any{"id": "1", "author": map[string]any{"accountId": "557058:0000", "displayName": "Bob Example"},
		"created": ch.at, "items": []any{item}}
}

// statusIssue is GET /rest/api/2/issue/PS-123?fields=status,assignee,created&expand=changelog's
// answer: status (its id, name and category), an assignee by accountId
// ("" for none), the creation time and the changelog's newest entries,
// newest first as Jira gives them, of total.
func statusIssue(id, name, category, assignee, created string, total int, newest ...change) string {
	hs := []any{}
	for _, ch := range newest {
		hs = append(hs, ch.json())
	}
	fields := map[string]any{
		"status":  map[string]any{"id": id, "name": name, "statusCategory": map[string]any{"id": 4, "key": category, "name": "In Progress"}},
		"created": created, "assignee": nil,
	}
	if assignee != "" {
		fields["assignee"] = map[string]any{"accountId": assignee, "displayName": "Alice Example", "emailAddress": "alice@example.com"}
	}
	b, _ := json.Marshal(map[string]any{"key": "PS-123", "fields": fields,
		"changelog": map[string]any{"startAt": 0, "maxResults": len(hs), "total": total, "histories": hs}})
	return string(b)
}

// pagedAnswer is the paged changelog's answer: entries oldest first, as
// GET /rest/api/2/issue/<key>/changelog gives them.
func pagedAnswer(start, total int, entries ...change) string {
	vals := []any{}
	for _, ch := range entries {
		vals = append(vals, ch.json())
	}
	b, _ := json.Marshal(map[string]any{"startAt": start, "maxResults": len(vals), "total": total, "isLast": start+len(vals) >= total,
		"values": vals})
	return string(b) + "\n200"
}

// edits are n changelog entries that edit a field and move no status.
func edits(n int) []change {
	out := make([]change, n)
	for i := range out {
		out[i] = change{at: "2026-11-04T10:00:00.000+0200", field: "Team", fieldName: "customfield_10020"}
	}
	return out
}

// Status reads an issue's status, its category and since when it holds it:
// the latest entry of the changelog that moved the issue into its current
// status, by the status's id (a move out of it and back in counts the
// move back), whatever the entries' order. One GET of the issue with only the status, the assignee, the
// creation time and the changelog, through curl with the credentials on
// stdin, then the account's own id (myself) once per client, never again.
func TestStatusReadsTheCurrentStatusAndSinceItsLastTransitionIntoIt(t *testing.T) {
	issue := statusIssue("10002", "In Review", "indeterminate", myAccount, "2026-11-01T09:00:00.000+0200", 4,
		change{at: "2026-11-05T14:30:00.000+0200", fromID: "3", from: "In Progress", toID: "10002", to: "In Review"},
		change{at: "2026-11-04T16:00:00.000+0200", fromID: "10002", from: "In Review", toID: "3", to: "In Progress"},
		change{at: "2026-11-04T10:00:00.000+0200", field: "Team", fieldName: "customfield_10020"},
		change{at: "2026-11-03T11:00:00.000+0200", fromID: "1", from: "To Do", toID: "10002", to: "In Review"})
	c, site := statusSite(t, issue, nil)
	got, err := c.Status(t.Context(), "PS-123")
	if err != nil {
		t.Fatal(err)
	}
	want := IssueStatus{Key: "PS-123", Name: "In Review", Category: "indeterminate", StatusID: "10002",
		Since: time.Date(2026, 11, 5, 12, 30, 0, 0, time.UTC), Searched: true, AssigneeYou: true, Assigned: true}
	if !got.Since.Equal(want.Since) {
		t.Errorf("Since = %v, want %v: the latest move into the status", got.Since, want.Since)
	}
	got.Since = want.Since
	if got != want {
		t.Errorf("Status = %+v, want %+v", got, want)
	}
	if urls := site.urls(); !slices.Equal(urls, []string{statusPath("PS-123"), "/rest/api/2/myself"}) {
		t.Errorf("curl asked %q, want the issue's status read, then myself", urls)
	}
	if _, err := c.Status(t.Context(), "PS-123"); err != nil {
		t.Fatal(err)
	}
	if urls := site.urls(); len(urls) != 3 || urls[2] != statusPath("PS-123") {
		t.Errorf("a second read asked %q, want the issue alone: the client keeps your account's id", urls)
	}
	// The entries' order aside (a page gives the oldest first), and a move
	// out of the status that the issue does not show (a changelog Jira did
	// not keep whole) counts none: only a move into the status does.
	reordered := statusIssue("10002", "In Review", "indeterminate", "", "2026-11-01T09:00:00.000+0200", 5,
		change{at: "2026-11-03T11:00:00.000+0200", fromID: "1", from: "To Do", toID: "10002", to: "In Review"},
		change{at: "2026-11-04T16:00:00.000+0200", fromID: "10002", from: "In Review", toID: "3", to: "In Progress"},
		change{at: "2026-11-05T14:30:00.000+0200", fromID: "3", from: "In Progress", toID: "10002", to: "In Review"},
		change{at: "2026-11-06T09:00:00.000+0200", fromID: "10002", from: "In Review", toID: "3", to: "In Progress"},
		change{at: "2026-11-04T10:00:00.000+0200", field: "Team", fieldName: "customfield_10020"})
	c2, _ := statusSite(t, reordered, nil)
	if got, err := c2.Status(t.Context(), "PS-123"); err != nil || !got.Since.Equal(want.Since) {
		t.Errorf("entries oldest first, a move out after: Since = %v, %v; want %v", got.Since, err, want.Since)
	}
	fake := c.Runner.(*execx.Fake)
	for _, cmd := range fake.CallsWithPrefix("curl") {
		if !slices.Equal(cmd.Args, []string{"--disable", "--config", "-"}) || strings.Contains(cmd.String(), testToken) ||
			!strings.Contains(string(cmd.Stdin), `user = "`+testUser+`:`+testToken+`"`) {
			t.Errorf("curl %q: the credentials must go on stdin alone", cmd.String())
		}
	}
}

// An issue whose changelog never moved its status has held it since its
// creation.
func TestStatusWithoutATransitionHoldsSinceTheIssuesCreation(t *testing.T) {
	issue := statusIssue("1", "To Do", "new", "", "2026-11-01T09:00:00.000+0200", 2, edits(2)...)
	c, _ := statusSite(t, issue, nil)
	got, err := c.Status(t.Context(), "PS-123")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "To Do" || got.Category != "new" || !got.Since.Equal(time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("Status = %+v, want To Do (new) since the creation, 07:00 UTC", got)
	}
	empty := statusIssue("1", "To Do", "new", "", "2026-11-01T09:00:00.000+0200", 0)
	c, _ = statusSite(t, empty, nil)
	if got, err = c.Status(t.Context(), "PS-123"); err != nil || !got.Since.Equal(time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("an empty changelog: %+v, %v; want since the creation", got, err)
	}
}

// The issue's own changelog holds only its 100 newest entries: when none
// of them moved the issue into its status, the older entries are read from
// the paged changelog (oldest first there), the page just before the 100
// first, then the one before it, until a move into the status is found.
func TestATruncatedChangelogIsPagedBackFromItsNewestMissingEntry(t *testing.T) {
	issue := statusIssue("10002", "In Review", "indeterminate", "", "2026-10-01T09:00:00.000+0200", 250, edits(100)...)
	older := append([]change{{at: "2026-10-20T08:15:00.000+0000", fromID: "3", from: "In Progress", toID: "10002", to: "In Review"}}, edits(49)...)
	c, site := statusSite(t, issue, map[string]string{
		"/rest/api/2/issue/PS-123/changelog?maxResults=100&startAt=50": pagedAnswer(50, 250, edits(100)...),
		"/rest/api/2/issue/PS-123/changelog?maxResults=50&startAt=0":   pagedAnswer(0, 250, older...),
	})
	got, err := c.Status(t.Context(), "PS-123")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Since.Equal(time.Date(2026, 10, 20, 8, 15, 0, 0, time.UTC)) {
		t.Errorf("Since = %v, want the move found on the oldest page", got.Since)
	}
	want := []string{statusPath("PS-123"), "/rest/api/2/issue/PS-123/changelog?maxResults=100&startAt=50",
		"/rest/api/2/issue/PS-123/changelog?maxResults=50&startAt=0"}
	if urls := site.urls(); !slices.Equal(urls, want) {
		t.Errorf("curl asked\n%q\nwant\n%q", urls, want)
	}
	// A move into the status among the newest entries needs no page.
	found := statusIssue("10002", "In Review", "indeterminate", "", "2026-10-01T09:00:00.000+0200", 250,
		append(edits(99), change{at: "2026-10-30T08:00:00.000+0000", fromID: "3", from: "In Progress", toID: "10002", to: "In Review"})...)
	c, site = statusSite(t, found, nil)
	if got, err = c.Status(t.Context(), "PS-123"); err != nil || !got.Since.Equal(time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("a move among the newest: %+v, %v", got, err)
	}
	if urls := site.urls(); len(urls) != 1 {
		t.Errorf("curl asked %q, want the issue alone", urls)
	}
}

// The pages read are bounded: a changelog whose move into the status is
// older than them leaves Since unknown (zero), never the creation time,
// which would say a wrong date.
func TestAChangelogPastThePagesReadLeavesSinceUnknown(t *testing.T) {
	total := 100 + (maxChangelogPages+1)*100
	issue := statusIssue("10002", "In Review", "indeterminate", "", "2026-10-01T09:00:00.000+0200", total, edits(100)...)
	more := map[string]string{}
	for start := total - 200; start >= 0; start -= 100 {
		more[fmt.Sprintf("/rest/api/2/issue/PS-123/changelog?maxResults=100&startAt=%d", start)] = pagedAnswer(start, total, edits(100)...)
	}
	c, site := statusSite(t, issue, more)
	got, err := c.Status(t.Context(), "PS-123")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Since.IsZero() || got.Name != "In Review" {
		t.Errorf("Status = %+v, want In Review with no Since", got)
	}
	if n := len(site.urls()); n != 1+maxChangelogPages {
		t.Errorf("curl ran %d times, want the issue and %d pages", n, maxChangelogPages)
	}
}

// Status says whether the issue has an assignee and whether it is you (the
// assignee's accountId is the one myself answers): an unassigned issue
// asks nothing of myself, and no person's name or email is kept.
func TestStatusTellsWhetherTheIssueIsAssignedToYou(t *testing.T) {
	for _, tc := range []struct {
		name                string
		assignee            string
		assigned, you, asks bool
	}{
		{"you", myAccount, true, true, true},
		{"someone else", "557058:f58131cb-b67d-43c7-b30d-6b58d40bd077", true, false, true},
		{"no one", "", false, false, false},
	} {
		issue := statusIssue("3", "In Progress", "indeterminate", tc.assignee, "2026-11-01T09:00:00.000+0200", 0)
		c, site := statusSite(t, issue, nil)
		got, err := c.Status(t.Context(), "PS-123")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Assigned != tc.assigned || got.AssigneeYou != tc.you {
			t.Errorf("%s: assigned %v, you %v; want %v, %v", tc.name, got.Assigned, got.AssigneeYou, tc.assigned, tc.you)
		}
		if asked := slices.Contains(site.urls(), "/rest/api/2/myself"); asked != tc.asks {
			t.Errorf("%s: myself asked %v, want %v", tc.name, asked, tc.asks)
		}
		if s := fmt.Sprintf("%+v", got); strings.Contains(s, "Alice") || strings.Contains(s, "alice@") || strings.Contains(s, "Bob") {
			t.Errorf("%s: Status %s keeps a person", tc.name, s)
		}
	}
}

// Every value of the answer is data: the status's name comes on one line
// (control characters, escapes among them, as spaces; format characters,
// the bidi overrides and zero-width spaces among them, dropped) cut at
// MaxStatusName bytes on a character, a category that is not a lowercase word is
// unknown (""), and a key that is not one never reaches curl.
func TestAHostileStatusNameIsOneShortLine(t *testing.T) {
	hostile := "In\nRe\u202eview\u200b\x1b[31m\u0007\u2066 " + strings.Repeat("ü", MaxStatusName)
	issue := statusIssue("10002", hostile, "IN-PROGRESS\nrm", "", "2026-11-01T09:00:00.000+0200", 0)
	c, site := statusSite(t, issue, nil)
	got, err := c.Status(t.Context(), "PS-123")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Name) > MaxStatusName || !utf8.ValidString(got.Name) || strings.ContainsAny(got.Name, "\n\x1b\u0007\u202e\u200b\u2066") ||
		!strings.HasPrefix(got.Name, "In Review [31m") {
		t.Errorf("Name = %q (%d bytes), want one valid line of at most %d bytes", got.Name, len(got.Name), MaxStatusName)
	}
	if got.Category != "" {
		t.Errorf("Category = %q, want unknown", got.Category)
	}
	for _, bad := range []string{"", "PS-", "PS-1/../../myself", "PS-1?fields=*all", "PS-1 x", "-1"} {
		if _, err := c.Status(t.Context(), bad); err == nil {
			t.Errorf("key %q: no error", bad)
		}
	}
	if n := len(site.urls()); n != 1 {
		t.Errorf("curl ran %d times, want once: a bad key must not reach it", n)
	}
	noName := statusIssue("10002", " \n ", "new", "", "2026-11-01T09:00:00.000+0200", 0)
	c, _ = statusSite(t, noName, nil)
	var je *Error
	if _, err := c.Status(t.Context(), "PS-123"); !errors.As(err, &je) || je.Class != ClassMalformed {
		t.Errorf("a status without a name: %v, want a malformed answer", err)
	}
}

// A refusal (401, 404), a timeout and an answer that is not an issue's are
// errors of a class, whose message carries no ticket text, the user or the
// token.
func TestStatusFailuresAreAClassWithNoTicketText(t *testing.T) {
	const secret = `{"errorMessages":["Issue PS-123 does not exist or you do not have permission to see it: Payouts"]}`
	for _, tc := range []struct {
		name  string
		rule  execx.Rule
		class string
	}{
		{"401", execx.Rule{Result: execx.Result{Stdout: []byte(secret + "\n401")}}, ClassUnauthorized},
		{"404", execx.Rule{Result: execx.Result{Stdout: []byte(secret + "\n404")}}, ClassNotFound},
		{"timeout", execx.Rule{Result: execx.Result{Code: 28, Stderr: []byte("curl: (28) Operation timed out: Payouts")}}, ClassTimeout},
		{"not an issue", execx.Rule{Result: execx.Result{Stdout: []byte(`{"key":"PS-123","names":{"x":"Payouts"}}` + "\n200")}}, ClassMalformed},
		{"no changelog", execx.Rule{Result: execx.Result{Stdout: []byte(`{"key":"PS-123","fields":{"status":{"name":"Payouts"}}}` + "\n200")}}, ClassMalformed},
	} {
		tc.rule.Prefix = []string{"curl"}
		c, _ := withRule(t, tc.rule)
		_, err := c.Status(t.Context(), "PS-123")
		var je *Error
		if !errors.As(err, &je) || je.Class != tc.class {
			t.Errorf("%s: err = %v, want class %s", tc.name, err, tc.class)
			continue
		}
		for _, leak := range []string{"Payouts", "permission", "timed out", testToken, testUser} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: %q carries %q", tc.name, err, leak)
			}
		}
	}
}

// statusOnly is GET /rest/api/2/issue/PS-123?fields=status,assignee,created's
// answer: the issue's status, assignee and creation, no changelog.
func statusOnly(id, name, category, assignee string) string {
	fields := map[string]any{"status": map[string]any{"id": id, "name": name, "statusCategory": map[string]any{"key": category}},
		"created": "2026-11-01T09:00:00.000+0200", "assignee": nil}
	if assignee != "" {
		fields["assignee"] = map[string]any{"accountId": assignee}
	}
	b, _ := json.Marshal(map[string]any{"key": "PS-123", "fields": fields})
	return string(b) + "\n200"
}

// fieldsPath is the URL of an issue's read without its changelog.
func fieldsPath(key string) string {
	return "/rest/api/2/issue/" + key + "?fields=status,assignee,created"
}

// After a read that searched the changelog, StatusSince reads the issue's
// status, assignee and creation alone (no changelog, a small answer): the
// same status keeps the read's Since; a new status, or a read whose Since
// was not searched, reads the issue again with its changelog.
func TestStatusSinceKeepsAKnownStatusWithoutReadingItsChangelog(t *testing.T) {
	prior := IssueStatus{Key: "PS-123", Name: "In Review", Category: "indeterminate", StatusID: "10002",
		Since: time.Date(2026, 11, 5, 12, 30, 0, 0, time.UTC), Searched: true}
	moved := statusIssue("3", "In Progress", "indeterminate", "", "2026-11-01T09:00:00.000+0200", 1,
		change{at: "2026-11-08T10:00:00.000+0000", fromID: "10002", from: "In Review", toID: "3", to: "In Progress"})
	c, site := statusSite(t, moved, map[string]string{fieldsPath("PS-123"): statusOnly("10002", "In Review", "indeterminate", myAccount)})
	got, err := c.StatusSince(t.Context(), "PS-123", prior)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Since.Equal(prior.Since) || !got.Searched || got.Name != "In Review" || !got.AssigneeYou {
		t.Errorf("the same status: %+v, want the prior Since kept and the assignee read", got)
	}
	if urls := site.urls(); !slices.Equal(urls, []string{fieldsPath("PS-123"), "/rest/api/2/myself"}) {
		t.Errorf("curl asked %q, want the issue without its changelog (and myself)", urls)
	}
	c, site = statusSite(t, moved, map[string]string{fieldsPath("PS-123"): statusOnly("3", "In Progress", "indeterminate", "")})
	if got, err = c.StatusSince(t.Context(), "PS-123", prior); err != nil {
		t.Fatal(err)
	}
	if got.Name != "In Progress" || got.StatusID != "3" || !got.Since.Equal(time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)) || !got.Searched {
		t.Errorf("a new status: %+v, want its move read from the changelog", got)
	}
	if urls := site.urls(); !slices.Equal(urls, []string{fieldsPath("PS-123"), statusPath("PS-123")}) {
		t.Errorf("curl asked %q, want the issue, then the issue with its changelog", urls)
	}
	unsearched := prior
	unsearched.Searched, unsearched.Since = false, time.Time{}
	c, site = statusSite(t, moved, nil)
	if _, err = c.StatusSince(t.Context(), "PS-123", unsearched); err != nil {
		t.Fatal(err)
	}
	if urls := site.urls(); !slices.Equal(urls, []string{statusPath("PS-123")}) {
		t.Errorf("a prior read whose Since was not searched: curl asked %q, want the issue with its changelog alone", urls)
	}
}

// A changelog whose move into the status is past the pages read is
// searched once: the read says so (Searched, Since zero), and the next
// read of the same status reads no page again.
func TestAChangelogPastThePagesIsNotSearchedAgainForTheSameStatus(t *testing.T) {
	total := 100 + (maxChangelogPages+1)*100
	issue := statusIssue("10002", "In Review", "indeterminate", "", "2026-10-01T09:00:00.000+0200", total, edits(100)...)
	more := map[string]string{fieldsPath("PS-123"): statusOnly("10002", "In Review", "indeterminate", "")}
	for start := total - 200; start >= 0; start -= 100 {
		more[fmt.Sprintf("/rest/api/2/issue/PS-123/changelog?maxResults=100&startAt=%d", start)] = pagedAnswer(start, total, edits(100)...)
	}
	c, site := statusSite(t, issue, more)
	first, err := c.Status(t.Context(), "PS-123")
	if err != nil || !first.Searched || !first.Since.IsZero() {
		t.Fatalf("the first read: %+v, %v; want searched, Since unknown", first, err)
	}
	n := len(site.urls())
	if _, err := c.StatusSince(t.Context(), "PS-123", first); err != nil {
		t.Fatal(err)
	}
	if urls := site.urls()[n:]; !slices.Equal(urls, []string{fieldsPath("PS-123")}) {
		t.Errorf("the next read asked %q, want the issue alone", urls)
	}
}

// Once the issue answered, a changelog page or myself that fails loses
// none of what the issue said: Status returns its status and assignee with
// a PartError naming the part (its Since unknown and not searched, or its
// assignee not known to be you), which carries the class of the part's
// failure and no ticket text.
func TestAFailedPageOrMyselfKeepsTheIssuesFacts(t *testing.T) {
	const secret = `{"errorMessages":["Payouts: you do not have permission"]}`
	truncated := statusIssue("3", "In Progress", "indeterminate", "", "2026-11-01T09:00:00.000+0200", 150, edits(100)...)
	c, _ := statusSite(t, truncated, nil) // the page answers 404
	got, err := c.Status(t.Context(), "PS-123")
	var pe *PartError
	var je *Error
	if !errors.As(err, &pe) || pe.Part != PartChangelog || !errors.As(err, &je) || je.Class != ClassNotFound {
		t.Fatalf("a page's 404: %v, want a PartError of the changelog, not found", err)
	}
	if got.Name != "In Progress" || got.Category != "indeterminate" || got.StatusID != "3" || !got.Since.IsZero() || got.Searched {
		t.Errorf("a page's 404: %+v, want the status kept, Since unknown and not searched", got)
	}
	assigned := statusIssue("3", "In Progress", "indeterminate", "557058:0000", "2026-11-01T09:00:00.000+0200", 0)
	c, _ = statusSite(t, assigned, map[string]string{"/rest/api/2/myself": secret + "\n401"})
	got, err = c.Status(t.Context(), "PS-123")
	if !errors.As(err, &pe) || pe.Part != PartAccount || !errors.As(err, &je) || je.Class != ClassUnauthorized {
		t.Fatalf("myself's 401: %v, want a PartError of the account, unauthorized", err)
	}
	if got.Name != "In Progress" || !got.Assigned || got.AssigneeYou || !got.Searched {
		t.Errorf("myself's 401: %+v, want the status kept, assigned, not known to be you", got)
	}
	if strings.Contains(err.Error(), "Payouts") || strings.Contains(err.Error(), testToken) {
		t.Errorf("%q carries ticket text or the token", err)
	}
}

// An issue whose own changelog came oldest first (its 100 oldest entries,
// not its newest) is paged from the newest missing entry back, so its
// newest move is never left out; a move among its own entries counts once
// no newer entry moved the status.
func TestAnEmbeddedChangelogOldestFirstIsPagedFromItsNewestEntry(t *testing.T) {
	oldest := append([]change{{at: "2026-09-01T08:00:00.000+0000", fromID: "1", from: "To Do", toID: "10002", to: "In Review"}}, edits(99)...)
	for i := range oldest[1:] {
		oldest[i+1].at = fmt.Sprintf("2026-09-%02dT08:00:00.000+0000", 2+i%27)
	}
	issue := statusIssue("10002", "In Review", "indeterminate", "", "2026-08-01T09:00:00.000+0200", 150, oldest...)
	newest := append(edits(49), change{at: "2026-10-30T08:00:00.000+0000", fromID: "3", from: "In Progress", toID: "10002", to: "In Review"})
	c, site := statusSite(t, issue, map[string]string{
		"/rest/api/2/issue/PS-123/changelog?maxResults=50&startAt=100": pagedAnswer(100, 150, newest...),
	})
	got, err := c.Status(t.Context(), "PS-123")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Since.Equal(time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("Since = %v, want the newest move, on the page after the embedded entries", got.Since)
	}
	if urls := site.urls(); !slices.Equal(urls, []string{statusPath("PS-123"), "/rest/api/2/issue/PS-123/changelog?maxResults=50&startAt=100"}) {
		t.Errorf("curl asked %q", urls)
	}
	// Newer entries that move no status leave the answer's own move the latest.
	c, _ = statusSite(t, issue, map[string]string{
		"/rest/api/2/issue/PS-123/changelog?maxResults=50&startAt=100": pagedAnswer(100, 150, edits(50)...),
	})
	if got, err = c.Status(t.Context(), "PS-123"); err != nil || !got.Since.Equal(time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("no newer move: %+v, %v; want the answer's own move", got, err)
	}
}
