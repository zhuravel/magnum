package cli

// `magnum prs`: the PR board. On a terminal it opens tui.RunPRBoard (tab
// switches to the status dashboard and back); elsewhere, and with --json, it
// prints the same rows. Rows come from the registry only (store.Board): the
// board never asks GitHub, the daemon's poller keeps the fields fresh.

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

const prsUsage = "[--repo owner/name|name] [--view all|magnum|mine|ready] [--sort updated|last-review|reviewer-activity|requested|changes|state] [--desc] [--all] [--json] [--limit N]"

func newPRsCmd(c *Context) *cobra.Command {
	var f prsFlags
	cmd := newCommand(groupInspect, "prs "+prsUsage, "PR board: every watched PR with its reviews, reviewers and changes since",
		"List the pull requests magnum knows of, most recently updated first, with the last review (who, verdict, "+
			"whether the head moved since), every reviewer's latest verdict or pending request, and what changed "+
			"since the last review. Rows come from the registry only; the daemon's poller keeps them fresh, so the "+
			"board never spends GitHub rate budget.\n\n"+
			"On a terminal this opens the live board, refreshed every 5s: j/k move, enter shows the PR card (with the "+
			"last round's stage timings), / filters (fuzzy words plus state:<s>, assignee:<login>, author:<login> and "+
			"review:requested; @me means you), v cycles the views, s cycles the sort and S reverses it, L cycles the "+
			"layout (auto: one line per PR when every column fits, else two; one line; two lines; kept across runs), "+
			"r reviews (R fresh, i with /simplify), o opens the "+
			"judge's pane, p/u pin and unpin, x releases, M/U mute and unmute (every review, release, mute and unmute "+
			"key asks y/N first; only y confirms), b opens the PR in the browser, a "+
			"jumps to what needs you, ctrl+r or F5 refreshes now, tab switches to the status dashboard (and back), ? lists "+
			"every key and q quits. With the mouse ([terminal] mouse, on by default; m toggles it) the wheel scrolls, a "+
			"click selects and a double click opens the card, a click on a heading sorts by it (again reverses), "+
			"dragging the gap between two headings resizes a column (W resets) and a right click opens the PR's "+
			"actions. Elsewhere, or with --json, it prints the rows once; the JSON has snake_case keys, times in RFC 3339 "+
			"(left out while unset) and durations in seconds.\n\n"+
			"--view picks the rows: all, magnum (what magnum reviewed or is reviewing), mine (assigned to you or your "+
			"review requested) or ready (approved, no changes requested, not a draft); on the live board it is the "+
			"view the board opens in. "+
			"PRs GitHub merged or closed within [board] recent_closed (24h by default; \"0\" turns it off) follow the "+
			"open ones, newest closed first, dimmed; \"merged · unreviewed\" (closed,merged,unreviewed in the printed "+
			"rows) marks one GitHub merged before magnum reviewed its last push. "+
			"--repo shows one repository, --all adds every closed and merged PR, --limit caps the rows. --sort picks the "+
			"order (updated, last-review, reviewer-activity, requested, changes, state); --desc (the default) puts the "+
			"newest, latest, most recently requested, most changed or most urgent first and --desc=false reverses the "+
			"printed rows. REQUESTED is when a review was last requested of you (starred on the board), else of "+
			"anyone; requested sorts by it, PRs with no request last.",
		func(pos []string) int { return runPRs(c, f, pos) })
	fs := cmd.Flags()
	fs.StringVar(&f.repo, "repo", "", "show one repository (owner/name or name)")
	fs.StringVar(&f.view, "view", string(tui.ViewAll), "rows: all, magnum, mine or ready (v cycles them on the board)")
	fs.StringVar(&f.sort, "sort", string(tui.SortUpdated), "order: updated, last-review, reviewer-activity, requested, changes or state")
	fs.BoolVar(&f.desc, "desc", true, "largest first (newest, latest, most recently requested, most changed, most urgent); --desc=false reverses")
	fs.BoolVar(&f.all, "all", false, "also list closed and merged PRs")
	fs.BoolVar(&f.json, "json", false, "print JSON")
	fs.IntVar(&f.limit, "limit", 0, "show at most N PRs (0 = all)")
	_ = cmd.RegisterFlagCompletionFunc("repo", completeFlag(c.completeRepos))
	_ = cmd.RegisterFlagCompletionFunc("sort", completeFlag(completeSorts))
	_ = cmd.RegisterFlagCompletionFunc("view", completeFlag(completeViews))
	return cmd
}

// prsFlags are the parsed `magnum prs` flags.
type prsFlags struct {
	repo, sort, view string
	desc, all, json  bool
	limit            int
}

// prsOptions are what the board and the printed rows show.
type prsOptions struct {
	Repo  string
	Owner string // the board's owner scope (O), kept across a tab round-trip; "" = all
	View  tui.PRView
	Sort  tui.PRSort
	Desc  bool
	All   bool
	Limit int
}

// filter is the registry query of o. The registry orders by last update,
// so the board (which re-sorts interactively) caps the most recently
// updated PRs; the printed rows read everything and cut after the requested
// sort (prsRows).
func (o prsOptions) filter() store.BoardFilter {
	return store.BoardFilter{Repo: o.Repo, IncludeClosed: o.All, Limit: o.Limit}
}

// prsRows reads the PRs of o, keeps o.View's, sorts them by o.Sort and
// o.Desc and only then applies o.Limit, so `--sort changes --limit 1` is
// the most changed PR.
func prsRows(ctx context.Context, st *store.Store, cfg *config.Config, o prsOptions, self []string, layout paths.Layout) ([]tui.PRBoardRow, error) {
	f := o.filter()
	f.Limit = 0
	rows, err := prsSource(st, cfg, f, self, layout)(ctx)
	if err != nil {
		return nil, err
	}
	rows = tui.SortPRBoard(tui.FilterPRBoard(rows, o.View, self), o.Sort, o.Desc)
	if o.Limit > 0 && len(rows) > o.Limit {
		rows = rows[:o.Limit]
	}
	return rows, nil
}

func runPRs(c *Context, f prsFlags, pos []string) int {
	if len(pos) > 0 {
		return inspUsage(c, "prs", fmt.Sprintf("unexpected argument %q", pos[0]), prsUsage)
	}
	by, err := tui.ParsePRSort(f.sort)
	if err != nil {
		return inspUsage(c, "prs", err.Error(), prsUsage)
	}
	view, err := tui.ParsePRView(f.view)
	if err != nil {
		return inspUsage(c, "prs", "--view: "+err.Error(), prsUsage)
	}
	if f.limit < 0 {
		return inspUsage(c, "prs", "--limit must be 0 (all) or more", prsUsage)
	}
	repo := strings.TrimSpace(f.repo)
	if owner, name, ok := strings.Cut(repo, "/"); ok && (owner == "" || name == "" || strings.Contains(name, "/")) {
		return inspUsage(c, "prs", fmt.Sprintf("--repo %q: want owner/name or name", f.repo), prsUsage)
	}
	o := prsOptions{Repo: repo, View: view, Sort: by, Desc: f.desc, All: f.all, Limit: f.limit}

	a, err := inspOpenApp(c, false)
	if err != nil {
		return cmdFail(c, "prs", err)
	}
	defer a.Close()
	ctx, cancel := signalContext()
	defer cancel()
	d := newStatusDeps(a)

	if !f.json && inspScreen(c) {
		if err := runInspScreens(ctx, c, d, statusOptions{}, o, screenBoard); err != nil {
			return cmdFail(c, "prs", err)
		}
		return 0
	}
	rows, err := prsRows(ctx, d.Store, d.Config, o, prsSelfLogins(d.Config), c.Layout)
	if err != nil {
		return cmdFail(c, "prs", err)
	}
	var b bytes.Buffer
	if f.json {
		err = writeJSON(&b, prsJSONRows(rows))
	} else {
		prsRender(&b, rows, prsDefaultRepo(d.Config), inspNow())
	}
	if err != nil {
		return cmdFail(c, "prs", err)
	}
	io.WriteString(c.Stdout, b.String())
	return 0
}

// runInspScreens runs the status dashboard and the PR board with one set of
// actions, starting with first; tab switches between them. Both keep their
// dragged column widths in the registry, the mouse toggle (m) carries over
// to the other screen and the board reopens in the view it was left in,
// with the hide toggle (h) and the layout (L) the registry keeps.
func runInspScreens(ctx context.Context, c *Context, d statusDeps, so statusOptions, po prsOptions, first string) error {
	acts := newScreenActions(c)
	defer acts.Close()
	mouse := d.Config == nil || d.Config.Terminal.Mouse
	toggled := func(on bool) { mouse = on }
	var widths tui.ColumnWidths
	if d.Store != nil {
		widths = kvColumnWidths{st: d.Store}
	}
	dashboard := func(ctx context.Context) error {
		return tuiDashboard(ctx, statusDashSource(d, so), acts, tui.DashboardOptions{
			Refresh: statusRefresh, ShowManual: so.All, Title: "magnum status", Now: inspNow, Judge: rolesJudgeName(d.Config),
			Icons: screenIcons(d.Config), NoMouse: !mouse, MouseToggled: toggled, Widths: widths,
		})
	}
	src := prsSource(d.Store, d.Config, po.filter(), prsSelfLogins(d.Config), c.Layout) // one source: its timings cache survives tab
	hide, layout := false, tui.LayoutAuto                                               // h and L: kept in the registry, so the board opens the way it was left
	if d.Store != nil {
		if v, ok, err := d.Store.GetKV(ctx, kvBoardHideSkipped); err == nil && ok {
			hide = v == "1"
		}
		if v, ok, err := d.Store.GetKV(ctx, kvBoardLayout); err == nil && ok {
			layout = tui.PRLayout(v) // the board reads an unknown one as auto
		}
	}
	board := func(ctx context.Context) error {
		o := prsBoardOptions(d.Config, po)
		o.NoMouse, o.MouseToggled, o.Widths = !mouse, toggled, widths
		o.ViewChanged = func(v tui.PRView) { po.View = v }
		o.OwnerChanged = func(owner string) { po.Owner = owner }
		o.HideSkipped = hide
		o.HideToggled = func(h bool) {
			hide = h
			if d.Store != nil {
				_ = d.Store.SetKV(context.WithoutCancel(ctx), kvBoardHideSkipped, map[bool]string{true: "1", false: "0"}[h])
			}
		}
		o.Layout = layout
		o.LayoutChanged = func(l tui.PRLayout) {
			layout = l
			if d.Store != nil {
				_ = d.Store.SetKV(context.WithoutCancel(ctx), kvBoardLayout, string(l))
			}
		}
		return tuiPRBoard(ctx, src, acts, o)
	}
	return runScreens(ctx, first, dashboard, board)
}

// kvBoardHideSkipped keeps the board's h (hide ignored and skipped PRs),
// kvBoardLayout its L (auto, 1-line or 2-line).
const (
	kvBoardHideSkipped = "board.hide_skipped"
	kvBoardLayout      = "board.layout"
)

// prsBoardOptions are the board's options for o.
func prsBoardOptions(cfg *config.Config, o prsOptions) tui.PRBoardOptions {
	return tui.PRBoardOptions{SelfLogins: prsSelfLogins(cfg), DefaultSort: o.Sort, DefaultView: o.View, Repo: o.Repo, Now: inspNow,
		Judge: rolesJudgeName(cfg), Icons: screenIcons(cfg), DefaultRepo: defaultRepo(cfg), DefaultOwner: o.Owner,
		RecentClosed: prsRecentClosed(cfg)}
}

// screenIcons is the screens' symbols, [terminal] icons: the screens read
// an empty or unknown mode as unicode.
func screenIcons(cfg *config.Config) tui.IconMode {
	if cfg == nil {
		return tui.IconsUnicode
	}
	return tui.IconMode(cfg.Terminal.Icons)
}

// prsSource reads the board's rows from the registry; it never asks GitHub.
// Notes says whether the repository has reviewer notes under layout
// (looked up once per repository and load); LastRound carries the stage
// timings of each PR's last round (cfg names the judge). Each load also
// reads the PRs GitHub merged or closed within [board] recent_closed of
// now and marks them Recent, the board's section after the open PRs.
func prsSource(st *store.Store, cfg *config.Config, f store.BoardFilter, self []string, layout paths.Layout) tui.PRBoardSourceFunc {
	timings := &boardTimings{}
	window := prsRecentClosed(cfg)
	return func(ctx context.Context) ([]tui.PRBoardRow, error) {
		if st == nil {
			return nil, errors.New("no registry")
		}
		f := f
		if window > 0 {
			f.ClosedSince = inspNow().Add(-window)
		}
		rows, err := st.Board(ctx, f)
		if err != nil {
			return nil, err
		}
		notes := map[string]bool{}
		required := map[string]store.RequiredChecks{} // per repository, for this load
		trackers := map[string][]config.Tracker{}     // per repository, for this load
		out := make([]tui.PRBoardRow, 0, len(rows))
		ids := make([]int64, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.PRID)
			row := prsBoardRow(r, self)
			row.Recent = window > 0 && !row.ClosedAt.IsZero() && !row.ClosedAt.Before(f.ClosedSince) &&
				(r.GHState == store.GHMerged || r.GHState == store.GHClosed)
			full := r.Owner + "/" + r.Name
			req, ok := required[full]
			if !ok {
				var configured []string
				if cfg != nil {
					configured = cfg.RequiredChecks(full)
				}
				req, _ = st.RequiredChecks(ctx, full, configured)
				required[full] = req
			}
			row.CI = prsCI(r.CI, r.HeadSHA, req)
			if cfg != nil {
				row.Badges = prsBadges(r.Labels, cfg.Board.Badges)
			}
			if cfg != nil {
				ts, ok := trackers[full]
				if !ok {
					ts = cfg.TrackersFor(full)
					trackers[full] = ts
				}
				row.Issue, row.IssueURL = config.Issue(r.Title, ts)
			}
			has, seen := notes[full]
			if !seen {
				has = notesExist(layout, r.Owner, r.Name)
				notes[full] = has
			}
			row.Notes = has
			if r.State == store.PRQueued || r.State == store.PRRereviewPending {
				if v, ok, err := st.GetKV(ctx, engine.KVPRWait(r.PRID)); err == nil && ok {
					if w, ok := engine.ParseWait(v); ok {
						defRepo := ""
						if cfg != nil {
							defRepo = cfg.Daemon.DefaultRepo
						}
						row.Wait, row.WaitDetail = w.Short(inspNow()), w.Sentence(actRefLabel(defRepo, full, r.Number), inspNow())
					}
				}
			}
			if r.LastError != "" {
				pr := store.PR{ID: r.PRID, State: r.State, LastError: &r.LastError}
				extra := strings.TrimPrefix(row.LastError, r.LastError) // "; compare since the last review: …"
				if why, ok := prAttention(ctx, st, pr, actRefLabel(defaultRepo(cfg), full, r.Number)); ok {
					row.LastError, row.ErrorFix, row.ErrorDetail = why.Summary+extra, why.Fix, why.Tail
				} else {
					row.LastError = errorSummary(r.LastError) + extra
				}
			}
			if v, ok, err := st.GetKV(ctx, engine.KVPRTrivial(r.PRID)); err == nil && ok {
				if t, ok := engine.ParseTrivialSkip(v); ok {
					row.Note = t.Note()
				}
			}
			out = append(out, row)
		}
		if sums, err := st.LastReviewSummaries(ctx, ids); err == nil {
			for i := range out {
				if s, ok := sums[ids[i]]; ok {
					out[i].Findings = &tui.FindingsInfo{Counts: s.Counts, Simplifications: s.Simplifications,
						Fixed: s.Fixed, Open: s.Open, Answered: s.Answered, Verdict: s.Verdict, Posted: s.Event, SHA: s.SHA}
				}
			}
		}
		if err := timings.fill(ctx, st, cfg, ids, out, inspNow()); err != nil {
			return nil, err
		}
		return out, nil
	}
}

// prsRecentClosed is [board] recent_closed (0 without a config: the section
// is off).
func prsRecentClosed(cfg *config.Config) time.Duration {
	if cfg == nil {
		return 0
	}
	return max(cfg.Board.RecentClosed.Duration, 0)
}

// prsSelfLogins are the logins that count as "me" on the board: every
// watch's posting identity and every gh identity (the user).
func prsSelfLogins(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(login string) {
		if k := prsLoginKey(login); k != "" && !seen[k] {
			seen[k] = true
			out = append(out, login)
		}
	}
	for _, w := range cfg.Watches {
		if id := cfg.IdentityByName(w.Identity); id != nil {
			add(id.Login)
		}
	}
	for _, id := range cfg.Identities {
		if id.Kind == "gh" {
			add(id.Login)
		}
	}
	return out
}

// prsAccountKey folds a login for telling accounts apart: case and "@"
// dropped, "[bot]" kept (the App "zhuravel[bot]" is not the user "zhuravel").
func prsAccountKey(s string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "@")
}

// prsLoginKey folds a login for "mine": case, a leading "@" and a "[bot]"
// suffix do not matter, so the user and magnum's App both count.
func prsLoginKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimSuffix(strings.TrimPrefix(s, "@"), "[bot]")
}

// prsBoardState maps the automation state onto the board's states: the
// short-lived claiming and verifying steps read as reviewing, releasing as
// closed.
func prsBoardState(s string) string {
	switch s {
	case store.PRClaiming, store.PRVerifying:
		return store.PRReviewing
	case store.PRReleasing:
		return store.PRClosed
	}
	return s
}

// prsRowState is the board state of a row: ignored for a PR `magnum
// ignore` muted (until it closes), else prsBoardState.
func prsRowState(b store.BoardRow) string {
	s := prsBoardState(b.State)
	if b.Muted && b.SkipReason == engine.SkipIgnored && s != store.PRClosed && s != store.PRReleased {
		return "ignored"
	}
	return s
}

// prsVerdict normalizes a GitHub review state for ReviewerInfo.Verdict.
func prsVerdict(state string) string {
	switch s := strings.ToUpper(strings.TrimSpace(state)); s {
	case "APPROVED":
		return "approved"
	case "CHANGES_REQUESTED":
		return "changes_requested"
	case "COMMENTED":
		return "commented"
	case "DISMISSED":
		return "dismissed"
	case "PENDING", "":
		return "pending"
	default:
		return strings.ToLower(s)
	}
}

// prsBoardRow maps one registry row onto the board.
func prsBoardRow(b store.BoardRow, self []string) tui.PRBoardRow {
	mine := map[string]bool{}
	for _, l := range self {
		if k := prsLoginKey(l); k != "" {
			mine[k] = true
		}
	}
	r := tui.PRBoardRow{
		Ref: b.Ref, Owner: b.Owner, Repo: b.Name, Number: b.Number,
		Title: b.Title, Author: b.Author, URL: b.URL, Draft: b.Draft,
		Labels: b.Labels, Assignees: b.Assignees,
		State: prsRowState(b), GHState: b.GHState, UpdatedAt: b.UpdatedAt, HeadSHA: b.HeadSHA,
		Slot: b.Slot, Pinned: b.Pinned, Muted: b.Muted, NextEligibleAt: b.NextEligibleAt,
		LastError: b.LastError, RoundsToday: b.RoundsToday, SkipReason: b.SkipReason,
		ClosedAt: cmp.Or(b.MergedAt, b.ClosedAt), MergedUnreviewed: b.MergedUnreviewed, FlagDismissed: b.FlagDismissed,
	}
	if r.Ref == "" && b.Owner != "" && b.Name != "" && b.Number > 0 {
		r.Ref = fmt.Sprintf("%s/%s#%d", b.Owner, b.Name, b.Number)
	}
	r.RequestedToMe, r.LastRequest, r.Requests = prsRequests(b.ReviewRequests, mine)
	if b.ReviewedSHA != "" || b.LastReviewEvent != "" || !b.LastReviewAt.IsZero() {
		r.LastReview = &tui.ReviewInfo{
			Login: b.LastReviewLogin, Event: b.LastReviewEvent, SubmittedAt: b.LastReviewAt, CommitSHA: b.ReviewedSHA,
			Stale: b.HeadSHA != "" && b.ReviewedSHA != b.HeadSHA,
			Mine:  b.LastReviewLogin != "" && mine[prsLoginKey(b.LastReviewLogin)],
		}
	}

	// Reviewers: every latest review, then the requested reviewers who have
	// not reviewed yet; a re-requested reviewer keeps the verdict and is
	// marked requested.
	index := map[string]int{}
	for _, lr := range b.LatestReviews {
		login := lr.Login
		if login == "" {
			login = "ghost" // GitHub's name for a deleted account
		}
		v := tui.ReviewerInfo{
			Login: login, Verdict: prsVerdict(lr.State), CommitSHA: lr.CommitSHA,
			Stale: b.HeadSHA != "" && lr.CommitSHA != b.HeadSHA,
			Mine:  mine[prsLoginKey(login)],
		}
		if lr.SubmittedAt != nil {
			v.SubmittedAt = *lr.SubmittedAt
		}
		if v.Verdict == "pending" {
			v.Stale = false // a draft review has no verdict to go stale
		}
		if i, ok := index[prsAccountKey(login)]; ok {
			r.Reviewers[i] = v // GitHub lists one review per reviewer; keep the last
			continue
		}
		index[prsAccountKey(login)] = len(r.Reviewers)
		r.Reviewers = append(r.Reviewers, v)
	}
	for _, login := range b.RequestedReviewers {
		if login == "" {
			continue
		}
		if i, ok := index[prsAccountKey(login)]; ok {
			r.Reviewers[i].Requested = true
			continue
		}
		index[prsAccountKey(login)] = len(r.Reviewers)
		r.Reviewers = append(r.Reviewers, tui.ReviewerInfo{
			Login: login, Verdict: "pending", Requested: true, Mine: mine[prsLoginKey(login)],
		})
	}

	if s := b.SinceReview; s != nil {
		if s.Error != "" {
			msg := "compare since the last review: " + s.Error
			if r.LastError == "" {
				r.LastError = msg
			} else {
				r.LastError += "; " + msg
			}
		} else {
			d := &tui.ReviewDelta{Base: "reviewed", BaseSHA: s.Base, Commits: s.Commits, Files: s.Files,
				Additions: s.Additions, Deletions: s.Deletions}
			if s.Source == store.SinceFromBase {
				d.Base = "base branch"
			}
			if s.Files < 0 {
				d.Files, d.Truncated = github.CompareFileLimit, true
			}
			r.SinceReview = d
		}
	}
	return r
}

// prsRequests sums up a PR's review requests (oldest first, as the registry
// keeps them): the latest request to each reviewer, newest first, with the
// latest of them all and the latest asking one of mine ("mine" is keyed by
// prsLoginKey, which is what the ★ means everywhere). A request without a
// reviewer or a time says nothing and is left out.
func prsRequests(list []store.ReviewRequest, mine map[string]bool) (toMe, last *tui.RequestInfo, per []tui.RequestInfo) {
	index := map[string]int{}
	for _, q := range list {
		if q.To == "" || q.At.IsZero() {
			continue
		}
		info := tui.RequestInfo{To: q.To, By: q.By, At: q.At, Mine: mine[prsLoginKey(q.To)]}
		if i, ok := index[prsAccountKey(q.To)]; ok {
			if !info.At.Before(per[i].At) {
				per[i] = info
			}
			continue
		}
		index[prsAccountKey(q.To)] = len(per)
		per = append(per, info)
	}
	slices.SortStableFunc(per, func(a, b tui.RequestInfo) int { return b.At.Compare(a.At) })
	if len(per) == 0 {
		return nil, nil, nil
	}
	newest := per[0]
	last = &newest
	if i := slices.IndexFunc(per, func(q tui.RequestInfo) bool { return q.Mine }); i >= 0 {
		asked := per[i]
		toMe = &asked
	}
	return toMe, last, per
}

// prsDefaultRepo is daemon.default_repo ("" without a config).
func prsDefaultRepo(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.Daemon.DefaultRepo
}

// prsRender prints rows as a table.
func prsRender(w io.Writer, rows []tui.PRBoardRow, defaultRepo string, now time.Time) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "no pull requests (magnum lists what its watches polled; --all adds closed ones)")
		return
	}
	tw := inspTable(w)
	fmt.Fprintln(tw, "REF\tTITLE\tAUTHOR\tASSIGNEE\tUPDATED\tREQUESTED\tSTATE\tLAST REVIEW\tFINDINGS\tCI\tSINCE\tREVIEWERS")
	for _, r := range rows {
		cells := []string{
			prsRefLabel(r, defaultRepo),
			trunc(actClean(r.Title), 48),
			inspOrDash(actClean(r.Author)),
			inspOrDash(actClean(strings.Join(r.Assignees, ","))),
			actAgo(now, r.UpdatedAt),
			prsRequestedCell(r, now),
			prsStateCell(r),
			prsLastReviewCell(r.LastReview, now),
			prsFindingsCell(r.Findings),
			prsCICell(r.CI),
			prsSinceCell(r.SinceReview),
			prsReviewersCell(r.Reviewers),
		}
		for i := range cells {
			cells[i] = strings.ReplaceAll(cells[i], "\t", " ")
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	_ = tw.Flush()
}

// prsRefLabel is the short reference: name#N in the default owner.
func prsRefLabel(r tui.PRBoardRow, defaultRepo string) string {
	if r.Owner != "" && r.Repo != "" && r.Number > 0 {
		return actRefLabel(defaultRepo, r.Owner+"/"+r.Repo, r.Number)
	}
	return actClean(r.Ref)
}

// prsRequestedCell is the review request the board's REQUESTED column shows:
// the latest to me, else the latest to anyone, "me 2h by alice" or
// "bob 3d by alice"; "-" when the PR shows none.
func prsRequestedCell(r tui.PRBoardRow, now time.Time) string {
	q := r.RequestedToMe
	if q == nil {
		q = r.LastRequest
	}
	if q == nil || q.At.IsZero() {
		return "-"
	}
	to := actClean(q.To)
	if r.RequestedToMe != nil {
		to = "me"
	}
	s := to + " " + actAgo(now, q.At)
	if q.By != "" {
		s += " by " + actClean(q.By)
	}
	return s
}

// prsStateCell is the magnum state with the flags that matter
// ("reviewed,pinned,draft"; "closed,merged,unreviewed" for a PR GitHub
// merged before magnum reviewed its last push).
func prsStateCell(r tui.PRBoardRow) string {
	s := inspOrDash(r.State)
	if g := strings.ToUpper(r.GHState); g == "MERGED" || g == "CLOSED" {
		s += "," + strings.ToLower(g)
	}
	if r.MergedUnreviewed {
		s += ",unreviewed"
	}
	for _, f := range []struct {
		on   bool
		name string
	}{{r.Draft, "draft"}, {r.Pinned, "pinned"}, {r.Muted, "muted"}, {r.LastError != "", "error"}} {
		if f.on {
			s += "," + f.name
		}
	}
	return s
}

// prsCICell is the head's CI: the required checks by label when the
// repository has some ("Completion:missing, ci:passed 3/3"), else the counts
// ("passed 65/65", "failed 2/65", "pending 40/65"); "-" when unknown.
func prsCICell(ci *tui.CIInfo) string {
	if ci == nil {
		return "-"
	}
	var s string
	if len(ci.Required) > 0 {
		parts := make([]string, len(ci.Required))
		for i, r := range ci.Required {
			parts[i] = cmp.Or(r.Label, r.Name) + ":" + r.State
			if n := r.Count(); n != "" {
				parts[i] += " " + n
			}
		}
		s = strings.Join(parts, ", ")
	} else {
		switch ci.State {
		case "failed":
			s = fmt.Sprintf("failed %d/%d", ci.Failed, ci.Total)
		case "pending":
			s = fmt.Sprintf("pending %d/%d", ci.Total-ci.Pending, ci.Total)
		case "passed":
			s = fmt.Sprintf("passed %d/%d", ci.Passed, ci.Total)
		default:
			s = ci.State
		}
	}
	if ci.Stale {
		s += " (older commit)"
	}
	return s
}

// prsFindingsCell is what magnum's latest review concluded:
// "blocking P1:1 P2:3 simplify:4", "clean", or "-" when it has not
// reviewed the PR.
func prsFindingsCell(f *tui.FindingsInfo) string {
	if f == nil {
		return "-"
	}
	parts := []string{strings.ReplaceAll(f.Verdict, "_", "-")}
	for i, n := range f.Counts {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("P%d:%d", i, n))
		}
	}
	if f.Simplifications > 0 {
		parts = append(parts, fmt.Sprintf("simplify:%d", f.Simplifications))
	}
	return strings.Join(parts, " ")
}

// prsLastReviewCell is "approved 3h by talkable" (", stale" when the head
// moved since), "-" when nothing was reviewed.
func prsLastReviewCell(li *tui.ReviewInfo, now time.Time) string {
	if li == nil {
		return "-"
	}
	s := prsVerdict(li.Event)
	if li.Event == "" {
		s = "reviewed"
	}
	if !li.SubmittedAt.IsZero() {
		s += " " + actAgo(now, li.SubmittedAt)
	}
	if li.Login != "" {
		s += " by " + actClean(li.Login)
	}
	if li.Stale {
		s += ", stale"
	}
	return s
}

// prsSinceCell is the change since the last review: "3c 5f +120/-4", with
// "PR " in front when nothing was reviewed (the whole PR against its base)
// and "≥" when GitHub truncated the comparison.
func prsSinceCell(d *tui.ReviewDelta) string {
	if d == nil {
		return "-"
	}
	s := fmt.Sprintf("%dc %df +%d/-%d", d.Commits, d.Files, d.Additions, d.Deletions)
	if d.Truncated {
		s = ">=" + s
	}
	if strings.Contains(strings.ToLower(d.Base), "base") {
		s = "PR " + s
	}
	return s
}

// prsReviewersCell lists each reviewer's verdict: "alice(approved),
// team:core(requested)".
func prsReviewersCell(list []tui.ReviewerInfo) string {
	if len(list) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(list))
	for _, v := range list {
		verdict := v.Verdict
		if verdict == "pending" && v.Requested {
			verdict = "requested"
		} else if v.Requested {
			verdict += ",re-requested"
		}
		if v.Stale {
			verdict += ",stale"
		}
		parts = append(parts, actClean(v.Login)+"("+verdict+")")
	}
	return strings.Join(parts, ", ")
}

// completeViews offers the board's views.
func completeViews(string) []cobra.Completion {
	desc := map[tui.PRView]string{
		tui.ViewAll:    "every PR",
		tui.ViewMagnum: "what magnum reviewed or is reviewing",
		tui.ViewMine:   "assigned to you or your review requested",
		tui.ViewReady:  "approved, no changes requested, not a draft",
	}
	var out []cobra.Completion
	for _, v := range tui.PRViews() {
		out = append(out, cobra.CompletionWithDesc(string(v), desc[v]))
	}
	return out
}

// completeSorts offers the board's sorts.
func completeSorts(string) []cobra.Completion {
	desc := map[tui.PRSort]string{
		tui.SortUpdated:          "newest update first",
		tui.SortLastReview:       "latest review first",
		tui.SortReviewerActivity: "latest verdict by anyone first",
		tui.SortRequested:        "latest review request first (yours, else anyone's)",
		tui.SortChanges:          "most lines changed since the review first",
		tui.SortState:            "most urgent state first",
	}
	var out []cobra.Completion
	for _, s := range tui.PRSorts() {
		out = append(out, cobra.CompletionWithDesc(string(s), desc[s]))
	}
	return out
}

// defaultRepo is [daemon] default_repo ("" without a config).
func defaultRepo(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.Daemon.DefaultRepo
}
