package cli

// The convergence section of `magnum stats` (DECISIONS "`magnum stats`
// counts the reviewed heads to a pass and the merges on magnum's approval
// alone"): how many reviewed heads a PR takes until magnum's review leaves
// no P0, P1 or P2 to fix, over the PRs that merged or closed in the window,
// so that young PRs, which have had no time to need five heads, do not look
// fast; and where the window's re-review findings come from.

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// statsKindRoundStart is the engine's event at the start of a round, whose
// data says whether the round is a full pass (full_pass) and of which head
// (target_sha).
const statsKindRoundStart = "engine.round_start"

// statsLongChain is the number of reviewed heads that makes a PR's chain a
// long one (the 5+ column, the open PRs' line).
const statsLongChain = 5

// Who wrote the PRs of a convergence row (statsConvergenceRow.Authors): the
// operator (config.SelfLogins, the logins autofix = "own" reads) or others.
const (
	statsOthers = "others"
	statsOwn    = "own"
)

// statsConvergence is `magnum stats --json`'s convergence.
type statsConvergence struct {
	// Rows are the PRs that merged or closed in the window with a posted
	// review, per repository and authors, then in total (no repo).
	Rows []statsConvergenceRow `json:"rows"`
	// OpenFivePlus counts the open PRs that already have statsLongChain or
	// more reviewed heads.
	OpenFivePlus statsOwnSplit `json:"open_five_plus"`
	// Rereviews splits the posted P0 to P2 findings of the window's
	// re-reviews by where they come from (store.FindingOrigin), FullPasses
	// those of the re-reviews that were full passes.
	Rereviews  statsOrigins `json:"rereview_findings"`
	FullPasses statsOrigins `json:"full_pass_findings"`
}

// statsConvergenceRow is one row of the convergence section. Heads count
// the distinct heads magnum's posted reviews reviewed, in order, until the
// first review that passes: no P0, P1 or P2 new or still open and a verdict
// that does not block.
type statsConvergenceRow struct {
	Repo     string `json:"repo,omitempty"` // owner/name; "" for all repositories
	Authors  string `json:"authors"`        // statsOthers | statsOwn
	PRs      int    `json:"prs"`
	PassAt1  int    `json:"pass_at_1"` // passed at the first reviewed head
	PassBy2  int    `json:"pass_by_2"` // at the first or the second
	FivePlus int    `json:"five_plus"` // passed at the fifth or later, or reviewed at five without passing
	Never    int    `json:"never"`     // never passed
	// MeanHeads (two decimals) and P90Heads (nearest rank) are the heads to
	// pass of the PRs that passed; 0 when none did.
	MeanHeads float64 `json:"mean_heads"`
	P90Heads  int     `json:"p90_heads"`
	// MergedOpen counts the PRs merged while their last review before the
	// merge had a P0, P1 or P2 new or still open.
	MergedOpen int `json:"merged_open"`
}

// statsOwnSplit is a count of others' PRs and of the operator's own.
type statsOwnSplit struct {
	Others int `json:"others"`
	Own    int `json:"own"`
}

// statsOrigins counts posted findings by origin; MissedShare is the missed
// earlier ones' fraction of all of them (four decimals; 0 without any).
type statsOrigins struct {
	New           int     `json:"new"`
	MissedEarlier int     `json:"missed_earlier"`
	Fallout       int     `json:"fallout"`
	MissedShare   float64 `json:"missed_earlier_share"`
}

func (o *statsOrigins) add(origin string) {
	switch origin {
	case store.OriginMissedEarlier:
		o.MissedEarlier++
	case store.OriginFallout:
		o.Fallout++
	default:
		o.New++
	}
	o.MissedShare = math.Round(10000*float64(o.MissedEarlier)/float64(o.total())) / 10000
}

func (o statsOrigins) total() int { return o.New + o.MissedEarlier + o.Fallout }

// statsPasses reports whether a review passes: it left no P0, P1 or P2,
// new or still open (an open P3 stays optional), and its verdict does not
// block; a review whose result cannot be read does not.
func statsPasses(r store.PostedReview) bool {
	return r.OK && statsProblems(r.Summary) == 0 && r.Summary.Verdict != store.VerdictBlocking
}

// statsProblems is the review's P0, P1 and P2, new and still open.
func statsProblems(s store.ReviewSummary) int {
	return s.Counts[0] + s.Counts[1] + s.Counts[2] + s.OpenProblems()
}

// statsHeadsToPass numbers reviews' distinct heads in order (a second
// review of a head already counted adds nothing) and returns how many there
// are and the number of the first whose review passes (0: none did).
func statsHeadsToPass(reviews []store.PostedReview) (heads, passAt int) {
	seen := map[string]bool{}
	for _, r := range reviews {
		if seen[r.SHA] {
			continue
		}
		seen[r.SHA] = true
		heads++
		if passAt == 0 && statsPasses(r) {
			passAt = heads
		}
	}
	return heads, passAt
}

// statsConvergenceAcc accumulates one row and its PRs' heads to pass.
type statsConvergenceAcc struct {
	row   statsConvergenceRow
	heads []int
}

// statsConvergenceOf computes the convergence section. prs are the PRs
// store.ReviewedPRs returns, findings those recorded in the window (oldest
// first), starts the engine.round_start events that tell full passes; own
// tells the operator's logins, repo ("owner/name" or "") keeps one
// repository.
func statsConvergenceOf(prs []store.ReviewedPR, findings []store.Finding, starts []store.Event, own func(string) bool,
	since time.Time, repo string) statsConvergence {
	out := statsConvergence{Rows: []statsConvergenceRow{}}
	type key struct{ repo, authors string }
	accs := map[key]*statsConvergenceAcc{}
	add := func(k key, heads, passAt int, mergedOpen bool) {
		a := accs[k]
		if a == nil {
			a = &statsConvergenceAcc{row: statsConvergenceRow{Repo: k.repo, Authors: k.authors}}
			accs[k] = a
		}
		a.row.PRs++
		switch passAt {
		case 0:
			a.row.Never++
		case 1:
			a.row.PassAt1++
		}
		if passAt > 0 && passAt <= 2 {
			a.row.PassBy2++
		}
		if passAt >= statsLongChain || (passAt == 0 && heads >= statsLongChain) {
			a.row.FivePlus++
		}
		if passAt > 0 {
			a.heads = append(a.heads, passAt)
		}
		if mergedOpen {
			a.row.MergedOpen++
		}
	}
	byID := map[int64]store.ReviewedPR{}
	for _, p := range prs {
		byID[p.ID] = p
		if repo != "" && p.Repo != repo {
			continue
		}
		authors := statsOthers
		if own != nil && own(p.Author) {
			authors = statsOwn
		}
		if p.GHState == store.GHOpen {
			if heads, _ := statsHeadsToPass(p.Reviews); heads >= statsLongChain {
				if authors == statsOwn {
					out.OpenFivePlus.Own++
				} else {
					out.OpenFivePlus.Others++
				}
			}
			continue
		}
		end := p.End()
		if end == nil || end.Before(since) || (p.GHState != store.GHMerged && p.GHState != store.GHClosed) {
			continue
		}
		// A round after the merge (a post-merge review) is no step to it.
		reviews := slices.DeleteFunc(slices.Clone(p.Reviews), func(r store.PostedReview) bool { return r.CreatedAt.After(*end) })
		if len(reviews) == 0 {
			continue
		}
		heads, passAt := statsHeadsToPass(reviews)
		last := reviews[len(reviews)-1]
		mergedOpen := p.GHState == store.GHMerged && last.OK && statsProblems(last.Summary) > 0
		add(key{p.Repo, authors}, heads, passAt, mergedOpen)
		add(key{"", authors}, heads, passAt, mergedOpen)
	}
	after := func(b bool) int { // 1 sorts after 0
		if b {
			return 1
		}
		return 0
	}
	keys := slices.SortedFunc(maps.Keys(accs), func(a, b key) int { // the repositories, others first, then the totals
		return cmp.Or(cmp.Compare(after(a.repo == ""), after(b.repo == "")), cmp.Compare(a.repo, b.repo),
			cmp.Compare(after(a.authors == statsOwn), after(b.authors == statsOwn)))
	})
	for _, k := range keys {
		a := accs[k]
		if len(a.heads) > 0 {
			sorted := slices.Sorted(slices.Values(a.heads))
			sum := 0
			for _, h := range sorted {
				sum += h
			}
			a.row.MeanHeads = math.Round(100*float64(sum)/float64(len(sorted))) / 100
			a.row.P90Heads = statsRank(sorted, 90)
		}
		out.Rows = append(out.Rows, a.row)
	}
	out.Rereviews, out.FullPasses = statsReReviewOrigins(byID, findings, starts, since, repo)
	return out
}

// statsReReviewOrigins splits the posted P0 to P2 findings recorded since
// (of repo when it is set) by origin: those of re-reviews, a round of the
// PR after its first posted review, and apart those of the re-reviews that
// were full passes (statsFullPass). A round's findings count by the last run
// that recorded them; a PR byID does not hold is left out.
func statsReReviewOrigins(byID map[int64]store.ReviewedPR, findings []store.Finding, starts []store.Event, since time.Time,
	repo string) (rereviews, fullPasses statsOrigins) {
	latest := map[statsValueKey]string{}
	for _, f := range findings { // oldest first
		latest[statsValueKey{f.PRID, f.Round}] = f.RunID
	}
	bySubject := statsStartsBySubject(starts)
	for _, f := range findings {
		k := statsValueKey{f.PRID, f.Round}
		p, ok := byID[f.PRID]
		sev := strings.ToUpper(strings.TrimSpace(f.Severity))
		if !ok || len(p.Reviews) == 0 || (repo != "" && p.Repo != repo) || f.CreatedAt.Before(since) || f.Verdict != store.FindingPosted ||
			f.RunID != latest[k] || !slices.Contains(statsSeverities[:3], sev) || f.Round <= p.Reviews[0].Round {
			continue
		}
		i := slices.IndexFunc(p.Reviews, func(r store.PostedReview) bool { return r.Round == f.Round })
		if i >= 0 && statsFullPass(bySubject["pr:"+p.Repo+"#"+strconv.Itoa(p.Number)], p.Reviews[i]) {
			fullPasses.add(store.FindingOrigin(f.Title))
		} else {
			rereviews.add(store.FindingOrigin(f.Title))
		}
	}
	return rereviews, fullPasses
}

// statsStartsBySubject groups round start events by subject, each group by
// time.
func statsStartsBySubject(starts []store.Event) map[string][]store.Event {
	out := map[string][]store.Event{}
	for _, ev := range starts {
		if ev.Kind == statsKindRoundStart && ev.Subject != nil {
			out[*ev.Subject] = append(out[*ev.Subject], ev)
		}
	}
	for _, evs := range out {
		slices.SortStableFunc(evs, func(a, b store.Event) int { return a.At.Compare(b.At) })
	}
	return out
}

// statsFullPass reports whether the round of review r was a full pass: the
// last start of a round of its PR (starts, by time) at or before the
// round's first run says so, of r's head.
func statsFullPass(starts []store.Event, r store.PostedReview) bool {
	var last *store.Event
	for i := range starts {
		if starts[i].At.After(r.RoundStart) {
			break
		}
		last = &starts[i]
	}
	if last == nil {
		return false
	}
	var data struct {
		FullPass bool   `json:"full_pass"`
		Target   string `json:"target_sha"`
	}
	return json.Unmarshal(last.Data, &data) == nil && data.FullPass && data.Target == r.SHA
}

// statsStartsSince is how far back the round starts statsFullPass reads
// go: the first run of the earliest round with a finding recorded in the
// window, else since.
func statsStartsSince(prs []store.ReviewedPR, findings []store.Finding, since time.Time) time.Time {
	rounds := map[statsValueKey]bool{}
	for _, f := range findings {
		rounds[statsValueKey{f.PRID, f.Round}] = true
	}
	out := since
	for _, p := range prs {
		for _, r := range p.Reviews {
			if rounds[statsValueKey{p.ID, r.Round}] && r.RoundStart.Before(out) {
				out = r.RoundStart
			}
		}
	}
	return out
}

// statsRenderConvergence prints the convergence section: its table, the
// open PRs with long chains and the origins of the re-review findings.
// Nothing when there is nothing to count.
func statsRenderConvergence(w io.Writer, c statsConvergence) {
	var rows [][]string
	share := func(n, of int) string { return fmt.Sprintf("%d (%s)", n, statsPercent(float64(n)/float64(of))) }
	for _, r := range c.Rows {
		mean, p90 := "-", "-"
		if r.P90Heads > 0 {
			mean, p90 = fmt.Sprintf("%.2f", r.MeanHeads), strconv.Itoa(r.P90Heads)
		}
		rows = append(rows, []string{cmp.Or(actClean(r.Repo), "all"), r.Authors, strconv.Itoa(r.PRs), share(r.PassAt1, r.PRs),
			share(r.PassBy2, r.PRs), share(r.FivePlus, r.PRs), share(r.Never, r.PRs), mean, p90, strconv.Itoa(r.MergedOpen)})
	}
	var lines []string
	if o := c.OpenFivePlus; o.Others+o.Own > 0 {
		lines = append(lines, fmt.Sprintf("open PRs with %d or more reviewed heads: %d others', %d own", statsLongChain, o.Others, o.Own))
	}
	for _, s := range []struct {
		what string
		o    statsOrigins
	}{{"re-review", c.Rereviews}, {"full pass", c.FullPasses}} {
		if n := s.o.total(); n > 0 {
			lines = append(lines, fmt.Sprintf("%s P0-P2 posted: %d: %d new, %d missed earlier (%s), %d fallout of fixes", s.what, n,
				s.o.New, s.o.MissedEarlier, statsPercent(s.o.MissedShare), s.o.Fallout))
		}
	}
	if len(rows) == 0 && len(lines) == 0 {
		return
	}
	if len(rows) > 0 {
		statsTable(w, "CONVERGENCE", []string{"REPO", "AUTHORS", "PRS", "PASS@1", "PASS<=2", "5+", "NEVER", "MEAN", "P90", "MERGED OPEN"}, rows)
	} else {
		fmt.Fprintf(w, "\nCONVERGENCE\n")
	}
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}
