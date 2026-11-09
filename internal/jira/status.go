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
)

// MaxStatusName is the cut of a status's name, in bytes.
const MaxStatusName = 64

// The bounds of the changelog Status reads past the issue's own answer,
// which holds only its newest entries (Jira cuts it at 100): the pages of
// the paged changelog, each of at most changelogPage entries (the
// endpoint's own limit).
const (
	maxChangelogPages = 5
	changelogPage     = 100
)

// IssueStatus is where an issue stands in Jira: its key, its status's name
// (on one line, cut at MaxStatusName bytes) and Jira's category of it
// (statusCategory's key: new, indeterminate or done; "" for a word that is
// not a lowercase one), since when it holds it, and whether it has an
// assignee and that assignee is you (the account of the client's
// credentials). No person's name or email is kept.
type IssueStatus struct {
	Key, Name, Category string
	// Since is when the issue moved into its status: the latest entry of
	// its changelog that did, else its creation when no entry moved its
	// status; zero when the move is older than the pages Status reads, or
	// Jira gave no time Status can parse.
	Since time.Time
	// AssigneeYou says the issue is assigned to you, Assigned that it is
	// assigned to anyone.
	AssigneeYou bool
	Assigned    bool
}

// categoryPattern is a status category's key as Status keeps it.
var categoryPattern = regexp.MustCompile(`^[a-z]{1,32}$`)

// accountPattern is an Atlassian accountId ("5b10ac8d82e05b22cc7d4ef5",
// "557058:f58131cb-b67d-43c7-b30d-6b58d40bd077").
var accountPattern = regexp.MustCompile(`^[A-Za-z0-9:_-]{1,128}$`)

// Status reads where issue key ("PS-123") stands: one GET of
// /rest/api/2/issue/<KEY>?fields=status,assignee,created&expand=changelog,
// then, when the changelog's newest entries hold no move into the status
// and the changelog has older ones, its paged changelog
// (/rest/api/2/issue/<KEY>/changelog, oldest first) back from the newest
// missing entry, at most maxChangelogPages pages; and, for an assigned
// issue, your account's id (GET /rest/api/2/myself) the first time only:
// the client keeps it. The credentials go to curl as Ticket's do, and a
// failure is an *Error of a class, as Ticket's is.
func (c *Client) Status(ctx context.Context, key string) (IssueStatus, error) {
	if !keyPattern.MatchString(key) {
		return IssueStatus{}, fmt.Errorf("%q is not a Jira issue key", key)
	}
	body, err := c.get(ctx, "/rest/api/2/issue/"+key+"?fields=status,assignee,created&expand=changelog", "jira status")
	if err != nil {
		return IssueStatus{}, err
	}
	var ans struct {
		Key       string                     `json:"key"`
		Fields    map[string]json.RawMessage `json:"fields"`
		Changelog *struct {
			Total     int               `json:"total"`
			Histories []json.RawMessage `json:"histories"`
		} `json:"changelog"`
	}
	if err := json.Unmarshal(body, &ans); err != nil || ans.Fields == nil {
		return IssueStatus{}, failed(ClassMalformed, 0, "Jira's answer is malformed: not an issue")
	}
	var status struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Category struct {
			Key string `json:"key"`
		} `json:"statusCategory"`
	}
	if !decode(ans.Fields["status"], &status) || statusName(status.Name) == "" {
		return IssueStatus{}, failed(ClassMalformed, 0, "Jira's answer is malformed: an issue without a status")
	}
	if ans.Changelog == nil {
		return IssueStatus{}, failed(ClassMalformed, 0, "Jira's answer is malformed: an issue without its changelog")
	}
	s := IssueStatus{Key: key, Name: statusName(status.Name)}
	if keyPattern.MatchString(ans.Key) { // an issue moved to another project answers with its new key
		s.Key = ans.Key
	}
	if categoryPattern.MatchString(status.Category.Key) {
		s.Category = status.Category.Key
	}
	into := func(h history) bool { return h.into(status.ID, s.Name) }
	since, found := latest(ans.Changelog.Histories, into)
	if !found {
		// The issue's answer holds the newest entries: the missing ones are
		// the paged changelog's first total-len(histories), oldest first.
		end := ans.Changelog.Total - len(ans.Changelog.Histories)
		for page := 0; page < maxChangelogPages && end > 0 && !found; page++ {
			start := max(end-changelogPage, 0)
			vals, err := c.changelog(ctx, key, start, end-start)
			if err != nil {
				return IssueStatus{}, err
			}
			since, found = latest(vals, into)
			end = start
		}
		if !found && end <= 0 { // the whole changelog read: the status never moved
			var created string
			if decode(ans.Fields["created"], &created) {
				since = jiraTime(created)
			}
		}
	}
	s.Since = since
	var assignee struct {
		AccountID string `json:"accountId"`
	}
	if decode(ans.Fields["assignee"], &assignee) && assignee.AccountID != "" {
		s.Assigned = true
		me, err := c.account(ctx)
		if err != nil {
			return IssueStatus{}, err
		}
		s.AssigneeYou = assignee.AccountID == me
	}
	return s, nil
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
	Items   []struct {
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

// latest is the time of the newest entry of raws that match keeps (its
// order aside: the issue's answer gives the newest first, a page the
// oldest), and whether there is one; an entry of another shape, or whose
// time does not parse, counts none.
func latest(raws []json.RawMessage, match func(history) bool) (time.Time, bool) {
	var at time.Time
	found := false
	for _, raw := range raws {
		var h history
		if !decode(raw, &h) || !match(h) {
			continue
		}
		if t := jiraTime(h.Created); !t.IsZero() && (!found || t.After(at)) {
			at, found = t, true
		}
	}
	return at, found
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

// statusName is a status's name as Status keeps it: on one line, cut at
// MaxStatusName bytes on a character, trimmed.
func statusName(s string) string {
	return strings.TrimSpace(cut(line(s), MaxStatusName))
}
