package jira

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// The bounds of a search: the most words it asks for (the first of the
// text) and the most issues it reads (the API's own page limit).
const (
	MaxSearchWords   = 8
	MaxSearchResults = 50
)

// minSearchWord is the shortest word a title search uses.
const minSearchWord = 3

// commonWords are the words a title search leaves out: Jira's text search
// ignores them, and they match every title.
var commonWords = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "but": true, "not": true, "you": true, "all": true,
	"any": true, "can": true, "has": true, "have": true, "had": true, "its": true, "our": true, "out": true,
	"was": true, "were": true, "will": true, "with": true, "from": true, "into": true, "onto": true,
	"that": true, "this": true, "these": true, "those": true, "than": true, "then": true, "there": true,
	"when": true, "where": true, "which": true, "who": true, "why": true, "how": true, "what": true,
	"also": true, "just": true, "only": true, "very": true, "some": true, "such": true, "should": true,
	"would": true, "could": true, "must": true, "may": true, "does": true, "did": true, "done": true,
	"their": true, "them": true, "they": true, "your": true, "his": true, "her": true, "him": true,
}

// Search finds the issues whose summary holds one of the first words of
// words (SearchWords; none asks Jira nothing): one GET of the enhanced
// search, /rest/api/3/search/jql, with the JQL `summary ~ "<word>" OR ...
// ORDER BY updated DESC`, at most limit issues (1 to MaxSearchResults) and
// the fields of a ticket's head. Each issue is a Ticket of its key, URL,
// summary, status, type and priority, in Jira's order; an issue without a
// valid key is left out. The credentials go to curl as Ticket's do.
func (c *Client) Search(ctx context.Context, words string, limit int) ([]Ticket, error) {
	ws := SearchWords(words)
	if len(ws) == 0 {
		return nil, nil
	}
	terms := make([]string, len(ws))
	for i, w := range ws {
		terms[i] = `summary ~ "` + w + `"`
	}
	q := url.Values{
		"jql":        {strings.Join(terms, " OR ") + " ORDER BY updated DESC"},
		"maxResults": {strconv.Itoa(min(max(limit, 1), MaxSearchResults))},
		"fields":     {"summary,status,issuetype,priority"},
	}
	body, err := c.get(ctx, "/rest/api/3/search/jql?"+q.Encode(), "jira search")
	if err != nil {
		return nil, err
	}
	var ans struct {
		Issues []struct {
			Key    string                     `json:"key"`
			Fields map[string]json.RawMessage `json:"fields"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &ans); err != nil || ans.Issues == nil {
		return nil, failed(ClassMalformed, 0, "Jira's answer is malformed: not a search result")
	}
	site := strings.TrimRight(c.URL, "/")
	out := make([]Ticket, 0, len(ans.Issues))
	for _, is := range ans.Issues {
		if !keyPattern.MatchString(is.Key) {
			continue
		}
		t := Ticket{Key: is.Key, URL: site + "/browse/" + is.Key}
		var s string
		if decode(is.Fields["summary"], &s) {
			t.Summary = line(s)
		}
		var named struct {
			Name string `json:"name"`
		}
		for id, dst := range map[string]*string{"status": &t.Status, "issuetype": &t.Type, "priority": &t.Priority} {
			named.Name = ""
			if decode(is.Fields[id], &named) {
				*dst = line(named.Name)
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// SearchWords are the words a title search asks for: the first
// MaxSearchWords distinct ones of text, lowercased, of letters and digits
// only (an apostrophe inside a word dropped, any other mark a break), at
// least minSearchWord long and not a common word. No mark of JQL or of
// Jira's text search survives, so the words need no escaping.
func SearchWords(text string) []string {
	text = strings.NewReplacer("'", "", "’", "").Replace(strings.ToLower(text))
	var out []string
	for _, w := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) < minSearchWord || commonWords[w] || slices.Contains(out, w) {
			continue
		}
		if out = append(out, w); len(out) == MaxSearchWords {
			break
		}
	}
	return out
}
