package cli

// The approvals section of `magnum stats` (DECISIONS "`magnum stats`
// counts the reviewed heads to a pass and the merges on magnum's approval
// alone"): on each repository magnum approves as the operator
// ([[watch]] auto_approve), how many of the window's merges rest on
// magnum's approval alone, with no person's approval beside it.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// Events the approvals section reads: a PR merged with a push magnum never
// reviewed (the engine's poll), and the operator's own approval posted with
// `magnum approve --as` or the board's A (the engine's approveAsOperator),
// whose data names the auto_approvals row it recorded (approval_id).
const (
	statsKindMergedUnreviewed = "pr.merged_unreviewed"
	statsKindManualApproval   = "review.manual_verdict_begin"
)

// statsApprovalSlack is how much later than magnum recorded its post GitHub
// may say one of magnum's approvals was submitted (the clocks, the post's
// answer): a later approval of the same head by the same login is another
// one, the operator's by hand.
const statsApprovalSlack = time.Minute

// statsApprovals is one repository's row of the approvals section
// (`magnum stats --json`'s approvals): of the PRs merged in the window,
// AutoApproved are those on which an approval magnum posted by itself stood
// at the merge, MagnumOnly those of them no person approved, PastApproval
// those of the latter merged at a head after the one magnum approved,
// Unreviewed those of these with a pr.merged_unreviewed event, and Misses
// the retro's misses (class miss) on the MagnumOnly ones, less the declines
// of magnum's own findings it routed to the notes as misses.
type statsApprovals struct {
	Repo         string `json:"repo"`
	Merged       int    `json:"merged"`
	AutoApproved int    `json:"auto_approved"`
	MagnumOnly   int    `json:"magnum_only"`
	PastApproval int    `json:"past_approval"`
	Unreviewed   int    `json:"unreviewed"`
	Misses       int    `json:"misses"`
}

// statsApprovalInput is what the approvals section reads of the registry:
// the PRs merged in the window, every approval magnum recorded on them
// (by PR, oldest first), the ids of those the operator posted by hand
// (`magnum approve --as`), the subjects of the PRs merged unreviewed and the
// retro's misses per PR.
type statsApprovalInput struct {
	merged     []store.RepoPR
	autos      map[int64][]store.AutoApproval
	byHand     map[int64]bool
	unreviewed map[string]bool
	misses     map[int64]int
}

// statsApprovalsOf counts the approvals section, a row per repository on
// returns true for (auto_approve on), of repo when it is set, by repository.
func statsApprovalsOf(in statsApprovalInput, on func(repo string) bool, repo string) []statsApprovals {
	out := []statsApprovals{}
	for _, rp := range in.merged {
		if (repo != "" && rp.Repo != repo) || !on(rp.Repo) {
			continue
		}
		if len(out) == 0 || out[len(out)-1].Repo != rp.Repo {
			out = append(out, statsApprovals{Repo: rp.Repo})
		}
		row := &out[len(out)-1]
		row.Merged++
		var auto []store.AutoApproval // magnum's own, posted by itself
		for _, a := range in.autos[rp.PR.ID] {
			if a.ReviewID != 0 && !in.byHand[a.ID] {
				auto = append(auto, a)
			}
		}
		var stood *store.AutoApproval // the latest that stood at the merge
		for i := range auto {
			if auto[i].Stands() {
				stood = &auto[i]
			}
		}
		if stood == nil {
			continue
		}
		row.AutoApproved++
		if statsPersonApproved(rp.PR, auto) {
			continue
		}
		row.MagnumOnly++
		row.Misses += in.misses[rp.PR.ID]
		if rp.PR.HeadSHA != stood.HeadSHA {
			row.PastApproval++
			if in.unreviewed["pr:"+rp.Repo+"#"+strconv.Itoa(rp.PR.Number)] {
				row.Unreviewed++
			}
		}
	}
	return out
}

// statsPersonApproved reports whether a person approved pr as the last
// Details read saw it: an approval in GitHub's latest reviews or in the
// merge gate's opinions whose login is not a bot's (statsBotLogin), not the
// author's and not one of auto, magnum's own approvals (statsMagnums). The
// operator's approval by hand is a person's.
func statsPersonApproved(pr store.PR, auto []store.AutoApproval) bool {
	reviews := slices.Clone(pr.LatestReviews)
	if pr.ReviewGate != nil {
		reviews = append(reviews, pr.ReviewGate.Opinions...)
	}
	for _, r := range reviews {
		if strings.EqualFold(r.State, "APPROVED") && r.Login != "" && !statsBotLogin(r.Login) &&
			!github.SameAccount(r.Login, store.Deref(pr.AuthorLogin)) && !statsMagnums(r, auto) {
			return true
		}
	}
	return false
}

// statsBotLogin reports whether login is a bot's: an App's ("[bot]") or a
// machine account named "-bot".
func statsBotLogin(login string) bool {
	l := strings.ToLower(login)
	return strings.HasSuffix(l, "[bot]") || strings.HasSuffix(l, "-bot")
}

// statsMagnums reports whether approval r is one of auto, magnum's own
// posted approvals: by the same login, of the same head and, when r says
// when it was submitted (GitHub's latest reviews do, the merge gate's
// opinions do not), submitted by the time magnum recorded its post. GitHub
// keeps one latest review per login, so an approval of that head by that
// login after magnum's is the operator's by hand. The registry keeps
// neither the review's id nor its body (magnum's hidden marker), so the
// head and the time are the join.
func statsMagnums(r store.LatestReview, auto []store.AutoApproval) bool {
	return slices.ContainsFunc(auto, func(a store.AutoApproval) bool {
		return github.SameAccount(a.Login, r.Login) && a.HeadSHA == r.CommitSHA &&
			(r.SubmittedAt == nil || a.PostedAt == nil || !r.SubmittedAt.After(a.PostedAt.Add(statsApprovalSlack)))
	})
}

// statsGatherApprovals reads what the approvals section counts: the PRs
// merged since on the repositories cfg approves as the operator (of repo
// when it is set), magnum's approvals of them, the events and the misses.
func statsGatherApprovals(ctx context.Context, st *store.Store, cfg *config.Config, since time.Time, repo string) ([]statsApprovals, error) {
	if cfg == nil {
		return []statsApprovals{}, nil
	}
	on := func(r string) bool { return cfg.AutoApproveFor(r) != nil }
	merged, err := st.MergedPRsSince(ctx, since)
	if err != nil {
		return nil, err
	}
	merged = slices.DeleteFunc(merged, func(rp store.RepoPR) bool { return (repo != "" && rp.Repo != repo) || !on(rp.Repo) })
	in := statsApprovalInput{merged: merged, byHand: map[int64]bool{}, unreviewed: map[string]bool{}, misses: map[int64]int{}}
	if len(merged) == 0 {
		return statsApprovalsOf(in, on, repo), nil
	}
	ids := make([]int64, 0, len(merged))
	for _, rp := range merged {
		ids = append(ids, rp.PR.ID)
	}
	if in.autos, err = st.AutoApprovalsByPR(ctx, ids); err != nil {
		return nil, err
	}
	first := since // the event of an approval by hand follows its row
	for _, list := range in.autos {
		for _, a := range list {
			if a.CreatedAt.Before(first) {
				first = a.CreatedAt
			}
		}
	}
	manual, err := st.EventsOfKindsSince(ctx, first, statsKindManualApproval)
	if err != nil {
		return nil, err
	}
	for _, ev := range manual {
		var data struct {
			ApprovalID int64 `json:"approval_id"`
		}
		if json.Unmarshal(ev.Data, &data) == nil && data.ApprovalID != 0 {
			in.byHand[data.ApprovalID] = true
		}
	}
	unreviewed, err := st.EventsOfKindsSince(ctx, since, statsKindMergedUnreviewed)
	if err != nil {
		return nil, err
	}
	for _, ev := range unreviewed {
		in.unreviewed[store.Deref(ev.Subject)] = true
	}
	// A decline the retro routed to the notes is magnum's own finding that
	// its author declined: no defect a reviewer caught.
	misses, err := st.Misses(ctx, store.MissFilter{Classes: []string{store.MissMiss}, NoRoutedDeclines: true})
	if err != nil {
		return nil, err
	}
	for _, m := range misses {
		in.misses[m.PRID]++
	}
	return statsApprovalsOf(in, on, repo), nil
}

// statsRenderApprovals prints the approvals section, MAGNUM ONLY with its
// share of AUTO-APPROVED; nothing without a repository.
func statsRenderApprovals(w io.Writer, rows []statsApprovals) {
	var cells [][]string
	for _, r := range rows {
		only := strconv.Itoa(r.MagnumOnly)
		if r.AutoApproved > 0 {
			only = fmt.Sprintf("%d (%s)", r.MagnumOnly, statsPercent(float64(r.MagnumOnly)/float64(r.AutoApproved)))
		}
		cells = append(cells, []string{actClean(r.Repo), strconv.Itoa(r.Merged), strconv.Itoa(r.AutoApproved), only,
			strconv.Itoa(r.PastApproval), strconv.Itoa(r.Unreviewed), strconv.Itoa(r.Misses)})
	}
	statsTable(w, "APPROVALS", []string{"REPO", "MERGED", "AUTO-APPROVED", "MAGNUM ONLY", "PAST APPROVAL", "UNREVIEWED", "MISSES"}, cells)
}
