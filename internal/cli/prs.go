package cli

// `magnum tui` (alias `prs`, and what a bare `magnum` runs): the PR board.
// On a terminal it opens tui.RunPRBoard (tab
// switches to the status dashboard and back); elsewhere, and with --json, it
// prints the same rows. Rows come from the registry only (store.Board): the
// board never asks GitHub, the daemon's poller keeps the fields fresh.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/prview"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/tui"
)

const prsUsage = "[--repo owner/name|name] [--view all|magnum|mine|ready] [--sort updated|last-review|reviewer-activity|requested|changes|state] [--desc] [--all] [--needs-me] [--auto-approved] [--json] [--limit N]"

func newPRsCmd(c *Context) *cobra.Command {
	var f prsFlags
	cmd := newCommand(groupInspect, "tui "+prsUsage, "PR board: every watched PR with its reviews, reviewers and changes since (what `magnum` alone opens)",
		"List the pull requests magnum knows of, most recently updated first, with the last review (who, verdict, "+
			"whether the head moved since), every reviewer's latest verdict or pending request, and what changed "+
			"since the last review. Rows come from the registry only; the daemon's poller keeps them fresh, so the "+
			"board never spends GitHub rate budget.\n\n"+
			"On a terminal this opens the live board, refreshed every 5s: j/k move (g/G to the first and last PR), enter "+
			"shows the PR card (with the last round's stage timings), / filters (fuzzy words plus state:<s>, "+
			"assignee:<login>, author:<login> and review:requested; @me means you), v cycles the views and O the "+
			"owners (the factory screen's O too: both open in the owner you left), s cycles the sort and S reverses it, h hides ignored and skipped PRs. On the selected PR: r "+
			"reviews (R fresh, i with /simplify), K kills its running or queued review, I ignores it (kill, mute, free "+
			"its slot), A/C approve and request changes (on a row that needs you, A approves as you), D withdraws "+
			"the approval magnum posted as you, z snoozes it for 2h (or lifts its snooze), f fixes its findings as the fixer's login "+
			"(F takes it out of autofix or puts it back; on a PR whose fix loop left you calls, d says you did them all: "+
			"back to reviewed, with no review), c fixes your PR's failed required CI as the fixer's login (asking y/N with the "+
			"daemon's plan), P merges its base into it as the fixer App's bot (asking y/N with the daemon's "+
			"plan; while magnum holds a merge the fixer resolved for your y, it pushes that one, and e shows its "+
			"held-merge.diff in the pager), E deploys your PR to its staging envs (1-3 choose them, a all, enter asks y/N with the daemon's plan, as "+
			"`magnum staging --deploy` does), w opens its whiteboard, o opens the judge's pane, "+
			"p/u pin and unpin, * stars it or unstars it (`magnum star`), x releases, M/U mute and unmute, b opens the "+
			"PR in the browser and t its issue in the tracker; every one of them but o, p, u, *, b and t asks y/N "+
			"first, and only y confirms. Starred PRs (✪ before the title) lead their section, the open PRs or the "+
			"recently closed ones, newest update first whatever the sort. a jumps to "+
			"what needs you, ! shows the action log, ctrl+r or F5 refreshes now, tab switches to the status "+
			"dashboard (and back), `,` opens the settings screen (esc comes back), ? lists every key and q quits. With the mouse ([terminal] mouse, on by default; "+
			"m toggles it) the wheel scrolls, a "+
			"click selects and a double click opens the card, a click on a heading sorts by it (again reverses), "+
			"dragging the gap between two headings resizes a column (W resets) and a right click opens the PR's "+
			"actions. Elsewhere, or with --json, it prints the rows once; the JSON has snake_case keys, times in RFC 3339 "+
			"(left out while unset) and durations in seconds.\n\n"+
			"--view picks the rows: all, magnum (what magnum reviewed or is reviewing, or your review is requested on), mine (assigned to you or your "+
			"review requested) or ready (approved, no changes requested, not a draft, no staging deploy first); on the live board it is the "+
			"view the board opens in. "+
			"PRs GitHub merged or closed within [board] recent_closed (24h by default; \"0\" turns it off) follow the "+
			"open ones, newest closed first, dimmed; \"merged · unreviewed\" (closed,merged,unreviewed in the printed "+
			"rows) marks one GitHub merged before magnum reviewed its last push. "+
			"\"✔ needs you\" (needs-you in the printed rows) marks a PR magnum approved on its head that GitHub still "+
			"blocks on your approval, because it never counts a GitHub App's (its review decision is REVIEW_REQUIRED), "+
			"and \"✔ lift your ✗\" (lift-yours) one whose only changes request is yours; the updated sort lists them "+
			"first, the title counts them and --needs-me shows only them, and your own PRs whose merge is yours but must "+
			"wait for a staging deploy (deploy-first in the printed rows; deploy_before_merge and open.deploy in the JSON), "+
			"and the PRs where magnum stopped arguing after two rebuttals that you have not decided yet (stalemate). "+
			"Your own open PRs carry autofix = \"open\"'s verdict as a tag before the title (open: needs you, open: waits, "+
			"open: none for nothing open, open: looking while the verdict no longer tells the PR's state) and on the card "+
			"(verdict: ...; open in the JSON); with [board] own_prs = \"factory\" they are left out (your drafts and closed PRs "+
			"stay), since the factory screen lists them, and the board's footer counts them. "+
			"--repo shows one repository, --all adds every closed and merged PR, --limit caps the rows once they are "+
			"sorted (on the live board too, so a starred PR is never left out for its age). --sort picks the "+
			"order (updated, last-review, reviewer-activity, requested, changes, state); --desc (the default) puts the "+
			"newest, latest, most recently requested, most changed or most urgent first and --desc=false reverses the "+
			"printed rows. REQUESTED is when a review was last requested of you (marked ★ on the board), else of "+
			"anyone; requested sorts by it, PRs with no request last.",
		func(pos []string) int { return runPRs(c, f, pos) })
	cmd.Aliases = []string{"prs"} // the board's name until 2026-10-15: scripts and the gh shim keep working
	fs := cmd.Flags()
	fs.StringVar(&f.repo, "repo", "", "show one repository (owner/name or name)")
	fs.StringVar(&f.view, "view", string(tui.ViewAll), "rows: all, magnum, mine or ready (v cycles them on the board)")
	fs.StringVar(&f.sort, "sort", string(tui.SortUpdated), "order: updated, last-review, reviewer-activity, requested, changes or state")
	fs.BoolVar(&f.desc, "desc", true, "largest first (newest, latest, most recently requested, most changed, most urgent); --desc=false reverses")
	fs.BoolVar(&f.all, "all", false, "also list closed and merged PRs")
	fs.BoolVar(&f.needsMe, "needs-me", false, "only the PRs magnum approved that still need your approval on GitHub, "+
		"your own PRs whose merge is yours (approved, or no review required) but must go to a staging first, "+
		"or the PRs where magnum stopped arguing that you have not decided yet")
	fs.BoolVar(&f.autoApproved, "auto-approved", false, "only the PRs magnum approved as you ([[watch]] auto_approve) whose approval stands")
	fs.BoolVar(&f.json, "json", false, "print JSON")
	fs.IntVar(&f.limit, "limit", 0, "show at most N PRs (0 = all)")
	_ = cmd.RegisterFlagCompletionFunc("repo", completeFlag(c.completeRepos))
	_ = cmd.RegisterFlagCompletionFunc("sort", completeFlag(completeSorts))
	_ = cmd.RegisterFlagCompletionFunc("view", completeFlag(completeViews))
	return cmd
}

// prsFlags are the parsed `magnum tui` flags.
type prsFlags struct {
	repo, sort, view                       string
	desc, all, json, needsMe, autoApproved bool
	limit                                  int
}

// prsOptions are what the board and the printed rows show.
type prsOptions struct {
	Repo  string
	Owner string // the board's owner scope (O): kvScreenOwner's at each open, else the last board's; "" = all
	View  tui.PRView
	Sort  tui.PRSort
	Desc  bool
	All   bool
	Limit int
	// NeedsMe lists only the PRs that need the operator's approval
	// (tui.PRBoardRow.NeedsMe), their own PRs whose merge is theirs but
	// waits for a staging deploy (tui.PRBoardRow.DeployNeedsYou) and the
	// PRs with a stalemate they have not acted on (tui.PRBoardRow.Stalemate):
	// --needs-me.
	NeedsMe bool
	// AutoApproved lists only the PRs magnum approved as the operator whose
	// approval stands (tui.PRBoardRow.AutoApproved): --auto-approved.
	AutoApproved bool
}

// filter is the registry query of o. It never caps the rows: the registry
// orders by the last update, while the printed rows (prsRows) and the live
// board (tui.PRBoardOptions.Limit) cut after their own sort, which puts the
// starred PRs first however old.
func (o prsOptions) filter() store.BoardFilter {
	return store.BoardFilter{Repo: o.Repo, IncludeClosed: o.All}
}

// prsRows reads the PRs of o, keeps o.View's, sorts them by o.Sort and
// o.Desc and only then applies o.Limit, so `--sort changes --limit 1` is
// the most changed PR.
func prsRows(ctx context.Context, st *store.Store, cfg *config.Config, o prsOptions, self []string, layout paths.Layout) ([]tui.PRBoardRow, error) {
	rows, err := ownPRsSource(cfg, o.source(prsSource(st, cfg, o.filter(), self, layout)), nil)(ctx)
	if err != nil {
		return nil, err
	}
	rows = tui.SortPRBoard(tui.FilterPRBoard(rows, o.View, self), o.Sort, o.Desc)
	if o.Limit > 0 && len(rows) > o.Limit {
		rows = rows[:o.Limit]
	}
	return rows, nil
}

// ownPRsSource is src without your own open PRs when the factory screen
// lists them ([board] own_prs = "factory": cfg.OwnPRsOnFactory): every row
// GitHub has open, not a draft (the factory screen lists no drafts), whose
// author is one of your logins (config.SelfMatch, the logins magnum's look
// takes as yours) and whose repository a [[watch]] covers (the factory
// screen lists no other) leaves; a closed or merged one stays in its
// section. hidden, when set, keeps how many the last load left out, for
// the board's footer. src itself otherwise.
func ownPRsSource(cfg *config.Config, src tui.PRBoardSourceFunc, hidden *atomic.Int64) tui.PRBoardSourceFunc {
	if !cfg.OwnPRsOnFactory() {
		return src
	}
	self := cfg.SelfMatch()
	return func(ctx context.Context) ([]tui.PRBoardRow, error) {
		rows, err := src(ctx)
		if err != nil {
			return rows, err
		}
		n := 0
		rows = slices.DeleteFunc(rows, func(r tui.PRBoardRow) bool {
			own := !r.Draft && strings.EqualFold(r.GHState, store.GHOpen) && self(r.Author) && cfg.WatchFor(r.Owner+"/"+r.Repo) != nil
			if own {
				n++
			}
			return own
		})
		if hidden != nil {
			hidden.Store(int64(n))
		}
		return rows, nil
	}
}

// source is src narrowed to what o shows beyond the registry query: with
// NeedsMe, the PRs that need the operator's approval, their own PRs whose
// merge waits for their staging deploy (DeployNeedsYou) and the PRs with
// threads magnum stopped arguing in that they have not acted on
// (PRBoardRow.Stalemate); with AutoApproved, those magnum approved as the
// operator.
func (o prsOptions) source(src tui.PRBoardSourceFunc) tui.PRBoardSourceFunc {
	if !o.NeedsMe && !o.AutoApproved {
		return src
	}
	return func(ctx context.Context) ([]tui.PRBoardRow, error) {
		rows, err := src(ctx)
		return slices.DeleteFunc(rows, func(r tui.PRBoardRow) bool {
			return (o.NeedsMe && r.NeedsMe == "" && !r.DeployNeedsYou() && len(r.Stalemate) == 0) || (o.AutoApproved && r.AutoApproved == nil)
		}), err
	}
}

func runPRs(c *Context, f prsFlags, pos []string) int {
	if len(pos) > 0 {
		return inspUsage(c, "tui", fmt.Sprintf("unexpected argument %q", pos[0]), prsUsage)
	}
	by, err := tui.ParsePRSort(f.sort)
	if err != nil {
		return inspUsage(c, "tui", err.Error(), prsUsage)
	}
	view, err := tui.ParsePRView(f.view)
	if err != nil {
		return inspUsage(c, "tui", "--view: "+err.Error(), prsUsage)
	}
	if f.limit < 0 {
		return inspUsage(c, "tui", "--limit must be 0 (all) or more", prsUsage)
	}
	repo, err := repoFlag(f.repo)
	if err != nil {
		return inspUsage(c, "tui", err.Error(), prsUsage)
	}
	o := prsOptions{Repo: repo, View: view, Sort: by, Desc: f.desc, All: f.all, Limit: f.limit, NeedsMe: f.needsMe, AutoApproved: f.autoApproved}

	a, err := inspOpenApp(c, false)
	if err != nil {
		return cmdFail(c, "tui", err)
	}
	defer a.Close()
	ctx, cancel := signalContext()
	defer cancel()
	d := newStatusDeps(a, c.Version)

	if !f.json && inspScreen(c) {
		if err := runInspScreens(ctx, c, d, statusOptions{}, o, screenBoard); err != nil {
			return cmdFail(c, "tui", err)
		}
		return 0
	}
	rows, err := prsRows(ctx, d.Store, d.Config, o, prsSelfLogins(d.Config), c.Layout)
	if err != nil {
		return cmdFail(c, "tui", err)
	}
	var b bytes.Buffer
	if f.json {
		err = writeJSON(&b, prsJSONRows(rows))
	} else {
		prsRender(&b, rows, prsDefaultRepo(d.Config), inspNow())
	}
	if err != nil {
		return cmdFail(c, "tui", err)
	}
	io.WriteString(c.Stdout, b.String())
	return 0
}

// runInspScreens runs the status dashboard and the PR board with one set of
// actions, starting with first; tab switches between them. Both keep their
// dragged column widths in the registry, the mouse toggle (m) carries over
// to the other screen and the board reopens in the view it was left in, and
// in the owner scope the registry keeps for it and the factory screen
// (kvScreenOwner).
// `,` on either opens the settings screen, whose pauses go through the same
// actions.
func runInspScreens(ctx context.Context, c *Context, d statusDeps, so statusOptions, po prsOptions, first string) error {
	acts := newScreenActions(c)
	defer acts.Close()
	settings := &tui.SettingsScreen{Source: newSettingsSource(c, acts), Runtime: screenRuntime{acts}}
	log := tui.NewActionLog() // one for both screens: tab keeps the outcomes and the requests followed
	mouse := d.Config == nil || d.Config.Terminal.Mouse
	toggled := func(on bool) { mouse = on }
	var widths tui.ColumnWidths
	if d.Store != nil {
		widths = kvColumnWidths{st: d.Store}
	}
	dashboard := func(ctx context.Context) error {
		return tuiDashboard(ctx, statusDashSource(d, so), acts, tui.DashboardOptions{
			Refresh: statusRefresh, ShowManual: so.All, Title: "magnum status", Now: inspNow, Judge: rolesJudgeName(d.Config),
			Icons: screenIcons(d.Config), NoMouse: !mouse, MouseToggled: toggled, Widths: widths, Log: log, Settings: settings,
		})
	}
	// One source: its timings cache survives tab. ownHidden counts your
	// open PRs it leaves to the factory screen (own_prs).
	var ownHidden atomic.Int64
	src := ownPRsSource(d.Config, po.source(prsSource(d.Store, d.Config, po.filter(), prsSelfLogins(d.Config), c.Layout)), &ownHidden)
	hide := false // h: kept in the registry, so the board opens the way it was left
	if d.Store != nil {
		if v, ok, err := d.Store.GetKV(ctx, kvBoardHideSkipped); err == nil && ok {
			hide = v == "1"
		}
	}
	board := func(ctx context.Context) error {
		if owner, ok := screenOwner(ctx, d.Store); ok { // the scope the factory screen or the last board left
			po.Owner = owner
		}
		o := prsBoardOptions(d.Config, po)
		o.NoMouse, o.MouseToggled, o.Widths, o.Log, o.Settings = !mouse, toggled, widths, log, settings
		o.Facts = func(ctx context.Context) tui.DaemonFacts { return screenFacts(ctx, d) }
		o.ViewChanged = func(v tui.PRView) { po.View = v }
		o.OwnerChanged = func(owner string) {
			po.Owner = owner // the tab round-trip's, whatever the registry keeps
			keepScreenOwner(ctx, d.Store, owner)
		}
		o.HideSkipped = hide
		if d.Config.OwnPRsOnFactory() {
			o.OwnElsewhere = func() int { return int(ownHidden.Load()) }
		}
		o.HideToggled = func(h bool) {
			hide = h
			if d.Store != nil {
				_ = d.Store.SetKV(context.WithoutCancel(ctx), kvBoardHideSkipped, map[bool]string{true: "1", false: "0"}[h])
			}
		}
		return tuiPRBoard(ctx, src, acts, o)
	}
	return runScreens(ctx, first, dashboard, board)
}

// repoFlag is a screen's --repo, the board's and the factory screen's:
// "owner/name" or "name", trimmed; "" shows every repository.
func repoFlag(raw string) (string, error) {
	repo := strings.TrimSpace(raw)
	if owner, name, ok := strings.Cut(repo, "/"); ok && (owner == "" || name == "" || strings.Contains(name, "/")) {
		return "", fmt.Errorf("--repo %q: want owner/name or name", raw)
	}
	return repo, nil
}

// kvBoardHideSkipped keeps the board's h (hide ignored and skipped PRs).
const kvBoardHideSkipped = "board.hide_skipped"

// kvScreenOwner keeps the owner scope (O) the PR board and the factory
// screen share: each opens in it and writes every change ("" = every owner).
const kvScreenOwner = "screen.owner"

// screenOwner is the owner scope the screens share (kvScreenOwner); ok is
// false without a registry, or when it keeps none or cannot be read.
func screenOwner(ctx context.Context, st *store.Store) (owner string, ok bool) {
	if st == nil {
		return "", false
	}
	v, ok, err := st.GetKV(ctx, kvScreenOwner)
	return strings.TrimSpace(v), ok && err == nil
}

// keepScreenOwner writes owner as the screens' owner scope, best effort: a
// registry that cannot keep it leaves the next screen in the scope it had.
func keepScreenOwner(ctx context.Context, st *store.Store, owner string) {
	if st != nil {
		_ = st.SetKV(context.WithoutCancel(ctx), kvScreenOwner, owner)
	}
}

// prsBoardOptions are the board's options for o.
func prsBoardOptions(cfg *config.Config, o prsOptions) tui.PRBoardOptions {
	return tui.PRBoardOptions{SelfLogins: prsSelfLogins(cfg), DefaultSort: o.Sort, DefaultView: o.View, Repo: o.Repo, Now: inspNow,
		Judge: rolesJudgeName(cfg), Icons: screenIcons(cfg), DefaultRepo: defaultRepo(cfg), DefaultOwner: o.Owner,
		RecentClosed: prsRecentClosed(cfg), NoShimmer: cfg != nil && !cfg.Board.Shimmer, Limit: o.Limit}
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
		stagingTick := daemonLastTick(ctx, st)        // the staging records hold while the daemon ticks
		deploys := stagingDeployValues(ctx, st, cfg)  // every staging deploy's record, in one read
		required := map[string]store.RequiredChecks{} // per repository, for this load
		ignored := map[string][]string{}              // per repository, for this load
		trackers := map[string][]config.Tracker{}     // per repository, for this load
		held := heldMergeDiffs(ctx, st, layout)       // held_merge.go: the merges magnum holds for your y
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
			ignore, ok := ignored[full]
			if !ok && cfg != nil {
				ignore = cfg.IgnoreChecks(full)
				ignored[full] = ignore
			}
			// A check ignore_checks names is absent, as the approval's gates
			// read the checks; GitHub's block of the merge reads the review
			// gate of the same Details read.
			merge := prview.Merge{Draft: r.Draft}
			if r.ReviewGate != nil {
				merge.Decision = r.ReviewGate.Decision
			}
			row.CI = prview.CI(store.SeenCI(r.CI, r.HeadSHA, ignore), r.HeadSHA, req, merge)
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
						now := inspNow()
						row.Wait, row.WaitDetail = w.Short(now), w.Sentence(actRefLabel(defRepo, full, r.Number), now)
						if narrow := w.Narrow(now); narrow != row.Wait {
							row.WaitNarrow = narrow
						}
						row.PinWait = w.Reason == engine.WaitPinned
						if row.PinWait {
							row.PinnedBy = w.Subject
						}
						row.DeltaCheck = w.DeltaCheck
					}
				}
			}
			if r.LastError != "" {
				row.AttentionSum = attentionSum(r.LastError) // the attention the board's d says you saw (fix.go)
				pr := store.PR{ID: r.PRID, State: r.State, LastError: &r.LastError}
				extra := strings.TrimPrefix(row.LastError, r.LastError) // "; compare since the last review: …"
				if why, ok := prAttention(ctx, st, pr, actRefLabel(defaultRepo(cfg), full, r.Number)); ok {
					row.LastError, row.ErrorFix, row.ErrorDetail = why.Summary+extra, why.Fix, why.Tail
					row.AttentionKind = why.Kind // the pill's cause: the recorded kind, never the sentence
				} else {
					row.LastError = errorSummary(r.LastError) + extra
				}
			}
			if v, ok, err := st.GetKV(ctx, engine.KVPRSnooze(r.PRID)); err == nil && ok {
				if s, ok := engine.ParseSnooze(v); ok && s.Active(inspNow()) {
					row.SnoozedUntil, row.SnoozedAt, row.SnoozedBy = s.Until, s.At, s.By
				}
			}
			row.Fixing, row.FixLoop, row.FixOff = boardFix(ctx, st, r.PRID)
			row.Fixer = fixerLogin(cfg, full) // f's question names it
			row.HeldMerge = held[r.PRID]      // e opens it, P pushes it
			row.Whiteboard = boardWhiteboard(ctx, st, r.PRID)
			if f, ok := codexFlagOf(ctx, st, r.PRID); ok { // codex_flag.go
				row.CodexFlag, row.CodexFlagSentence = f.Short(), f.Sentence(actRefLabel(defaultRepo(cfg), full, r.Number))
			}
			if r.Muted {
				row.MuteReason = prsMuteReason(ctx, st, full, r.Number)
			}
			if v, ok, err := st.GetKV(ctx, engine.KVPRTrivial(r.PRID)); err == nil && ok {
				if t, ok := engine.ParseTrivialSkip(v); ok {
					row.Note = t.Note()
				}
			}
			if r.GHState != store.GHMerged && r.GHState != store.GHClosed { // nothing to decide on a PR that is over
				if v, ok, err := st.GetKV(ctx, engine.KVPRStalemate(r.PRID)); err == nil && ok {
					row.Stalemate = prsStalemate(v)
				}
			}
			row.ProjectNote = agents.ProjectSentences(ctx, st, r.PRID, r.HeadSHA)
			row.Stagings, row.DeployFirst = boardStagings(ctx, st, cfg, r, stagingTick, inspNow())
			if row.Stagings != nil {
				row.StagingDeploys = stagingDeploysOf(deploys, r.Owner+"/"+r.Name, r.Number)
				if deployingHead(row.StagingDeploys, r.HeadSHA) {
					row.DeployFirst = "" // the deploy E would start already runs
				}
			}
			out = append(out, row)
		}
		// Every star in one read (star.go), before the board filters and
		// sorts; a registry that cannot say leaves them out rather than the
		// board.
		if stars, err := prStars(ctx, st); err == nil {
			for i := range out {
				if s, ok := stars[ids[i]]; ok {
					out[i].Starred, out[i].StarredAt = true, s.At
				}
			}
		}
		prsOpenFacts(ctx, st, cfg, rows, out)
		prsOpenVerdicts(ctx, st, cfg, rows, out, textx.MatchLogins(self))
		sums, err := st.LastReviewSummaries(ctx, ids)
		if err == nil {
			for i := range out {
				if s, ok := sums[ids[i]]; ok {
					out[i].Findings = prview.Findings(s, prview.Held(ctx, st, ids[i], s))
				}
			}
		}
		mine := textx.MatchLogins(self)
		refused, _ := st.NoApprovalReviews(ctx) // a registry that cannot say counts no gate's comment
		for i, r := range rows {
			if r.ReviewGate != nil {
				out[i].ReviewDecision = r.ReviewGate.Decision
			}
			nf := r.NeedsMeFacts()
			nf.CommentWhenClean = cfg.CommentsWhenClean(r.Owner+"/"+r.Name, r.Identity, r.Author)
			nf.Verdict, nf.Optional = sums[r.PRID].Verdict, sums[r.PRID].OnlyOptional()
			nf.Refused = refused[r.PRID].For(sums[r.PRID].ReviewID) // magnum's latest posted round's review
			if out[i].NeedsMe = store.NeedsMe(nf, mine); out[i].NeedsMe != "" {
				out[i].NeedsMeReason = store.NeedsMeWords(nf.Refused)
			}
		}
		_ = prsAutoApprovals(ctx, st, ids, out) // a registry that cannot say leaves them out rather than the board

		// With magnum's rounds read: whom A approves the rows that need you as (verdict.go).
		if sums != nil {
			_ = prsApproveAs(ctx, st, cfg, rows, sums, out)
		}
		if err := timings.fill(ctx, st, cfg, ids, out, inspNow()); err != nil {
			return nil, err
		}
		// The card's spend and round roles, then the rounds in flight (which
		// read the round roles): a registry that cannot say leaves them out
		// rather than the board.
		_ = boardRoundFacts(ctx, st, ids, out, inspNow())
		_ = boardRoundProgress(ctx, st, cfg, ids, out)
		return out, nil
	}
}

// prsOpenFacts fills what the board says of `magnum open` on each row: when
// the hold an open set on a pinned PR ends (HeldUntil; every hold in one
// read), whether o restores the PR, so it asks first (OpenRestores: the role
// open opens has no live pane and one of its sessions is parked, or, when its
// own session is gone, another role's is and stands in for it,
// engine.OpenWouldRestore), and for how long a restore holds it (OpenHold, [daemon]
// open_hold). A registry that cannot say leaves them out rather than the
// board.
func prsOpenFacts(ctx context.Context, st *store.Store, cfg *config.Config, rows []store.BoardRow, out []tui.PRBoardRow) {
	var d config.Daemon
	if cfg != nil {
		d = cfg.Daemon
	}
	holds, _ := engine.OpenHolds(ctx, st)
	for i, r := range rows {
		out[i].OpenHold = d.OpenHold.Duration
		if h, ok := holds[r.PRID]; ok && r.Pinned {
			out[i].HeldUntil = openHoldEnd(ctx, st, d, h, r.PRID)
		}
		if ss, err := st.SessionsByPR(ctx, r.PRID); err == nil {
			full := r.Owner + "/" + r.Name
			out[i].OpenRestores = engine.OpenWouldRestore(ss, openRoleOf(ctx, st, cfg, r.PRID, full, ss), engine.OpenRestorable(cfg, full))
		}
	}
}

// prsOpenVerdicts marks your own open PRs (mine: their author is one of
// your logins) with autofix = "open"'s verdict (engine.KVPROpen), stale
// (OpenVerdict.Stale) when magnum's last tick is four poll intervals old,
// the PR's head moved past it or its row changed after it, and Merge when
// it is idle with the merge yours (engine.OpenMergeIsYours over the PR's
// review gate, which the verdict read: a later row is stale), where the
// board puts a staging deploy first (tui.PRBoardRow.DeployNeedsYou). A
// registry that cannot say leaves them unmarked rather than the board.
func prsOpenVerdicts(ctx context.Context, st *store.Store, cfg *config.Config, rows []store.BoardRow, out []tui.PRBoardRow, mine func(string) bool) {
	var lastTick time.Time
	if s, ok, err := st.GetKV(ctx, store.KVDaemonLastTick); err == nil && ok {
		lastTick, _ = store.ParseTime(s)
	}
	var poll time.Duration
	if cfg != nil {
		poll = cfg.Daemon.PollInterval.Duration
	}
	now := inspNow()
	for i, r := range rows {
		if r.GHState != store.GHOpen || !mine(r.Author) {
			continue
		}
		s, ok, err := st.GetKV(ctx, engine.KVPROpen(r.PRID))
		if err != nil || !ok {
			continue
		}
		v, ok := engine.ParseOpenVerdict(s)
		if !ok {
			continue
		}
		pr, err := st.PRByID(ctx, r.PRID)
		if err != nil {
			continue
		}
		out[i].Open = &tui.PROpen{Verdict: v.Verdict, Why: v.Why, Head: v.Head,
			Stale: v.Stale(pr.HeadSHA, pr.UpdatedAt, lastTick, now, poll),
			Merge: v.Verdict == engine.OpenIdle && engine.OpenMergeIsYours(pr.ReviewGate)}
	}
}

// daemonLastTick is the daemon's last tick (store.KVDaemonLastTick); zero
// when it never ticked or the registry cannot say.
func daemonLastTick(ctx context.Context, st *store.Store) time.Time {
	v, ok, err := st.GetKV(ctx, store.KVDaemonLastTick)
	if err != nil || !ok {
		return time.Time{}
	}
	t, _ := store.ParseTime(v)
	return t
}

// boardStagings is where the open PR r stands on its repository's staging
// envs, in the config's order, from the daemon's record (engine.KVPRStaging,
// engine.StagingShown: unknown while there is none, of an older head, or
// the daemon's last tick lastTick is stale at now), and staging's words
// for a deploy before the merge (engine.StagingFirst over the head's file
// list, pr_files: no read of GitHub or the mirror), "" when none is
// needed or the PR can never deploy to a staging (engine.StagingNeverDeploys:
// a fork's, read from its row only when the words would show); nil and ""
// for a PR that is not open and for a repository without staging envs.
func boardStagings(ctx context.Context, st *store.Store, cfg *config.Config, r store.BoardRow, lastTick, now time.Time) ([]tui.StagingInfo, string) {
	if r.GHState != store.GHOpen {
		return nil, ""
	}
	envs := engine.StagingEnvs(cfg, r.Owner+"/"+r.Name)
	if len(envs) == 0 {
		return nil, ""
	}
	rec, ok := readStagingRecord(ctx, st, r.PRID)
	shown := engine.StagingShown(rec, ok, envs, r.HeadSHA, lastTick, now)
	out := make([]tui.StagingInfo, len(shown))
	for i, s := range shown {
		out[i] = tui.StagingInfo{Env: s.Env, State: s.State, Detail: s.Detail, InTip: s.InTip, Behind: s.Behind,
			PushedAt: s.PushedAt, DeployedAt: s.DeployedAt, RunURL: s.RunURL, Why: s.Why}
	}
	first := ""
	if files, listed, err := st.PRFilesOf(ctx, r.PRID); err == nil { // a registry that cannot say says nothing of it
		if ms := engine.StagingFirst(files, listed, r.HeadSHA, shown); ms != nil {
			if pr, err := st.PRByID(ctx, r.PRID); err == nil && !engine.StagingNeverDeploys(pr) {
				first = staging.DeployBeforeMergeWords(ms)
			}
		}
	}
	return out, first
}

// stagingDeployValues is every staging deploy's record
// (engine.KVStagingDeploys) and every resolver job's row
// (engine.KVStagingResolves), whose keys differ, in two reads; none without
// a repository with staging envs, or when the registry cannot say (a
// failed read of the jobs leaves them out).
func stagingDeployValues(ctx context.Context, st *store.Store, cfg *config.Config) map[string]string {
	if len(engine.StagingRepos(cfg)) == 0 {
		return nil
	}
	values, err := st.KVValues(ctx, engine.KVStagingDeploys)
	if err != nil {
		return nil
	}
	if jobs, err := st.KVValues(ctx, engine.KVStagingResolves); err == nil {
		maps.Copy(values, jobs)
	}
	return values
}

// stagingDeploysOf is repo#number's staging deploys among values
// (stagingDeployValues), the live ones first, as the screens show them,
// each live one with what the resolver does for its conflict.
func stagingDeploysOf(values map[string]string, repo string, number int) []tui.StagingDeploy {
	var out []tui.StagingDeploy
	for _, r := range engine.StagingDeploysOf(values, repo, number) {
		d := tui.StagingDeploy{Effect: r.Effect, Env: r.Env, Head: r.Head, Step: r.Step, Phase: r.Phase, State: r.State, Why: r.Why,
			Text: r.Text, RunURL: r.RunURL, At: r.At, Live: r.Live(), Unconfirmed: r.Unconfirmed()}
		if d.Live {
			d.Resolver = engine.StagingResolverSays(values, r.Effect)
		}
		out = append(out, d)
	}
	return out
}

// deployingHead reports whether one of ds, a PR's staging deploys
// (stagingDeploysOf), deploys head and still runs: queued behind another
// effect on its env, or running (the conflict resolver's work included).
// The deploy before the merge is then on its way and E is refused ("already
// deploying") until the push lands and a staging read says pushed, so the
// board leaves the line out meanwhile (boardStagings' words: no needs-you,
// no --needs-me, no deploy_before_merge). A deploy that needs you is not
// live, it stopped for the operator, and keeps the line: E resumes it. Both
// heads are full shas (staging's checkPR, the PR's row).
func deployingHead(ds []tui.StagingDeploy, head string) bool {
	return head != "" && slices.ContainsFunc(ds, func(d tui.StagingDeploy) bool { return d.Live && d.Head == head })
}

// readStagingRecord is the PR's staging record (engine.KVPRStaging); false
// when it has none or the registry cannot say.
func readStagingRecord(ctx context.Context, st *store.Store, prID int64) (engine.StagingRecord, bool) {
	v, ok, err := st.GetKV(ctx, engine.KVPRStaging(prID))
	if err != nil || !ok {
		return engine.StagingRecord{}, false
	}
	return engine.ParseStagingRecord(v)
}

// boardFix is the PR's fix loop as the board shows it: fixing is its state
// cell while the fixer works ("fixing 1/3 · claude-fix", engine.FixState.Short;
// "" otherwise), loop whether it has a loop in either phase (its
// engine.KVPRFix row exists, so K aborts it), and off whether it was switched
// out of autofix (its engine.KVPRFixOff row exists). A registry that cannot
// say shows none of them.
func boardFix(ctx context.Context, st *store.Store, prID int64) (fixing string, loop, off bool) {
	if v, ok, err := st.GetKV(ctx, engine.KVPRFix(prID)); err == nil && ok {
		loop = true
		if f, ok := engine.ParseFixState(v); ok && f.Phase == engine.FixPhaseFixing {
			fixing = f.Short()
		}
	}
	_, off, err := st.GetKV(ctx, engine.KVPRFixOff(prID))
	return fixing, loop, off && err == nil
}

// boardWhiteboard is the PR's whiteboard as the board shows it (its
// engine.KVPRWhiteboard row: the Whiteboard session w opens and the head it
// describes); nil when it has none, or the registry cannot say.
func boardWhiteboard(ctx context.Context, st *store.Store, prID int64) *tui.PRWhiteboard {
	v, ok, err := st.GetKV(ctx, engine.KVPRWhiteboard(prID))
	if err != nil || !ok {
		return nil
	}
	rec, ok := engine.ParseWhiteboardRecord(v)
	if !ok {
		return nil
	}
	return &tui.PRWhiteboard{Session: rec.Session, Head: rec.Head}
}

// prsMuteReason is the reason of the PR's latest mute (`magnum mute`'s, in
// its pr.muted event); "" when it gave none or the registry cannot say.
func prsMuteReason(ctx context.Context, st *store.Store, full string, number int) string {
	evs, err := st.EventsBySubject(ctx, fmt.Sprintf("pr:%s#%d", full, number), 0)
	if err != nil {
		return ""
	}
	for _, ev := range slices.Backward(evs) {
		if ev.Kind != engine.EvPRMuted {
			continue
		}
		var data struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(ev.Data, &data) != nil {
			return ""
		}
		return data.Reason
	}
	return ""
}

// prsStalemate is the threads of a KVPRStalemate value as the board names
// them: each one's URL, else its id; nil when the value names none.
func prsStalemate(v string) []string {
	st, ok := engine.ParseStalemate(v)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(st.Threads))
	for _, t := range st.Threads {
		if name := cmp.Or(t.URL, t.ID); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// prsRecentClosed is [board] recent_closed (0 without a config: the section
// is off).
func prsRecentClosed(cfg *config.Config) time.Duration {
	if cfg == nil {
		return 0
	}
	return max(cfg.Board.RecentClosed.Duration, 0)
}

// prsSelfLogins are the logins that count as "me" on the board
// (config.SelfLogins): every watch's posting identity and every gh identity
// (the user).
func prsSelfLogins(cfg *config.Config) []string { return cfg.SelfLogins() }

// prsAccountKey folds a login for telling accounts apart: case and "@"
// dropped, "[bot]" kept (the App "zhuravel[bot]" is not the user "zhuravel").
func prsAccountKey(s string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "@")
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
		if k := textx.FoldLogin(l); k != "" {
			mine[k] = true
		}
	}
	r := tui.PRBoardRow{
		Ref: b.Ref, Owner: b.Owner, Repo: b.Name, Number: b.Number,
		Title: b.Title, Author: b.Author, URL: b.URL, Draft: b.Draft,
		Labels: b.Labels, Assignees: b.Assignees,
		State: prsRowState(b), GHState: b.GHState, ActivityAt: b.ActivityAt, GitHubUpdatedAt: b.UpdatedAt, HeadSHA: b.HeadSHA,
		Slot: b.Slot, SlotHold: b.SlotHold, Pinned: b.Pinned, Muted: b.Muted, NextEligibleAt: b.NextEligibleAt,
		LastError: b.LastError, RoundsToday: b.RoundsToday, SkipReason: b.SkipReason,
		ClosedAt: cmp.Or(b.MergedAt, b.ClosedAt), MergedUnreviewed: b.MergedUnreviewed, FlagDismissed: b.FlagDismissed,
		Replies: b.PendingReplies, ReleaseAfter: b.ReleaseAfter,
	}
	if r.Ref == "" && b.Owner != "" && b.Name != "" && b.Number > 0 {
		r.Ref = fmt.Sprintf("%s/%s#%d", b.Owner, b.Name, b.Number)
	}
	r.RequestedToMe, r.LastRequest, r.Requests = prsRequests(b.ReviewRequests, mine)
	if b.ReviewedSHA != "" || b.LastReviewEvent != "" || !b.LastReviewAt.IsZero() {
		r.LastReview = &tui.ReviewInfo{
			Login: b.LastReviewLogin, Event: b.LastReviewEvent, SubmittedAt: b.LastReviewAt, CommitSHA: b.ReviewedSHA,
			Stale: b.HeadSHA != "" && b.ReviewedSHA != b.HeadSHA,
			Mine:  b.LastReviewLogin != "" && mine[textx.FoldLogin(b.LastReviewLogin)],
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
			Mine:  mine[textx.FoldLogin(login)],
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
			Login: login, Verdict: "pending", Requested: true, Mine: mine[textx.FoldLogin(login)],
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
			d.MergedBase, d.RawBase = sinceBases(*s)
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

// sinceBases are the base branch a since-review size left out (the PR's
// own counts across a merge of it) and the one it includes (raw).
func sinceBases(s store.SinceReview) (merged, raw string) {
	base := cmp.Or(s.BaseRef, "the base branch")
	switch {
	case s.BaseMerged:
		return base, ""
	case s.Raw:
		return "", base
	}
	return "", ""
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
		info := tui.RequestInfo{To: q.To, By: q.By, At: q.At, Mine: mine[textx.FoldLogin(q.To)]}
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
			textx.Clip(actClean(r.Title), 48),
			inspOrDash(actClean(r.Author)),
			inspOrDash(actClean(strings.Join(r.Assignees, ","))),
			actAgo(now, r.ActivityAt),
			prsRequestedCell(r, now),
			prsStateCell(r, now),
			prsLastReviewCellOf(r, now),
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
// ("reviewed,starred,pinned,draft"; "closed,merged,unreviewed" for a PR GitHub
// merged before magnum reviewed its last push; "reviewed,deploy-first" for
// your own PR whose merge waits for its staging deploy; "reviewed,fixing" while its
// fixer works, "fix-off" when it was switched out of autofix, "held" when
// its latest review held its approval for a full pass), magnum open's hold
// in place of the pin ("reviewed,open-hold→03:21") and a snooze that holds
// at now while no round runs ("reviewed,snoozed→18:00", as the board's
// cell).
func prsStateCell(r tui.PRBoardRow, now time.Time) string {
	s := inspOrDash(r.State)
	if g := strings.ToUpper(r.GHState); g == "MERGED" || g == "CLOSED" {
		s += "," + strings.ToLower(g)
	}
	if r.MergedUnreviewed {
		s += ",unreviewed"
	}
	switch r.NeedsMe {
	case tui.NeedsMeApprove:
		s += ",needs-you"
	case tui.NeedsMeLift:
		s += ",lift-yours"
	}
	if r.DeployNeedsYou() {
		s += ",deploy-first"
	}
	if r.AutoApproved != nil {
		s += ",auto-approved"
	}
	for _, f := range []struct {
		on   bool
		name string
	}{{r.Fixing != "", "fixing"}, {r.Draft, "draft"}, {r.Starred, "starred"}, {r.Pinned && r.HeldUntil.IsZero(), "pinned"}, {r.Muted, "muted"}, {r.FixOff, "fix-off"},
		{r.CodexFlag != "", "codex-flagged"}, {r.LastError != "", "error"}, {len(r.Stalemate) > 0, "stalemate"},
		{r.Findings != nil && r.Findings.Held, "held"}} {
		if f.on {
			s += "," + f.name
		}
	}
	if r.Pinned && !r.HeldUntil.IsZero() {
		s += ",open-hold→" + inspClock(now, r.HeldUntil)
	}
	if r.SnoozedUntil.After(now) && !slices.Contains(prInFlight, r.State) {
		s += ",snoozed→" + inspClock(now, r.SnoozedUntil)
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
// "blocking P1:1 P2:3 simplify:4", "clean", "blocking 3 open" (none new,
// earlier ones still open, as the board says), the earlier ones still open
// after the new ones ("non-blocking P3:3 +P2:1", or "+3 open" when the result
// gives no priorities), or "-" when it has not reviewed the PR.
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
	switch {
	case len(parts) == 1 && f.Open > 0:
		parts = append(parts, fmt.Sprintf("%d open", f.Open))
	case f.Open > 0:
		n := len(parts)
		if f.OpenCounts != nil {
			for i, c := range f.OpenCounts {
				if c > 0 {
					parts = append(parts, fmt.Sprintf("+P%d:%d", i, c))
				}
			}
		}
		if len(parts) == n {
			parts = append(parts, fmt.Sprintf("+%d open", f.Open))
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

// prsLastReviewCellOf is the table's LAST REVIEW cell of r: its last
// review, then the replies waiting for the judge the board marks "↩2"
// ("approved 3h by talkable, 2 replies"; "2 replies" when it shows no review).
func prsLastReviewCellOf(r tui.PRBoardRow, now time.Time) string {
	s := prsLastReviewCell(r.LastReview, now)
	if r.Replies <= 0 {
		return s
	}
	n := textx.Count(r.Replies, "reply", "replies")
	if r.LastReview == nil {
		return n
	}
	return s + ", " + n
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
	switch {
	case d.MergedBase != "":
		s += " own"
	case d.RawBase != "":
		s += " raw"
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
		tui.ViewMagnum: "what magnum reviewed or is reviewing, or your review is requested on",
		tui.ViewMine:   "assigned to you or your review requested",
		tui.ViewReady:  "approved, no changes requested, not a draft, no staging deploy first",
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

// prsAutoApprovals fills the rows' automatic approvals (ids are their PRs):
// the one standing (or that stood when its PR was merged or closed), why
// magnum stopped approving a PR as the operator, and why it does not
// approve the head as them although the review left nothing to fix
// (engine.KVPRAutoApproveRefused, for the row's head). A PR approved as the
// operator no longer needs them.
func prsAutoApprovals(ctx context.Context, st *store.Store, ids []int64, out []tui.PRBoardRow) error {
	latest, err := st.LatestAutoApprovals(ctx, ids)
	if err != nil {
		return err
	}
	holds, err := st.AutoApproveHolds(ctx, ids)
	if err != nil {
		return err
	}
	for i, id := range ids {
		if a, ok := latest[id]; ok && (a.State == store.AutoStanding || a.State == store.AutoDismissed && a.EndedBy == store.AutoEndedClosed) {
			out[i].AutoApproved = &tui.AutoApproval{ReviewID: a.ReviewID, Head: a.HeadSHA, URL: a.ReviewURL, At: store.Deref(a.PostedAt)}
			out[i].NeedsMe = ""
		}
		if h, ok := holds[id]; ok && h.Held {
			out[i].AutoStopped = h.Reason
		}
		if v, ok, err := st.GetKV(ctx, engine.KVPRAutoApproveRefused(id)); err == nil && ok {
			if r, ok := engine.ParseAutoApproveRefused(v); ok && r.Head == out[i].HeadSHA && out[i].AutoApproved == nil {
				out[i].AutoRefused = r.Reason
			}
		}
	}
	return nil
}
