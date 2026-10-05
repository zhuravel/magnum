package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

const reviewUsage = "review <url|owner/repo#N|repo#N|N> [--fresh] [--role <role>]... [--simplify] [--as <identity>] [--no-post] [--focus] [--wait] [--timeout <duration>] [--dry-run] [--json]"

const (
	// reviewDefaultTimeout is how long --wait/--focus follow a round by default.
	reviewDefaultTimeout = 2 * time.Hour
	// reviewFocusTries is how often --focus tries to focus the judge pane.
	reviewFocusTries = 3
)

type reviewOpts struct {
	again, fresh, simplify, focus, wait, dryRun, noPost, json bool
	as, workspace, cwd                                        string
	roles                                                     []string      // --role: on-request roles to run this round
	timeout                                                   time.Duration // --timeout: stop following after this long (0 = no limit)
}

// reviewClosing are the end of a PR's life (prInFlight are the states of a
// running round, which the daemon refuses to start again).
var reviewClosing = []string{store.PRClosed, store.PRReleasing, store.PRReleased}

func newReviewCmd(c *Context) *cobra.Command {
	var o reviewOpts
	cmd := newCommand(groupAct, reviewUsage, "force a review round for a PR now (--wait follows it to the posted review)",
		"Force a review round for a PR now; a PR magnum does not know yet is added to the registry from "+
			"GitHub. The round runs even on a muted PR and even when its head was already reviewed. A PR GitHub "+
			"merged gets a post-merge review: the commits magnum missed since its last review (the whole PR when "+
			"it never reviewed it), posted as a comment only, after which the PR is released again; a merged PR "+
			"whose head was reviewed, and a PR closed without merging, are refused. --fresh parks "+
			"the sessions and starts new conversations, --role also runs an on-request role of the PR's watch "+
			"this round (runs = \"first\" after its first completion, or \"manual\"; repeat it for several; "+
			"`magnum roles` lists them), --simplify is the shorthand for the role aliased simplify "+
			"(claude-simplify by default), --as switches the posting identity from now on, --no-post runs the "+
			"round but posts nothing (the judge writes its planned review), and --dry-run only prints the "+
			"request. --wait follows the round until it is reviewed, needs attention or pauses; --timeout "+
			"(default 2h, 0 = no limit) stops following it after that long, and the round goes on.",
		func(pos []string) int { return runReview(c, o, pos) })
	fs := cmd.Flags()
	fs.BoolVar(&o.again, "again", false, "no effect: a forced round reviews the head again anyway")
	_ = fs.MarkHidden("again") // kept so existing scripts keep working
	fs.BoolVar(&o.fresh, "fresh", false, "park the sessions and start new conversations (no resume)")
	fs.StringArrayVar(&o.roles, "role", nil, "also run this on-request `role` this round; repeatable")
	fs.BoolVar(&o.simplify, "simplify", false, "shorthand for --role <the role aliased simplify> (claude-simplify by default)")
	fs.StringVar(&o.as, "as", "", "post as this identity from now on (an [[identity]] name)")
	fs.BoolVar(&o.focus, "focus", false, "focus the judge pane once its session starts")
	fs.BoolVar(&o.wait, "wait", false, "follow the round until it is reviewed, needs attention or pauses")
	fs.DurationVar(&o.timeout, "timeout", reviewDefaultTimeout, "with --wait or --focus: stop following after this `duration` (0 = no limit); the round goes on")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print the resolution and the request without queuing anything")
	fs.BoolVar(&o.noPost, "no-post", false, "run the round but post nothing: the judge writes its planned review (request dry_run)")
	fs.BoolVar(&o.json, "json", false, "print the result as JSON (progress goes to stderr)")
	fs.StringVar(&o.workspace, "workspace", "", "herdr workspace id instead of a PR (plugin context)")
	fs.StringVar(&o.cwd, "cwd", "", "directory inside a magnum slot instead of a PR (plugin context)")
	completePluginContext(cmd)
	_ = cmd.RegisterFlagCompletionFunc("as", completeFlag(c.completeIdentities))
	_ = cmd.RegisterFlagCompletionFunc("role", completeFlag(c.completeRequestableRoles))
	cmd.ValidArgsFunction = completeFirst(c.completePRs)
	return cmd
}

func runReview(c *Context, o reviewOpts, pos []string) int {
	if len(pos) > 1 {
		return actUsage(c, "review", "one PR at a time", reviewUsage)
	}
	ref := ""
	if len(pos) == 1 {
		ref = pos[0]
	}
	if ref == "" && o.workspace == "" && o.cwd == "" {
		return actUsage(c, "review", "which PR?", reviewUsage)
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "review", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return reviewMain(ctx, c, d, ref, o)
}

// reviewJSON is what --json prints.
type reviewJSON struct {
	PR        string                `json:"pr"`
	URL       string                `json:"url"`
	Added     bool                  `json:"added,omitempty"`
	DryRun    bool                  `json:"dry_run,omitempty"`
	Payload   *engine.ReviewPayload `json:"payload,omitempty"`
	Request   *actRequestJSON       `json:"request,omitempty"`
	State     string                `json:"state"`
	Event     string                `json:"review_event,omitempty"`
	ReviewURL string                `json:"review_url,omitempty"`
	Error     string                `json:"error,omitempty"`
}

// reviewProgress is where progress lines go: stdout, or stderr with --json
// (which keeps stdout a single JSON document).
func reviewProgress(c *Context, o reviewOpts) io.Writer {
	if o.json {
		return c.Stderr
	}
	return c.Stdout
}

// reviewCheckOpts refuses an unknown identity or role before anything is
// resolved; ok is false when it printed why (code is then the exit code).
func reviewCheckOpts(c *Context, d *actDeps, o reviewOpts) (code int, ok bool) {
	if o.as != "" && d.Cfg.IdentityByName(o.as) == nil {
		names := make([]string, 0, len(d.Cfg.Identities))
		for _, id := range d.Cfg.Identities {
			names = append(names, id.Name)
		}
		return cmdFail(c, "review", fmt.Errorf("unknown identity %q: use one of %s ([[identity]] in config.toml)", o.as, strings.Join(names, ", "))), false
	}
	for _, r := range o.roles {
		if strings.TrimSpace(r) == "" {
			return actUsage(c, "review", "--role needs a role name", reviewUsage), false
		}
		if err := actKnownRole(d.Cfg, r); err != nil {
			return actUsage(c, "review", err.Error(), reviewUsage), false
		}
	}
	return 0, true
}

func reviewMain(ctx context.Context, c *Context, d *actDeps, ref string, o reviewOpts) int {
	if code, ok := reviewCheckOpts(c, d, o); !ok {
		return code
	}
	t, added, err := reviewResolve(ctx, d, ref, o)
	if err != nil {
		return cmdFail(c, "review", err)
	}
	roles, err := reviewRoles(d.Cfg, t.full(), o.roles)
	if err != nil {
		return cmdFail(c, "review", err)
	}
	label := d.actLabel(t.full(), t.PR.Number)
	out := reviewJSON{PR: label, URL: t.PR.URL, Added: added, DryRun: o.dryRun, State: t.PR.State}
	if !o.json {
		reviewDescribe(ctx, c.Stdout, d, t, label, added, o.dryRun)
	}
	progress := reviewProgress(c, o)

	merged := t.PR.GHState == store.GHMerged
	switch {
	case merged && store.Deref(t.PR.ReviewedSHA) == t.PR.HeadSHA:
		return reviewFailJSON(c, o, out, fmt.Errorf("%s: its merged head %s was already reviewed", label, sha7(t.PR.HeadSHA)))
	case merged && t.PR.State == store.PRReleasing:
		return reviewFailJSON(c, o, out, fmt.Errorf("%s: its checkout is being released; run `magnum review` again in a minute", label))
	case !merged && t.PR.GHState == store.GHClosed:
		err := fmt.Errorf("%s was closed without merging (%s, GitHub %s): only open or merged PRs are reviewed", label, t.PR.State, t.PR.GHState)
		return reviewFailJSON(c, o, out, err)
	case !merged && (slices.Contains(reviewClosing, t.PR.State) || (t.PR.GHState != "" && t.PR.GHState != store.GHOpen)):
		err := fmt.Errorf("%s is %s (GitHub %s): only open or merged PRs are reviewed", label, t.PR.State, t.PR.GHState)
		return reviewFailJSON(c, o, out, err)
	case slices.Contains(prInFlight, t.PR.State):
		return reviewAttach(ctx, c, d, t, label, o, out)
	}
	if merged {
		fmt.Fprintf(progress, "%s was merged: a post-merge review of %s, posted as a comment only\n", label, reviewPostMergeScope(t.PR))
	}
	if t.PR.Muted {
		fmt.Fprintf(progress, "%s is muted: this forced round runs anyway (`magnum unmute %s` resumes automatic reviews)\n", label, label)
	}
	if rs := store.Deref(t.PR.ReviewedSHA); rs != "" && rs == t.PR.HeadSHA {
		fmt.Fprintf(progress, "head %s was already reviewed (%s); reviewing it again\n", sha7(rs), store.Deref(t.PR.LastReviewEvent))
	}

	// Again is sent for the daemons that look at it; a forced round always reviews the head again.
	payload := engine.ReviewPayload{PRTarget: t.prTarget(), Again: o.again, Fresh: o.fresh, Simplify: o.simplify, Roles: roles, As: o.as, DryRun: o.noPost}
	if o.dryRun {
		out.Payload = &payload
		if o.json {
			_ = writeJSON(c.Stdout, out)
			return 0
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(c.Stdout, "dry run: would queue request %s %s and wake the daemon; nothing was changed\n", engine.ReqReview, b)
		return 0
	}
	return reviewQueue(ctx, c, d, t, label, payload, o, out)
}

// reviewAttach handles a PR whose round is already running: the daemon
// refuses a second one, so nothing is queued; --wait and --focus follow the
// running round (--dry-run only says so).
func reviewAttach(ctx context.Context, c *Context, d *actDeps, t actTarget, label string, o reviewOpts, out reviewJSON) int {
	w, progress := c.Stdout, reviewProgress(c, o)
	fmt.Fprintf(progress, "%s: a round is already running (%s)\n", label, t.PR.State)
	switch {
	case o.dryRun:
		if o.json {
			_ = writeJSON(w, out)
			return 0
		}
		var steps []string
		if o.focus {
			steps = append(steps, "focus its "+actRoleName(actJudgeFor(d.Cfg, t.full()))+" pane")
		}
		if o.wait {
			steps = append(steps, "wait until it is reviewed, needs attention or pauses")
		}
		if len(steps) == 0 {
			fmt.Fprintln(w, "dry run: no request would be queued while that round runs; nothing was changed")
		} else {
			fmt.Fprintf(w, "dry run: no request would be queued; would follow the running round (%s); nothing was changed\n", strings.Join(steps, ", "))
		}
		return 0
	case !o.wait && !o.focus:
		if o.json {
			_ = writeJSON(w, out)
		} else {
			fmt.Fprintf(w, "follow it with `magnum review %s --wait`, or watch it with `magnum open %s`\n", label, label)
		}
		return 0
	}
	return reviewFollow(ctx, c, d, t, label, 0, reviewAttachStart(ctx, d, t.PR), o, out)
}

// reviewAttachStart is where following a round that is already running
// starts: the earliest run of the PR's highest round number, so the running
// judge run belongs to the followed round (progress lines, the posted review).
// A claiming PR has no run of its round yet (the highest round is the
// previous, finished one) and runs created before last_round_started_at
// belong to an earlier round: those, and a PR without runs, start now.
func reviewAttachStart(ctx context.Context, d *actDeps, pr store.PR) time.Time {
	start := d.now()
	if pr.State == store.PRClaiming {
		return start
	}
	runs, err := d.Store.RunsByPR(ctx, pr.ID)
	if err != nil {
		return start
	}
	top := 0
	for _, r := range runs {
		top = max(top, r.Round)
	}
	for _, r := range runs {
		if r.Round != top || (pr.LastRoundStartedAt != nil && r.CreatedAt.Before(*pr.LastRoundStartedAt)) {
			continue
		}
		if r.CreatedAt.Before(start) {
			start = r.CreatedAt
		}
	}
	return start
}

// reviewQueue submits the review request, prints its outcome and, with
// --wait or --focus, follows the round.
func reviewQueue(ctx context.Context, c *Context, d *actDeps, t actTarget, label string, payload engine.ReviewPayload, o reviewOpts, out reviewJSON) int {
	w, ew := c.Stdout, c.Stderr
	start := d.now()
	id, pid, err := d.submit(ctx, engine.ReqReview, payload)
	if err != nil {
		if id == 0 {
			return reviewFailJSON(c, o, out, err)
		}
		fmt.Fprintln(ew, err)
	}
	var req store.Request
	if pid != 0 {
		req, err = d.await(ctx, id, d.quickPoll(), d.Quick)
	} else {
		req, err = d.Store.RequestByID(ctx, id)
	}
	if err != nil && req.ID == 0 {
		return reviewFailJSON(c, o, out, err)
	}
	rv := actRequestView(req, pid)
	out.Request = &rv
	if req.State == store.RequestFailed {
		return reviewFailJSON(c, o, out, errors.New(store.Deref(req.Result)))
	}
	if !o.json {
		actRequestOutcome(w, ew, req, pid)
	}
	if pid == 0 {
		if o.wait || o.focus {
			err := fmt.Errorf("--wait and --focus need a running daemon (request %d stays queued until one starts): %s", id, actDaemonFix)
			return reviewFailJSON(c, o, out, err)
		}
		if o.json {
			_ = writeJSON(w, out)
		}
		return 0
	}
	if !o.wait && !o.focus {
		if o.json {
			if pr, err := d.Store.PRByID(ctx, t.PR.ID); err == nil {
				out.State = pr.State
			}
			_ = writeJSON(w, out)
		}
		return 0
	}
	return reviewFollow(ctx, c, d, t, label, id, start, o, out)
}

func reviewFailJSON(c *Context, o reviewOpts, out reviewJSON, err error) int {
	if o.json {
		out.Error = err.Error()
		_ = writeJSON(c.Stdout, out)
		return 1
	}
	return cmdFail(c, "review", err)
}

// reviewResolve finds the PR, adding it to the registry when GitHub has it
// but the daemon has not recorded it yet. A shorthand that resolved to a
// registered repository (repo#N under another watched owner) keeps that
// repository when the PR itself is new.
func reviewResolve(ctx context.Context, d *actDeps, ref string, o reviewOpts) (actTarget, bool, error) {
	if ref == "" {
		t, err := d.resolve(ctx, "", o.workspace, o.cwd)
		if err != nil {
			return t, false, err
		}
		if !t.hasPR() {
			return t, false, fmt.Errorf("slot %s holds no PR: pass the PR to review", t.Slot.Name)
		}
		return t, false, nil
	}
	owner, name, number, err := d.refs().ResolvePR(ctx, ref)
	if err != nil {
		return actTarget{}, false, err
	}
	t, err := d.resolveRef(ctx, ref)
	if err == nil {
		return t, false, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return t, false, err
	}
	if t.Repo.ID != 0 || t.Repo.Owner != "" {
		owner, name = t.Repo.Owner, t.Repo.Name
	}
	t, err = reviewImport(ctx, d, owner, name, number, o.dryRun)
	return t, err == nil, err
}

// reviewImport reads one PR from GitHub (a single Details call) and records
// it, plus its repository when that is new, the way the poller would. The
// PR enters as baseline; the review request then queues it (forced).
func reviewImport(ctx context.Context, d *actDeps, owner, name string, number int, dry bool) (actTarget, error) {
	full := owner + "/" + name
	w := d.Cfg.WatchFor(full)
	if w == nil {
		return actTarget{}, fmt.Errorf("%s is not watched: add it to a [[watch]] (owner = %q, include = [%q]) in ~/.config/magnum/config.toml (`magnum init` writes one)", full, owner, name)
	}
	ident := w.PollIdentity
	if ident == "" {
		ident = w.Identity
	}
	p, err := reviewReadPR(ctx, d, ident, owner, name, number)
	if err != nil {
		return actTarget{}, err
	}
	repo, err := reviewRecordRepo(ctx, d, w, ident, owner, name, dry)
	if err != nil {
		return actTarget{}, err
	}
	url := p.URL
	if url == "" {
		url = fmt.Sprintf("https://github.com/%s/pull/%d", repo.FullName(), number)
	}
	in := reviewGitHubPR(d, w, repo, p, number, url)
	if dry {
		pr := store.PR{Number: number, URL: url, Title: in.Title, AuthorLogin: in.AuthorLogin, HeadSHA: in.HeadSHA,
			IsDraft: in.IsDraft, BaseRef: in.BaseRef, GHState: store.GHOpen, State: "new", Identity: w.Identity}
		return actTarget{Repo: repo, PR: pr}, nil
	}
	res, err := d.Store.UpsertPRFromGitHub(ctx, in)
	if err != nil {
		return actTarget{}, err
	}
	_, _ = d.Store.AppendEvent(ctx, store.Event{Level: "info", Subject: store.Ptr(fmt.Sprintf("pr:%s#%d", repo.FullName(), number)),
		Kind: "pr.added", Message: "magnum review added the PR to the registry (baseline until the forced round)"})
	return actTarget{Repo: repo, PR: res.PR}, nil
}

// reviewReadPR reads the open PR from GitHub as the watch's polling
// identity, whose credentials are prepared first (a GitHub App identity
// mints its installation token there).
func reviewReadPR(ctx context.Context, d *actDeps, ident, owner, name string, number int) (github.PRDetails, error) {
	full := owner + "/" + name
	if d.PrepareIdentity != nil {
		if err := d.PrepareIdentity(ctx, ident); err != nil {
			return github.PRDetails{}, fmt.Errorf("prepare the %s GitHub identity to read %s#%d: %w", ident, full, number, err)
		}
	}
	var gh actGitHub
	if d.GitHub != nil {
		gh = d.GitHub(ident)
	}
	if gh == nil {
		return github.PRDetails{}, fmt.Errorf("no GitHub client for identity %q: check [[identity]] and the watch's poll_identity in config.toml", ident)
	}
	det, _, err := gh.Details(ctx, owner, name, []int{number})
	if err != nil {
		return github.PRDetails{}, fmt.Errorf("read %s#%d from GitHub: %w (check `gh auth status`)", full, number, err)
	}
	p, ok := det[number]
	if !ok {
		return github.PRDetails{}, fmt.Errorf("GitHub has no pull request %s#%d (or the %s identity cannot see it)", full, number, ident)
	}
	if p.State != "" && p.State != store.GHOpen {
		return github.PRDetails{}, fmt.Errorf("%s#%d is %s on GitHub: only open PRs are reviewed", full, number, strings.ToLower(p.State))
	}
	return p, nil
}

// reviewRecordRepo returns the PR's repository row, recording the repository
// (from GitHub's own spelling of its name) when the registry has not seen it;
// a dry run only builds the row.
func reviewRecordRepo(ctx context.Context, d *actDeps, w *config.Watch, ident, owner, name string, dry bool) (store.Repo, error) {
	repo, err := d.Store.RepoByFullName(ctx, owner+"/"+name)
	if err == nil {
		return repo, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Repo{}, err
	}
	info, err := reviewRepoInfo(ctx, d, ident, owner, name)
	if err != nil {
		return store.Repo{}, err
	}
	full := owner + "/" + name
	if o, n, ok := strings.Cut(info.FullName, "/"); ok {
		owner, name, full = o, n, info.FullName
	}
	// A per-PR repository's clone path is left to the daemon, which
	// discovers the clone (it may be named <owner>-<name>) on its next
	// poll and records it; the round finds it the same way.
	row := store.Repo{NodeID: info.NodeID, Owner: owner, Name: name, WatchOwner: w.Owner,
		Mode: store.RepoModePerPR, DefaultBranch: info.DefaultBranch}
	if pool := d.Cfg.PoolFor(full); pool != nil {
		row.Mode, row.DefaultBranch, row.ClonePath = store.RepoModePool, pool.Base, store.Ptr(pool.MainClone)
	}
	if dry {
		return row, nil
	}
	return d.Store.UpsertRepo(ctx, row)
}

// reviewGitHubPR is the registry row of a PR read from GitHub: baseline,
// so the forced round (not the poller) queues it.
func reviewGitHubPR(d *actDeps, w *config.Watch, repo store.Repo, p github.PRDetails, number int, url string) store.GitHubPR {
	in := store.GitHubPR{
		RepoID: repo.ID, NodeID: p.NodeID, Number: number, URL: url, HeadSHA: p.HeadRefOid, IsDraft: p.IsDraft,
		Title: store.Ptr(p.Title), AuthorLogin: store.Ptr(p.AuthorLogin), AuthorType: store.Ptr(p.AuthorType),
		HeadRef: store.Ptr(p.HeadRefName), IsCrossRepo: store.Ptr(p.IsCrossRepository),
		GHState: store.GHOpen, InitialState: store.PRBaseline, Identity: w.Identity,
	}
	if p.BaseRefName != "" {
		in.BaseRef = store.Ptr(p.BaseRefName)
	}
	if !p.UpdatedAt.IsZero() {
		in.GHUpdatedAt = store.Ptr(p.UpdatedAt)
	}
	in.Labels = p.Labels
	if in.Labels == nil {
		in.Labels = []string{}
	}
	in.ReviewRequested = store.Ptr(reviewRequested(d, *w, p.ReviewRequests))
	return in
}

// reviewRequested mirrors the poller: the watch's poll or posting login is
// a requested (non-team) reviewer.
func reviewRequested(d *actDeps, w config.Watch, reqs []github.Reviewer) bool {
	var logins []string
	for _, n := range []string{w.PollIdentity, w.Identity} {
		if id := d.Cfg.IdentityByName(n); id != nil && id.Login != "" {
			logins = append(logins, id.Login)
		}
	}
	for _, r := range reqs {
		if r.Type == "Team" {
			continue
		}
		for _, l := range logins {
			if github.SameAccount(github.Account(r.Login, r.Type), l) {
				return true
			}
		}
	}
	return false
}

type reviewRepo struct {
	NodeID        string `json:"node_id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

// reviewRepoInfo reads the repository's node id and default branch (needed
// to record a repository the poller has not seen yet).
func reviewRepoInfo(ctx context.Context, d *actDeps, ident, owner, name string) (reviewRepo, error) {
	env := map[string]string{"GH_PROMPT_DISABLED": "1", "GH_NO_UPDATE_NOTIFIER": "1"}
	if d.GHEnv != nil {
		for k, v := range d.GHEnv(ident) {
			env[k] = v
		}
	}
	res, err := d.Run.Run(ctx, execx.Cmd{Name: "gh", Args: []string{"api", "repos/" + owner + "/" + name}, Env: env,
		Timeout: 30 * time.Second, Label: "gh repo"})
	if err != nil {
		return reviewRepo{}, fmt.Errorf("look up %s/%s on GitHub: %w (check `gh auth status`)", owner, name, err)
	}
	var r reviewRepo
	if err := json.Unmarshal(res.Stdout, &r); err != nil || r.NodeID == "" {
		return reviewRepo{}, fmt.Errorf("look up %s/%s on GitHub: unexpected reply", owner, name)
	}
	return r, nil
}

// reviewDescribe prints how the reference resolved.
func reviewDescribe(ctx context.Context, w io.Writer, d *actDeps, t actTarget, label string, added, dry bool) {
	title := trunc(actClean(store.Deref(t.PR.Title)), 80)
	author := actClean(store.Deref(t.PR.AuthorLogin))
	line := label
	if title != "" {
		line += "  " + title
	}
	if author != "" {
		line += " (@" + author + ")"
	}
	fmt.Fprintln(w, line)
	if t.PR.URL != "" {
		fmt.Fprintln(w, "  "+t.PR.URL)
	}
	facts := []string{"state " + t.PR.State, "head " + sha7(t.PR.HeadSHA)}
	if t.PR.IsDraft {
		facts = append(facts, "draft")
	}
	if rs := store.Deref(t.PR.ReviewedSHA); rs != "" {
		facts = append(facts, fmt.Sprintf("last review %s on %s", store.Deref(t.PR.LastReviewEvent), sha7(rs)))
	}
	if t.PR.Identity != "" {
		facts = append(facts, "identity "+t.PR.Identity)
	}
	fmt.Fprintln(w, "  "+strings.Join(facts, " · "))
	if t.PR.ID != 0 {
		if s, err := slotOfPR(ctx, d.Store, t.PR.ID); err == nil && s != nil {
			fmt.Fprintf(w, "  slot %s (%s)\n", s.Name, s.Path)
		}
	}
	switch {
	case added && dry:
		fmt.Fprintln(w, "  not in the registry yet: would be added from GitHub")
	case added:
		fmt.Fprintln(w, "  added to the registry from GitHub")
	}
}

// reviewFollower prints the PR's state, gate and run transitions every Poll
// until the round posts a review, ends as a dry run, needs attention, pauses,
// the PR closes or --timeout passes. With --focus it focuses the judge pane
// once the round has selected its session.
type reviewFollower struct {
	c        *Context
	d        *actDeps
	t        actTarget
	label    string
	reqID    int64     // the request being followed; 0 when attached to a running round
	start    time.Time // the followed round starts here: its runs and its review are at or after it
	o        reviewOpts
	out      reviewJSON
	judge    string
	progress io.Writer

	reqDone    bool
	lastState  string
	lastGate   string
	runs       map[string]string
	judgeRun   bool // the followed round created its judge run
	focused    bool
	focusTries int
}

// reviewFollow follows a round: the request reqID just queued (start is its
// submit time), or with reqID 0 the round already running (start is its
// first run).
func reviewFollow(ctx context.Context, c *Context, d *actDeps, t actTarget, label string, reqID int64, start time.Time, o reviewOpts, out reviewJSON) int {
	f := &reviewFollower{c: c, d: d, t: t, label: label, reqID: reqID, start: start, o: o, out: out,
		judge: actJudgeFor(d.Cfg, t.full()), progress: reviewProgress(c, o), reqDone: reqID == 0, runs: map[string]string{}}
	return f.run(ctx)
}

func (f *reviewFollower) run(ctx context.Context) int {
	var deadline time.Time
	if f.o.timeout > 0 {
		deadline = f.d.now().Add(f.o.timeout)
	}
	for {
		if code, done := f.pollRequest(ctx); done {
			return code
		}
		pr, err := f.d.Store.PRByID(ctx, f.t.PR.ID)
		if err != nil {
			return cmdFail(f.c, "review", err)
		}
		f.printState(pr)
		f.printGate(ctx, pr)
		f.printRuns(ctx, pr)
		if code, done := f.focusStep(ctx, pr); done {
			return code
		}
		if code, done := f.completion(ctx, pr); done {
			return f.finish(code, pr)
		}
		if !deadline.IsZero() && !f.d.now().Before(deadline) {
			f.out.Error = fmt.Sprintf("still following %s after %s", f.label, f.o.timeout)
			fmt.Fprintf(f.c.Stderr, "%s: stopped following after %s; the round goes on (`magnum review %s --wait` follows it again)\n", f.label, f.o.timeout, f.label)
			return f.finish(1, pr)
		}
		if err := f.d.sleep(ctx, f.d.Poll); err != nil {
			fmt.Fprintf(f.c.Stderr, "stopped following; the round goes on (`magnum review %s --wait` follows it again)\n", f.label)
			return f.finish(130, pr)
		}
	}
}

// finish prints the JSON result. A round that ended well still fails when
// --focus was asked and the judge pane was never focused.
func (f *reviewFollower) finish(code int, pr store.PR) int {
	if code == 0 && f.o.focus && !f.focused {
		fmt.Fprintln(f.c.Stderr, "magnum review: "+f.focusMissed())
		f.out.Error = f.focusMissed()
		code = 1
	}
	f.out.State = pr.State
	if f.o.json {
		_ = writeJSON(f.c.Stdout, f.out)
	}
	return code
}

func (f *reviewFollower) focusMissed() string {
	if f.focusTries > 0 {
		return fmt.Sprintf("could not focus the %s pane after %d attempts", actRoleName(f.judge), f.focusTries)
	}
	return fmt.Sprintf("the round ended before the %s pane could be focused", actRoleName(f.judge))
}

// pollRequest watches the queued request until the daemon handled it; done
// is true (with the exit code) when it failed.
func (f *reviewFollower) pollRequest(ctx context.Context) (code int, done bool) {
	if f.reqDone {
		return 0, false
	}
	req, err := f.d.Store.RequestByID(ctx, f.reqID)
	if err != nil {
		return cmdFail(f.c, "review", err), true
	}
	switch req.State {
	case store.RequestFailed:
		f.out.Error = store.Deref(req.Result)
		fmt.Fprintln(f.c.Stderr, store.Deref(req.Result))
		return f.finish(1, f.t.PR), true
	case store.RequestDone:
		f.reqDone = true
	}
	return 0, false
}

// printState prints a PR state change.
func (f *reviewFollower) printState(pr store.PR) {
	if pr.State == f.lastState {
		return
	}
	change := f.lastState + " → " + pr.State
	if f.lastState == "" {
		change = "state " + pr.State
	}
	extra := ""
	if pr.State == store.PRQueued || pr.State == store.PRRereviewPending {
		if pr.NextAttemptAt != nil && pr.NextAttemptAt.After(f.d.now()) {
			extra = " (retry after " + actClock(*pr.NextAttemptAt) + ")"
		}
	}
	fmt.Fprintf(f.progress, "%s  %s%s\n", actClock(f.d.now()), change, extra)
	f.lastState = pr.State
}

// printGate prints why the daemon does not dispatch the handled request's
// PR yet (store.KVPRGate) whenever that reason changes.
func (f *reviewFollower) printGate(ctx context.Context, pr store.PR) {
	if !f.reqDone || (pr.State != store.PRQueued && pr.State != store.PRRereviewPending) {
		f.lastGate = ""
		return
	}
	gate, _, err := f.d.Store.GetKV(ctx, store.KVPRGate(pr.ID))
	if err != nil || gate == f.lastGate {
		return
	}
	f.lastGate = gate
	if gate != "" {
		fmt.Fprintf(f.progress, "%s  waiting: %s\n", actClock(f.d.now()), gate)
	}
}

// printRuns prints the followed round's run transitions and notes whether
// its judge run exists.
func (f *reviewFollower) printRuns(ctx context.Context, pr store.PR) {
	rs, err := f.d.Store.RunsByPR(ctx, pr.ID)
	if err != nil {
		return
	}
	for _, r := range rs {
		if r.CreatedAt.Before(f.start) {
			continue
		}
		f.judgeRun = f.judgeRun || actIsJudge(f.d.Cfg, r.Role)
		desc := r.State
		if oc := store.Deref(r.Outcome); oc != "" {
			desc += " (" + oc + ")"
		}
		if f.runs[r.ID] != desc {
			f.runs[r.ID] = desc
			fmt.Fprintf(f.progress, "%s    %s %s round %d: %s\n", actClock(f.d.now()), actRoleName(r.Role), r.Kind, r.Round, desc)
		}
	}
}

// focusStep focuses the judge pane (--focus) once the round selected its
// session, retrying a failed attempt on later polls up to reviewFocusTries
// times. done is true when the command ends: focus-only mode after the focus,
// or after the last failed attempt.
func (f *reviewFollower) focusStep(ctx context.Context, pr store.PR) (code int, done bool) {
	if !f.o.focus || f.focused || f.focusTries >= reviewFocusTries {
		return 0, false
	}
	sess, ok := f.focusSession(ctx, pr)
	if !ok {
		return 0, false
	}
	f.focusTries++
	res := actFocusResult{PR: f.label, URL: pr.URL, Role: f.judge}
	if err := f.d.focus(ctx, &res, sess, false, false); err != nil {
		fmt.Fprintln(f.c.Stderr, "magnum review: "+err.Error())
		if f.focusTries >= reviewFocusTries && !f.o.wait {
			fmt.Fprintln(f.c.Stderr, "magnum review: "+f.focusMissed())
			f.out.Error = f.focusMissed()
			return f.finish(1, pr), true
		}
		return 0, false
	}
	f.focused = true
	fmt.Fprintln(f.progress, actFocusLine(res))
	if !f.o.wait {
		return f.finish(0, pr), true
	}
	return 0, false
}

// focusSession is the judge session to focus: only once the handled request's
// round selected or created it (the PR is in flight, or the round's judge run
// exists); with --fresh only the conversation the round started, not the one
// it is about to park.
func (f *reviewFollower) focusSession(ctx context.Context, pr store.PR) (store.Session, bool) {
	if !f.reqDone || !(slices.Contains(prInFlight, pr.State) || f.judgeRun) {
		return store.Session{}, false
	}
	sess, err := f.d.Store.LiveSessionByPRRole(ctx, pr.ID, f.judge)
	if err != nil || store.Deref(sess.HerdrPaneID) == "" {
		return store.Session{}, false
	}
	if f.o.fresh && f.reqID != 0 && sess.StartedAt.Before(f.start) {
		return store.Session{}, false
	}
	return sess, true
}

// completion checks whether the followed round is over: a dry run ended, a
// review was posted, or the PR needs attention, paused or closed. done is
// true with the exit code then.
func (f *reviewFollower) completion(ctx context.Context, pr store.PR) (code int, done bool) {
	if !f.reqDone {
		return 0, false
	}
	// A dry-run round records no review; its end is the round.dry_run
	// event (the PR is back in its earlier state).
	if msg, ok := reviewDryRunEnded(ctx, f.d, f.t, f.start); ok {
		fmt.Fprintln(f.progress, msg)
		return 0, true
	}
	if pr.ReviewedAt != nil && !pr.ReviewedAt.Before(f.start) {
		ev, url, login := reviewPosted(ctx, f.d, pr, f.start)
		f.out.Event, f.out.ReviewURL = ev, url
		msg := fmt.Sprintf("reviewed %s: %s on %s", f.label, ev, sha7(store.Deref(pr.ReviewedSHA)))
		if login != "" {
			msg += " as " + login
		}
		if url != "" {
			msg += " · " + url
		}
		fmt.Fprintln(f.progress, msg)
		return 0, true
	}
	ew := f.c.Stderr
	switch pr.State {
	case store.PRNeedsAttention:
		f.out.Error = store.Deref(pr.LastError)
		why := attention.Explain("", f.out.Error, f.label)
		fmt.Fprintf(ew, "%s needs attention: %s\nfix: %s\n", f.label, why.Summary, why.Fix)
		return 1, true
	case store.PRPaused:
		f.out.Error = store.Deref(pr.LastError)
		when := ""
		if pr.NextAttemptAt != nil {
			when = "; it continues after " + pr.NextAttemptAt.Local().Format("Jan 2 15:04")
		}
		fmt.Fprintf(ew, "%s paused: %s%s (`magnum status` shows the pause)\n", f.label, store.Deref(pr.LastError), when)
		return 1, true
	case store.PRClosed, store.PRReleasing, store.PRReleased:
		if pr.GHState == store.GHMerged { // a post-merge round ended without a review
			f.out.Error = cmp.Or(store.Deref(pr.LastError), "the post-merge round ended without a review")
			fmt.Fprintf(ew, "%s: the post-merge review was not posted: %s\n", f.label, f.out.Error)
			return 1, true
		}
		f.out.Error = "the PR closed"
		fmt.Fprintf(ew, "%s closed on GitHub (%s); stopped following\n", f.label, pr.State)
		return 1, true
	}
	return 0, false
}

// reviewPostMergeScope is what a post-merge review of the merged pr covers:
// "1111111..2222222" since its last review, else "the whole PR at 2222222".
func reviewPostMergeScope(pr store.PR) string {
	if rs := store.Deref(pr.ReviewedSHA); rs != "" {
		return sha7(rs) + ".." + sha7(pr.HeadSHA)
	}
	return "the whole PR at " + sha7(pr.HeadSHA)
}

// reviewDryRunEnded reports the end of a dry-run round started at or after
// start (its round.dry_run audit event).
func reviewDryRunEnded(ctx context.Context, d *actDeps, t actTarget, start time.Time) (string, bool) {
	evs, err := d.Store.EventsBySubject(ctx, fmt.Sprintf("pr:%s#%d", t.full(), t.PR.Number), 20)
	if err != nil {
		return "", false
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if ev := evs[i]; ev.Kind == "round.dry_run" && !ev.At.Before(start) {
			return ev.Message, true
		}
	}
	return "", false
}

// reviewPosted finds the review the round posted: event, URL and login.
func reviewPosted(ctx context.Context, d *actDeps, pr store.PR, start time.Time) (event, url, login string) {
	event = store.Deref(pr.LastReviewEvent)
	rs, err := d.Store.RunsByPR(ctx, pr.ID)
	if err != nil {
		return event, "", ""
	}
	for i := len(rs) - 1; i >= 0; i-- {
		r := rs[i]
		if !actIsJudge(d.Cfg, r.Role) || r.CreatedAt.Before(start) || store.Deref(r.ReviewURL) == "" {
			continue
		}
		if e := store.Deref(r.ReviewEvent); e != "" {
			event = e
		}
		return event, store.Deref(r.ReviewURL), r.ReviewerLogin
	}
	return event, "", ""
}

// reviewRoles resolves the --role values against the PR's watch: the
// configured names, once each. A role the watch does not run, or one with
// runs = "never", is refused here as the daemon would.
func reviewRoles(cfg *config.Config, full string, requested []string) ([]string, error) {
	var out []string
	for _, s := range requested {
		r, err := actRoleFor(cfg, full, s)
		if err != nil {
			return nil, err
		}
		if r.Runs == config.RunsNever {
			return nil, fmt.Errorf("role %s is disabled (runs = %q in config.toml)", r.Name, config.RunsNever)
		}
		if !slices.Contains(out, r.Name) {
			out = append(out, r.Name)
		}
	}
	return out, nil
}
