package pipeline

// Related PRs (DECISIONS "The judge knows the related PRs"): a round sees
// only its own PR, so two open PRs fixing the same flaky spec, or a PR
// undoing what another just fixed, went unnoticed. Every judge prompt (the
// own pass and the candidates phase alike, and a round's one prompt) names
// related.json: the repository's other open PRs, drafts included, and those
// merged within related_lookback whose changed paths (the poller's lists,
// pr_files) overlap this PR's at the head under review, with what magnum
// knows of them. Numbers, URLs, heads and paths only: PR text is data, never
// in magnum's files (the judge reads a PR itself with gh when it matters).
// A blind replay never gets one: it would tell of later PRs.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
)

// RelatedFile is the related PRs' file in the round's report directory.
const RelatedFile = "related.json"

// related.json's caps: the PRs sharing the most paths, and the paths of each.
const (
	maxRelatedPRs   = 10
	maxRelatedPaths = 20
)

// RelatedPRs is related.json.
type RelatedPRs struct {
	PR      string `json:"pr"`       // owner/repo#N
	HeadSHA string `json:"head_sha"` // the head under review, whose paths were compared
	// FilesTruncated: this PR changes more files than its list holds (100),
	// so a PR sharing only a later one is missing.
	FilesTruncated bool        `json:"files_truncated,omitempty"`
	MergedSince    time.Time   `json:"merged_since"` // merged PRs from then on count (related_lookback)
	Related        []RelatedPR `json:"related"`
	More           int         `json:"more,omitempty"` // related PRs past the cap of ten
}

// RelatedPR is one related PR of RelatedPRs.
type RelatedPR struct {
	Number   int        `json:"number"`
	URL      string     `json:"url"`
	State    string     `json:"state"` // open | merged
	Draft    bool       `json:"draft,omitempty"`
	MergedAt *time.Time `json:"merged_at,omitempty"`
	HeadSHA  string     `json:"head_sha"` // the head its paths were read at
	// Overlap counts the changed paths both PRs share (related_ignore's
	// left out); Paths are the first twenty of them, sorted.
	Overlap int      `json:"overlap"`
	Paths   []string `json:"paths"`
	// FilesTruncated: it changes more files than its list holds (100), so
	// the overlap may be larger.
	FilesTruncated bool `json:"files_truncated,omitempty"`
	// Reviewed: magnum reviewed it; ReviewURL and ReviewVerdict are its last
	// posted review's (store.ReviewSummary), when magnum has its result.
	Reviewed      bool   `json:"reviewed"`
	ReviewURL     string `json:"review_url,omitempty"`
	ReviewVerdict string `json:"review_verdict,omitempty"`
	// FindingsOnPaths counts the findings magnum posted on it, over all its
	// rounds, on the shared paths (all of them, not only Paths).
	FindingsOnPaths int `json:"findings_on_paths"`
	id              int64
	shared          []string
}

// relatedPRs is the candidates whose paths overlap own's, paths matching an
// ignore glob left out: most shared paths first, then the newest PR; at
// most maxRelatedPRs of them, each with at most maxRelatedPaths paths, and
// more counts the rest.
func relatedPRs(own []string, cands []store.FilesPR, ignore []string) (out []RelatedPR, more int) {
	ignored := func(p string) bool {
		return slices.ContainsFunc(ignore, func(g string) bool { return config.MatchPath(g, p) })
	}
	mine := map[string]bool{}
	for _, p := range own {
		if !ignored(p) {
			mine[p] = true
		}
	}
	for _, c := range cands {
		var shared []string
		for _, p := range c.Files.Paths {
			if mine[p] {
				shared = append(shared, p)
			}
		}
		if len(shared) == 0 {
			continue
		}
		slices.Sort(shared)
		shared = slices.Compact(shared)
		r := RelatedPR{Number: c.Number, URL: c.URL, State: "open", Draft: c.IsDraft, HeadSHA: c.Files.HeadSHA,
			Overlap: len(shared), Paths: shared[:min(len(shared), maxRelatedPaths)], FilesTruncated: c.Files.Truncated,
			Reviewed: store.Deref(c.ReviewedSHA) != "", id: c.ID, shared: shared}
		if c.GHState == store.GHMerged {
			r.State, r.MergedAt, r.Draft = "merged", c.MergedAt, false
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b RelatedPR) int {
		return cmp.Or(cmp.Compare(b.Overlap, a.Overlap), cmp.Compare(b.Number, a.Number))
	})
	if len(out) > maxRelatedPRs {
		more = len(out) - maxRelatedPRs
		out = out[:maxRelatedPRs]
	}
	return out, more
}

// related computes related.json's content for the round; an empty Related
// when the PR has no file list for the head under review.
func (rd *round) related(ctx context.Context) (RelatedPRs, error) {
	in := rd.in
	since := rd.r.now().Add(-in.Related.Lookback).UTC()
	out := RelatedPRs{PR: fmt.Sprintf("%s/%s#%d", rd.owner, rd.name, in.PR.Number), HeadSHA: in.TargetSHA, MergedSince: since}
	own, ok, err := rd.r.Store.PRFilesOf(ctx, in.PR.ID)
	if err != nil || !ok || own.HeadSHA != in.TargetSHA {
		return out, err
	}
	out.FilesTruncated = own.Truncated
	cands, err := rd.r.Store.PRsWithFiles(ctx, in.PR.RepoID, in.PR.ID, since)
	if err != nil {
		return out, err
	}
	out.Related, out.More = relatedPRs(own.Paths, cands, in.Related.Ignore)
	if len(out.Related) == 0 {
		return out, nil
	}
	ids := make([]int64, len(out.Related))
	for i, r := range out.Related {
		ids[i] = r.id
	}
	reviews, err := rd.r.Store.LastReviewSummaries(ctx, ids)
	if err != nil {
		return out, err
	}
	findings, err := rd.r.Store.PostedFindingPaths(ctx, ids)
	if err != nil {
		return out, err
	}
	for i := range out.Related {
		r := &out.Related[i]
		if s, ok := reviews[r.id]; ok {
			r.Reviewed, r.ReviewURL, r.ReviewVerdict = true, s.URL, s.Verdict
		}
		for _, p := range r.shared {
			r.FindingsOnPaths += findings[r.id][p]
		}
	}
	return out, nil
}

// addRelated writes related.json for the judge prompt about to go out and
// names it in jd, when there is a related PR; a blind replay gets none. A
// failure leaves the prompt without it (a warning): the review goes on.
func (rd *round) addRelated(ctx context.Context, jd *agents.JudgeData) {
	if rd.in.Blind || rd.r.Store == nil {
		return
	}
	rel, err := rd.related(ctx)
	if err == nil && len(rel.Related) == 0 {
		return
	}
	path := filepath.Join(rd.dir, RelatedFile)
	if err == nil {
		var b []byte
		if b, err = json.MarshalIndent(rel, "", "  "); err == nil {
			err = fsx.WriteFileAtomic(path, append(b, '\n'), 0o600)
		}
	}
	if err != nil {
		if ctx.Err() == nil {
			rd.event(ctx, "warn", "round.related", fmt.Sprintf("could not list the related PRs; the judge goes without: %v", err), nil)
		}
		return
	}
	jd.RelatedPRs = path
	nums := make([]string, len(rel.Related))
	for i, r := range rel.Related {
		nums[i] = fmt.Sprintf("#%d", r.Number)
	}
	rd.event(ctx, "info", "round.related", fmt.Sprintf("%s: %d related PR(s) on the same paths: %s", RelatedFile, len(rel.Related)+rel.More, strings.Join(nums, ", ")),
		map[string]any{"related": len(rel.Related), "more": rel.More, "file": path})
}
