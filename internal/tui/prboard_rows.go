package tui

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// FindingsInfo is what magnum's latest posted review of a PR concluded.
type FindingsInfo struct {
	Counts          [4]int // P0..P3 findings posted
	Simplifications int    // optional simplification suggestions posted
	Fixed, Open     int    // earlier findings fixed / still open (a re-review)
	Answered        int    // earlier findings answered with a reason
	// Verdict is the review's decision whatever its repository lets it
	// post: blocking (request changes), non_blocking (comment) or clean
	// (approve).
	Verdict string
	Posted  string // the event it posted: APPROVE, REQUEST_CHANGES or COMMENT
	SHA     string // the reviewed head
}

// CIInfo is the head commit's CI as the board shows it.
type CIInfo struct {
	// State is "passed" | "failed" | "pending" | "skipped" (every check
	// skipped: none ran) | "none" (no checks).
	State                                   string
	Total, Passed, Failed, Pending, Skipped int
	// Failing names the failed checks, "workflow / job" when the workflow is known.
	Failing []string
	// Workflows summarises the checks per GitHub Actions workflow ("" = checks outside a workflow).
	Workflows []WorkflowCI
	// Required are the repository's required checks with their state: Name
	// as GitHub or [[repo]] required_checks names it ("Completion",
	// "workflow:CI"), State "passed" | "failed" | "pending" | "skipped" |
	// "missing" (never ran on this head).
	Required []CheckState
	// RequiredSource says who requires them: "github" (the repository's
	// rulesets) or "config" ([[repo]] required_checks overrides them).
	RequiredSource string
	Stale          bool // describes an older commit than the PR's head
}

// WorkflowCI is one workflow's checks on the head commit.
type WorkflowCI struct {
	Name, State                    string
	Passed, Failed, Pending, Total int
}

// CheckState is one required check and its state: Name as configured
// ("ci / *", "workflow:CI"), Label the short name the board's cell shows
// ("" = Name), Total the checks it matched on the head and Done those that
// finished (Total 0: none, or the head's are not known yet).
type CheckState struct {
	Name, Label, State string
	Done, Total        int
}

// Badge marks a PR that carries a GitHub label: Text (e.g. "🚩") shows
// before the title, Label names it on the card.
type Badge struct {
	Label, Text string
	// Color is one of red, green, yellow, blue, magenta, cyan or gray ("" =
	// the terminal's; an emoji keeps its own colors).
	Color string
}

// PRBoardRow is one pull request on the PR board. Ref is what actions
// receive; Owner, Repo and Number label the row (Ref is parsed when they
// are empty).
type PRBoardRow struct {
	Ref, Owner, Repo   string
	Number             int
	Title, Author, URL string
	// Issue is the first issue key in the title a [board] trackers template
	// knows ("PS-38553"), IssueURL its page; "" when the title names none.
	Issue, IssueURL   string
	Draft             bool
	Labels, Assignees []string
	// State is magnum's state: baseline, queued, reviewing, reviewed,
	// rereview_pending, needs_attention, paused, closed, released,
	// ineligible (the configuration skips it; the board says "skipped") or
	// ignored (magnum ignore).
	State string
	// SkipReason says why the configuration skips an ineligible PR ("bot
	// author", `author "x" is in skip_authors`, ...).
	SkipReason string
	// Badges are the PR's labels that [board] badges marks, in its order.
	Badges  []Badge
	GHState string // GitHub's state: OPEN, CLOSED, MERGED
	// ActivityAt is the PR's last activity, which the UPDATED column, the
	// updated sort and the card show: a push, a comment, a review, a label,
	// a review request, a draft change, a rename, a description edit, a base
	// change, a close, reopen or merge; GitHubUpdatedAt until magnum read it.
	ActivityAt time.Time
	// GitHubUpdatedAt is GitHub's updatedAt, which also moves for what a
	// reviewer never sees (a project field, a resolved thread); prs --json
	// only.
	GitHubUpdatedAt time.Time
	HeadSHA         string
	LastReview      *ReviewInfo // the latest review magnum knows of; nil when none
	// Findings is what magnum's latest posted review concluded (its findings
	// by priority, simplifications and verdict), also where it could only
	// comment; nil when magnum has not reviewed the PR.
	Findings *FindingsInfo
	// CI is the head commit's checks; nil when unknown.
	CI             *CIInfo
	Reviewers      []ReviewerInfo // everyone who reviewed or was asked to
	SinceReview    *ReviewDelta   // what changed since the last review; nil when unknown
	Slot           string         // folder of the review slot holding the PR, if any
	Pinned, Muted  bool
	Notes          bool      // the repository has reviewer notes (magnum notes)
	NextEligibleAt time.Time // earliest next automatic review; zero when not scheduled
	// LastError is the one-line explanation of the PR's stored error (for a
	// PR in needs_attention, why it needs you); ErrorFix the next step and
	// ErrorDetail the end of the failing command's output.
	LastError   string
	ErrorFix    string
	ErrorDetail []string
	RoundsToday int
	LastRound   *RoundTimings // the stages of the last review round; nil when none ran
	// RoundWhy says which roles the last round ran and why; nil when unknown.
	RoundWhy *RoundWhy
	// Progress is the round in flight: its start and its roles, which the
	// state cell turns into the stage and the time ("simplify · 17m") and
	// the card into a timeline; nil when no round runs or it is unknown.
	Progress *RoundProgress
	// Spend is the agent time the PR's runs took over the last 7 days and
	// how many rounds they ran in; nil when none ran.
	Spend *SpendInfo

	// Wait and WaitDetail say why a PR waiting for a round has none yet (the
	// daemon's account): the compact form the state cell shows ("re-review
	// · quiet → 14:09") and the sentence with the command that lifts it,
	// which the card shows. "" when the PR does not wait or no daemon said.
	Wait, WaitDetail string
	// DeltaCheck: the round the PR waits for is a delta check (the judge
	// alone on a small delta); the state cell says so, as it does for a
	// round in flight whose RoundWhy is one.
	DeltaCheck bool
	// Note is a one-line remark about the last review shown under LAST REVIEW
	// on the card (e.g. "comment-only push skipped (a7b3f8c → 602da9d)").
	Note string

	// RequestedToMe is the latest review request that asked one of the self
	// logins; LastRequest the latest one whoever it asked; Requests the
	// latest request to each reviewer, newest first. Nil or empty when the
	// registry knows none (only the newest ten requests of a PR are kept).
	RequestedToMe, LastRequest *RequestInfo
	Requests                   []RequestInfo

	// ClosedAt is when GitHub merged the PR, else closed it; zero while it
	// is open. Recent marks a PR GitHub merged or closed within [board]
	// recent_closed: the board lists it in a section after the open PRs.
	ClosedAt time.Time
	Recent   bool
	// MergedUnreviewed: GitHub merged the PR before magnum reviewed its last
	// push (store.IsMergedUnreviewed); LastReview.CommitSHA is the commit
	// magnum reviewed last, if any.
	MergedUnreviewed bool
	// FlagDismissed: the PR is muted without being flagged though it would
	// be flagged unmuted (store.IsFlagDismissed): a mute dismissed the
	// merged-unreviewed flag, and M restores it.
	FlagDismissed bool
}

// RequestInfo is a review request: who was asked, by whom and when.
type RequestInfo struct {
	To   string // the reviewer: a login (a bot's keeps "[bot]") or "team:<slug>"
	By   string // who asked; "" when unknown (a deleted account)
	At   time.Time
	Mine bool // To is one of the self logins
}

// ReviewInfo is the latest review on a PR.
type ReviewInfo struct {
	Login, Event string // Event: APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED (any case)
	SubmittedAt  time.Time
	CommitSHA    string
	Stale        bool // the head moved since
	Mine         bool // Login is one of the self logins
}

// ReviewerInfo is one reviewer's latest verdict on a PR.
type ReviewerInfo struct {
	Login       string
	Verdict     string // approved | changes_requested | commented | dismissed | pending
	SubmittedAt time.Time
	CommitSHA   string
	Stale       bool // the head moved since the verdict
	Requested   bool // a review is requested from them
	Mine        bool
}

// ReviewDelta is what changed on a PR since Base: "reviewed" (the last
// reviewed head) or "base branch" (no review yet: the whole PR).
type ReviewDelta struct {
	Base                                 string
	BaseSHA                              string
	Commits, Files, Additions, Deletions int
	Truncated                            bool // the counts are lower bounds
	// MergedBase is the base branch the commits since merged in (or were
	// rebased onto) when the counts leave its changes out (the PR's own);
	// Raw: they merged it, but the counts include its changes (the PR's
	// own diff could not be compared in full). RawBase names it then.
	MergedBase, RawBase string
}

// PRSort orders the board.
type PRSort string

// The board's sorts, in the order s cycles through them.
const (
	SortUpdated          PRSort = "updated"           // newest update first
	SortLastReview       PRSort = "last-review"       // latest review first
	SortReviewerActivity PRSort = "reviewer-activity" // latest verdict by anyone first
	SortRequested        PRSort = "requested"         // latest review request first
	SortChanges          PRSort = "changes"           // most lines changed since the review first
	SortState            PRSort = "state"             // most urgent state first
)

var prSortOrder = []PRSort{SortUpdated, SortLastReview, SortReviewerActivity, SortRequested, SortChanges, SortState}

// PRSorts lists the sorts in the order the s key cycles through them.
func PRSorts() []PRSort { return slices.Clone(prSortOrder) }

// ParsePRSort reads a sort name such as "updated" or "last-review" (case,
// "_" and spaces do not matter); empty means SortUpdated.
func ParsePRSort(s string) (PRSort, error) {
	n := strings.ToLower(strings.TrimSpace(s))
	n = strings.NewReplacer("_", "-", " ", "-").Replace(n)
	if n == "" {
		return SortUpdated, nil
	}
	for _, v := range prSortOrder {
		if string(v) == n {
			return v, nil
		}
	}
	names := make([]string, len(prSortOrder))
	for i, v := range prSortOrder {
		names[i] = string(v)
	}
	return "", fmt.Errorf("unknown sort %q (want %s)", s, strings.Join(names, ", "))
}

func (s PRSort) valid() bool { return slices.Contains(prSortOrder, s) }

// label names the sort in the title bar.
func (s PRSort) label() string { return strings.ReplaceAll(string(s), "-", " ") }

func (s PRSort) next() PRSort {
	i := slices.Index(prSortOrder, s)
	return prSortOrder[(i+1)%len(prSortOrder)]
}

// PRBoardSource supplies the board's rows; Rows is called once per
// refresh, never concurrently with itself.
type PRBoardSource interface {
	Rows(ctx context.Context) ([]PRBoardRow, error)
}

// PRBoardSourceFunc adapts a function to PRBoardSource.
type PRBoardSourceFunc func(ctx context.Context) ([]PRBoardRow, error)

// Rows calls f.
func (f PRBoardSourceFunc) Rows(ctx context.Context) ([]PRBoardRow, error) { return f(ctx) }

// SortPRBoard returns a copy of rows in the given order; desc puts the
// largest key first (newest update, latest review, most recent verdict,
// newest request, most lines changed, most urgent state). Rows lacking the
// key (never reviewed, no request, no delta) come last either way; ties put
// the newest update first, then order by ref. An unknown sort means
// SortUpdated. The recently closed rows (Recent) come after all the others,
// newest closed first, whatever the sort: they are the board's own section.
func SortPRBoard(rows []PRBoardRow, by PRSort, desc bool) []PRBoardRow {
	out := slices.Clone(rows)
	key := prSortKey(by)
	slices.SortStableFunc(out, func(a, b PRBoardRow) int {
		switch {
		case a.Recent != b.Recent:
			if b.Recent {
				return -1
			}
			return 1
		case a.Recent:
			if c := b.ClosedAt.Compare(a.ClosedAt); c != 0 {
				return c
			}
			return cmp.Compare(prRef(a), prRef(b))
		}
		va, oka := key(a)
		vb, okb := key(b)
		switch {
		case oka != okb:
			if oka {
				return -1
			}
			return 1
		case oka && va != vb:
			if desc {
				return cmp.Compare(vb, va)
			}
			return cmp.Compare(va, vb)
		}
		if c := b.ActivityAt.Compare(a.ActivityAt); c != 0 {
			return c
		}
		return cmp.Compare(prRef(a), prRef(b))
	})
	return out
}

// prSortKey is the value a sort compares; false when the row lacks it.
func prSortKey(by PRSort) func(PRBoardRow) (int64, bool) {
	at := func(t time.Time) (int64, bool) { return t.UnixNano(), !t.IsZero() }
	switch by {
	case SortLastReview:
		return func(r PRBoardRow) (int64, bool) {
			if r.LastReview == nil {
				return 0, false
			}
			return at(r.LastReview.SubmittedAt)
		}
	case SortReviewerActivity:
		return func(r PRBoardRow) (int64, bool) { return at(latestVerdict(r)) }
	case SortRequested:
		return func(r PRBoardRow) (int64, bool) {
			q, ok := shownRequest(r)
			if !ok {
				return 0, false
			}
			return at(q.At)
		}
	case SortChanges:
		return func(r PRBoardRow) (int64, bool) {
			d := r.SinceReview
			if d == nil {
				return 0, false
			}
			return int64(d.Additions+d.Deletions)*1_000_000 + int64(min(d.Commits, 999_999)), true
		}
	case SortState:
		return func(r PRBoardRow) (int64, bool) { return int64(stateUrgency(r.State)), true }
	}
	return func(r PRBoardRow) (int64, bool) { return at(r.ActivityAt) }
}

// shownRequest is the review request the REQUESTED column shows and the
// requested sort orders by: the latest to me, else the latest of all;
// false when the PR has none (or none with a time).
func shownRequest(r PRBoardRow) (RequestInfo, bool) {
	for _, q := range []*RequestInfo{r.RequestedToMe, r.LastRequest} {
		if q != nil && !q.At.IsZero() {
			return *q, true
		}
	}
	return RequestInfo{}, false
}

// latestVerdict is when anyone last reviewed the PR.
func latestVerdict(r PRBoardRow) time.Time {
	var t time.Time
	if r.LastReview != nil {
		t = r.LastReview.SubmittedAt
	}
	for _, v := range r.Reviewers {
		if v.SubmittedAt.After(t) {
			t = v.SubmittedAt
		}
	}
	return t
}

// prStateOrder lists magnum's states from the most urgent; the summary
// line counts them in this order.
var prStateOrder = []string{
	"needs_attention", "paused", "reviewing", "rereview_pending", "queued",
	"reviewed", "baseline", "ineligible", "ignored", "closed", "released",
}

// stateUrgency ranks a state: higher is more urgent, unknown is 0.
func stateUrgency(state string) int {
	if i := slices.Index(prStateOrder, normState(state)); i >= 0 {
		return len(prStateOrder) - i
	}
	return 0
}

func normState(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
}

// prRefParts names the row's PR, from its fields or else its Ref or URL.
func prRefParts(r PRBoardRow) (owner, repo string, number int) {
	owner, repo, number = r.Owner, r.Repo, r.Number
	if repo != "" && number > 0 {
		return owner, repo, number
	}
	for _, s := range []string{r.Ref, r.URL} {
		if p, ok := parsePickRef(s); ok {
			if owner == "" {
				owner = p.owner
			}
			if repo == "" {
				repo = p.repo
			}
			if number <= 0 {
				number = p.number
			}
		}
	}
	return owner, repo, number
}

// prRef is what actions receive: Ref, else owner/repo#N built from the
// fields.
func prRef(r PRBoardRow) string {
	if ref := strings.TrimSpace(r.Ref); ref != "" {
		return ref
	}
	owner, repo, n := prRefParts(r)
	switch {
	case owner != "" && repo != "":
		return fmt.Sprintf("%s/%s#%d", owner, repo, n)
	case repo != "":
		return fmt.Sprintf("%s#%d", repo, n)
	case n > 0:
		return fmt.Sprintf("#%d", n)
	}
	return ""
}

// prURL is the PR's web page: URL, else github.com's page when the owner
// and repository are known.
func prURL(r PRBoardRow) string {
	if r.URL != "" {
		return r.URL
	}
	if owner, repo, n := prRefParts(r); owner != "" && repo != "" && n > 0 {
		return fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, n)
	}
	return ""
}

// inRepo reports whether the row belongs to repo ("name" or "owner/name").
func inRepo(r PRBoardRow, repo string) bool {
	repo = strings.ToLower(strings.TrimSpace(repo))
	if repo == "" {
		return true
	}
	owner, name, _ := prRefParts(r)
	if strings.Contains(repo, "/") {
		return repo == strings.ToLower(owner+"/"+name)
	}
	return repo == strings.ToLower(name)
}

// isOpen reports whether the PR is still open on GitHub (an unknown
// GitHub state counts as open unless magnum closed or released it).
func isOpen(r PRBoardRow) bool {
	switch strings.ToUpper(strings.TrimSpace(r.GHState)) {
	case "", "OPEN":
		s := normState(r.State)
		return s != "closed" && s != "released"
	}
	return false
}

// mergedOnGitHub reports whether GitHub merged the PR.
func mergedOnGitHub(r PRBoardRow) bool {
	return strings.EqualFold(strings.TrimSpace(r.GHState), "MERGED")
}

// closedUnmerged reports whether GitHub closed the PR without merging it:
// there is nothing to review.
func closedUnmerged(r PRBoardRow) bool {
	return strings.EqualFold(strings.TrimSpace(r.GHState), "CLOSED")
}

// postMergeRound reports whether a post-merge review of the merged PR waits
// or runs: its state is a round's, not the closed or released of a PR magnum
// is done with.
func postMergeRound(r PRBoardRow) bool {
	if !mergedOnGitHub(r) {
		return false
	}
	switch normState(r.State) {
	case "queued", "rereview_pending", "claiming", "reviewing", "verifying", "paused":
		return true
	}
	return false
}

// shortLogin is a login without "@" and "[bot]" ("talkable[bot]" →
// "talkable"); the narrow columns mark a bot's (prbPainter.loginText).
func shortLogin(s string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "@"), "[bot]")
}

// fuzzyContains reports whether term's runes appear in s in order and
// close together: the tightest such run may be at most about twice as
// long as term, so "rfrlwdgt" finds "referral widget" but "frank" does
// not find "Referral analytics: add the campaign breakdown".
func fuzzyContains(s, term string) bool {
	if strings.Contains(s, term) {
		return true
	}
	rs, ts := []rune(s), []rune(term)
	if len(ts) == 0 {
		return true
	}
	limit := 2*len(ts) + 2
	for start := range rs {
		if rs[start] != ts[0] {
			continue
		}
		i := 1
		for j := start + 1; j < len(rs) && i < len(ts) && j-start < limit; j++ {
			if rs[j] == ts[i] {
				i++
			}
		}
		if i == len(ts) {
			return true
		}
	}
	return false
}

// scopeRows keeps the rows in repo, with their text made safe to draw:
// GitHub's strings may carry escape sequences or control characters.
func scopeRows(rows []PRBoardRow, repo string) []PRBoardRow {
	out := make([]PRBoardRow, 0, len(rows))
	for _, r := range rows {
		if inRepo(r, repo) {
			out = append(out, sanitizeRow(r))
		}
	}
	return out
}

func sanitizeRow(r PRBoardRow) PRBoardRow {
	r.Ref, r.Owner, r.Repo = cleanText(r.Ref), cleanText(r.Owner), cleanText(r.Repo)
	r.Title, r.Author, r.URL = cleanText(r.Title), cleanText(r.Author), cleanText(r.URL)
	r.Issue, r.IssueURL = cleanText(r.Issue), cleanText(r.IssueURL)
	r.State, r.GHState, r.Slot, r.LastError = cleanText(r.State), cleanText(r.GHState), cleanText(r.Slot), cleanText(r.LastError)
	r.HeadSHA = cleanText(r.HeadSHA)
	r.Wait, r.WaitDetail = cleanText(r.Wait), cleanText(r.WaitDetail)
	r.Note, r.SkipReason = cleanText(r.Note), cleanText(r.SkipReason)
	if r.Badges != nil {
		badges := make([]Badge, 0, len(r.Badges))
		for _, b := range r.Badges {
			if b.Text = cleanText(b.Text); b.Text != "" {
				badges = append(badges, Badge{Label: cleanText(b.Label), Text: b.Text, Color: cleanText(b.Color)})
			}
		}
		r.Badges = badges
	}
	r.ErrorFix, r.ErrorDetail = cleanText(r.ErrorFix), cleanAll(r.ErrorDetail)
	r.Labels, r.Assignees = cleanAll(r.Labels), cleanAll(r.Assignees)
	if r.LastReview != nil {
		li := *r.LastReview
		li.Login, li.Event, li.CommitSHA = cleanText(li.Login), cleanText(li.Event), cleanText(li.CommitSHA)
		r.LastReview = &li
	}
	if r.Reviewers != nil {
		revs := make([]ReviewerInfo, len(r.Reviewers))
		for i, v := range r.Reviewers {
			v.Login, v.Verdict, v.CommitSHA = cleanText(v.Login), cleanText(v.Verdict), cleanText(v.CommitSHA)
			revs[i] = v
		}
		r.Reviewers = revs
	}
	if r.RequestedToMe != nil {
		q := cleanRequest(*r.RequestedToMe)
		r.RequestedToMe = &q
	}
	if r.LastRequest != nil {
		q := cleanRequest(*r.LastRequest)
		r.LastRequest = &q
	}
	if r.Requests != nil {
		qs := make([]RequestInfo, len(r.Requests))
		for i, q := range r.Requests {
			qs[i] = cleanRequest(q)
		}
		r.Requests = qs
	}
	if r.SinceReview != nil {
		d := *r.SinceReview
		d.Base, d.BaseSHA = cleanText(d.Base), cleanText(d.BaseSHA)
		d.MergedBase, d.RawBase = cleanText(d.MergedBase), cleanText(d.RawBase)
		r.SinceReview = &d
	}
	if r.CI != nil {
		ci := *r.CI
		ci.State, ci.RequiredSource, ci.Failing = cleanText(ci.State), cleanText(ci.RequiredSource), cleanAll(ci.Failing)
		ci.Workflows = slices.Clone(ci.Workflows)
		for i := range ci.Workflows {
			ci.Workflows[i].Name, ci.Workflows[i].State = cleanText(ci.Workflows[i].Name), cleanText(ci.Workflows[i].State)
		}
		ci.Required = slices.Clone(ci.Required)
		for i := range ci.Required {
			r := &ci.Required[i]
			r.Name, r.Label, r.State = cleanText(r.Name), cleanText(r.Label), cleanText(r.State)
		}
		r.CI = &ci
	}
	if r.LastRound != nil {
		lr := *r.LastRound
		lr.Kind = cleanText(lr.Kind)
		lr.Stages = slices.Clone(lr.Stages)
		for i := range lr.Stages {
			lr.Stages[i].Name = cleanText(lr.Stages[i].Name)
		}
		r.LastRound = &lr
	}
	if r.RoundWhy != nil {
		w := cleanRoundWhy(*r.RoundWhy)
		r.RoundWhy = &w
	}
	if r.Progress != nil {
		g := cleanRoundProgress(*r.Progress)
		r.Progress = &g
	}
	return r
}

func cleanRequest(q RequestInfo) RequestInfo {
	q.To, q.By = cleanText(q.To), cleanText(q.By)
	return q
}

// cleanText drops escape sequences and control characters and folds
// whitespace, so the text measures and draws as what it says.
func cleanText(s string) string {
	s = ansi.Strip(s)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, s)
	return oneLine(s)
}

func cleanAll(ss []string) []string {
	if ss == nil {
		return nil
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = cleanText(s)
	}
	return out
}
