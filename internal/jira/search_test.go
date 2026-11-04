package jira

import (
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// searchJSON is what GET /rest/api/3/search/jql answers, cut down: the
// issues with the fields asked for, and the people and comments a ticket
// never keeps.
const searchJSON = `{
  "issues": [
    {"id": "10001", "key": "PR-12341", "fields": {
      "summary": "Weekly digest shows last week's numbers",
      "status": {"name": "To Do"}, "issuetype": {"name": "Bug"}, "priority": {"name": "High"},
      "assignee": {"displayName": "Alice Example", "emailAddress": "alice@example.com"}}},
    {"id": "10002", "key": "PR-12342", "fields": {
      "summary": "Digest email layout",
      "status": {"name": "Done"}, "issuetype": {"name": "Story"}}},
    {"id": "10003", "key": "not a key", "fields": {"summary": "dropped"}}
  ],
  "isLast": true
}`

// Search reads the issues whose title holds one of the words: one GET of
// the enhanced search (/rest/api/3/search/jql) with the JQL in the query,
// the fields a ticket's head needs and the limit, through curl like Ticket
// (the credentials on stdin). Each issue is a ticket of its key, URL,
// summary, status, type and priority; an issue without a key is left out.
func TestSearchReadsIssuesByTheWordsOfTheirTitle(t *testing.T) {
	c, fake := answering(t, "200", searchJSON)
	got, err := c.Search(t.Context(), "The weekly DIGEST: week's numbers, of UTC+10", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "PR-12341" || got[0].Summary != "Weekly digest shows last week's numbers" ||
		got[0].Status != "To Do" || got[0].Type != "Bug" || got[0].Priority != "High" || got[0].URL != testSite+"/browse/PR-12341" ||
		got[1].Key != "PR-12342" || got[1].Type != "Story" {
		t.Fatalf("Search = %+v", got)
	}
	calls := fake.CallsWithPrefix("curl")
	if len(calls) != 1 {
		t.Fatalf("curl calls = %d, want 1", len(calls))
	}
	if s := calls[0].String(); strings.Contains(s, testToken) || strings.Contains(s, testUser) {
		t.Errorf("the command line %q carries the credentials", s)
	}
	conf := string(calls[0].Stdin)
	if !strings.Contains(conf, `user = "`+testUser+`:`+testToken+`"`) {
		t.Errorf("curl's config has no credentials:\n%s", conf)
	}
	var raw string
	for line := range strings.SplitSeq(conf, "\n") {
		if v, ok := strings.CutPrefix(line, "url = "); ok {
			raw = strings.Trim(v, `"`)
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "example.atlassian.net" || u.Path != "/rest/api/3/search/jql" {
		t.Fatalf("url %q (%v), want the site's enhanced search", raw, err)
	}
	q := u.Query()
	if q.Get("maxResults") != "5" || q.Get("fields") != "summary,status,issuetype,priority" {
		t.Errorf("query %v, want 5 results with the head's fields", q)
	}
	want := `summary ~ "weekly" OR summary ~ "digest" OR summary ~ "weeks" OR summary ~ "numbers" OR summary ~ "utc" ORDER BY updated DESC`
	if got := q.Get("jql"); got != want {
		t.Errorf("jql %q,\nwant %q", got, want)
	}
	if strings.Contains(conf, "Alice") {
		t.Errorf("the search kept a person")
	}
}

// Words a title search cannot use (none left after the short and common
// words go, or only marks) ask Jira nothing; the limit stays within the
// API's 1 to 50.
func TestSearchWithoutWordsAsksNothing(t *testing.T) {
	c, fake := answering(t, "200", searchJSON)
	for _, words := range []string{"", "  ", "the of a to", `"" ~ * \ :`} {
		if got, err := c.Search(t.Context(), words, 3); err != nil || len(got) != 0 {
			t.Errorf("Search(%q) = %+v, %v; want nothing", words, got, err)
		}
	}
	if n := len(fake.CallsWithPrefix("curl")); n != 0 {
		t.Fatalf("curl ran %d times for no words", n)
	}
	if _, err := c.Search(t.Context(), "digest", 500); err != nil {
		t.Fatal(err)
	}
	calls := fake.CallsWithPrefix("curl")
	if len(calls) != 1 || !strings.Contains(string(calls[0].Stdin), "maxResults=50") {
		t.Fatalf("a limit of 500 asked %+v, want 50", calls)
	}
}

// An answer that is no search result is malformed, of a class, and never
// quotes the body.
func TestASearchAnswerWithoutIssuesIsMalformed(t *testing.T) {
	c, _ := answering(t, "200", `{"errorMessages":["Payouts"]}`)
	_, err := c.Search(t.Context(), "digest", 3)
	var je *Error
	if !errors.As(err, &je) || je.Class != ClassMalformed || strings.Contains(err.Error(), "Payouts") {
		t.Fatalf("err = %v, want a malformed answer that quotes nothing", err)
	}
}

// A token file that does not exist is told apart from the other token file
// problems: the error names the file and wraps fs.ErrNotExist.
func TestAMissingTokenFileNamesItsPath(t *testing.T) {
	c, fake := answering(t, "200", issueJSON)
	c.TokenFile = filepath.Join(t.TempDir(), "jira.token")
	_, err := c.Ticket(t.Context(), "PR-12345")
	var je *Error
	if !errors.As(err, &je) || je.Class != ClassTokenFile || je.File != c.TokenFile || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v (%+v), want a token file error naming %s and wrapping fs.ErrNotExist", err, je, c.TokenFile)
	}
	if _, err := c.Search(t.Context(), "digest", 3); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Search = %v, want the missing token file", err)
	}
	if n := len(fake.CallsWithPrefix("curl")); n != 0 {
		t.Fatalf("curl ran %d times", n)
	}
	open := filepath.Join(t.TempDir(), "open")
	writeMode(t, open, 0o644)
	c.TokenFile = open
	if _, err := c.Ticket(t.Context(), "PR-12345"); errors.Is(err, fs.ErrNotExist) || !errors.As(err, &je) || je.File != open {
		t.Fatalf("an open token file: %v (%+v), want a token file error naming it, not a missing one", err, je)
	}
}

// The words of a search are the first 8 distinct ones of 3 letters or more
// that are not common words, lowercased, letters and digits only (an
// apostrophe dropped inside a word).
func TestSearchWordsKeepTheFirstWordsOnce(t *testing.T) {
	got := SearchWords("Fix fix the FIX checkout's total, again; checkout totals 12 a b c d e f g h i j k l m n o p q r s t u v w x y z alpha beta gamma delta epsilon zeta")
	if want := []string{"fix", "checkouts", "total", "again", "checkout", "totals", "alpha", "beta"}; !slices.Equal(got, want) {
		t.Fatalf("SearchWords = %q, want %q", got, want)
	}
}

// writeMode writes a token file of mode perm.
func writeMode(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(testToken+"\n"), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}
