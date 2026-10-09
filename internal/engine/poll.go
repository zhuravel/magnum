package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/eligibility"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// confirmGap is how far apart the two CLOSED/MERGED confirmations of a PR
// that left the OPEN list must be before it counts as closed.
const confirmGap = 60 * time.Second

// mergedUnreviewedWindow: a PR merged before its last push was reviewed is
// announced once a day at most.
const mergedUnreviewedWindow = 24 * time.Hour

// rateFloor is the GraphQL budget below which polling waits for the reset.
const rateFloor = 50

// closingStates are the end of a PR's life (cleanup owns releasing/released).
var closingStates = []string{store.PRClosed, store.PRReleasing, store.PRReleased}

// openStates are every state a PR can leave when GitHub confirms it closed.
var openStates = []string{store.PRBaseline, store.PRIneligible, store.PRQueued, store.PRClaiming,
	store.PRReviewing, store.PRVerifying, store.PRReviewed, store.PRRereviewPending, store.PRPaused,
	store.PRNeedsAttention}

// poll runs one radar pass per (watch owner, poll identity).
func (e *Engine) poll(ctx context.Context) error {
	if e.d.GitHub == nil {
		return nil
	}
	now := e.now()
	if until, ok := e.kvTime(ctx, store.KVGHPollPausedUntil); ok && now.Before(until) {
		e.log.Debug("poll skipped: GitHub rate budget low", "until", until)
		return nil
	}
	type key struct{ owner, poll string }
	seen := map[key]bool{}
	var errs []error
	var radar radarResults
	for _, w := range e.cfg.Watches {
		k := key{strings.ToLower(w.Owner), w.PollIdentity}
		if seen[k] {
			continue
		}
		seen[k] = true
		gh := e.gh(w.PollIdentity)
		if gh == nil {
			errs = append(errs, fmt.Errorf("poll %s: no GitHub client for identity %q", w.Owner, w.PollIdentity))
			continue
		}
		repos, rl, err := gh.Radar(e.betweenCalls(ctx), w.Owner)
		e.recordRateLimit(ctx, rl, now)
		radar.add(w.Owner, err)
		if err != nil {
			// A failure right after a sleep is the Mac's network (sleptBefore).
			if !e.afterSleep && e.logOnce("poll:"+w.Owner, err.Error(), now) {
				e.event(ctx, "warn", "watch:"+w.Owner, "poll.error", fmt.Sprintf("radar %s: %v", w.Owner, err), nil)
			}
			errs = append(errs, fmt.Errorf("poll %s: %w", w.Owner, err))
			continue
		}
		if err := e.pollOwner(ctx, w, gh, repos, now); err != nil {
			errs = append(errs, err)
		}
	}
	// The poll's attempt, whatever its radar calls did; each watch keeps
	// its own last good poll (poll_health.go).
	e.setKV(ctx, store.KVDaemonLastPoll, store.FormatTime(now))
	e.recordWatchPolls(ctx, radar, now)
	return errors.Join(errs...)
}

// requestsMidPoll answers the requests a CLI or a screen queued while the
// poll waits for GitHub (pin, mute, review, ...): the tick handles requests
// before and after the poll, which takes 10 to 12 s with several watches,
// and the kick a request sends cannot cut it short. The poll calls it where
// it holds no PR row it decides on afterwards: before each GitHub read of
// the radar (its pages included), the CI rollups and the Details
// (betweenCalls), before each repository and before each PR it applies. So
// a request waits for one GitHub call, the daemon stays the only writer of
// the PR and slot rows a request changes, and the order of requests holds.
// A nested call (a handler reading GitHub) does nothing.
func (e *Engine) requestsMidPoll(ctx context.Context) {
	if e.midPoll || ctx.Err() != nil {
		return
	}
	e.midPoll = true
	defer func() { e.midPoll = false }()
	e.handleRequests(ctx)
}

// betweenCalls is ctx with requestsMidPoll(ctx) run before each GitHub
// call made with it (github.WithBetweenCalls); the handlers get ctx itself.
func (e *Engine) betweenCalls(ctx context.Context) context.Context {
	return github.WithBetweenCalls(ctx, func() { e.requestsMidPoll(ctx) })
}

func (e *Engine) recordRateLimit(ctx context.Context, rl github.RateLimit, now time.Time) {
	if rl.Limit == 0 && rl.Remaining == 0 {
		return
	}
	e.setKV(ctx, store.KVGHRemaining, strconv.Itoa(rl.Remaining))
	e.setKV(ctx, store.KVGHLimit, strconv.Itoa(rl.Limit))
	if !rl.ResetAt.IsZero() {
		e.setKV(ctx, store.KVGHReset, store.FormatTime(rl.ResetAt))
	}
	if rl.Remaining < rateFloor && rl.ResetAt.After(now) {
		e.setKV(ctx, store.KVGHPollPausedUntil, store.FormatTime(rl.ResetAt))
		e.event(ctx, "warn", "", "poll.rate_limited", fmt.Sprintf("GitHub budget at %d points; polling paused until %s", rl.Remaining, rl.ResetAt.Local().Format("15:04")), nil)
	}
}

// pollOwner applies one owner's radar, with the CI of its watched
// repositories' PRs (readCI).
func (e *Engine) pollOwner(ctx context.Context, w config.Watch, gh GitHub, repos []github.RepoRadar, now time.Time) error {
	known, err := e.st.ListRepos(ctx)
	if err != nil {
		return fmt.Errorf("poll %s: %w", w.Owner, err)
	}
	ownerSynced := false
	for _, r := range known {
		if strings.EqualFold(r.WatchOwner, w.Owner) && r.FirstSyncedAt != nil {
			ownerSynced = true
		}
	}
	watches := make([]*config.Watch, len(repos))
	for i, rr := range repos {
		if ww := e.cfg.WatchFor(rr.NameWithOwner); ww != nil && strings.EqualFold(ww.Owner, w.Owner) && ww.PollIdentity == w.PollIdentity {
			watches[i] = ww
		}
	}
	e.readCI(e.betweenCalls(ctx), w, gh, repos, watches, now)
	var errs []error
	for i, rr := range repos {
		if watches[i] == nil {
			continue
		}
		e.requestsMidPoll(ctx)
		if err := e.pollRepo(ctx, *watches[i], gh, rr, ownerSynced, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// readCI fills in the head rollup (CIState, CIKnown) of the open PRs of the
// repositories this poll of w's owner watches (watches[i] != nil), in one
// CIStates read: a CI run that ends does not move a PR's updatedAt, so the
// radar cannot see it, and the radar no longer computes the rollup of every
// open PR of every repository it lists (its first page ran out of time). A
// failed read leaves the rollups it did not read unknown, which keeps the
// stored CI and fetches nothing for it, and is one warning per distinct
// error an hour (none right after a sleep, sleptBefore); the poll goes on.
func (e *Engine) readCI(ctx context.Context, w config.Watch, gh GitHub, repos []github.RepoRadar, watches []*config.Watch, now time.Time) {
	var prs []github.PRRadar
	for i, rr := range repos {
		if watches[i] != nil {
			prs = append(prs, rr.PRs...)
		}
	}
	if len(prs) == 0 {
		return
	}
	states, rl, err := gh.CIStates(ctx, prs)
	e.recordRateLimit(ctx, rl, now)
	if err != nil && !e.afterSleep && e.logOnce("ci:"+w.Owner, err.Error(), now) {
		e.event(ctx, "warn", "watch:"+w.Owner, "poll.ci_error", fmt.Sprintf("CI of %d PRs (%d read): %v", len(prs), len(states), err), nil)
	}
	for i := range repos {
		for j := range repos[i].PRs {
			p := &repos[i].PRs[j]
			if state, ok := states[p.NodeID]; ok {
				p.CIState, p.CIKnown = state, true
			}
		}
	}
}

// pollRepo upserts one repository and its open PRs and applies the
// resulting transitions.
func (e *Engine) pollRepo(ctx context.Context, w config.Watch, gh GitHub, rr github.RepoRadar, ownerSynced bool, now time.Time) error {
	full := rr.NameWithOwner
	owner, name, ok := strings.Cut(full, "/")
	if !ok {
		return fmt.Errorf("poll: bad repository name %q", full)
	}
	existing, err := e.st.RepoByFullName(ctx, full)
	isNew := errors.Is(err, store.ErrNotFound)
	if err != nil && !isNew {
		return fmt.Errorf("poll %s: %w", full, err)
	}
	firstSync := isNew || existing.FirstSyncedAt == nil
	repoRow := store.Repo{NodeID: rr.NodeID, Owner: owner, Name: name, WatchOwner: w.Owner,
		Mode: store.RepoModePerPR, DefaultBranch: existing.DefaultBranch}
	staleClone := false
	if pool := e.cfg.PoolFor(full); pool != nil {
		repoRow.Mode, repoRow.DefaultBranch, repoRow.ClonePath = store.RepoModePool, pool.Base, store.Ptr(pool.MainClone)
	} else {
		repoRow.ClonePath, staleClone = e.clonePath(ctx, w, existing, owner, name)
	}
	repo, err := e.st.UpsertRepo(ctx, repoRow)
	if err != nil {
		return fmt.Errorf("poll %s: %w", full, err)
	}
	if staleClone && repoRow.ClonePath == nil {
		if err := e.st.SetRepoClonePath(ctx, repo.ID, ""); err != nil {
			e.log.Warn("clear stale clone path", "repo", full, "err", err)
		} else {
			repo.ClonePath = nil
		}
	}
	if isNew && ownerSynced {
		e.event(ctx, "info", "repo:"+full, "repo.new", "new watched repository "+full, nil)
		e.info(notify.Item{Key: "repo.new:" + full, Title: "magnum: new repo " + full,
			Body: "magnum now watches " + full + "; its open PRs are a baseline.", Line: full,
			Kind: notify.KindNewRepo, Window: newRepoWindow})
	}

	stored, err := e.pollRows(ctx, repo.ID, rr.PRs)
	if err != nil {
		return fmt.Errorf("poll %s: %w", full, err)
	}
	byNode := map[string]store.PR{}
	for _, p := range stored {
		byNode[p.NodeID] = p
	}

	if len(rr.PRs) > 0 {
		branch := rr.DefaultBranch
		if branch == "" {
			branch = repo.DefaultBranch
		}
		e.refreshRequiredChecks(ctx, gh, repo, branch, firstSync, now)
	}
	// Requests may be answered during the Details read: byNode serves its
	// GitHub fields only from here on, and applyRadarPRs decides on the rows
	// its upserts read.
	details, needed := e.fetchDetails(e.betweenCalls(ctx), gh, full, rr.PRs, byNode)
	errs, inRadar := e.applyRadarPRs(ctx, w, gh, repo, rr.PRs, byNode, details, needed, firstSync, now)

	var missing []store.PR
	for _, p := range stored {
		if !inRadar[p.NodeID] && p.GHState == store.GHOpen {
			missing = append(missing, p)
		}
	}
	if err := e.confirmMissing(ctx, gh, repo, missing, now); err != nil {
		errs = append(errs, err)
	}

	if firstSync && len(errs) == 0 {
		repoRow.FirstSyncedAt = store.Ptr(now)
		if _, err := e.st.UpsertRepo(ctx, repoRow); err != nil {
			errs = append(errs, err)
		} else {
			e.event(ctx, "info", "repo:"+full, "repo.synced", fmt.Sprintf("first sync of %s: %d open PRs recorded as baseline", full, len(rr.PRs)), nil)
		}
	}
	return errors.Join(errs...)
}

// pollRows reads the repository's rows a poll uses: the PRs GitHub has
// open, which the PRs missing from the radar are confirmed against, and the
// PRs the radar lists, whatever their GitHub state (a reopened PR keeps its
// row). The closed, merged and released rows, most of a repository's, stay
// unread.
func (e *Engine) pollRows(ctx context.Context, repoID int64, radar []github.PRRadar) ([]store.PR, error) {
	nodes := make([]string, len(radar))
	for i, p := range radar {
		nodes[i] = p.NodeID
	}
	return e.st.ListPRs(ctx, store.PRFilter{RepoID: repoID, GHOpen: true, OrNodeIDs: nodes})
}

// fetchDetails fetches the Details of the radar PRs that are new or changed
// (or never had them) in one call; needed lists every PR asked for. A
// failure is reported once per distinct error (the next poll asks again).
// A finished CI run does not move a PR's updatedAt: its checks are fetched
// again when the rollup readCI read differs from the stored checks' or
// those are not the head's.
func (e *Engine) fetchDetails(ctx context.Context, gh GitHub, full string, prs []github.PRRadar, byNode map[string]store.PR) (map[int]github.PRDetails, map[int]bool) {
	var need []int
	needed := map[int]bool{}
	for _, p := range prs {
		cur, ok := byNode[p.NodeID]
		// DetailsAt == nil: a PR recorded before the board fields existed
		// gets its Details (assignees, reviewers, size) once.
		// AuthorAssociation == nil: one fetch fills it (skip_departed_authors).
		// CI == nil: likewise for its checks.
		if !ok || cur.HeadSHA != p.HeadRefOid || cur.IsDraft != p.IsDraft || cur.Title == nil || cur.DetailsAt == nil ||
			cur.AuthorAssociation == nil ||
			cur.CI == nil || cur.CI.SHA != p.HeadRefOid || (p.CIKnown && cur.CI.State != p.CIState) ||
			cur.GHState != store.GHOpen || cur.GHUpdatedAt == nil || !cur.GHUpdatedAt.Equal(p.UpdatedAt) {
			need = append(need, p.Number)
			needed[p.Number] = true
		}
	}
	details := map[int]github.PRDetails{}
	if len(need) == 0 {
		return details, needed
	}
	owner, name, _ := strings.Cut(full, "/")
	d, _, err := gh.Details(ctx, owner, name, need)
	if err != nil {
		if e.changed("details:"+full, err.Error()) {
			e.event(ctx, "warn", "repo:"+full, "poll.details_error", fmt.Sprintf("details of %d PRs: %v", len(need), err), nil)
		}
		return details, needed
	}
	e.changed("details:"+full, "")
	return d, needed
}

// applyRadarPRs upserts the repository's open PRs from the radar (with their
// Details when fetched) and applies the resulting transitions: a new PR is
// classified (a baseline on the repository's first sync), a known one goes
// through onSeenPR. It returns the errors and the node ids seen.
func (e *Engine) applyRadarPRs(ctx context.Context, w config.Watch, gh GitHub, repo store.Repo, prs []github.PRRadar,
	byNode map[string]store.PR, details map[int]github.PRDetails, needed map[int]bool, firstSync bool, now time.Time) ([]error, map[string]bool) {
	full := repo.FullName()
	selfLogins := e.watchLogins(w)
	compares := maxComparesPerRepo
	fileFetches := maxFileFetchesPerRepo
	var errs []error
	inRadar := map[string]bool{}
	for _, p := range prs {
		// The previous PR's files, comparisons and dismissals were GitHub
		// calls; cur below serves its GitHub fields only.
		e.requestsMidPoll(ctx)
		inRadar[p.NodeID] = true
		cur, exists := byNode[p.NodeID]
		d, hasD := details[p.Number]
		if !exists && !hasD && !firstSync {
			continue // a new PR cannot be classified without its details; next tick
		}
		in := store.GitHubPR{
			RepoID: repo.ID, NodeID: p.NodeID, Number: p.Number, HeadSHA: p.HeadRefOid, IsDraft: p.IsDraft,
			URL: fmt.Sprintf("https://github.com/%s/pull/%d", full, p.Number), BaseRef: store.Ptr(p.BaseRefName),
			GHState: store.GHOpen, Identity: w.Identity,
		}
		if !p.UpdatedAt.IsZero() && (hasD || !needed[p.Number]) {
			in.GHUpdatedAt = store.Ptr(p.UpdatedAt) // else: details missed, fetch them next poll
		}
		if p.CIKnown {
			in.CIState = store.Ptr(p.CIState) // fillDetails replaces it with the Details' (newer) one
		}
		if exists {
			in.URL = cur.URL
		}
		if hasD {
			fillDetails(&in, d, selfLogins, now)
			in.Replies = repliesOf(d, e.ownLogins(ctx, w, cur))
			e.noteTruncatedLabels(ctx, w, repo, d)
		}
		if !exists {
			in.InitialState = store.PRBaseline
			if !firstSync {
				facts := e.factsFor(ctx, prFromInput(in, now), w, now)
				if dec := eligibility.Classify(w, facts); dec.Eligible && safeBase(deref(in.BaseRef)) {
					in.InitialState = store.PRQueued
				} else {
					in.InitialState = store.PRIneligible
				}
			}
		}
		res, err := e.st.UpsertPRFromGitHub(ctx, in)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !firstSync && e.wantsFiles(ctx, w, res, now) && e.fetchChangedFiles(ctx, gh, w, repo, res.PR, &fileFetches) {
			res.Changed = true // a fresh file list: classify again (skip_paths)
		}
		// A review request, or a draft marked ready, is recorded before the
		// transitions below, so the throttle they consult already knows it.
		var req Request
		requested := false
		switch {
		case !res.New && cur.IsDraft && !res.PR.IsDraft:
			req, requested = e.noteReady(ctx, repo, res.PR, now)
		case hasD && !firstSync:
			req, requested = e.noteRequest(ctx, w, repo, res.PR, d)
		}
		if res.New {
			err = e.onNewPR(ctx, repo, w, res.PR, now)
		} else {
			err = e.onSeenPR(ctx, repo, w, cur, res, now)
		}
		if err == nil && requested {
			err = e.onRequest(ctx, repo, w, res.PR.ID, req, now)
		}
		if err == nil && hasD && !firstSync && !res.New {
			err = e.considerReplies(ctx, repo, w, res.PR.ID, now) // replies.go
		}
		if err != nil {
			errs = append(errs, err)
		}
		var dp *github.PRDetails
		if hasD {
			dp = &d
		}
		e.refreshSinceReview(ctx, gh, repo, res.PR, dp, &compares)
	}
	return errs, inRadar
}

// reasonUnsafeBase is the skip reason of a PR whose base branch name
// safeBase refuses, in agents.ErrUnsafeBaseRef's words.
var reasonUnsafeBase = agents.ErrUnsafeBaseRef.Error()

// safeBase reports whether a PR's base branch (as the poll records it) may
// reach the prompts: unknown, or a name gitx.ShellSafeRef passes. The judge
// prompts put origin/<base> into a git command when the merge base is
// unknown, and git allows '$', '|', '&' and parentheses in a branch name,
// which anyone who can push may create; classify makes such a PR
// ineligible, and the prompts refuse it too (agents.ErrUnsafeBaseRef).
func safeBase(base string) bool { return base == "" || gitx.ShellSafeRef(base) }

// noteTruncatedLabels reports, once per PR, that GitHub listed only the first
// page of a PR's labels while the watch has skip_labels: a skip label on that
// page still skips the PR, but one beyond it cannot be seen, so the PR is
// not skipped for it.
func (e *Engine) noteTruncatedLabels(ctx context.Context, w config.Watch, repo store.Repo, d github.PRDetails) {
	key := fmt.Sprintf("labels:%s#%d", repo.FullName(), d.Number)
	if d.LabelsComplete || len(w.SkipLabels) == 0 {
		delete(e.lastSeen, key)
		return
	}
	if e.changed(key, "truncated") {
		e.event(ctx, "warn", prSubject(repo, d.Number), "poll.labels_truncated",
			fmt.Sprintf("GitHub listed only %d of the PR's labels; skip_labels is checked against those only", len(d.Labels)), nil)
	}
}

// clonePath decides repos.clone_path of a per-PR repository: nil keeps the
// recorded one while it is still a git repository; otherwise the clone
// gitx FindClone discovers under the watch's clone_root (named <name>,
// <owner>-<name>, <owner>_<name> or anything whose origin matches) is
// persisted. stale reports a recorded path that is not a repository (a
// same-named folder): the caller clears it when nothing was found, so
// nothing keeps pointing at it.
func (e *Engine) clonePath(ctx context.Context, w config.Watch, existing store.Repo, owner, name string) (path *string, stale bool) {
	if p := deref(existing.ClonePath); p != "" {
		if gitx.IsRepo(paths.Expand(p)) {
			return nil, false
		}
		stale = true
	}
	if e.d.Git == nil {
		return nil, stale
	}
	found, err := e.d.Git.FindClone(ctx, slots.CloneRoot(w), owner, name)
	if err != nil {
		if !errors.Is(err, gitx.ErrNoClone) && e.changed("clone:"+owner+"/"+name, err.Error()) {
			e.log.Warn("find clone", "repo", owner+"/"+name, "err", err)
		}
		return nil, stale
	}
	if found != deref(existing.ClonePath) {
		e.event(ctx, "info", "repo:"+owner+"/"+name, "repo.clone_found", "main clone: "+found, nil)
	}
	return &found, stale
}

// watchLogins are the logins a review request "for us" names: the poll
// identity's and the posting identity's.
func (e *Engine) watchLogins(w config.Watch) []string {
	var out []string
	for _, n := range []string{w.PollIdentity, w.Identity} {
		if id := e.cfg.IdentityByName(n); id != nil && id.Login != "" {
			out = append(out, id.Login)
		}
	}
	return out
}

// selfLogin is the user's own login for a watch (its poll identity).
func (e *Engine) selfLogin(w config.Watch) string {
	if id := e.cfg.IdentityByName(w.PollIdentity); id != nil {
		return id.Login
	}
	return ""
}

func fillDetails(in *store.GitHubPR, d github.PRDetails, logins []string, now time.Time) {
	if d.URL != "" {
		in.URL = d.URL
	}
	in.DetailsAt = store.Ptr(now)
	if d.BaseRefOid != "" {
		in.BaseSHA = store.Ptr(d.BaseRefOid)
	}
	in.Assignees = make([]string, 0, len(d.Assignees))
	for _, a := range d.Assignees {
		in.Assignees = append(in.Assignees, github.NormalizeLogin(a))
	}
	in.RequestedReviewers = make([]string, 0, len(d.ReviewRequests))
	for _, r := range d.ReviewRequests {
		if r.Type == "Team" {
			in.RequestedReviewers = append(in.RequestedReviewers, store.TeamReviewerPrefix+r.Login)
		} else {
			in.RequestedReviewers = append(in.RequestedReviewers, github.Account(r.Login, r.Type))
		}
	}
	in.LatestReviews = make([]store.LatestReview, 0, len(d.LatestReviews))
	for _, r := range d.LatestReviews {
		lr := store.LatestReview{Login: github.Account(r.AuthorLogin, r.AuthorType), State: r.State, CommitSHA: r.CommitOid}
		if !r.SubmittedAt.IsZero() {
			lr.SubmittedAt = store.Ptr(r.SubmittedAt.UTC())
		}
		in.LatestReviews = append(in.LatestReviews, lr)
	}
	in.ReviewRequests = make([]store.ReviewRequest, 0, len(d.ReviewRequestEvents))
	for _, ev := range d.ReviewRequestEvents {
		to := github.Account(ev.Reviewer.Login, ev.Reviewer.Type)
		if ev.Reviewer.Type == "Team" {
			to = store.TeamReviewerPrefix + ev.Reviewer.Login
		}
		in.ReviewRequests = append(in.ReviewRequests, store.ReviewRequest{At: ev.CreatedAt.UTC(), By: ev.Actor, To: to})
	}
	in.Title = store.Ptr(d.Title)
	in.AuthorLogin = store.Ptr(d.AuthorLogin)
	in.AuthorType = store.Ptr(d.AuthorType)
	in.AuthorAssociation = store.Ptr(d.AuthorAssociation) // "" = fetched but unknown: never fetched again for it
	in.HeadRef = store.Ptr(d.HeadRefName)
	in.CI = ciStatus(d)
	in.CIState = store.Ptr(in.CI.State)
	if d.BaseRefName != "" {
		in.BaseRef = store.Ptr(d.BaseRefName)
	}
	in.IsCrossRepo = store.Ptr(d.IsCrossRepository)
	in.Files = prFiles(d)
	if !d.ActivityAt.IsZero() {
		in.ActivityAt = store.Ptr(d.ActivityAt) // the registry adds the head moves it saw
	}
	in.ReviewGate = reviewGate(d.ReviewGate)
	labels := d.Labels
	if labels == nil {
		labels = []string{}
	}
	in.Labels = labels
	req := false
	for _, r := range d.ReviewRequests {
		if r.Type == "Team" {
			continue
		}
		for _, l := range logins {
			if github.SameAccount(github.Account(r.Login, r.Type), l) {
				req = true
			}
		}
	}
	in.ReviewRequested = store.Ptr(req)
}

// prFiles is the registry's file list of the Details' head (the related
// PRs' paths); nil when GitHub listed none, which keeps the stored one.
func prFiles(d github.PRDetails) *store.PRFiles {
	if d.Files == nil || d.HeadRefOid == "" {
		return nil
	}
	return &store.PRFiles{HeadSHA: d.HeadRefOid, Paths: d.Files, Truncated: !d.FilesComplete}
}

// ciStatus is the registry's view of the Details' head checks.
func ciStatus(d github.PRDetails) *store.CIStatus {
	ci := &store.CIStatus{SHA: d.CI.SHA, State: d.CI.State, Total: d.CI.Total, Complete: d.CI.Complete,
		Checks: make([]store.CheckResult, 0, len(d.CI.Checks))}
	if ci.SHA == "" {
		ci.SHA = d.HeadRefOid // GitHub listed no commit: the head has no checks to wait for
	}
	for _, c := range d.CI.Checks {
		ci.Checks = append(ci.Checks, store.CheckResult{Name: c.Name, State: c.State, Workflow: c.Workflow, At: c.At})
	}
	ci.Tally()
	return ci
}

// prFromInput builds the PR row a new PR is about to get (for Classify; its
// repository for manual_repos).
func prFromInput(in store.GitHubPR, now time.Time) store.PR {
	pr := store.PR{RepoID: in.RepoID, Number: in.Number, HeadSHA: in.HeadSHA, IsDraft: in.IsDraft, Labels: in.Labels,
		AuthorLogin: in.AuthorLogin, AuthorType: in.AuthorType, AuthorAssociation: in.AuthorAssociation, HeadChangedAt: now}
	if in.IsCrossRepo != nil {
		pr.IsCrossRepo = *in.IsCrossRepo
	}
	return pr
}

// factsFor is the eligibility view of a PR. Its repository's name is read
// only when the watch has manual_repos, and its pending review request
// (Requested) only for a draft the watch skips: the one rule that needs
// each.
func (e *Engine) factsFor(ctx context.Context, pr store.PR, w config.Watch, now time.Time) eligibility.PRFacts {
	rounds := pr.RoundsToday
	if deref(pr.RoundsDay) != store.DayKey(now) {
		rounds = 0
	}
	repo := ""
	if len(w.ManualRepos) > 0 {
		r, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			e.log.Warn("eligibility: repository", "pr", pr.ID, "err", err)
		}
		repo = r.Name
	}
	requested := false
	if pr.IsDraft && !w.DraftsIncluded() {
		req, ok := e.pendingRequest(ctx, pr)
		requested = ok && req.By != RequestReadyForReview
	}
	return eligibility.PRFacts{
		Repo:   repo,
		Number: pr.Number, AuthorLogin: deref(pr.AuthorLogin),
		AuthorIsBot: github.IsBot(deref(pr.AuthorType), deref(pr.AuthorLogin)),
		IsDraft:     pr.IsDraft, Requested: requested, IsCrossRepo: pr.IsCrossRepo, Labels: pr.Labels,
		AuthorAssociation: deref(pr.AuthorAssociation),
		HeadSHA:           pr.HeadSHA, ReviewedSHA: deref(pr.ReviewedSHA), State: pr.State,
		HeadChangedAt: pr.HeadChangedAt, PendingSince: deref(pr.PendingSince),
		LastRoundStartedAt: deref(pr.LastRoundStartedAt), RoundsToday: rounds,
		Own:    e.ownPR(pr),
		Forced: pr.Forced, Muted: pr.Muted, Pinned: pr.Pinned,
	}
}

// ownPR reports whether the operator authored pr: its author is one of
// config.SelfLogins (the board's "mine").
func (e *Engine) ownPR(pr store.PR) bool {
	a := deref(pr.AuthorLogin)
	return a != "" && e.cfg.SelfMatch()(a)
}

// claimableState is where a PR waits for its next round.
func claimableState(pr store.PR) string {
	if deref(pr.ReviewedSHA) != "" {
		return store.PRRereviewPending
	}
	return store.PRQueued
}

// queue moves pr from one of from to its claimable state with the throttle
// timers eligibility.Throttle gives (forced PRs are due now). headChanged
// also restarts pending_since and the retry budget.
func (e *Engine) queue(ctx context.Context, pr store.PR, w config.Watch, from []string, headChanged bool, now time.Time, why string) error {
	to := claimableState(pr)
	f := e.factsFor(ctx, pr, w, now)
	if headChanged || pr.PendingSince == nil {
		f.PendingSince = now
	}
	td := e.throttle(ctx, w, pr, f, now)
	err := e.st.TransitionPR(ctx, pr.ID, from, to, func(u *store.PRUpdate) {
		u.Set("skip_reason", nil)
		if headChanged || pr.PendingSince == nil {
			u.Set("pending_since", now)
		}
		if headChanged {
			u.Set("attempts", 0)
			u.Set("next_attempt_at", nil)
		}
		if pr.Forced {
			u.Set("next_eligible_at", nil)
		} else {
			u.Set("next_eligible_at", td.NextEligibleAt)
		}
	})
	if err != nil {
		return err
	}
	repo, _ := e.st.RepoByID(ctx, pr.RepoID)
	msg := fmt.Sprintf("%s → %s (%s)", pr.State, to, why)
	if !td.Ready && !pr.Forced {
		msg += fmt.Sprintf("; eligible at %s (%s)", td.NextEligibleAt.Local().Format("15:04:05"), td.Reason)
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "pr."+to, msg, nil)
	return nil
}

// markIneligible moves pr to ineligible with the watch rule that rejects it.
func (e *Engine) markIneligible(ctx context.Context, pr store.PR, from []string, reason string, headChanged bool, now time.Time) error {
	err := e.st.TransitionPR(ctx, pr.ID, from, store.PRIneligible, func(u *store.PRUpdate) {
		u.Set("skip_reason", reason)
		if headChanged {
			u.Set("pending_since", now)
			u.Set("attempts", 0)
			u.Set("next_attempt_at", nil)
		}
	})
	if err != nil {
		return err
	}
	repo, _ := e.st.RepoByID(ctx, pr.RepoID)
	e.event(ctx, "info", prSubject(repo, pr.Number), "pr.ineligible", fmt.Sprintf("%s → ineligible: %s", pr.State, reason), nil)
	return nil
}

// onNewPR finishes a PR inserted by this poll: queued PRs get their first
// review quiet period, ineligible ones their skip reason.
func (e *Engine) onNewPR(ctx context.Context, repo store.Repo, w config.Watch, pr store.PR, now time.Time) error {
	subject := prSubject(repo, pr.Number)
	switch pr.State {
	case store.PRBaseline:
		e.log.Debug("baseline PR recorded", "subject", subject)
		return nil
	case store.PRQueued:
		if dec := e.classify(ctx, w, pr, now); !dec.Eligible { // inserted queued before its files were known
			return e.markIneligible(ctx, pr, []string{store.PRQueued}, dec.Reason, false, now)
		}
		f := e.factsFor(ctx, pr, w, now)
		f.PendingSince = now
		td := e.throttle(ctx, w, pr, f, now)
		if err := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) {
			u.Set("pending_since", now)
			u.Set("next_eligible_at", td.NextEligibleAt)
		}); err != nil {
			return err
		}
		e.event(ctx, "info", subject, "pr.queued", fmt.Sprintf("new PR queued; eligible at %s", td.NextEligibleAt.Local().Format("15:04:05")), nil)
	case store.PRIneligible:
		dec := e.classify(ctx, w, pr, now)
		if dec.Eligible { // a review request this poll recorded lets a draft through
			return e.queue(ctx, pr, w, []string{store.PRIneligible}, true, now, "new PR, "+e.eligibleWhy(ctx, w, pr))
		}
		if err := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) { u.Set("skip_reason", dec.Reason) }); err != nil {
			return err
		}
		e.event(ctx, "info", subject, "pr.ineligible", "new PR skipped: "+dec.Reason, nil)
	}
	return nil
}

// onSeenPR applies the transitions for a PR the store already knew (prev is
// its row before this poll's upsert).
func (e *Engine) onSeenPR(ctx context.Context, repo store.Repo, w config.Watch, prev store.PR, res store.PRUpsert, now time.Time) error {
	pr := res.PR
	if pr.MissingSince != nil || pr.ConfirmCount != 0 {
		if err := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) {
			u.Set("missing_since", nil)
			u.Set("confirm_count", 0)
		}); err != nil {
			return err
		}
	}
	switch pr.State {
	case store.PRClosed, store.PRReleased:
		return e.reopen(ctx, repo, w, pr, res.HeadChanged, now)
	case store.PRReleasing:
		e.log.Info("PR reopened while its slot is being released; revisiting next poll", "subject", prSubject(repo, pr.Number))
		return nil
	}
	if res.HeadChanged {
		return e.onHeadChange(ctx, repo, w, pr, now)
	}
	// A dismissal GitHub did not answer yet, or an approval posted on a head
	// that moved during its round (onHeadChange handles new heads).
	e.followApproval(ctx, repo, pr)
	if !res.Changed || pr.Forced || pr.DetailsAt == nil { // no filters without Details (prGate holds such PRs)
		return nil
	}
	// Eligibility may have changed (labels, draft, ...).
	dec := e.classify(ctx, w, pr, now)
	waiting := pr.State == store.PRQueued || pr.State == store.PRRereviewPending
	switch {
	case pr.State == store.PRIneligible && dec.Eligible && pr.HeadSHA == deref(pr.ReviewedSHA):
		return e.st.TransitionPR(ctx, pr.ID, []string{store.PRIneligible}, store.PRReviewed,
			func(u *store.PRUpdate) { u.Set("skip_reason", nil) })
	case pr.State == store.PRIneligible && dec.Eligible:
		return e.queue(ctx, pr, w, []string{store.PRIneligible}, false, now, e.eligibleWhy(ctx, w, pr))
	case waiting && !dec.Eligible:
		return e.markIneligible(ctx, pr, []string{pr.State}, dec.Reason, false, now)
	case waiting && prev.IsDraft != pr.IsDraft:
		// The (draft) re-review interval changed: next_eligible_at again,
		// without restarting the push quiet period.
		return e.queue(ctx, pr, w, []string{pr.State}, false, now, map[bool]string{true: "now a draft", false: "ready for review"}[pr.IsDraft])
	}
	return nil
}

// eligibleWhy is why the watch's filters accept a PR they rejected: the
// review request that lets a draft through (Request.Phrase), else "now
// eligible" (the PR or the filters changed).
func (e *Engine) eligibleWhy(ctx context.Context, w config.Watch, pr store.PR) string {
	if pr.IsDraft && !w.DraftsIncluded() {
		if req, ok := e.pendingRequest(ctx, pr); ok {
			return req.Phrase()
		}
	}
	return "now eligible"
}

// onHeadChange reacts to a push (recorded for the burst quiet period).
func (e *Engine) onHeadChange(ctx context.Context, repo store.Repo, w config.Watch, pr store.PR, now time.Time) error {
	subject := prSubject(repo, pr.Number)
	e.recordPush(ctx, pr.ID, now)
	e.event(ctx, "info", subject, "pr.head_changed", "new head "+textx.ShortSHA(pr.HeadSHA), map[string]any{"head_sha": pr.HeadSHA, "state": pr.State})
	switch pr.State {
	case store.PRBaseline, store.PRIneligible, store.PRNeedsAttention, store.PRReviewed,
		store.PRQueued, store.PRRereviewPending:
		if !pr.Forced {
			if dec := e.classify(ctx, w, pr, now); !dec.Eligible {
				return e.markIneligible(ctx, pr, []string{pr.State}, dec.Reason, true, now)
			}
			if rs := deref(pr.ReviewedSHA); rs != "" && rs == pr.HeadSHA {
				return e.alreadyReviewed(ctx, repo, pr)
			}
			// A push of comments, whitespace or docs only: the review
			// stands for the new head (and its approval with it).
			if handled, err := e.settlePush(ctx, repo, w, pr); handled {
				return err
			}
		}
		// An App approval of an older commit goes before the re-review is
		// queued.
		e.followApproval(ctx, repo, pr)
		return e.queue(ctx, pr, w, []string{pr.State}, true, now, "new head "+textx.ShortSHA(pr.HeadSHA))
	case store.PRClaiming, store.PRReviewing, store.PRVerifying, store.PRPaused:
		e.followApproval(ctx, repo, pr)
		// The round in flight reviews the old head: while its reviewers run
		// it restarts on the new one (pipeline, daemon.max_round_restarts),
		// else its result moves the PR to rereview_pending.
		return e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) {
			u.Set("pending_since", now)
			u.Set("attempts", 0)
		})
	}
	return nil
}

// alreadyReviewed settles a PR whose new head is the commit it was last
// reviewed at: a round's checkout fetched that head before the radar saw it
// (onPosted then left the PR rereview_pending against the older head), or a
// force-push went back to it. Nothing is queued.
func (e *Engine) alreadyReviewed(ctx context.Context, repo store.Repo, pr store.PR) error {
	err := e.st.TransitionPR(ctx, pr.ID, []string{pr.State}, store.PRReviewed, func(u *store.PRUpdate) {
		u.Set("skip_reason", nil)
		u.Set("next_eligible_at", nil)
		u.Set("attempts", 0)
		u.Set("next_attempt_at", nil)
	})
	if err != nil {
		return err
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "pr.reviewed",
		fmt.Sprintf("%s → reviewed: head %s was already reviewed", pr.State, textx.ShortSHA(pr.HeadSHA)), nil)
	return nil
}

// reopen brings back a PR that GitHub lists as open again.
func (e *Engine) reopen(ctx context.Context, repo store.Repo, w config.Watch, pr store.PR, headChanged bool, now time.Time) error {
	subject := prSubject(repo, pr.Number)
	to := deref(pr.PrevState)
	if to == "" || slices.Contains(closingStates, to) {
		to = store.PRReviewed
		if deref(pr.ReviewedSHA) == "" {
			to = store.PRBaseline
		}
	}
	if slices.Contains(store.InFlightStates, to) && !e.roundActive(pr.ID) {
		to = claimableState(pr)
	}
	err := e.st.TransitionPR(ctx, pr.ID, []string{pr.State}, to, func(u *store.PRUpdate) {
		u.Set("prev_state", nil)
		u.Set("release_after", nil)
		u.Set("missing_since", nil)
		u.Set("confirm_count", 0)
		u.Set("merged_at", nil)
		u.Set("closed_at", nil)
	})
	if err != nil {
		return err
	}
	e.event(ctx, "info", subject, "pr.reopened", fmt.Sprintf("reopened: %s → %s", pr.State, to), nil)
	cur, err := e.st.PRByID(ctx, pr.ID)
	if err != nil {
		return err
	}
	switch {
	case headChanged || (to == store.PRReviewed && deref(cur.ReviewedSHA) != cur.HeadSHA):
		return e.onHeadChange(ctx, repo, w, cur, now)
	case to == store.PRQueued || to == store.PRRereviewPending:
		return e.queue(ctx, cur, w, []string{to}, false, now, "reopened")
	}
	return nil
}

// confirmMissing handles open PRs that left the radar: confirmed
// CLOSED/MERGED twice at least confirmGap apart → closed with a release
// grace; NOT_FOUND → UNKNOWN (never cleaned); OPEN again → cleared. The gap
// is measured from the first successful CLOSED/MERGED answer
// (missing_since), so a confirmation outage never shortens it.
func (e *Engine) confirmMissing(ctx context.Context, gh GitHub, repo store.Repo, missing []store.PR, now time.Time) error {
	if len(missing) == 0 {
		return nil
	}
	nums := make([]int, 0, len(missing))
	for _, pr := range missing {
		nums = append(nums, pr.Number)
	}
	states, notFound, err := gh.ConfirmStates(ctx, repo.Owner, repo.Name, nums)
	if err != nil {
		if e.changed("confirm:"+repo.FullName(), err.Error()) {
			e.event(ctx, "warn", "repo:"+repo.FullName(), "poll.confirm_error", fmt.Sprintf("confirm %d PRs: %v", len(nums), err), nil)
		}
		return nil
	}
	e.changed("confirm:"+repo.FullName(), "")
	var errs []error
	for _, pr := range missing {
		subject := prSubject(repo, pr.Number)
		if slices.Contains(notFound, pr.Number) {
			if err := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) { u.Set("gh_state", store.GHUnknown) }); err != nil {
				errs = append(errs, err)
				continue
			}
			e.event(ctx, "warn", subject, "pr.unknown", "GitHub no longer finds this PR; marked UNKNOWN (never cleaned up)", nil)
			continue
		}
		st, ok := states[pr.Number]
		if !ok {
			continue
		}
		switch st.State {
		case store.GHOpen:
			if err := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) {
				u.Set("missing_since", nil)
				u.Set("confirm_count", 0)
			}); err != nil {
				errs = append(errs, err)
			}
		case store.GHClosed, store.GHMerged:
			if pr.ConfirmCount == 0 || pr.MissingSince == nil {
				if err := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) {
					u.Set("missing_since", now)
					u.Set("confirm_count", 1)
				}); err != nil {
					errs = append(errs, err)
				}
				continue
			}
			count := pr.ConfirmCount + 1
			if count >= 2 && now.Sub(*pr.MissingSince) >= confirmGap && slices.Contains(openStates, pr.State) {
				err := e.st.TransitionPR(ctx, pr.ID, openStates, store.PRClosed, func(u *store.PRUpdate) {
					u.Copy("prev_state", "state")
					u.Set("release_after", now.Add(e.cfg.Daemon.CloseGrace.Duration))
					u.Set("gh_state", st.State)
					u.Set("confirm_count", count)
					if !st.MergedAt.IsZero() {
						u.Set("merged_at", st.MergedAt)
					}
					if !st.ClosedAt.IsZero() {
						u.Set("closed_at", st.ClosedAt)
					}
					// The merge or close is the PR's last activity: its
					// Details are not read again.
					if at := cmp.Or(st.MergedAt, st.ClosedAt); !at.IsZero() && (pr.ActivityAt == nil || at.After(*pr.ActivityAt)) {
						u.Set("activity_at", at)
					}
				})
				if err != nil {
					errs = append(errs, err)
					continue
				}
				e.event(ctx, "info", subject, "pr.closed", fmt.Sprintf("%s on GitHub (confirmed %d×); slot released after %s", strings.ToLower(st.State), count, e.cfg.Daemon.CloseGrace.Duration), nil)
				if cur, err := e.st.PRByID(ctx, pr.ID); err == nil {
					e.mergedUnreviewed(ctx, repo, cur)
				}
				continue
			}
			if err := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) { u.Set("confirm_count", count) }); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// mergedUnreviewed flags a PR, just confirmed closed, that GitHub merged
// before magnum reviewed its last push (PR.MergedUnreviewed: prev_state is
// the state it closed in): a warn event and one toast per PR, since this is
// a PR magnum meant to review and did not get to.
func (e *Engine) mergedUnreviewed(ctx context.Context, repo store.Repo, pr store.PR) {
	if !pr.MergedUnreviewed() {
		return
	}
	reviewed := deref(pr.ReviewedSHA)
	label := fmt.Sprintf("%s#%d", repo.Name, pr.Number)
	last := "never reviewed"
	data := map[string]any{"head_sha": pr.HeadSHA, "prev_state": deref(pr.PrevState)}
	if reviewed != "" {
		last = "last review " + textx.ShortSHA(reviewed)
		data["reviewed_sha"] = reviewed
	}
	e.event(ctx, "warn", prSubject(repo, pr.Number), "pr.merged_unreviewed",
		fmt.Sprintf("merged before magnum reviewed %s (%s)", textx.ShortSHA(pr.HeadSHA), last), data)
	e.urgent(fmt.Sprintf("merged-unreviewed:%d", pr.ID), "magnum: "+label+" merged unreviewed",
		fmt.Sprintf("%s merged before magnum reviewed its last push: merged head %s, %s", label, textx.ShortSHA(pr.HeadSHA), last),
		mergedUnreviewedWindow)
}
