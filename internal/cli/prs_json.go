package cli

// The JSON shape of `magnum prs --json`. It is its own set of types, not tags
// on the board's (tui.PRBoardRow and what it holds): the screen's types are
// free to change with the screen, and the output is a contract. Like the other
// --json outputs its keys are snake_case; a time is RFC 3339 and left out
// while unset (never 0001-01-01), a duration is whole seconds under a key
// ending _seconds, an optional part is null and a list is [] when empty, so a
// consumer never has to test for a missing key. TestPRsJSONMirrorsEveryBoardRowField
// fails when a field is added on one side only.

import (
	"time"

	"github.com/zhuravel/magnum/internal/tui"
)

// prsJSONRow is one PR of `magnum prs --json`: tui.PRBoardRow's fields, in its order.
type prsJSONRow struct {
	Ref              string            `json:"ref"` // owner/name#N
	Owner            string            `json:"owner"`
	Repo             string            `json:"repo"` // the name without the owner
	Number           int               `json:"number"`
	Title            string            `json:"title"`
	Author           string            `json:"author"`
	URL              string            `json:"url"`
	Issue            string            `json:"issue"`     // the issue key of the title a [board] trackers template knows; "" when none
	IssueURL         string            `json:"issue_url"` // its page
	Draft            bool              `json:"draft"`
	Labels           []string          `json:"labels"`
	Assignees        []string          `json:"assignees"`
	State            string            `json:"state"` // magnum's state: queued, reviewing, reviewed, ..., ineligible (skipped), ignored
	SkipReason       string            `json:"skip_reason"`
	Badges           []prsJSONBadge    `json:"badges"`
	GHState          string            `json:"gh_state"` // GitHub's: OPEN, CLOSED, MERGED
	UpdatedAt        time.Time         `json:"updated_at,omitzero"`
	HeadSHA          string            `json:"head_sha"`
	LastReview       *prsJSONReview    `json:"last_review"`
	Findings         *prsJSONFindings  `json:"findings"`
	CI               *prsJSONCI        `json:"ci"`
	Reviewers        []prsJSONReviewer `json:"reviewers"`
	SinceReview      *prsJSONDelta     `json:"since_review"`
	Slot             string            `json:"slot"`
	Pinned           bool              `json:"pinned"`
	Muted            bool              `json:"muted"`
	Notes            bool              `json:"notes"`
	NextEligibleAt   time.Time         `json:"next_eligible_at,omitzero"`
	LastError        string            `json:"last_error"`
	ErrorFix         string            `json:"error_fix"`
	ErrorDetail      []string          `json:"error_detail"`
	RoundsToday      int               `json:"rounds_today"`
	LastRound        *prsJSONRound     `json:"last_round"`
	RoundWhy         *prsJSONRoundWhy  `json:"round_why"` // which roles the last round ran and why
	Spend            *prsJSONSpend     `json:"spend"`     // agent time over the last 7 days
	Wait             string            `json:"wait"`
	WaitDetail       string            `json:"wait_detail"`
	DeltaCheck       bool              `json:"delta_check"` // the round it waits for is a delta check
	Note             string            `json:"note"`
	RequestedToMe    *prsJSONRequest   `json:"requested_to_me"`
	LastRequest      *prsJSONRequest   `json:"last_request"`
	Requests         []prsJSONRequest  `json:"requests"`
	ClosedAt         time.Time         `json:"closed_at,omitzero"` // when GitHub merged the PR, else closed it
	Recent           bool              `json:"recent"`             // merged or closed within [board] recent_closed
	MergedUnreviewed bool              `json:"merged_unreviewed"`
	FlagDismissed    bool              `json:"flag_dismissed"`
}

type prsJSONBadge struct {
	Label string `json:"label"`
	Text  string `json:"text"`
	Color string `json:"color"`
}

type prsJSONReview struct {
	Login       string    `json:"login"`
	Event       string    `json:"event"` // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED
	SubmittedAt time.Time `json:"submitted_at,omitzero"`
	CommitSHA   string    `json:"commit_sha"`
	Stale       bool      `json:"stale"`
	Mine        bool      `json:"mine"`
}

type prsJSONFindings struct {
	Counts          [4]int `json:"counts"` // P0, P1, P2, P3 posted
	Simplifications int    `json:"simplifications"`
	Fixed           int    `json:"fixed"`
	Open            int    `json:"open"`
	Answered        int    `json:"answered"`
	Verdict         string `json:"verdict"` // blocking, non_blocking, clean
	Posted          string `json:"posted"`  // APPROVE, REQUEST_CHANGES, COMMENT
	SHA             string `json:"sha"`
}

type prsJSONCI struct {
	State          string            `json:"state"` // passed, failed, pending, skipped, none
	Total          int               `json:"total"`
	Passed         int               `json:"passed"`
	Failed         int               `json:"failed"`
	Pending        int               `json:"pending"`
	Skipped        int               `json:"skipped"`
	Failing        []string          `json:"failing"`
	Workflows      []prsJSONWorkflow `json:"workflows"`
	Required       []prsJSONCheck    `json:"required"`
	RequiredSource string            `json:"required_source"` // github or config
	Stale          bool              `json:"stale"`
}

type prsJSONWorkflow struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Passed  int    `json:"passed"`
	Failed  int    `json:"failed"`
	Pending int    `json:"pending"`
	Total   int    `json:"total"`
}

type prsJSONCheck struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	State string `json:"state"` // passed, failed, pending, skipped, missing
	Done  int    `json:"done"`
	Total int    `json:"total"`
}

type prsJSONReviewer struct {
	Login       string    `json:"login"`
	Verdict     string    `json:"verdict"` // approved, changes_requested, commented, dismissed, pending
	SubmittedAt time.Time `json:"submitted_at,omitzero"`
	CommitSHA   string    `json:"commit_sha"`
	Stale       bool      `json:"stale"`
	Requested   bool      `json:"requested"`
	Mine        bool      `json:"mine"`
}

type prsJSONDelta struct {
	Base      string `json:"base"` // reviewed, or base branch
	BaseSHA   string `json:"base_sha"`
	Commits   int    `json:"commits"`
	Files     int    `json:"files"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Truncated bool   `json:"truncated"` // the counts are lower bounds
	// MergedBase: the base branch a merge brought in, left out of the counts
	// (the PR's own); RawBase: the one the counts include (raw).
	MergedBase string `json:"merged_base"`
	RawBase    string `json:"raw_base"`
}

type prsJSONRound struct {
	Round        int            `json:"round"`
	Kind         string         `json:"kind"`
	Stages       []prsJSONStage `json:"stages"`
	TotalSeconds int64          `json:"total_seconds"`
	Running      bool           `json:"running"`
}

type prsJSONStage struct {
	Name            string `json:"name"`
	DurationSeconds int64  `json:"duration_seconds"`
	Running         bool   `json:"running"`
	Failed          bool   `json:"failed"`
}

type prsJSONRoundWhy struct {
	Kind       string             `json:"kind"`
	PostMerge  bool               `json:"post_merge"`
	DeltaCheck bool               `json:"delta_check"` // the judge alone on a small delta
	DeltaLines int                `json:"delta_lines"`
	Roles      []string           `json:"roles"`
	Requested  []string           `json:"requested"`
	Reruns     []prsJSONRoleRerun `json:"reruns"`
	Triaged    bool               `json:"triaged"`
	Skipped    []string           `json:"skipped"`
	Reason     string             `json:"reason"`
	EveryRole  string             `json:"every_role"` // why triage kept every role
}

type prsJSONRoleRerun struct {
	Role  string `json:"role"`
	Lines int    `json:"lines"` // code lines changed since its last run
}

type prsJSONSpend struct {
	WindowSeconds int64 `json:"window_seconds"`
	AgentSeconds  int64 `json:"agent_seconds"`
	Rounds        int   `json:"rounds"`
}

type prsJSONRequest struct {
	To   string    `json:"to"` // a login, or team:<slug>
	By   string    `json:"by"`
	At   time.Time `json:"at,omitzero"`
	Mine bool      `json:"mine"`
}

// prsJSONRows is the output of `prs --json` for rows: never nil, so no rows
// print as [].
func prsJSONRows(rows []tui.PRBoardRow) []prsJSONRow {
	out := make([]prsJSONRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, prsJSONOf(r))
	}
	return out
}

func prsJSONOf(r tui.PRBoardRow) prsJSONRow {
	return prsJSONRow{
		Ref: r.Ref, Owner: r.Owner, Repo: r.Repo, Number: r.Number, Title: r.Title, Author: r.Author, URL: r.URL,
		Issue: r.Issue, IssueURL: r.IssueURL, Draft: r.Draft, Labels: listOf(r.Labels), Assignees: listOf(r.Assignees),
		State: r.State, SkipReason: r.SkipReason, Badges: mapList(r.Badges, prsJSONBadgeOf), GHState: r.GHState,
		UpdatedAt: r.UpdatedAt, HeadSHA: r.HeadSHA, LastReview: mapPtr(r.LastReview, prsJSONReviewOf),
		Findings: mapPtr(r.Findings, prsJSONFindingsOf), CI: mapPtr(r.CI, prsJSONCIOf),
		Reviewers: mapList(r.Reviewers, prsJSONReviewerOf), SinceReview: mapPtr(r.SinceReview, prsJSONDeltaOf),
		Slot: r.Slot, Pinned: r.Pinned, Muted: r.Muted, Notes: r.Notes, NextEligibleAt: r.NextEligibleAt,
		LastError: r.LastError, ErrorFix: r.ErrorFix, ErrorDetail: listOf(r.ErrorDetail), RoundsToday: r.RoundsToday,
		LastRound: mapPtr(r.LastRound, prsJSONRoundOf), RoundWhy: mapPtr(r.RoundWhy, prsJSONRoundWhyOf),
		Spend: mapPtr(r.Spend, prsJSONSpendOf), Wait: r.Wait, WaitDetail: r.WaitDetail, DeltaCheck: r.DeltaCheck, Note: r.Note,
		RequestedToMe: mapPtr(r.RequestedToMe, prsJSONRequestOf), LastRequest: mapPtr(r.LastRequest, prsJSONRequestOf),
		Requests: mapList(r.Requests, prsJSONRequestOf), ClosedAt: r.ClosedAt, Recent: r.Recent, MergedUnreviewed: r.MergedUnreviewed, FlagDismissed: r.FlagDismissed,
	}
}

func prsJSONRoundWhyOf(w tui.RoundWhy) prsJSONRoundWhy {
	return prsJSONRoundWhy{Kind: w.Kind, PostMerge: w.PostMerge, DeltaCheck: w.DeltaCheck, DeltaLines: w.DeltaLines,
		Roles: listOf(w.Roles), Requested: listOf(w.Requested),
		Reruns:  mapList(w.Reruns, func(r tui.RoleRerun) prsJSONRoleRerun { return prsJSONRoleRerun{Role: r.Role, Lines: r.Lines} }),
		Triaged: w.Triaged, Skipped: listOf(w.Skipped), Reason: w.Reason, EveryRole: w.EveryRole}
}

func prsJSONSpendOf(s tui.SpendInfo) prsJSONSpend {
	return prsJSONSpend{WindowSeconds: int64(s.Window / time.Second), AgentSeconds: int64(s.AgentTime / time.Second), Rounds: s.Rounds}
}

func prsJSONBadgeOf(b tui.Badge) prsJSONBadge {
	return prsJSONBadge{Label: b.Label, Text: b.Text, Color: b.Color}
}

func prsJSONReviewOf(r tui.ReviewInfo) prsJSONReview {
	return prsJSONReview{Login: r.Login, Event: r.Event, SubmittedAt: r.SubmittedAt, CommitSHA: r.CommitSHA, Stale: r.Stale, Mine: r.Mine}
}

func prsJSONFindingsOf(f tui.FindingsInfo) prsJSONFindings {
	return prsJSONFindings{Counts: f.Counts, Simplifications: f.Simplifications, Fixed: f.Fixed, Open: f.Open,
		Answered: f.Answered, Verdict: f.Verdict, Posted: f.Posted, SHA: f.SHA}
}

func prsJSONCIOf(c tui.CIInfo) prsJSONCI {
	return prsJSONCI{State: c.State, Total: c.Total, Passed: c.Passed, Failed: c.Failed, Pending: c.Pending, Skipped: c.Skipped,
		Failing: listOf(c.Failing), Workflows: mapList(c.Workflows, prsJSONWorkflowOf), Required: mapList(c.Required, prsJSONCheckOf),
		RequiredSource: c.RequiredSource, Stale: c.Stale}
}

func prsJSONWorkflowOf(w tui.WorkflowCI) prsJSONWorkflow {
	return prsJSONWorkflow{Name: w.Name, State: w.State, Passed: w.Passed, Failed: w.Failed, Pending: w.Pending, Total: w.Total}
}

func prsJSONCheckOf(c tui.CheckState) prsJSONCheck {
	return prsJSONCheck{Name: c.Name, Label: c.Label, State: c.State, Done: c.Done, Total: c.Total}
}

func prsJSONReviewerOf(r tui.ReviewerInfo) prsJSONReviewer {
	return prsJSONReviewer{Login: r.Login, Verdict: r.Verdict, SubmittedAt: r.SubmittedAt, CommitSHA: r.CommitSHA,
		Stale: r.Stale, Requested: r.Requested, Mine: r.Mine}
}

func prsJSONDeltaOf(d tui.ReviewDelta) prsJSONDelta {
	return prsJSONDelta{Base: d.Base, BaseSHA: d.BaseSHA, Commits: d.Commits, Files: d.Files, Additions: d.Additions,
		Deletions: d.Deletions, Truncated: d.Truncated, MergedBase: d.MergedBase, RawBase: d.RawBase}
}

func prsJSONRoundOf(r tui.RoundTimings) prsJSONRound {
	return prsJSONRound{Round: r.Round, Kind: r.Kind, Stages: mapList(r.Stages, prsJSONStageOf), TotalSeconds: wholeSeconds(r.Total), Running: r.Running}
}

func prsJSONStageOf(s tui.StageTiming) prsJSONStage {
	return prsJSONStage{Name: s.Name, DurationSeconds: wholeSeconds(s.Duration), Running: s.Running, Failed: s.Failed}
}

func prsJSONRequestOf(r tui.RequestInfo) prsJSONRequest {
	return prsJSONRequest{To: r.To, By: r.By, At: r.At, Mine: r.Mine}
}

// wholeSeconds is d in seconds, rounded, and never negative.
func wholeSeconds(d time.Duration) int64 { return int64(max(d, 0).Round(time.Second) / time.Second) }

// listOf is s, or an empty (not nil) list, so it prints as [].
func listOf[E any](s []E) []E {
	if s == nil {
		return []E{}
	}
	return s
}

// mapList maps every element of in; an empty list prints as [].
func mapList[A, B any](in []A, f func(A) B) []B {
	out := make([]B, 0, len(in))
	for _, a := range in {
		out = append(out, f(a))
	}
	return out
}

// mapPtr maps *p, or is nil (null) for nil.
func mapPtr[A, B any](p *A, f func(A) B) *B {
	if p == nil {
		return nil
	}
	v := f(*p)
	return &v
}
