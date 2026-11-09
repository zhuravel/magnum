package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// MaxStatusName is the cut of a status's name, in bytes.
const MaxStatusName = 64

// The bounds of the changelog Status reads past the issue's own answer,
// which holds only some of its entries (Jira Cloud cuts it at the 100
// newest): the pages of the paged changelog, each of at most changelogPage
// entries (the endpoint's own limit).
const (
	maxChangelogPages = 5
	changelogPage     = 100
)

// IssueStatus is where an issue stands in Jira: its key, its status's name
// (on one line, without control or format characters, cut at
// MaxStatusName bytes) and id, Jira's category of it (statusCategory's
// key: new, indeterminate or done; "" for a word that is not a lowercase
// one), since when it holds it, and whether it has an assignee and that
// assignee is you (the account of the client's credentials). No person's
// name or email is kept.
type IssueStatus struct {
	Key, Name, Category string
	// StatusID is the status's id ("" when Jira gave none): StatusSince
	// tells a status that did not change by it.
	StatusID string
	// Since is when the issue moved into its status: the latest entry of
	// its changelog that did, else its creation when no entry moved its
	// status; zero when the move is older than the pages Status reads, a
	// page could not be read, or Jira gave no time Status can parse.
	// Searched says the changelog was searched for it (the whole of it, or
	// the pages up to their bound): false after a page that failed.
	Since    time.Time
	Searched bool
	// AssigneeYou says the issue is assigned to you, Assigned that it is
	// assigned to anyone.
	AssigneeYou bool
	Assigned    bool
	// MovedBy is who moved the issue into its status, by the author of the
	// changelog entry Since is of: MovedByYou (its accountId is your
	// account's), MovedByOther (another account: a person or Jira's
	// automation), "" when unknown (no such entry, an entry that names no
	// author, or your account could not be read). Never a name or an id.
	MovedBy string
}

// The authors of a move (IssueStatus.MovedBy).
const (
	MovedByYou   = "you"
	MovedByOther = "other"
)

// The parts of a read whose failure leaves the issue's own answer standing
// (PartError).
const (
	PartChangelog = "changelog" // a page of the changelog: Since is unknown
	PartAccount   = "account"   // your account's id (myself): the assignee is not known to be you
)

// PartError is a failure of a read after the issue itself answered: Status
// returns what the issue said with it. Err is the part's own failure (an
// *Error of a class, or the context's), and the message names the part and
// that class, never the answer's body.
type PartError struct {
	Part string
	Err  error
}

func (e *PartError) Error() string {
	return "Jira answered for the issue but not for its " + e.Part + ": " + e.Err.Error()
}

// Unwrap is the part's own failure.
func (e *PartError) Unwrap() error { return e.Err }

// categoryPattern is a status category's key as Status keeps it.
var categoryPattern = regexp.MustCompile(`^[a-z]{1,32}$`)

// accountPattern is an Atlassian accountId ("5b10ac8d82e05b22cc7d4ef5",
// "557058:f58131cb-b67d-43c7-b30d-6b58d40bd077").
var accountPattern = regexp.MustCompile(`^[A-Za-z0-9:_-]{1,128}$`)

// Status reads where issue key ("PS-123") stands, its changelog searched
// for since when: StatusSince with no read before.
func (c *Client) Status(ctx context.Context, key string) (IssueStatus, error) {
	return c.StatusSince(ctx, key, IssueStatus{})
}

// StatusSince reads where issue key stands after a read that gave prior.
// When prior's changelog was searched (prior.Searched), it first makes one
// GET of /rest/api/2/issue/<KEY>?fields=status,assignee,created, a small
// answer: a status that is still prior's (by its id, else by its name)
// keeps prior's Since, and no changelog is read. Otherwise (a new status,
// or no search before) it GETs the issue with &expand=changelog and
// searches the changelog: its entries the answer holds, then, when they
// hold no move into the status and the changelog has more, the paged
// changelog (/rest/api/2/issue/<KEY>/changelog, oldest first) back from
// the newest entry the answer did not hold, at most maxChangelogPages
// pages. Which entries the answer holds is told by their order: newest
// first (Jira Cloud's newest 100), or oldest first. The entry's author is
// MovedBy (prior's on the same status). For an assigned issue, or a move
// whose entry names its author, it asks your account's id (GET
// /rest/api/2/myself) once per read, failed or not, until it answered:
// the client keeps it. The credentials go to curl as Ticket's do, and a
// failure is an *Error of a class, as Ticket's is; a failure after the
// issue answered is a *PartError, returned with what the issue said.
func (c *Client) StatusSince(ctx context.Context, key string, prior IssueStatus) (IssueStatus, error) {
	if !keyPattern.MatchString(key) {
		return IssueStatus{}, fmt.Errorf("%q is not a Jira issue key", key)
	}
	if prior.Searched {
		ans, err := c.issue(ctx, key, false)
		if err != nil {
			return IssueStatus{}, err
		}
		if s, ok := ans.status(key); ok && sameStatus(s, prior) {
			s.Since, s.Searched, s.MovedBy = prior.Since, true, prior.MovedBy
			return c.assignee(ans, s, nil, c.accountOnce(ctx, ans, ""))
		}
	}
	ans, err := c.issue(ctx, key, true)
	if err != nil {
		return IssueStatus{}, err
	}
	s, ok := ans.status(key)
	if !ok {
		return IssueStatus{}, failed(ClassMalformed, 0, "Jira's answer is malformed: an issue without a status")
	}
	if ans.Changelog == nil {
		return IssueStatus{}, failed(ClassMalformed, 0, "Jira's answer is malformed: an issue without its changelog")
	}
	since, author, searched, perr := c.since(ctx, key, ans, s)
	s.Since, s.Searched = since, searched
	me := c.accountOnce(ctx, ans, author)
	s.MovedBy = movedBy(author, me)
	return c.assignee(ans, s, perr, me)
}

// meRead is one read's answer of your account: its id, or why it could
// not be read (err), or neither when the read needed none.
type meRead struct {
	id  string
	err error
}

// accountOnce asks your account at most once for one read, when the read needs it
// (an assignee to compare, or a move's author), failed or not: the
// assignee and the author share the answer.
func (c *Client) accountOnce(ctx context.Context, ans statusAnswer, author string) meRead {
	if !accountPattern.MatchString(author) && assigneeOf(ans) == "" {
		return meRead{}
	}
	id, err := c.account(ctx)
	return meRead{id: id, err: err}
}

// movedBy is who the author of a move (an accountId, "" for none) is to
// you (me): MovedByYou, MovedByOther, or "" when it is not known (no
// author, or your account could not be read: no error of the read, the
// move's author is only a hint).
func movedBy(author string, me meRead) string {
	switch {
	case !accountPattern.MatchString(author), me.err != nil, me.id == "":
		return ""
	case author == me.id:
		return MovedByYou
	}
	return MovedByOther
}

// statusAnswer is the part of an issue's answer Status reads.
type statusAnswer struct {
	Key       string                     `json:"key"`
	Fields    map[string]json.RawMessage `json:"fields"`
	Changelog *struct {
		Total     int               `json:"total"`
		Histories []json.RawMessage `json:"histories"`
	} `json:"changelog"`
}

// issue GETs key's status, assignee and creation, with its changelog when
// changelog is set; one with no fields object is malformed.
func (c *Client) issue(ctx context.Context, key string, changelog bool) (statusAnswer, error) {
	path := "/rest/api/2/issue/" + key + "?fields=status,assignee,created"
	if changelog {
		path += "&expand=changelog"
	}
	body, err := c.get(ctx, path, "jira status")
	if err != nil {
		return statusAnswer{}, err
	}
	var ans statusAnswer
	if err := json.Unmarshal(body, &ans); err != nil || ans.Fields == nil {
		return statusAnswer{}, failed(ClassMalformed, 0, "Jira's answer is malformed: not an issue")
	}
	return ans, nil
}

// status is the answer's status (key, name, id, category), false for an
// answer with no status of a name.
func (a statusAnswer) status(asked string) (IssueStatus, bool) {
	var st struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Category struct {
			Key string `json:"key"`
		} `json:"statusCategory"`
	}
	if !decode(a.Fields["status"], &st) || statusName(st.Name) == "" {
		return IssueStatus{}, false
	}
	s := IssueStatus{Key: asked, Name: statusName(st.Name), StatusID: oneLine(st.ID, 64)}
	if keyPattern.MatchString(a.Key) { // an issue moved to another project answers with its new key
		s.Key = a.Key
	}
	if categoryPattern.MatchString(st.Category.Key) {
		s.Category = st.Category.Key
	}
	return s, true
}

// sameStatus reports whether s is prior's status: by its id when both have
// one, else by its name, case aside.
func sameStatus(s, prior IssueStatus) bool {
	if s.StatusID != "" && prior.StatusID != "" {
		return s.StatusID == prior.StatusID
	}
	return strings.EqualFold(s.Name, prior.Name)
}

// since searches the changelog of ans for the latest move into s's status:
// its time and its author's accountId ("" for none), and whether the
// search ended (the whole changelog read, or the pages up to their bound);
// a page that fails ends it unsearched, with a PartError.
func (c *Client) since(ctx context.Context, key string, ans statusAnswer, s IssueStatus) (time.Time, string, bool, error) {
	into := func(h history) bool { return h.into(s.StatusID, s.Name) }
	hs := ans.Changelog.Histories
	held, heldBy, heldFound := latest(hs, into)
	// The entries the answer did not hold, as the paged changelog numbers
	// them (oldest first): before the answer's when it gave the newest
	// first, after them when it gave the oldest first, when a move among
	// its own entries is not the latest until the newer ones are read.
	lo, hi := 0, ans.Changelog.Total-len(hs)
	oldest := oldestFirst(hs)
	if oldest {
		lo, hi = len(hs), ans.Changelog.Total
	}
	if heldFound && (!oldest || hi <= lo) {
		return held, heldBy, true, nil
	}
	for page := 0; page < maxChangelogPages && hi > lo; page++ {
		start := max(hi-changelogPage, lo)
		vals, err := c.changelog(ctx, key, start, hi-start)
		if err != nil {
			return time.Time{}, "", false, &PartError{Part: PartChangelog, Err: err}
		}
		if at, by, found := latest(vals, into); found {
			return at, by, true, nil
		}
		hi = start
	}
	switch {
	case hi > lo: // the pages ran out first: the move is older than them
		return time.Time{}, "", true, nil
	case heldFound: // the answer's own entries came oldest first, and none newer moved the status
		return held, heldBy, true, nil
	}
	var created string
	if decode(ans.Fields["created"], &created) { // the whole changelog read: the status never moved
		return jiraTime(created), "", true, nil
	}
	return time.Time{}, "", true, nil
}

// assigneeOf is the accountId of ans's assignee, "" for none.
func assigneeOf(ans statusAnswer) string {
	var a struct {
		AccountID string `json:"accountId"`
	}
	if !decode(ans.Fields["assignee"], &a) {
		return ""
	}
	return a.AccountID
}

// assignee fills s's assignee from ans: whether it has one, and whether
// it is you (me, the read's one answer of your account); a failed read of
// your account leaves it not known to be you, with a PartError, unless
// perr already holds one.
func (c *Client) assignee(ans statusAnswer, s IssueStatus, perr error, me meRead) (IssueStatus, error) {
	a := assigneeOf(ans)
	if a == "" {
		return s, perr
	}
	s.Assigned = true
	if me.err != nil {
		if perr == nil {
			perr = &PartError{Part: PartAccount, Err: me.err}
		}
		return s, perr
	}
	s.AssigneeYou = a == me.id
	return s, perr
}

// changelog reads n entries of key's paged changelog from start on.
func (c *Client) changelog(ctx context.Context, key string, start, n int) ([]json.RawMessage, error) {
	q := url.Values{"startAt": {strconv.Itoa(start)}, "maxResults": {strconv.Itoa(n)}}
	body, err := c.get(ctx, "/rest/api/2/issue/"+key+"/changelog?"+q.Encode(), "jira changelog")
	if err != nil {
		return nil, err
	}
	var page struct {
		Values []json.RawMessage `json:"values"`
	}
	if err := json.Unmarshal(body, &page); err != nil || page.Values == nil {
		return nil, failed(ClassMalformed, 0, "Jira's answer is malformed: not a changelog")
	}
	return page.Values, nil
}

// account is your account's id, the accountId of GET /rest/api/2/myself,
// read once: the client keeps the first answer that holds one.
func (c *Client) account(ctx context.Context) (string, error) {
	c.meMu.Lock()
	defer c.meMu.Unlock()
	if c.me != "" {
		return c.me, nil
	}
	body, err := c.get(ctx, "/rest/api/2/myself", "jira myself")
	if err != nil {
		return "", err
	}
	var me struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(body, &me); err != nil || !accountPattern.MatchString(me.AccountID) {
		return "", failed(ClassMalformed, 0, "Jira's answer is malformed: no account id")
	}
	c.me = me.AccountID
	return c.me, nil
}

// history is one entry of an issue's changelog: when it was made, and
// what it changed.
type history struct {
	Created string `json:"created"`
	Author  struct {
		AccountID string `json:"accountId"`
	} `json:"author"`
	Items []struct {
		Field    string `json:"field"`
		FieldID  string `json:"fieldId"`
		To       string `json:"to"`
		ToString string `json:"toString"`
	} `json:"items"`
}

// into reports whether h moved the issue into the status of id and name:
// by the id when both have one, else by the name, case aside.
func (h history) into(id, name string) bool {
	for _, it := range h.Items {
		if it.Field != "status" && it.FieldID != "status" {
			continue
		}
		if id != "" && it.To != "" {
			if it.To == id {
				return true
			}
		} else if strings.EqualFold(statusName(it.ToString), name) {
			return true
		}
	}
	return false
}

// latest is the time and the author's accountId of the newest entry of
// raws that match keeps (its order aside: the issue's answer gives the
// newest first, a page the oldest), and whether there is one; an entry of
// another shape, or whose time does not parse, counts none.
func latest(raws []json.RawMessage, match func(history) bool) (time.Time, string, bool) {
	var at time.Time
	var by string
	found := false
	for _, raw := range raws {
		var h history
		if !decode(raw, &h) || !match(h) {
			continue
		}
		if t := jiraTime(h.Created); !t.IsZero() && (!found || t.After(at)) {
			at, by, found = t, h.Author.AccountID, true
		}
	}
	return at, by, found
}

// oldestFirst reports whether the entries raws come oldest first: the
// first one whose time parses is before the last one.
func oldestFirst(raws []json.RawMessage) bool {
	var times []time.Time
	for _, raw := range raws {
		var h history
		if decode(raw, &h) {
			if t := jiraTime(h.Created); !t.IsZero() {
				times = append(times, t)
			}
		}
	}
	return len(times) > 1 && times[0].Before(times[len(times)-1])
}

// jiraTime parses a time as Jira writes one ("2026-11-05T14:30:00.000+0200",
// or RFC 3339), in UTC; zero when it does not parse.
func jiraTime(s string) time.Time {
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05-0700", time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// statusName is a status's name as Status keeps it: its format characters
// (the bidi overrides and isolates, zero-width spaces) dropped, on one
// line, cut at MaxStatusName bytes on a character, trimmed.
func statusName(s string) string { return oneLine(s, MaxStatusName) }

// oneLine is s without format characters, on one line, cut at n bytes on
// a character, trimmed.
func oneLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(cut(line(s), n))
}
