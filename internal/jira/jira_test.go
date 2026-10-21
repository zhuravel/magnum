package jira

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/execx"
)

const (
	testToken = "ATATT3xFfGF0-test-token_0123456789abcdef="
	testUser  = "alice@example.com"
	testSite  = "https://example.atlassian.net"
)

// answering is a fake Jira behind a fake curl: it answers status with body,
// and records the commands curl was run with.
func answering(t *testing.T, status, body string) (*Client, *execx.Fake) {
	t.Helper()
	return withRule(t, execx.Rule{Prefix: []string{"curl"}, Result: execx.Result{Stdout: []byte(body + "\n" + status)}})
}

func withRule(t *testing.T, rule execx.Rule) (*Client, *execx.Fake) {
	t.Helper()
	token := filepath.Join(t.TempDir(), "jira.token")
	if err := os.WriteFile(token, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &execx.Fake{Rules: []execx.Rule{rule}}
	return &Client{Runner: fake, URL: testSite + "/", User: testUser, TokenFile: token}, fake
}

// issueJSON is what GET /rest/api/2/issue/PS-123?expand=names&fields=*all
// answers, cut down: the fields a ticket keeps, a requirement field, fields
// it never keeps (people, comments, a number), and the names map that
// labels the custom fields.
const issueJSON = `{
  "key": "PS-123",
  "fields": {
    "summary": "Let referrers pick a payout method",
    "status": {"name": "In Progress"},
    "issuetype": {"name": "Story"},
    "priority": {"name": "High"},
    "labels": ["payouts", "backend"],
    "parent": {"key": "PS-100", "fields": {"summary": "Payouts"}},
    "description": "Referrers choose PayPal or a gift card.\nThe default stays a gift card.",
    "customfield_10010": "Given a referrer without a payout method, the dashboard asks for one.",
    "customfield_10016": 3.0,
    "customfield_10020": "Platform",
    "customfield_10030": {"value": "Full scope"},
    "customfield_10040": "  ",
    "assignee": {"displayName": "Alice Example", "emailAddress": "alice@example.com"},
    "reporter": {"displayName": "Bob Example"},
    "watches": {"watchCount": 2},
    "comment": {"comments": [{"author": {"displayName": "Bob Example"}, "body": "Ship it after the release"}]},
    "attachment": [{"filename": "mockup.png"}]
  },
  "names": {
    "summary": "Summary", "status": "Status", "assignee": "Assignee", "reporter": "Reporter", "comment": "Comment",
    "customfield_10010": "Acceptance Criteria",
    "customfield_10016": "Story Points",
    "customfield_10020": "Team",
    "customfield_10030": "Scope",
    "customfield_10040": "Requirements"
  }
}`

// curl runs with a fixed argv that names no URL, no user and no token:
// --disable first (no ~/.curlrc), then the config on stdin, which is
// exactly these directives: quiet but for errors, HTTPS only, no URL
// globbing, the time bound, the issue's URL with the names and every field,
// JSON, the account's email and API token as curl's user (basic auth, never
// in argv, a log or an error) and the status code after the body. Any other
// directive (verbose, trace, a redirect followed) fails it.
func TestTheTicketRequestSendsTheCredentialsOnStdinNeverInArgv(t *testing.T) {
	c, fake := answering(t, "200", issueJSON)
	if _, err := c.Ticket(t.Context(), "PS-123"); err != nil {
		t.Fatal(err)
	}
	calls := fake.CallsWithPrefix("curl")
	if len(calls) != 1 {
		t.Fatalf("curl calls = %d, want 1", len(calls))
	}
	cmd := calls[0]
	if want := []string{"--disable", "--config", "-"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("argv = %q, want %q", cmd.Args, want)
	}
	if s := cmd.String(); strings.Contains(s, testToken) || strings.Contains(s, testUser) {
		t.Errorf("the command line %q carries the credentials", s)
	}
	want := strings.Join([]string{
		`silent`,
		`show-error`,
		`proto = "=https"`,
		`globoff`,
		`max-time = 20`,
		`url = "https://example.atlassian.net/rest/api/2/issue/PS-123?expand=names&fields=*all"`,
		`header = "Accept: application/json"`,
		`user = "` + testUser + `:` + testToken + `"`,
		`write-out = "\n%{http_code}"`,
	}, "\n") + "\n"
	if stdin := string(cmd.Stdin); stdin != want {
		t.Errorf("curl's config:\n%s\nwant exactly:\n%s", stdin, want)
	}
	if cmd.Timeout <= 20*time.Second || !cmd.Quiet || cmd.Mutates {
		t.Errorf("cmd = timeout %s, quiet %v, mutates %v: want a read past curl's own bound, reported by its caller", cmd.Timeout, cmd.Quiet, cmd.Mutates)
	}
	// A key that is not one never reaches curl: it would land in the URL.
	for _, bad := range []string{"", "PS-", "ps 1", "PS-1/../../myself", "PS-1?x=1", "-1", "PS-1a"} {
		if _, err := c.Ticket(t.Context(), bad); err == nil {
			t.Errorf("key %q: no error", bad)
		}
	}
	if n := len(fake.CallsWithPrefix("curl")); n != 1 {
		t.Errorf("curl ran %d times, want once: a bad key must not reach it", n)
	}
}

// The ticket keeps the issue's key, URL, summary, status, type, priority,
// labels, parent and description, and of the custom fields only those
// whose name speaks of requirements (acceptance, criteria, requirement,
// definition of done, scope) and whose value is text: never a person, a
// comment, an attachment, a number or an option. The file renders them as
// sections under a heading with the key and the URL, the text as given.
func TestTheTicketKeepsOnlyItsRequirementFields(t *testing.T) {
	c, _ := answering(t, "200", issueJSON)
	tk, err := c.Ticket(t.Context(), "PS-123")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Key != "PS-123" || tk.URL != testSite+"/browse/PS-123" || tk.Summary != "Let referrers pick a payout method" ||
		tk.Status != "In Progress" || tk.Type != "Story" || tk.Priority != "High" || !slices.Equal(tk.Labels, []string{"payouts", "backend"}) ||
		tk.ParentKey != "PS-100" || tk.ParentSummary != "Payouts" {
		t.Errorf("ticket = %+v", tk)
	}
	if len(tk.Fields) != 1 || tk.Fields[0].Name != "Acceptance Criteria" || !strings.HasPrefix(tk.Fields[0].Value, "Given a referrer") {
		t.Fatalf("fields = %+v, want the acceptance criteria alone", tk.Fields)
	}
	md := string(tk.Markdown())
	for _, want := range []string{
		"# PS-123 " + testSite + "/browse/PS-123\n",
		"\n## Summary\n\nLet referrers pick a payout method\n",
		"\n## Status\n\nIn Progress\n",
		"\n## Issue type\n\nStory\n",
		"\n## Priority\n\nHigh\n",
		"\n## Labels\n\npayouts, backend\n",
		"\n## Parent\n\nPS-100: Payouts\n",
		"\n## Description\n\nReferrers choose PayPal or a gift card.\nThe default stays a gift card.\n",
		"\n## Acceptance Criteria\n\nGiven a referrer without a payout method, the dashboard asks for one.\n",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("ticket.md lacks %q:\n%s", want, md)
		}
	}
	for _, gone := range []string{"Alice", "alice@example.com", "Bob", "Ship it", "mockup", "Story Points", "Team", "Platform", "Full scope", "Requirements", "watch"} {
		if strings.Contains(md, gone) {
			t.Errorf("ticket.md carries %q:\n%s", gone, md)
		}
	}
	if !strings.HasPrefix(md, "# PS-123 ") {
		t.Errorf("ticket.md does not open with the key:\n%s", md)
	}
}

// A long description is cut at 20,000 bytes and a long requirement field
// at 8,000, each on a character boundary and with a line saying how much
// was kept; a ticket without a description says so.
func TestTheTicketCutsLongText(t *testing.T) {
	desc := "x" + strings.Repeat("Заявка ", 4000) // 13 bytes a word after one: 52,000 bytes trimmed, byte 20,000 inside a letter
	crit := strings.Repeat("ü", 6000)             // 12,000 bytes
	body := strings.Replace(issueJSON, `"Referrers choose PayPal or a gift card.\nThe default stays a gift card."`, `"`+desc+`"`, 1)
	body = strings.Replace(body, `"Given a referrer without a payout method, the dashboard asks for one."`, `"`+crit+`"`, 1)
	c, _ := answering(t, "200", body)
	tk, err := c.Ticket(t.Context(), "PS-123")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(tk.Description); n != MaxDescription-1 || !utf8.ValidString(tk.Description) {
		t.Errorf("description kept %d bytes (valid UTF-8 %v), want %d: the most of %d that ends on a character", n, utf8.ValidString(tk.Description), MaxDescription-1, MaxDescription)
	}
	if tk.DescriptionBytes != len(strings.TrimSpace(desc)) {
		t.Errorf("DescriptionBytes = %d, want %d", tk.DescriptionBytes, len(strings.TrimSpace(desc)))
	}
	if len(tk.Fields) != 1 {
		t.Fatalf("fields = %+v", tk.Fields)
	}
	if f := tk.Fields[0]; len(f.Value) != MaxField || f.Bytes != len(crit) || !utf8.ValidString(f.Value) {
		t.Errorf("field kept %d of %d bytes, want %d", len(f.Value), f.Bytes, MaxField)
	}
	md := string(tk.Markdown())
	for _, want := range []string{"(cut: the first 19999 of 52000 bytes)", "(cut: the first 8000 of 12000 bytes)"} {
		if !strings.Contains(md, want) {
			t.Errorf("ticket.md lacks %q", want)
		}
	}
	empty := strings.Replace(issueJSON, `"description": "Referrers choose PayPal or a gift card.\nThe default stays a gift card.",`, `"description": null,`, 1)
	c, _ = answering(t, "200", empty)
	if tk, err = c.Ticket(t.Context(), "PS-123"); err != nil {
		t.Fatal(err)
	}
	if md := string(tk.Markdown()); !strings.Contains(md, "\n## Description\n\n(empty)\n") {
		t.Errorf("a ticket without a description:\n%s", md)
	}
}

// A refusal (401, 403, 404, another code), a timeout, an unreachable site
// and an answer that is not an issue are errors of a class, whose message
// names the class and the status code, never the answer's body, the user
// or the token.
func TestARefusedOrMalformedAnswerIsAClassOnly(t *testing.T) {
	const secretBody = `{"errorMessages":["Issue PS-123 does not exist or you do not have permission to see it: Payouts"]}`
	for _, tc := range []struct {
		name  string
		rule  execx.Rule
		class string
		says  string
	}{
		{"401", execx.Rule{Result: execx.Result{Stdout: []byte(secretBody + "\n401")}}, ClassUnauthorized, "HTTP 401"},
		{"403", execx.Rule{Result: execx.Result{Stdout: []byte(secretBody + "\n403")}}, ClassForbidden, "HTTP 403"},
		{"404", execx.Rule{Result: execx.Result{Stdout: []byte(secretBody + "\n404")}}, ClassNotFound, "HTTP 404"},
		{"500", execx.Rule{Result: execx.Result{Stdout: []byte(secretBody + "\n500")}}, ClassStatus, "HTTP 500"},
		{"302", execx.Rule{Result: execx.Result{Stdout: []byte("<html>moved: Payouts</html>\n302")}}, ClassStatus, "HTTP 302"},
		{"timeout", execx.Rule{Result: execx.Result{Code: 28, Stderr: []byte("curl: (28) Operation timed out after 20001 milliseconds")}}, ClassTimeout, "20s"},
		{"deadline", execx.Rule{Err: &execx.RunError{Err: context.DeadlineExceeded}}, ClassTimeout, "20s"},
		{"unreachable", execx.Rule{Result: execx.Result{Code: 6, Stderr: []byte("curl: (6) Could not resolve host: example.atlassian.net")}}, ClassUnreachable, "curl exit 6"},
		{"bad json", execx.Rule{Result: execx.Result{Stdout: []byte("Payouts <b>\n200")}}, ClassMalformed, "malformed"},
		{"no fields", execx.Rule{Result: execx.Result{Stdout: []byte(`{"key":"PS-123","names":{"x":"Payouts"}}` + "\n200")}}, ClassMalformed, "malformed"},
		{"no status", execx.Rule{Result: execx.Result{Stdout: []byte(issueJSON)}}, ClassMalformed, "malformed"},
		{"truncated", execx.Rule{Result: execx.Result{Stdout: []byte(issueJSON + "\n200"), Truncated: true}}, ClassMalformed, "malformed"},
	} {
		tc.rule.Prefix = []string{"curl"}
		c, _ := withRule(t, tc.rule)
		_, err := c.Ticket(t.Context(), "PS-123")
		var je *Error
		if !errors.As(err, &je) || je.Class != tc.class {
			t.Errorf("%s: err = %v, want class %s", tc.name, err, tc.class)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, tc.says) {
			t.Errorf("%s: %q does not say %q", tc.name, msg, tc.says)
		}
		for _, leak := range []string{"Payouts", "permission", "Could not resolve", "Operation timed out", testToken, testUser} {
			if strings.Contains(msg, leak) {
				t.Errorf("%s: %q carries %q", tc.name, msg, leak)
			}
		}
	}
}

// The token file must be a regular file only its owner can open, holding
// one token of printable ASCII; curl never runs otherwise, and the error is
// of the token-file class, naming the file, never the token.
func TestTheTokenFileMustBeOwnersOnly(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if tok, err := ReadToken(write("ok", "  "+testToken+"\n", 0o600)); err != nil || tok != testToken {
		t.Fatalf("ReadToken = %q, %v", tok, err)
	}
	for name, tc := range map[string]struct {
		path string
		want string
	}{
		"open":    {write("open", testToken, 0o644), "chmod 600"},
		"group":   {write("group", testToken, 0o640), "chmod 600"},
		"empty":   {write("empty", "\n", 0o600), "is empty"},
		"space":   {write("space", "two words", 0o600), "a character a token cannot have"},
		"newline": {write("newline", "tok\nen", 0o600), "a character a token cannot have"},
		"long":    {write("long", strings.Repeat("x", 4097), 0o600), "longer than"},
		"missing": {filepath.Join(dir, "missing"), "does not exist"},
		"dir":     {dir, "not a regular file"},
	} {
		c, fake := answering(t, "200", issueJSON)
		c.TokenFile = tc.path
		_, err := c.Ticket(t.Context(), "PS-123")
		var je *Error
		if !errors.As(err, &je) || je.Class != ClassTokenFile || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "jira.token_file") {
			t.Errorf("%s: err = %v, want a %s error saying %q", name, err, ClassTokenFile, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), testToken) {
			t.Errorf("%s: the error %q quotes the token", name, err)
		}
		if n := len(fake.CallsWithPrefix("curl")); n != 0 {
			t.Errorf("%s: curl ran %d times", name, n)
		}
	}
	c, fake := answering(t, "200", issueJSON)
	c.User = "alice:x@example.com"
	if _, err := c.Ticket(t.Context(), "PS-123"); err == nil || len(fake.CallsWithPrefix("curl")) != 0 {
		t.Errorf("a user with a colon: err = %v, curl calls %d", err, len(fake.CallsWithPrefix("curl")))
	}
}

// Myself asks Jira whether it accepts the credentials (doctor's live
// check, GET /rest/api/2/myself) the same way, and keeps nothing of the
// answer (the account's name and email).
func TestMyselfChecksTheCredentials(t *testing.T) {
	c, fake := answering(t, "200", `{"accountId":"1","displayName":"Alice Example","emailAddress":"alice@example.com"}`)
	if err := c.Myself(t.Context()); err != nil {
		t.Fatal(err)
	}
	calls := fake.CallsWithPrefix("curl")
	if len(calls) != 1 || !strings.Contains(string(calls[0].Stdin), `url = "https://example.atlassian.net/rest/api/2/myself"`) ||
		!strings.Contains(string(calls[0].Stdin), `user = "`+testUser+`:`+testToken+`"`) {
		t.Fatalf("curl calls = %+v", calls)
	}
	c, _ = answering(t, "401", `{"message":"Client must be authenticated"}`)
	var je *Error
	if err := c.Myself(t.Context()); !errors.As(err, &je) || je.Class != ClassUnauthorized || strings.Contains(err.Error(), "authenticated") {
		t.Errorf("401: err = %v", err)
	}
}
