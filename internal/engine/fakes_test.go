package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/cleanup"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/identity"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
)

// ---- clock ----

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// ---- GitHub ----

type prSpec struct {
	n       int
	head    string
	draft   bool
	author  string
	labels  []string
	updated time.Time
	request bool // zhuravel is a requested reviewer
	// requests are the PR's ReviewRequestedEvents (Details'
	// ReviewRequestEvents), oldest first.
	requests  []github.ReviewRequestEvent
	assignees []string
	reviews   []github.LatestReview
	// assoc is the author's authorAssociation ("" = MEMBER).
	assoc string
	// labelsTruncated and reviewsTruncated make Details report a cut-off
	// label or latestReviews page (LabelsComplete, LatestReviewsComplete).
	labelsTruncated, reviewsTruncated bool
	// ci is the head's check rollup ("" = no checks) CIStates and Details
	// report, checks the Details' checks; ciUnknown makes CIStates unable to
	// read the rollup (a fork's PR).
	ci        string
	checks    []github.Check
	ciUnknown bool
}

type fakeGH struct {
	mu         sync.Mutex
	repos      map[string][]prSpec // full name -> open PRs
	closed     map[int]string      // number -> CLOSED|MERGED for ConfirmStates
	notFound   []int
	radarErr   error
	ciErr      error                          // CIStates fails
	compare    map[string]github.CompareStats // "base...head" -> stats; absent = ErrNotFound
	compareErr error
	detailsErr error                   // Details fails
	confirmErr error                   // ConfirmStates fails
	allReviews map[int][]github.Review // number -> ReviewsWithMarker, oldest first
	dismissErr error                   // DismissReview fails
	createErr  error                   // CreateReview fails
	created    []string                // CreateReview calls: "<event>@<sha>:<body>"
	// files answers CompareFiles by "base...head" (absent = ErrNotFound).
	files map[string][]github.FileDelta
	// required answers RequiredChecks by full name (absent = GitHub does not
	// say); requiredErr fails it.
	required    map[string][]string
	requiredErr error
	// threads answers ReviewThreads by number; contents answers FileAt by
	// "<path>@<ref>" (absent = ErrNotFound); statuses gives
	// CompareFilesStatus its status by "base...head" (absent = "ahead").
	threads  map[int][]github.Thread
	contents map[string][]byte
	statuses map[string]string
	// mergeCommits answers ConfirmStates' MergeCommitOid of a merged PR by
	// number.
	mergeCommits map[int]string
	// merges marks the "base...head" ranges ComparePush finds a merge
	// commit in.
	merges map[string]bool
	calls  []string
}

// ComparePush answers like CompareFilesStatus (one "compare_files:" call),
// with the commit count and the stats (what Compare reads) of compare and
// the merge flag of merges.
func (g *fakeGH) ComparePush(ctx context.Context, owner, repo, base, head string) (github.PushComparison, error) {
	status, fs, err := g.CompareFilesStatus(ctx, owner, repo, base, head)
	if err != nil {
		return github.PushComparison{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	key := base + "..." + head
	return github.PushComparison{Status: status, Commits: g.compare[key].Commits, Merge: g.merges[key], Files: fs, Stats: g.compare[key]}, nil
}

func (g *fakeGH) CompareFilesStatus(ctx context.Context, owner, repo, base, head string) (string, []github.FileDelta, error) {
	fs, err := g.CompareFiles(ctx, owner, repo, base, head)
	if err != nil {
		return "", nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return cmp.Or(g.statuses[base+"..."+head], "ahead"), fs, nil
}

func (g *fakeGH) ReviewThreads(_ context.Context, owner, repo string, number int) ([]github.Thread, error) {
	g.record(fmt.Sprintf("threads:%s/%s#%d", owner, repo, number))
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.threads[number]), nil
}

func (g *fakeGH) Reviews(_ context.Context, owner, repo string, number int) ([]github.Review, error) {
	g.record(fmt.Sprintf("all_reviews:%s/%s#%d", owner, repo, number))
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.allReviews[number]), nil
}

func (g *fakeGH) FileAt(_ context.Context, owner, repo, path, ref string) ([]byte, error) {
	g.record(fmt.Sprintf("file:%s/%s:%s@%s", owner, repo, path, ref))
	g.mu.Lock()
	defer g.mu.Unlock()
	b, ok := g.contents[path+"@"+ref]
	switch {
	case slices.Contains(strings.Split(path, "/"), ".."): // as the real client refuses it
		return nil, fmt.Errorf("github: invalid file path %q", path)
	case !ok:
		return nil, &github.APIError{Op: "file", Status: 404, Message: "Not Found"}
	case len(b) > github.FileAtLimit:
		return nil, fmt.Errorf("file %s: %w", path, github.ErrFileTooLarge)
	}
	return b, nil
}

func (g *fakeGH) RequiredChecks(_ context.Context, owner, repo, branch string) ([]string, bool, error) {
	g.record(fmt.Sprintf("required:%s/%s@%s", owner, repo, branch))
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.requiredErr != nil {
		return nil, false, g.requiredErr
	}
	checks, ok := g.required[owner+"/"+repo]
	return slices.Clone(checks), ok, nil
}

func (g *fakeGH) CompareFiles(_ context.Context, owner, repo, base, head string) ([]github.FileDelta, error) {
	g.record(fmt.Sprintf("compare_files:%s/%s:%s...%s", owner, repo, base, head))
	g.mu.Lock()
	defer g.mu.Unlock()
	fs, ok := g.files[base+"..."+head]
	if !ok {
		return nil, &github.APIError{Op: "compare files", Status: 404, Message: "Not Found"}
	}
	return fs, nil
}

func (g *fakeGH) DismissReview(_ context.Context, owner, repo string, number int, reviewID int64, message string) error {
	g.record(fmt.Sprintf("dismiss:%s/%s#%d:%d:%s", owner, repo, number, reviewID, message))
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.dismissErr
}

func (g *fakeGH) CreateReview(_ context.Context, owner, repo string, number int, commitID, event, body string) (github.RESTReview, error) {
	g.record(fmt.Sprintf("review:%s/%s#%d:%s", owner, repo, number, event))
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.createErr != nil {
		return github.RESTReview{}, g.createErr
	}
	g.created = append(g.created, event+"@"+commitID+":"+body)
	return github.RESTReview{ID: int64(9000 + len(g.created)), UserLogin: "talkable[bot]", State: event, CommitID: commitID,
		HTMLURL: fmt.Sprintf("https://github.com/%s/%s/pull/%d#pullrequestreview-%d", owner, repo, number, 9000+len(g.created))}, nil
}

// fail sets the Details and ConfirmStates errors (nil clears them).
func (g *fakeGH) fail(details, confirm error) {
	g.mu.Lock()
	g.detailsErr, g.confirmErr = details, confirm
	g.mu.Unlock()
}

func newFakeGH() *fakeGH {
	return &fakeGH{repos: map[string][]prSpec{}, closed: map[int]string{}, compare: map[string]github.CompareStats{}}
}

// fakeBaseOid is the base branch tip every fake PR reports.
const fakeBaseOid = "base-tip"

func (g *fakeGH) set(full string, prs ...prSpec) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.repos[full] = prs
}

func (g *fakeGH) record(s string) {
	g.mu.Lock()
	g.calls = append(g.calls, s)
	g.mu.Unlock()
}

func (g *fakeGH) count(prefix string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (g *fakeGH) Radar(_ context.Context, org string) ([]github.RepoRadar, github.RateLimit, error) {
	g.record("radar:" + org)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.radarErr != nil {
		return nil, github.RateLimit{}, g.radarErr
	}
	var out []github.RepoRadar
	names := make([]string, 0, len(g.repos))
	for full := range g.repos {
		names = append(names, full)
	}
	slices.Sort(names)
	for _, full := range names {
		if !strings.HasPrefix(full, org+"/") {
			continue
		}
		rr := github.RepoRadar{NodeID: "R_" + full, NameWithOwner: full, DefaultBranch: "main"}
		for _, p := range g.repos[full] {
			rr.PRs = append(rr.PRs, github.PRRadar{NodeID: fmt.Sprintf("PR_%s_%d", full, p.n), Number: p.n,
				IsDraft: p.draft, UpdatedAt: p.updated, HeadRefOid: p.head, BaseRefName: "master"})
		}
		out = append(out, rr)
	}
	return out, github.RateLimit{Limit: 5000, Remaining: 4900, Cost: 1}, nil
}

// CIStates answers the rollup of each asked PR whose head is still the
// one asked about, from its spec (none for ciUnknown).
func (g *fakeGH) CIStates(_ context.Context, prs []github.PRRadar) (map[string]string, github.RateLimit, error) {
	ids := make([]string, len(prs))
	for i, p := range prs {
		ids[i] = p.NodeID
	}
	g.record("ci:" + strings.Join(ids, ","))
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ciErr != nil {
		return nil, github.RateLimit{}, g.ciErr
	}
	out := map[string]string{}
	for full, specs := range g.repos {
		for _, s := range specs {
			id := fmt.Sprintf("PR_%s_%d", full, s.n)
			if !s.ciUnknown && slices.ContainsFunc(prs, func(p github.PRRadar) bool { return p.NodeID == id && p.HeadRefOid == s.head }) {
				out[id] = s.ci
			}
		}
	}
	return out, github.RateLimit{Limit: 5000, Remaining: 4899, Cost: 1}, nil
}

func (g *fakeGH) Details(_ context.Context, owner, repo string, numbers []int) (map[int]github.PRDetails, []int, error) {
	g.record(fmt.Sprintf("details:%s/%s:%v", owner, repo, numbers))
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.detailsErr != nil {
		return nil, nil, g.detailsErr
	}
	full := owner + "/" + repo
	out := map[int]github.PRDetails{}
	var missing []int
	for _, n := range numbers {
		found := false
		for _, p := range g.repos[full] {
			if p.n != n {
				continue
			}
			found = true
			author := p.author
			if author == "" {
				author = "alice"
			}
			assoc := p.assoc
			if assoc == "" {
				assoc = "MEMBER"
			}
			d := github.PRDetails{NodeID: fmt.Sprintf("PR_%s_%d", full, n), Number: n, Title: fmt.Sprintf("PR %d", n), AuthorAssociation: assoc,
				URL: fmt.Sprintf("https://github.com/%s/pull/%d", full, n), AuthorLogin: author, AuthorType: "User",
				Labels: p.labels, LabelsComplete: !p.labelsTruncated, LatestReviewsComplete: !p.reviewsTruncated,
				HeadRefName: "feature", BaseRefName: "master", State: "OPEN", IsDraft: p.draft,
				HeadRefOid: p.head, BaseRefOid: fakeBaseOid, Assignees: append([]string{}, p.assignees...),
				ReviewRequests: []github.Reviewer{}, LatestReviews: append([]github.LatestReview{}, p.reviews...),
				Additions: 10 * n, Deletions: n, ChangedFiles: n, Commits: 1,
				CI: github.CIRollup{SHA: p.head, State: p.ci, Total: len(p.checks), Complete: true, Checks: append([]github.Check{}, p.checks...)}}
			if p.request {
				d.ReviewRequests = []github.Reviewer{{Type: "User", Login: "zhuravel"}, {Type: "Team", Login: "engineers"}}
			}
			d.ReviewRequestEvents = append([]github.ReviewRequestEvent{}, p.requests...)
			out[n] = d
		}
		if !found {
			missing = append(missing, n)
		}
	}
	return out, missing, nil
}

func (g *fakeGH) Compare(_ context.Context, owner, repo, base, head string) (github.CompareStats, error) {
	g.record(fmt.Sprintf("compare:%s/%s:%s...%s", owner, repo, base, head))
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.compareErr != nil {
		return github.CompareStats{}, g.compareErr
	}
	cs, ok := g.compare[base+"..."+head]
	if !ok {
		return github.CompareStats{}, &github.APIError{Op: "compare", Status: 404, Message: "Not Found"}
	}
	return cs, nil
}

func (g *fakeGH) ReviewsWithMarker(_ context.Context, owner, repo string, number int, marker string) ([]github.Review, error) {
	g.record(fmt.Sprintf("reviews:%s/%s#%d", owner, repo, number))
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []github.Review
	for _, r := range g.allReviews[number] {
		if strings.Contains(r.Body, marker) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (g *fakeGH) ConfirmStates(_ context.Context, owner, repo string, numbers []int) (map[int]github.PRState, []int, error) {
	g.record(fmt.Sprintf("confirm:%s/%s:%v", owner, repo, numbers))
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.confirmErr != nil {
		return nil, nil, g.confirmErr
	}
	out := map[int]github.PRState{}
	var nf []int
	for _, n := range numbers {
		if slices.Contains(g.notFound, n) {
			nf = append(nf, n)
			continue
		}
		st := g.closed[n]
		if st == "" {
			st = "OPEN"
		}
		ps := github.PRState{State: st, Merged: st == "MERGED", MergeCommitOid: g.mergeCommits[n]}
		if st == "MERGED" {
			ps.MergedAt = time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
		}
		out[n] = ps
	}
	return out, nf, nil
}

// ---- herdr ----

type fakeHerdr struct {
	mu         sync.Mutex
	err        error
	agents     []herdr.AgentInfo
	workspaces []herdr.Workspace
	panes      []herdr.Pane
	closed     []string // WorkspaceClose calls
	calls      int
}

func (h *fakeHerdr) Snapshot(context.Context) (herdr.Snapshot, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if h.err != nil {
		return herdr.Snapshot{}, h.err
	}
	return herdr.Snapshot{Agents: slices.Clone(h.agents), Workspaces: slices.Clone(h.workspaces), Panes: slices.Clone(h.panes)}, nil
}

// WorkspaceClose closes a workspace with its panes and agents.
func (h *fakeHerdr) WorkspaceClose(_ context.Context, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = append(h.closed, id)
	h.agents = slices.DeleteFunc(h.agents, func(a herdr.AgentInfo) bool { return a.WorkspaceID == id })
	h.panes = slices.DeleteFunc(h.panes, func(p herdr.Pane) bool { return p.WorkspaceID == id })
	h.workspaces = slices.DeleteFunc(h.workspaces, func(w herdr.Workspace) bool { return w.ID == id })
	return nil
}

type fakeNotifyHerdr struct {
	mu     sync.Mutex
	toasts []string
	meta   []string
	// decline, when set, is herdr's reason for not showing a toast
	// (rate_limited, busy, ...); nothing is recorded then.
	decline string
}

func (f *fakeNotifyHerdr) NotificationShow(_ context.Context, title, body string) (herdr.NotificationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.decline != "" {
		return herdr.NotificationResult{Reason: f.decline}, nil
	}
	f.toasts = append(f.toasts, title+" | "+body)
	return herdr.NotificationResult{Shown: true}, nil
}

// titles lists the toasts shown so far whose title contains sub.
func (f *fakeNotifyHerdr) titles(sub string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, t := range f.toasts {
		title, _, _ := strings.Cut(t, " | ")
		if strings.Contains(title, sub) {
			out = append(out, title)
		}
	}
	return out
}

func (f *fakeNotifyHerdr) WorkspaceReportMetadata(_ context.Context, ws, _ string, tokens map[string]string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.meta = append(f.meta, fmt.Sprintf("%s:%v", ws, tokens))
	return nil
}

func (f *fakeNotifyHerdr) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append(append([]string(nil), f.toasts...), f.meta...)
}

// ---- agents ----

type fakeAgents struct {
	st *store.Store

	mu           sync.Mutex
	calls        []string
	obs          []agents.Observation
	loggedOut    map[string]bool
	resumeIDs    map[agents.Role]string
	failResume   bool
	parkErr      error
	parkGate     chan struct{} // when set, Park signals parkStarted and waits for a value (or ctx)
	parkStarted  chan struct{}
	ensureErr    error
	wsMovedFrom  string
	startedRoles []agents.Role
	efforts      []string // StartAgent calls as "<role>:<effort>:<resume>"
}

func (f *fakeAgents) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeAgents) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeAgents) count(prefix string) int {
	n := 0
	for _, c := range f.all() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeAgents) ObserveSnapshotAt(context.Context, herdr.Snapshot, time.Time) ([]agents.Observation, error) {
	f.record("observe")
	f.mu.Lock()
	defer f.mu.Unlock()
	obs := f.obs
	f.obs = nil
	return obs, nil
}

// paneOf is the fake pane id of a role: p-judge, p-claude, p-codex and
// p-simplify for the built-in roles, else p-<role>.
func paneOf(role string) string {
	switch role {
	case store.RoleJudge:
		return "p-judge"
	case store.RoleClaude:
		return "p-claude"
	case store.RoleCodexReview:
		return "p-codex"
	case store.RoleSimplify:
		return "p-simplify"
	}
	return "p-" + role
}

func (f *fakeAgents) EnsureWorkspace(_ context.Context, pr store.PR, slotPath string, env map[string]string, label string, roles []config.Role) (agents.Workspace, error) {
	names := make([]string, len(roles))
	for i, r := range roles {
		names[i] = r.Name
	}
	f.record(fmt.Sprintf("ensure_workspace:%d:%s:%s:%s:%s", pr.ID, slotPath, label, env["WT_BRANCH"], strings.Join(names, ",")))
	if f.ensureErr != nil {
		return agents.Workspace{}, f.ensureErr
	}
	ws := agents.Workspace{WorkspaceID: "w1", TabID: "t1", Created: true, MovedFrom: f.wsMovedFrom, Panes: map[agents.Role]string{}}
	for _, r := range roles {
		ws.Panes[agents.Role(r.Name)] = paneOf(r.Name)
		ws.Roles = append(ws.Roles, agents.Role(r.Name))
	}
	return ws, nil
}

func (f *fakeAgents) EnsurePane(_ context.Context, pr store.PR, ws agents.Workspace, _ string, _ map[string]string, role config.Role) (agents.Workspace, error) {
	f.record(fmt.Sprintf("pane:%d:%s", pr.ID, role.Name))
	out := ws
	out.Panes = maps.Clone(ws.Panes)
	if out.Panes == nil {
		out.Panes = map[agents.Role]string{}
	}
	out.Panes[agents.Role(role.Name)] = paneOf(role.Name)
	out.Roles = append(slices.Clone(ws.Roles), agents.Role(role.Name))
	return out, nil
}

func (f *fakeAgents) StartAgent(ctx context.Context, pr store.PR, role config.Role, paneID, resume string) error {
	f.record(fmt.Sprintf("start:%d:%s:%s", pr.ID, role.Name, resume))
	if resume != "" && f.failResume {
		return errors.New("resume failed")
	}
	name := fmt.Sprintf("mg-t-%d-%s", pr.Number, role.Name)
	_, err := f.st.CreateSession(ctx, store.Session{PRID: pr.ID, Role: role.Name, AgentName: &name,
		AgentKind: store.Ptr(role.AgentKind()), HerdrPaneID: &paneID, HerdrWorkspaceID: store.Ptr("w1"), State: store.SessionLive})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		return err
	}
	f.mu.Lock()
	f.startedRoles = append(f.startedRoles, agents.Role(role.Name))
	f.efforts = append(f.efforts, role.Name+":"+role.Effort+":"+resume)
	f.mu.Unlock()
	return nil
}

func (f *fakeAgents) ResumeID(_ context.Context, _ int64, role agents.Role) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resumeIDs[role], nil
}

func (f *fakeAgents) Preflight(_ context.Context, kind string) error {
	f.record("preflight:" + kind)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loggedOut[kind] {
		return fmt.Errorf("%s: %w", kind, agents.ErrLoginRequired)
	}
	return nil
}

func (f *fakeAgents) Park(ctx context.Context, pr store.PR) error {
	f.record(fmt.Sprintf("park:%d", pr.ID))
	f.mu.Lock()
	gate, started := f.parkGate, f.parkStarted
	f.mu.Unlock()
	if gate != nil {
		if started != nil {
			started <- struct{}{}
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.parkErr != nil {
		return f.parkErr
	}
	ss, err := f.st.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return err
	}
	for _, s := range ss {
		if s.State == store.SessionLive || s.State == store.SessionStarting {
			_ = f.st.TransitionSession(ctx, s.ID, []string{s.State}, store.SessionParked, nil)
		}
	}
	return nil
}

func (f *fakeAgents) Recover(_ context.Context, pr store.PR) ([]agents.Recovered, error) {
	f.record(fmt.Sprintf("recover:%d", pr.ID))
	return nil, nil
}

func (f *fakeAgents) LatestRound(ctx context.Context, prID int64) (int, error) {
	runs, err := f.st.RunsByPR(ctx, prID)
	n := 0
	for _, r := range runs {
		n = max(n, r.Round)
	}
	return n, err
}

// ---- rounds ----

type fakeRounds struct {
	st     *store.Store
	mu     sync.Mutex
	inputs []pipeline.RoundInput
	script func(in pipeline.RoundInput) (pipeline.RoundResult, error)
	gate   chan struct{} // when set, RunRound waits for a value (or ctx)
	// appends records AppendToReview calls ("<review id>:<text>");
	// appendErr is their error.
	appends   []string
	appendErr error
}

func (f *fakeRounds) AppendToReview(_ context.Context, owner, repo string, number int, reviewID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appends = append(f.appends, fmt.Sprintf("%d:%s", reviewID, text))
	return f.appendErr
}

func (f *fakeRounds) appended() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.appends...)
}

func (f *fakeRounds) RunRound(ctx context.Context, in pipeline.RoundInput) (pipeline.RoundResult, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, in)
	n := len(f.inputs)
	script, gate := f.script, f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return pipeline.RoundResult{Outcome: pipeline.OutcomeStopped}, ctx.Err()
		}
	}
	if script != nil {
		return script(in)
	}
	return f.posted(ctx, in, n)
}

// posted is the default round: every non-judge role pipeline.RolesToRun
// picks reports ok and the review posts. A runs = "first" role gets a
// verified run row, so store.RoleRanBefore skips it next time as it would
// after a real round.
func (f *fakeRounds) posted(ctx context.Context, in pipeline.RoundInput, n int) (pipeline.RoundResult, error) {
	round := in.Round
	if round == 0 && f.st != nil {
		round = 1
		if runs, err := f.st.RunsByPR(ctx, in.PR.ID); err == nil {
			for _, r := range runs {
				round = max(round, r.Round+1)
			}
		}
	}
	round = max(round, 1)
	reports := map[agents.Role]pipeline.RoleReport{}
	toRun, err := pipeline.RolesToRun(ctx, f.st, nil, in.PR, in.Roles, in.Requested, in.Kind)
	if err != nil {
		return pipeline.RoundResult{Outcome: pipeline.OutcomeError, Error: err.Error()}, err
	}
	for _, r := range toRun {
		if r.Judge {
			continue
		}
		reports[agents.Role(r.Name)] = pipeline.RoleReport{Role: r.Name, Kind: r.AgentKind(), Capture: r.Capture, Status: pipeline.ReportOK}
		if r.Runs != config.RunsFirst || f.st == nil {
			continue
		}
		run, err := f.st.CreateRun(ctx, store.Run{PRID: in.PR.ID, Round: round, Role: r.Name, Kind: in.Kind, TargetSHA: in.TargetSHA,
			Identity: in.PR.Identity, ReviewerLogin: "x", State: store.RunPending, PromptText: r.Name})
		if err == nil {
			err = f.st.TransitionRun(ctx, run.ID, nil, store.RunVerified, func(u *store.RunUpdate) { u.Set("outcome", pipeline.ReportOK) })
		}
		if err != nil {
			return pipeline.RoundResult{Outcome: pipeline.OutcomeError, Error: err.Error()}, err
		}
	}
	return pipeline.RoundResult{Outcome: pipeline.OutcomePosted, Round: round, ReviewID: int64(100 + n),
		Event: "COMMENTED", ReviewCommit: in.TargetSHA, Findings: map[string]int{"P2": 1}, Reports: reports}, nil
}

func (f *fakeRounds) all() []pipeline.RoundInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pipeline.RoundInput(nil), f.inputs...)
}

// ---- slots ----

type fakeSlots struct {
	st *store.Store

	mu       sync.Mutex
	calls    []string
	moveHead string // Checkout "fetches" this head instead of the target
	holdErr  error
	// guardErr is what Guard returns for a slot without a persisted hold
	// (a person's agent in it, say); a hold_reason is returned as
	// slots.Guard does.
	guardErr error
	// checkoutGate, when set, makes Checkout signal checkoutStarted and wait
	// for a value (or ctx): a round held in its checkout / deps step.
	checkoutGate    chan struct{}
	checkoutStarted chan struct{}
	// schemaChange makes Checkout find that the PR changes the pool's
	// schema_paths: the slot becomes dirty_schema, as slots.Checkout's
	// schema step marks it.
	schemaChange bool
}

func (f *fakeSlots) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeSlots) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeSlots) Claim(ctx context.Context, pr store.PR, pool config.Pool) (store.Slot, error) {
	f.record(fmt.Sprintf("claim:%d", pr.ID))
	free, err := f.st.FreeSlots(ctx, pool.Repo, pr.ID)
	if err != nil {
		return store.Slot{}, err
	}
	for _, s := range free {
		if _, err := f.st.ClaimSlot(ctx, pr.ID, s.ID, pool.DBNames(s.Name)...); err != nil {
			continue
		}
		return f.st.SlotByID(ctx, s.ID)
	}
	return store.Slot{}, slots.ErrNoFreeSlot
}

func (f *fakeSlots) Checkout(ctx context.Context, slot store.Slot, pr store.PR, _ config.Pool, target string) error {
	f.record(fmt.Sprintf("checkout:%s:%d:%s", slot.Name, pr.ID, target))
	f.mu.Lock()
	gate, started := f.checkoutGate, f.checkoutStarted
	f.mu.Unlock()
	if gate != nil {
		if started != nil {
			started <- struct{}{}
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.holdErr != nil {
		return f.holdErr
	}
	sha := target
	if f.moveHead != "" {
		sha = f.moveHead
	}
	f.mu.Lock()
	schema := f.schemaChange
	f.mu.Unlock()
	return f.st.UpdateSlotFields(ctx, slot.ID, func(u *store.SlotUpdate) {
		u.Set("checked_out_sha", sha)
		if schema {
			u.Set("dirty_schema", true)
		}
	})
}

func (f *fakeSlots) CreatePRWorktree(ctx context.Context, _ config.Watch, repo string, pr store.PR, target string) (store.Slot, error) {
	f.record(fmt.Sprintf("worktree:%s#%d", repo, pr.Number))
	name := slots.PRSlotName(repo, pr.Number)
	sl, err := f.st.CreateSlot(ctx, store.Slot{Name: name, RepoFullName: repo,
		Kind: store.SlotKindPerPR, Path: "/tmp/wt/" + repo + fmt.Sprint(pr.Number), MainClone: "/tmp/main", State: store.SlotProvisioning})
	if errors.Is(err, store.ErrConflict) {
		// Like slots.Manager (perPRRow): a lost or broken row of the same
		// name is revived (path kept) instead of failing on the conflict.
		old, gerr := f.st.SlotByName(ctx, name)
		if gerr != nil || (old.State != store.SlotLost && old.State != store.SlotBroken) {
			return store.Slot{}, err
		}
		err = f.st.TransitionSlot(ctx, old.ID, []string{store.SlotLost, store.SlotBroken}, store.SlotProvisioning, func(u *store.SlotUpdate) {
			u.Set("pr_id", nil)
			u.Set("checked_out_sha", nil)
			u.Set("last_error", nil)
		})
		sl = old
	}
	if err != nil {
		return store.Slot{}, err
	}
	if err := f.st.TransitionSlot(ctx, sl.ID, []string{store.SlotProvisioning}, store.SlotClaimed, func(u *store.SlotUpdate) {
		u.Set("pr_id", pr.ID)
		u.Set("checked_out_sha", target)
	}); err != nil {
		return store.Slot{}, err
	}
	return f.st.SlotByID(ctx, sl.ID)
}

func (f *fakeSlots) Release(ctx context.Context, slot store.Slot, _ config.Pool, reason string) error {
	f.record(fmt.Sprintf("release:%s:%s", slot.Name, reason))
	if err := f.st.ReleaseSlot(ctx, slot.ID, []string{store.SlotClaimed, store.SlotHeld, store.SlotReleasing}, store.SlotFree, reason); err != nil {
		return err
	}
	return f.st.UpdateSlotFields(ctx, slot.ID, func(u *store.SlotUpdate) { u.Set("checked_out_sha", nil) })
}

func (f *fakeSlots) Remove(_ context.Context, slot store.Slot, _ config.Pool, force bool) error {
	f.record(fmt.Sprintf("remove:%s:%v", slot.Name, force))
	return nil
}

func (f *fakeSlots) RemovePRWorktree(ctx context.Context, slot store.Slot, force bool) error {
	f.record(fmt.Sprintf("remove_worktree:%s:%v", slot.Name, force))
	return f.st.ReleaseSlot(ctx, slot.ID, nil, store.SlotRemoved, "closed")
}

func (f *fakeSlots) Guard(ctx context.Context, slot store.Slot) error {
	sl := slot
	if slot.ID != 0 {
		var err error
		if sl, err = f.st.SlotByID(ctx, slot.ID); err != nil {
			return err
		}
	}
	switch {
	case sl.Pinned:
		return slots.ErrHold{Reason: slots.HoldPinned, Detail: sl.Name + " is pinned"}
	case deref(sl.HoldReason) != "":
		return slots.ErrHold{Reason: *sl.HoldReason, Detail: sl.Name + " is held"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.guardErr
}

func (f *fakeSlots) ClearPin(ctx context.Context, slot store.Slot) error {
	f.record("clear_pin:" + slot.Name)
	return f.st.UpdateSlotFields(ctx, slot.ID, func(u *store.SlotUpdate) { u.Set("pinned", false) })
}

func (f *fakeSlots) HumanEvidence(context.Context, store.Slot) (string, error) { return "", nil }

func (f *fakeSlots) ProvisionPool(ctx context.Context, pool config.Pool, n int) error {
	f.record(fmt.Sprintf("provision:%d", n))
	_, err := f.st.CreateSlot(ctx, store.Slot{Name: pool.Slot(n), RepoFullName: pool.Repo, Kind: store.SlotKindPool,
		Path: pool.Path(n), MainClone: pool.MainClone, PlaceholderBranch: store.Ptr(pool.Slot(n)), DBSlug: store.Ptr(pool.Slot(n)), State: store.SlotFree})
	return err
}

func (f *fakeSlots) NextSlotNumber(ctx context.Context, pool config.Pool) (int, error) {
	sls, err := f.st.ListSlots(ctx, store.SlotFilter{RepoFullName: pool.Repo})
	return len(sls) + 1, err
}

func (f *fakeSlots) Pin(ctx context.Context, slot store.Slot) error {
	f.record("pin:" + slot.Name)
	return f.st.UpdateSlotFields(ctx, slot.ID, func(u *store.SlotUpdate) { u.Set("pinned", true) })
}

func (f *fakeSlots) Reserve(ctx context.Context, pr store.PR, pool config.Pool) (store.Slot, error) {
	f.record(fmt.Sprintf("reserve:%d", pr.ID))
	if sl, err := f.st.SlotByPR(ctx, pr.ID); err == nil && (sl.State == store.SlotClaimed || sl.State == store.SlotHeld) {
		return sl, nil
	}
	free, err := f.st.FreeSlots(ctx, pool.Repo, pr.ID)
	if err != nil {
		return store.Slot{}, err
	}
	for _, s := range free {
		if _, err := f.st.AssignSlot(ctx, pr.ID, s.ID, pool.DBNames(s.Name)...); err != nil {
			continue
		}
		return f.st.SlotByID(ctx, s.ID)
	}
	return store.Slot{}, slots.ErrNoFreeSlot
}

func (f *fakeSlots) Repair(ctx context.Context, slot store.Slot, _ config.Pool) error {
	f.record("repair:" + slot.Name)
	if f.holdErr != nil {
		return f.holdErr
	}
	return f.st.TransitionSlot(ctx, slot.ID, []string{store.SlotFree, store.SlotBroken, store.SlotProvisioning, store.SlotLost}, store.SlotFree, nil)
}

func (f *fakeSlots) Adopt(ctx context.Context, pool config.Pool, path string) (store.Slot, error) {
	f.record("adopt:" + path)
	for n := 1; n < 100; n++ {
		if pool.Path(n) == path {
			return f.st.CreateSlot(ctx, store.Slot{Name: pool.Slot(n), RepoFullName: pool.Repo, Kind: store.SlotKindPool,
				Path: path, MainClone: pool.MainClone, State: store.SlotFree})
		}
	}
	return store.Slot{}, fmt.Errorf("%s is not a slot path", path)
}

func (f *fakeSlots) Unpin(ctx context.Context, slot store.Slot) error {
	f.record("unpin:" + slot.Name)
	return f.st.UpdateSlotFields(ctx, slot.ID, func(u *store.SlotUpdate) {
		u.Set("pinned", false)
		u.Set("hold_reason", nil)
	})
}

// ---- inventory, git, identities ----

type fakeInventory struct {
	now   func() time.Time
	drift []inventory.Finding
	scans int
	// gate, when set before the engine starts, makes Scan wait until it is
	// closed (at most 10s): a slow reconcile on the heavy worker.
	gate chan struct{}
}

func (f *fakeInventory) Scan(ctx context.Context, _ inventory.Options) (inventory.Inventory, error) {
	f.scans++
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
	}
	return inventory.Inventory{ScannedAt: f.now(), Drift: f.drift}, nil
}

func (f *fakeInventory) UpsertSlotDatabases(context.Context, inventory.Inventory) (inventory.SyncResult, error) {
	return inventory.SyncResult{}, inventory.ErrNotListed
}

type fakeGit struct {
	clones map[string]string // owner/name -> discovered clone
}

func (g fakeGit) FindClone(_ context.Context, _, owner, name string) (string, error) {
	if p, ok := g.clones[owner+"/"+name]; ok {
		return p, nil
	}
	return "", fmt.Errorf("%w: %s/%s", gitx.ErrNoClone, owner, name)
}

// RevParse knows no commit: a post-merge round falls back to the recorded
// base tip (postMergeBase).
func (fakeGit) RevParse(_ context.Context, _, ref string) (string, error) {
	return "", fmt.Errorf("rev-parse %s: %w", ref, gitx.ErrNoSuchRef)
}

func (fakeGit) FetchCommit(context.Context, string, string, int) error { return nil }

func (fakeGit) MergeBase(_ context.Context, _, a, _ string) (string, error) {
	if strings.HasPrefix(a, "origin/") {
		return "base0000", nil
	}
	return a, nil // every reviewed sha is an ancestor (no force push)
}

type fakeIdentity struct {
	name, login, kind string

	mu       sync.Mutex
	checks   int
	ensures  int
	checkErr string
}

func (f *fakeIdentity) Name() string  { return f.name }
func (f *fakeIdentity) Login() string { return f.login }
func (f *fakeIdentity) Kind() string  { return f.kind }
func (f *fakeIdentity) Env(context.Context) (map[string]string, error) {
	if f.kind == "app" {
		return map[string]string{"GH_CONFIG_DIR": "/state/gh/" + f.name, "GH_TOKEN": "", "GITHUB_TOKEN": ""}, nil
	}
	return map[string]string{}, nil
}
func (f *fakeIdentity) Check(context.Context) (identity.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	if f.checkErr != "" {
		return identity.Report{Pass: false, Lines: []string{"FAIL " + f.checkErr}}, nil
	}
	return identity.Report{Pass: true, Lines: []string{"PASS ok"}}, nil
}

type fakeAppIdentity struct{ fakeIdentity }

func (f *fakeAppIdentity) EnsureConfigDir(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensures++
	return "/state/gh/" + f.name, nil
}

// ---- harness ----

const testConfigTOML = `
[daemon]
poll_interval = "30s"
max_concurrent_reviews = 3
max_total_working_codex = 5
push_quiet_period = "5m"
min_rereview_interval = "30m"
draft_min_rereview_interval = "2h"
max_rounds_per_pr_per_day = 6
close_grace = "10m"
agent_start_stagger = "15s"
min_warm = "30m"
human_cooldown = "15m"
reconcile_interval = "10m"
default_repo = "talkable/talkable"

[herdr]
notify = true
toast_every_review = true

[claude]
simplify = "first"

[[identity]]
name = "zhuravel"
kind = "gh"
login = "zhuravel"
no_findings_event = "APPROVE"

[[identity]]
name = "talkable-app"
kind = "app"
login = "talkable[bot]"
app_id = 1
client_id = "Iv-test"
installation_id = 2
private_key_env = "MAGNUM_TEST_KEY"
no_findings_event = "COMMENT"
blocking_event = "REQUEST_CHANGES"

[[watch]]
owner = "talkable"
include = ["talkable"]
identity = "talkable-app"
poll_identity = "zhuravel"
skip_authors = ["dependabot"]

[[watch]]
owner = "zhuravel"
include = ["*"]
identity = "zhuravel"
poll_identity = "zhuravel"

[[pool]]
repo = "talkable/talkable"
main_clone = "HOME/talkable"
slot_name = "review{n}"
slot_path = "HOME/talkable.review{n}"
base = "master"
min = 1
max = 1
idle_remove_after = "48h"
setup = ["true"]
teardown = ["true"]
databases = ["talkable_development__{slug}", "talkable_test__{slug}"]
  [pool.env]
  WT_BRANCH = "{slot}"
`

type harness struct {
	t      *testing.T
	ctx    context.Context
	e      *Engine
	d      Deps
	st     *store.Store
	cfg    *config.Config
	layout paths.Layout
	clock  *fakeClock
	gh     *fakeGH
	hd     *fakeHerdr
	ag     *fakeAgents
	rd     *fakeRounds
	sl     *fakeSlots
	inv    *fakeInventory
	nh     *fakeNotifyHerdr
	ids    map[string]*fakeIdentity
	app    *fakeAppIdentity
}

func newHarness(t *testing.T, mods ...func(*harness)) *harness {
	t.Helper()
	home := t.TempDir()
	cfgText := strings.ReplaceAll(testConfigTOML, "HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	layout := paths.Layout{Home: home}
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(layout, "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(home, "state", "magnum.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clock := &fakeClock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.Local)}
	st.Clock = clock.Now

	h := &harness{t: t, ctx: context.Background(), st: st, cfg: cfg, layout: layout, clock: clock,
		gh: newFakeGH(), hd: &fakeHerdr{}, ag: &fakeAgents{st: st}, rd: &fakeRounds{st: st}, sl: &fakeSlots{st: st},
		nh: &fakeNotifyHerdr{}}
	h.inv = &fakeInventory{now: clock.Now}
	gh := &fakeIdentity{name: "zhuravel", login: "zhuravel", kind: "gh"}
	h.app = &fakeAppIdentity{fakeIdentity{name: "talkable-app", login: "talkable[bot]", kind: "app"}}
	h.ids = map[string]*fakeIdentity{"zhuravel": gh, "talkable-app": &h.app.fakeIdentity}

	// The pool's one slot exists and is free.
	pool := cfg.Pools[0]
	if _, err := st.CreateSlot(h.ctx, store.Slot{Name: pool.Slot(1), RepoFullName: pool.Repo, Kind: store.SlotKindPool,
		Path: pool.Path(1), MainClone: pool.MainClone, PlaceholderBranch: store.Ptr("review1"), DBSlug: store.Ptr("review1"),
		State: store.SlotFree}); err != nil {
		t.Fatal(err)
	}

	notifier := &notify.Notifier{Herdr: h.nh, Store: st, Layout: layout, Enabled: true}
	planner := &cleanup.Planner{Store: st, Slots: h.sl, Inventory: h.inv, Config: cfg, Clock: clock.Now, Park: h.ag.Park}
	h.d = Deps{
		Config: cfg, Layout: layout, Store: st, Now: clock.Now,
		Logger: slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Sleep:  func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		GitHub: func(id string) GitHub {
			if id == "zhuravel" {
				return h.gh
			}
			return nil
		},
		Herdr:  h.hd,
		Agents: h.ag,
		Rounds: func(string) Rounds { return h.rd },
		Slots:  h.sl, Git: fakeGit{}, Inventory: h.inv, Cleanup: planner, Notifier: notifier,
		Identities: map[string]identity.Source{"zhuravel": gh, "talkable-app": h.app},
	}
	for _, m := range mods {
		m(h)
	}
	h.e = New(h.d)
	return h
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// tick runs one tick, then lets launched rounds and heavy jobs finish.
func (h *harness) tick() {
	h.t.Helper()
	if err := h.e.Tick(h.ctx); err != nil {
		h.t.Fatalf("tick: %v", err)
	}
	h.settle()
}

// awaitRound waits until more than before rounds reached the pipeline: a
// round the tick dispatched runs in its own goroutine, which takes the PR
// from claiming to reviewing before it enters RunRound (and waits at
// h.rd.gate, when set).
func (h *harness) awaitRound(before int) {
	h.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); len(h.rd.all()) <= before; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			h.t.Fatal("the round never reached the pipeline")
		}
	}
}

func (h *harness) settle() {
	h.e.roundWG.Wait()
	h.e.retroWG.Wait()
	h.e.drainHeavy(h.ctx)
	h.e.toastWG.Wait() // urgent toasts run in goroutines
}

func (h *harness) startup() {
	h.t.Helper()
	h.e.startup(h.ctx)
	h.settle()
}

func (h *harness) advance(d time.Duration) { h.clock.Advance(d) }

func (h *harness) pr(n int) store.PR {
	h.t.Helper()
	repo, err := h.st.RepoByFullName(h.ctx, "talkable/talkable")
	if err != nil {
		h.t.Fatal(err)
	}
	pr, err := h.st.PRByRepoNumber(h.ctx, repo.ID, n)
	if err != nil {
		h.t.Fatalf("pr %d: %v", n, err)
	}
	return pr
}

func (h *harness) slot(name string) store.Slot {
	h.t.Helper()
	sl, err := h.st.SlotByName(h.ctx, name)
	if err != nil {
		h.t.Fatal(err)
	}
	return sl
}

func (h *harness) wantState(n int, state string) store.PR {
	h.t.Helper()
	pr := h.pr(n)
	if pr.State != state {
		h.t.Fatalf("PR #%d state = %s, want %s (last_error %q)", n, pr.State, state, deref(pr.LastError))
	}
	return pr
}

func (h *harness) open(prs ...prSpec) {
	for i := range prs {
		if prs[i].updated.IsZero() {
			prs[i].updated = h.clock.Now()
		}
	}
	h.gh.set("talkable/talkable", prs...)
}

// reviewedPR runs the standard flow to a reviewed PR #n at head.
func (h *harness) reviewedPR(n int, head string) store.PR {
	h.t.Helper()
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync: #1 baseline
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: n, head: head})
	h.tick() // #n queued
	h.advance(5 * time.Minute)
	h.tick() // dispatched and reviewed
	return h.wantState(n, store.PRReviewed)
}

// lastWorkspaceHas reports whether the PR's latest EnsureWorkspace laid out
// role.
func (h *harness) lastWorkspaceHas(prID int64, role string) bool {
	h.t.Helper()
	prefix := fmt.Sprintf("ensure_workspace:%d:", prID)
	last := ""
	for _, c := range h.ag.all() {
		if strings.HasPrefix(c, prefix) {
			last = c
		}
	}
	if last == "" {
		h.t.Fatalf("no EnsureWorkspace for pr %d: %v", prID, h.ag.all())
	}
	roles := last[strings.LastIndexByte(last, ':')+1:]
	return slices.Contains(strings.Split(roles, ","), role)
}

func agentInfo(i int) herdr.AgentInfo {
	return herdr.AgentInfo{PaneID: fmt.Sprintf("p%d", i), Name: fmt.Sprintf("human-%d", i), Agent: "codex", AgentStatus: herdr.StatusWorking}
}

func itoa(n int64) string { return fmt.Sprint(n) }

// pauseReason is why dispatch is closed for everything ("" = open): the
// daemon pause, a drain before a restart, an infrastructure pause. A paused
// agent kind only holds the rounds whose roles use it (kindPauseReason).
func (e *Engine) pauseReason(ctx context.Context) string {
	if r := e.userPause(ctx); r != "" {
		return r
	}
	return e.holdReason(ctx)
}
