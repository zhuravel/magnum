// Package jira reads the Jira issue a pull request names, for a review
// round (DECISIONS "A round hands the judge the PR's Jira ticket"): GET
// <url>/rest/api/2/issue/<KEY>?expand=names&fields=*all with curl through
// execx, as internal/webhooks reads the relay (the operator's outbound
// firewall blocks Go's own HTTP client in a rebuilt, unsigned binary). The
// v2 API returns the description as text, not the v3 API's document format.
//
// The account's email and API token go to curl on stdin, as the `user`
// line of the config curl reads with --config - (basic auth), never in
// argv, a log, an event or an error; --disable, the first argument, keeps a
// ~/.curlrc out. The token is read from a file only its owner can open.
//
// A Ticket keeps the issue's key, URL, summary, status, type, priority,
// labels, parent, its description (cut at MaxDescription) and the custom
// fields whose name speaks of requirements (cut at MaxField): never a
// comment, a person (assignee, reporter, watchers) or an attachment. A
// failure is an *Error of a class, whose message names the class and the
// status code, never the answer's body.
package jira

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
)

// The cuts of a ticket's text, in bytes: the description and each
// requirement field.
const (
	MaxDescription = 20000
	MaxField       = 8000
)

// MaxTime is curl's bound on one request (its --max-time); the command
// itself gets 5 seconds more.
const MaxTime = 20 * time.Second

// maxToken is the longest token file content read (an Atlassian API token
// is about 200 characters).
const maxToken = 4096

// The classes of an *Error.
const (
	ClassTokenFile    = "token_file"   // the token file is missing, open to others or holds no token
	ClassUnauthorized = "unauthorized" // HTTP 401: the email or the token is wrong
	ClassForbidden    = "forbidden"    // HTTP 403
	ClassNotFound     = "not_found"    // HTTP 404: no such issue, or the account cannot see it
	ClassStatus       = "status"       // any other HTTP status
	ClassTimeout      = "timeout"      // no answer within MaxTime
	ClassUnreachable  = "unreachable"  // curl failed otherwise (DNS, TLS, connection)
	ClassMalformed    = "malformed"    // an answer that is not an issue
)

// Error is a failure to read an issue, by class. Its message names the
// class and the status code or curl's exit code, never the answer's body,
// the user or the token; a token file's names the file.
type Error struct {
	Class string
	// Code is the HTTP status (ClassUnauthorized, ClassForbidden,
	// ClassNotFound, ClassStatus) or curl's exit code (ClassUnreachable).
	Code int
	msg  string
}

func (e *Error) Error() string { return e.msg }

func failed(class string, code int, format string, args ...any) *Error {
	return &Error{Class: class, Code: code, msg: fmt.Sprintf(format, args...)}
}

// Client reads issues from one Jira site.
type Client struct {
	Runner execx.Runner
	// URL is the site, https ("https://example.atlassian.net"; config.Jira
	// validates it).
	URL string
	// User is the account's email and TokenFile the file holding its API
	// token (ReadToken).
	User      string
	TokenFile string

	tokenWarned atomic.Bool
}

// WarnTokenFile reports whether a token file problem is to be a warning:
// true the first time it is asked, false after. The daemon makes one Client
// at startup, so a token file the config warned about is one warning per
// daemon start, then info.
func (c *Client) WarnTokenFile() bool { return c.tokenWarned.CompareAndSwap(false, true) }

// Field is one requirement field of a ticket: its name, its text (cut at
// MaxField) and the text's size before the cut.
type Field struct {
	Name  string
	Value string
	Bytes int
}

// Ticket is what a round hands the judge of an issue.
type Ticket struct {
	Key string // "PS-123"
	URL string // <site>/browse/<Key>
	// Summary, Status, Type and Priority are the issue's, on one line each
	// ("" when it has none); Labels its labels.
	Summary, Status, Type, Priority string
	Labels                          []string
	// ParentKey and ParentSummary are its parent's (an epic's), when it has one.
	ParentKey, ParentSummary string
	// Description is the issue's description, cut at MaxDescription;
	// DescriptionBytes its size before the cut.
	Description      string
	DescriptionBytes int
	// Fields are the custom fields whose name holds one of requirementWords
	// and whose value is text, by name.
	Fields []Field
}

// keyPattern is an issue key: a project key, a dash and a number.
var keyPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*-[0-9]+$`)

// requirementWords pick the custom fields a ticket keeps, by name in any case.
var requirementWords = []string{"acceptance", "criteria", "requirement", "definition of done", "scope"}

// Ticket reads the issue key ("PS-123") from the site.
func (c *Client) Ticket(ctx context.Context, key string) (Ticket, error) {
	if !keyPattern.MatchString(key) {
		return Ticket{}, fmt.Errorf("%q is not a Jira issue key", key)
	}
	body, err := c.get(ctx, "/rest/api/2/issue/"+key+"?expand=names&fields=*all", "jira issue")
	if err != nil {
		return Ticket{}, err
	}
	return c.parse(key, body)
}

// Myself asks the site whether it accepts the credentials (GET
// /rest/api/2/myself), keeping nothing of the answer: doctor's live check.
func (c *Client) Myself(ctx context.Context) error {
	_, err := c.get(ctx, "/rest/api/2/myself", "jira myself")
	return err
}

// get runs one GET of path on the site and returns a 200's body.
func (c *Client) get(ctx context.Context, path, label string) ([]byte, error) {
	token, err := ReadToken(c.TokenFile)
	if err != nil {
		return nil, err
	}
	conf, err := curlConfig(strings.TrimRight(c.URL, "/")+path, c.User, token)
	if err != nil {
		return nil, err
	}
	res, err := c.Runner.Run(ctx, execx.Cmd{
		Name: "curl", Args: []string{"--disable", "--config", "-"}, Stdin: conf,
		Timeout: MaxTime + 5*time.Second, Label: label, Quiet: true,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		exit, isExit := errors.AsType[*execx.ExitError](err)
		switch {
		case errors.Is(err, context.DeadlineExceeded), isExit && exit.Code == 28:
			return nil, failed(ClassTimeout, 0, "Jira did not answer within %s", MaxTime)
		case isExit:
			return nil, failed(ClassUnreachable, exit.Code, "Jira unreachable (curl exit %d)", exit.Code)
		}
		return nil, failed(ClassUnreachable, 0, "Jira unreachable (curl did not run)")
	}
	if res.Truncated {
		return nil, failed(ClassMalformed, 0, "Jira's answer is malformed: longer than curl's output may be")
	}
	code, body, ok := splitStatus(res.Stdout)
	switch {
	case !ok:
		return nil, failed(ClassMalformed, 0, "Jira's answer is malformed: curl wrote no status code after it")
	case code == 200:
		return body, nil
	case code == 401:
		return nil, failed(ClassUnauthorized, code, "Jira refused the credentials (HTTP 401): check jira.user and jira.token_file")
	case code == 403:
		return nil, failed(ClassForbidden, code, "Jira refused the account (HTTP 403)")
	case code == 404:
		return nil, failed(ClassNotFound, code, "Jira has no such issue, or the account cannot see it (HTTP 404)")
	}
	return nil, failed(ClassStatus, code, "Jira answered HTTP %d", code)
}

// curlConfig is what curl reads with --config -: HTTPS only, no URL
// globbing, no redirect followed, the time bound, the URL, JSON, the
// account as curl's user (basic auth) and the status code after the body
// on a line of its own.
func curlConfig(u, user, token string) ([]byte, error) {
	if !strings.HasPrefix(u, "https://") {
		return nil, errors.New("jira.url must be https")
	}
	qu, err := quote(u)
	if err != nil {
		return nil, errors.New("jira.url holds a control character")
	}
	if user == "" || strings.ContainsAny(user, ": ") {
		return nil, errors.New("jira.user must be the account's email (no colon, no space)")
	}
	cred, err := quote(user + ":" + token)
	if err != nil {
		return nil, errors.New("jira.user holds a control character") // ReadToken refuses one in the token first
	}
	var b bytes.Buffer
	b.WriteString("silent\nshow-error\nproto = \"=https\"\ngloboff\n")
	fmt.Fprintf(&b, "max-time = %s\n", strconv.FormatFloat(MaxTime.Seconds(), 'f', -1, 64))
	fmt.Fprintf(&b, "url = %s\n", qu)
	b.WriteString("header = \"Accept: application/json\"\n")
	b.WriteString("user = " + cred + "\n")
	b.WriteString("write-out = \"\\n%{http_code}\"\n")
	return b.Bytes(), nil
}

// quote double-quotes s for curl's config: a backslash and a quote escaped;
// a control character, which curl's config cannot carry, refused.
func quote(s string) (string, error) {
	if strings.ContainsFunc(s, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return "", errors.New("holds a control character")
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`, nil
}

// splitStatus cuts curl's output into the body and the status code that
// --write-out put on its last line.
func splitStatus(out []byte) (int, []byte, bool) {
	body, last, ok := bytes.CutLast(out, []byte("\n"))
	if !ok {
		return 0, nil, false
	}
	code, err := strconv.Atoi(string(bytes.TrimSpace(last)))
	if err != nil || code < 100 || code > 599 {
		return 0, nil, false
	}
	return code, body, true
}

// ReadToken reads the API token from path: a file fsx.CheckPrivate
// accepts, holding one word of printable ASCII, trimmed of surrounding
// white space. Its errors are of ClassTokenFile, name the file and never
// quote the token.
func ReadToken(path string) (string, error) {
	bad := func(format string, args ...any) error {
		return failed(ClassTokenFile, 0, "jira.token_file "+format, args...)
	}
	if path == "" {
		return "", bad("is not set")
	}
	if err := fsx.CheckPrivate(path); err != nil {
		return "", bad("%v", err)
	}
	data, err := fsx.ReadRegular(path, maxToken)
	if errors.Is(err, fsx.ErrTooLarge) {
		return "", bad("%s is longer than %d bytes: it must hold the API token alone", path, maxToken)
	}
	if err != nil {
		return "", bad("%v", err)
	}
	token := strings.TrimSpace(string(data))
	switch {
	case token == "":
		return "", bad("%s is empty", path)
	case strings.ContainsFunc(token, func(r rune) bool { return r < '!' || r > '~' }):
		return "", bad("%s holds a character a token cannot have (one word of printable ASCII)", path)
	}
	return token, nil
}

// issueAnswer is the part of an issue's answer a ticket reads: its key, its
// fields by id (decoded one by one, so a field of another shape is left
// out, not an error) and the names of the fields (expand=names).
type issueAnswer struct {
	Key    string                     `json:"key"`
	Fields map[string]json.RawMessage `json:"fields"`
	Names  map[string]string          `json:"names"`
}

// parse reads a 200's issue: one with no fields object is malformed.
func (c *Client) parse(asked string, body []byte) (Ticket, error) {
	var is issueAnswer
	if err := json.Unmarshal(body, &is); err != nil || is.Fields == nil {
		return Ticket{}, failed(ClassMalformed, 0, "Jira's answer is malformed: not an issue")
	}
	key := asked
	if keyPattern.MatchString(is.Key) { // an issue moved to another project answers with its new key
		key = is.Key
	}
	f := is.Fields
	var named struct {
		Name string `json:"name"`
	}
	nameOf := func(id string) string {
		named.Name = ""
		decode(f[id], &named)
		return line(named.Name)
	}
	t := Ticket{Key: key, URL: strings.TrimRight(c.URL, "/") + "/browse/" + key,
		Status: nameOf("status"), Type: nameOf("issuetype"), Priority: nameOf("priority")}
	var s string
	if decode(f["summary"], &s) {
		t.Summary = line(s)
	}
	var labels []string
	if decode(f["labels"], &labels) {
		for _, l := range labels {
			if l = line(l); l != "" {
				t.Labels = append(t.Labels, l)
			}
		}
	}
	var parent struct {
		Key    string `json:"key"`
		Fields struct {
			Summary string `json:"summary"`
		} `json:"fields"`
	}
	if decode(f["parent"], &parent) && keyPattern.MatchString(parent.Key) {
		t.ParentKey, t.ParentSummary = parent.Key, line(parent.Fields.Summary)
	}
	var desc string
	if decode(f["description"], &desc) {
		desc = strings.TrimSpace(desc)
		t.DescriptionBytes = len(desc)
		t.Description = cut(desc, MaxDescription)
	}
	for id, raw := range f {
		name := line(is.Names[id])
		if !strings.HasPrefix(id, "customfield_") || !requirement(name) {
			continue
		}
		var v string
		if !decode(raw, &v) {
			continue
		}
		if v = strings.TrimSpace(v); v != "" {
			t.Fields = append(t.Fields, Field{Name: name, Value: cut(v, MaxField), Bytes: len(v)})
		}
	}
	slices.SortFunc(t.Fields, func(a, b Field) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Value, b.Value))
	})
	return t, nil
}

// decode unmarshals raw into v and reports whether it held a value of v's
// shape (null and a missing field do not).
func decode(raw json.RawMessage, v any) bool {
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null")) && json.Unmarshal(raw, v) == nil
}

// requirement reports whether a custom field's name speaks of requirements.
func requirement(name string) bool {
	name = strings.ToLower(name)
	return slices.ContainsFunc(requirementWords, func(w string) bool { return strings.Contains(name, w) })
}

// line is s on one line: control characters (line breaks among them) as
// spaces, trimmed.
func line(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
}

// cut keeps at most n bytes of s, ending on a character.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Markdown is ticket.md: a heading with the key and the URL, then the
// fields as sections, the text as given, a cut text followed by a line
// saying how much of it was kept.
func (t Ticket) Markdown() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s %s\n", t.Key, t.URL)
	section := func(name, text string, size int) {
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", name, text)
		if size > len(text) {
			fmt.Fprintf(&b, "\n(cut: the first %d of %d bytes)\n", len(text), size)
		}
	}
	for _, s := range []struct{ name, text string }{
		{"Summary", t.Summary}, {"Status", t.Status}, {"Issue type", t.Type}, {"Priority", t.Priority},
		{"Labels", strings.Join(t.Labels, ", ")},
	} {
		if s.text != "" {
			section(s.name, s.text, 0)
		}
	}
	if t.ParentKey != "" {
		section("Parent", strings.TrimSuffix(t.ParentKey+": "+t.ParentSummary, ": "), 0)
	}
	if t.Description == "" {
		section("Description", "(empty)", 0)
	} else {
		section("Description", t.Description, t.DescriptionBytes)
	}
	for _, f := range t.Fields {
		section(f.Name, f.Value, f.Bytes)
	}
	return b.Bytes()
}
