package cli

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/tui"
)

const pickUsage = "pick [--query <url|ref|text>] [--limit N]"

// pickKeys maps the numbered prompt's action letters to the picker's actions.
var pickKeys = map[string]tui.PickAction{
	"r": tui.PickActionReview, "f": tui.PickActionFresh, "o": tui.PickActionOpen,
	"b": tui.PickActionBrowser, "p": tui.PickActionTogglePin, "x": tui.PickActionRelease,
}

type pickOpts struct {
	query string
	limit int
}

// pickEntry is one PR the picker lists.
type pickEntry struct {
	Label  string
	Repo   string // owner/name
	Number int
	URL    string
	State  string
	Title  string
	Author string
	Age    string
	Pinned bool
	Known  bool             // in the registry
	Review *tui.ReviewFacts // for the screen's y/N question; nil when unknown
	// GHState is GitHub's state of the PR, OPEN, CLOSED or MERGED; "" when
	// the registry does not know the PR.
	GHState string
}

// line is what the numbered prompt's filter matches.
func (e pickEntry) line() string {
	return fmt.Sprintf("%s | %s | %s | %s | %s", e.Label, e.State, e.Title, e.Author, e.Age)
}

func (e pickEntry) ref() string { return fmt.Sprintf("%s#%d", e.Repo, e.Number) }

// screenEntry is the entry as the picker screen lists it.
func (e pickEntry) screenEntry() tui.PickEntry {
	return tui.PickEntry{Ref: e.ref(), Title: e.Title, Author: e.Author, State: e.State, Age: e.Age, URL: e.URL,
		Pinned: e.Pinned, Review: e.Review, GHState: e.GHState}
}

func newPickCmd(c *Context) *cobra.Command {
	var o pickOpts
	cmd := newCommand(groupAct, pickUsage, "pick a PR to review, open, pin or release",
		"Pick a PR from a filterable list, then review it (enter; a reviewed head is reviewed again), review fresh "+
			"(ctrl+f), open its pane (ctrl+g), open it in the browser (ctrl+o), pin or unpin (ctrl+p), or release it "+
			"(ctrl+x); ctrl+r refreshes the list and esc cancels. A key that cannot act on the PR (a review while "+
			"its round runs, a release of a pinned PR) says why instead. Typing a PR URL or reference magnum does "+
			"not list reviews that PR. When stdin "+
			"or stdout is not a terminal a numbered prompt does the same. --query (or MAGNUM_PICK_QUERY) sets the "+
			"initial filter; a PR URL or reference preselects that PR, or offers it when magnum does not know it yet.",
		func(pos []string) int { return runPick(c, o, pos) })
	fs := cmd.Flags()
	fs.StringVar(&o.query, "query", "", "initial filter; a PR URL or reference preselects that PR (or offers it when magnum does not know it)")
	fs.IntVar(&o.limit, "limit", 300, "most PRs to list")
	return cmd
}

func runPick(c *Context, o pickOpts, pos []string) int {
	if len(pos) > 0 {
		o.query = strings.TrimSpace(o.query + " " + strings.Join(pos, " "))
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "pick", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return pickMain(ctx, c, d, o)
}

func pickMain(ctx context.Context, c *Context, d *actDeps, o pickOpts) int {
	entries, err := pickEntries(ctx, d, o.limit)
	if err != nil {
		return cmdFail(c, "pick", err)
	}
	var typed []pickEntry // the PR a typed query names when the list lacks it
	query := strings.TrimSpace(o.query)
	if query == "" {
		query = strings.TrimSpace(d.getenv("MAGNUM_PICK_QUERY")) // set by the plugin's link handler
	}
	if query != "" {
		// A URL or reference: preselect it, or offer it when unknown.
		if full, n, err := resolveRefRepo(ctx, d.Store, d.refs(), query); err == nil {
			label := d.actLabel(full, n)
			found := false
			for _, e := range entries {
				if strings.EqualFold(e.ref(), fmt.Sprintf("%s#%d", full, n)) {
					found, label = true, e.Label
				}
			}
			if !found {
				// entries lists open PRs only: offer the registry's row for a PR it
				// knows (a merged one), the stub for one it does not.
				e, ok := pickRefEntry(ctx, d, query)
				if !ok || !e.Known {
					e = pickEntry{Label: label, Repo: full, Number: n, State: "new",
						URL:   fmt.Sprintf("https://github.com/%s/pull/%d", full, n),
						Title: "(not in magnum yet: enter reviews it)"}
				}
				typed = []pickEntry{e}
				entries = append([]pickEntry{e}, entries...)
			}
			query = label
		}
	}
	if d.screen() {
		// ctrl+r lists the PRs again, keeping the typed PR the list lacks.
		reload := func(ctx context.Context) ([]pickEntry, error) {
			again, err := pickEntries(ctx, d, o.limit)
			return append(slices.Clone(typed), again...), err
		}
		return pickScreen(ctx, c, d, entries, query, reload)
	}
	return pickPrompt(ctx, c, d, entries, query)
}

// pickEntries lists the open PRs magnum knows, most recent activity first.
func pickEntries(ctx context.Context, d *actDeps, limit int) ([]pickEntry, error) {
	prs, err := d.Store.ListPRs(ctx, store.PRFilter{})
	if err != nil {
		return nil, err
	}
	repos, err := d.Store.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[int64]store.Repo{}
	for _, r := range repos {
		byID[r.ID] = r
	}
	type row struct {
		e  pickEntry
		at time.Time
	}
	var rows []row
	now := d.now()
	needs := map[int64]string{}
	if list, err := d.Store.NeedsMePRs(ctx, d.Cfg.CommentsWhenClean, d.Cfg.SelfMatch()); err == nil {
		for _, n := range list {
			needs[n.PR.ID] = n.NeedsMe
		}
	}
	if list, err := d.Store.StandingAutoApprovals(ctx); err == nil {
		for _, a := range list {
			needs[a.PRID] = "auto" // approved as the operator: they are not needed
		}
	}
	for _, pr := range prs {
		repo, ok := byID[pr.RepoID]
		if !ok || pr.GHState != store.GHOpen {
			continue
		}
		at := pr.UpdatedAt
		if a := pr.Activity(); a != nil {
			at = *a
		}
		state := pr.State
		switch needs[pr.ID] {
		case store.NeedsMeApprove:
			state += ",✔ needs you"
		case store.NeedsMeLift:
			state += ",✔ lift your ✗"
		case "auto":
			state += ",✔ auto"
		}
		if pr.Pinned {
			state += ",pinned"
		}
		if pr.Muted {
			state += ",muted"
		}
		author := actClean(store.Deref(pr.AuthorLogin))
		if author != "" {
			author = "@" + author
		}
		rows = append(rows, row{at: at, e: pickEntry{
			Label: d.actLabel(repo.FullName(), pr.Number), Repo: repo.FullName(), Number: pr.Number, URL: pr.URL,
			State: state, Title: textx.Clip(actClean(store.Deref(pr.Title)), 90),
			Author: author, Age: actAgo(now, at), Pinned: pr.Pinned, Known: true, Review: reviewFactsOf(pr), GHState: pr.GHState,
		}})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].at.After(rows[j].at) })
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]pickEntry, len(rows))
	for i, r := range rows {
		out[i] = r.e
	}
	return out, nil
}

// pickScreen runs the picker, then acts on the choice once the screen has
// closed, so the action prints to the terminal as usual (and the herdr popup
// keeps its result up). reload lists the PRs again for ctrl+r.
func pickScreen(ctx context.Context, c *Context, d *actDeps, entries []pickEntry, query string, reload func(context.Context) ([]pickEntry, error)) int {
	screen := func(entries []pickEntry) []tui.PickEntry {
		list := make([]tui.PickEntry, len(entries))
		for i, e := range entries {
			list[i] = e.screenEntry()
		}
		return list
	}
	// A reference typed in the filter that the list lacks is looked up in the
	// registry, so the y/N question knows a merged PR for what it is.
	lookup := func(q string) (tui.PickEntry, bool) {
		if e, ok := pickRefEntry(ctx, d, q); ok && e.Known {
			return e.screenEntry(), true
		}
		return tui.PickEntry{}, false
	}
	opts := tui.PickerOptions{Query: query, Now: d.now, Lookup: lookup}
	if reload != nil {
		opts.Reload = func(ctx context.Context) ([]tui.PickEntry, error) {
			again, err := reload(ctx)
			return screen(again), err
		}
	}
	out, err := tuiPicker(ctx, screen(entries), opts)
	if err != nil {
		return cmdFail(c, "pick", err)
	}
	if out.Action == tui.PickActionCancel {
		return 0
	}
	if out.Entry != nil {
		for _, e := range entries {
			if e.ref() == out.Entry.Ref {
				return pickAct(ctx, c, d, e, out.Action)
			}
		}
		// Listed by a refresh (ctrl+r): read it again from the registry.
		if e, ok := pickRefEntry(ctx, d, out.Entry.Ref); ok {
			return pickAct(ctx, c, d, e, out.Action)
		}
	}
	// Nothing listed matched: the typed URL or reference is acted on directly.
	if e, ok := pickRefEntry(ctx, d, out.Query); ok {
		return pickAct(ctx, c, d, e, out.Action)
	}
	fmt.Fprintf(c.Stderr, "no PR matches %q (type a URL, owner/repo#N, repo#N or N to review one magnum does not list)\n", out.Query)
	return 1
}

// pickPrompt is the picker off a terminal: a numbered list and one line of input.
func pickPrompt(ctx context.Context, c *Context, d *actDeps, entries []pickEntry, query string) int {
	shown := entries
	if query != "" {
		shown = nil
		lq := strings.ToLower(query)
		for _, e := range entries {
			if strings.Contains(strings.ToLower(e.line()), lq) {
				shown = append(shown, e)
			}
		}
	}
	if query != "" {
		// A reference that matches nothing magnum lists: review it.
		if len(shown) == 1 && !shown[0].Known {
			return pickAct(ctx, c, d, shown[0], tui.PickActionReview)
		}
		if len(shown) == 0 {
			if e, ok := pickRefEntry(ctx, d, query); ok {
				return pickAct(ctx, c, d, e, tui.PickActionReview)
			}
			fmt.Fprintf(c.Stderr, "no PR matches %q\n", query)
			return 1
		}
	}
	if len(shown) == 0 {
		fmt.Fprintln(c.Stdout, "magnum lists no open PRs yet (the daemon records them on its next poll); type a PR URL or reference to review one")
	}
	const most = 40
	if len(shown) > most {
		shown = shown[:most]
	}
	rows := make([][]string, len(shown))
	for i, e := range shown {
		rows[i] = []string{strconv.Itoa(i+1) + ")", e.Label, e.State, e.Title, e.Author, e.Age}
	}
	actTable(c.Stdout, nil, rows)
	fmt.Fprint(c.Stdout, "row number or PR (#N, repo#N, URL), then an optional action (r review [default], f fresh, o open, b browser, p pin/unpin, x release); blank quits: ")
	line, err := d.readLine(ctx)
	if err != nil || line == "" {
		fmt.Fprintln(c.Stdout)
		return 0
	}
	fields := strings.Fields(line)
	act := "r"
	if len(fields) > 1 {
		act = strings.ToLower(fields[1])
	}
	action, ok := pickKeys[act]
	if !ok {
		fmt.Fprintf(c.Stderr, "unknown action %q (r, f, o, b, p or x)\n", act)
		return 2
	}
	e, err := pickChoice(ctx, d, shown, fields[0])
	if err != nil {
		fmt.Fprintln(c.Stderr, err)
		return 2
	}
	return pickAct(ctx, c, d, e, action)
}

// pickChoice reads the numbered prompt's answer: a row number ("12" or
// "12)"), or a PR as #N, repo#N, owner/repo#N or a URL. A bare number that
// is a row and also the number of a PR listed on another row is refused as
// ambiguous rather than guessed.
func pickChoice(ctx context.Context, d *actDeps, shown []pickEntry, s string) (pickEntry, error) {
	row, explicitRow := strings.CutSuffix(s, ")")
	if n, err := strconv.Atoi(row); err == nil && n >= 1 && n <= len(shown) {
		if !explicitRow {
			for i, e := range shown {
				if e.Number == n && i != n-1 {
					return pickEntry{}, fmt.Errorf("%q is ambiguous: row %d is %s, and %s is PR #%d; type %d) for the row or #%d for the PR",
						s, n, shown[n-1].Label, e.Label, n, n, n)
				}
			}
		}
		return shown[n-1], nil
	}
	if e, ok := pickRefEntry(ctx, d, strings.TrimPrefix(s, "#")); ok {
		return e, nil
	}
	return pickEntry{}, fmt.Errorf("%q is neither a row number from the list nor a PR reference", s)
}

// pickRefEntry turns a typed URL or reference into an entry; a PR the
// registry knows (an open or a merged one) brings its state, title and GitHub
// state along.
func pickRefEntry(ctx context.Context, d *actDeps, s string) (pickEntry, bool) {
	if strings.TrimSpace(s) == "" {
		return pickEntry{}, false
	}
	full, n, err := resolveRefRepo(ctx, d.Store, d.refs(), s)
	if err != nil {
		return pickEntry{}, false
	}
	e := pickEntry{Label: d.actLabel(full, n), Repo: full, Number: n, State: "new", URL: fmt.Sprintf("https://github.com/%s/pull/%d", full, n)}
	if t, err := d.resolveRef(ctx, e.ref()); err == nil {
		e.Known, e.Pinned, e.State, e.Review = true, t.PR.Pinned, t.PR.State, reviewFactsOf(t.PR)
		e.GHState, e.Title = t.PR.GHState, textx.Clip(actClean(store.Deref(t.PR.Title)), 90)
		if t.PR.URL != "" {
			e.URL = t.PR.URL
		}
	}
	return e, true
}

// pickAct runs the picked action on the entry. In the herdr popup the result
// of a review, pin or release stays up until a key is pressed, and an open or
// a browser that worked closes it at once; a failure of any of them is held
// open by the plugin's script (magnum-ctl.sh), which waits for a key after
// any non-zero exit, so it is not held here as well.
func pickAct(ctx context.Context, c *Context, d *actDeps, e pickEntry, a tui.PickAction) int {
	code := pickRun(ctx, c, d, e, a)
	if code == 0 && a != tui.PickActionOpen && a != tui.PickActionBrowser {
		d.pressAnyKey(ctx, c.Stdout)
	}
	return code
}

// pickRun runs action a on e.
func pickRun(ctx context.Context, c *Context, d *actDeps, e pickEntry, a tui.PickAction) int {
	ref := e.ref()
	switch a {
	case tui.PickActionReview:
		return reviewMain(ctx, c, d, ref, reviewOpts{})
	case tui.PickActionFresh:
		return reviewMain(ctx, c, d, ref, reviewOpts{fresh: true})
	case tui.PickActionOpen:
		return openMain(ctx, c, d, ref, openOpts{timeout: 5 * time.Minute})
	case tui.PickActionBrowser:
		if err := actOpenURL(ctx, d, e.URL); err != nil {
			return cmdFail(c, "pick", err)
		}
		fmt.Fprintf(c.Stdout, "opened %s\n", e.URL)
		return 0
	case tui.PickActionTogglePin:
		k := targetKindByName("pin")
		if e.Pinned {
			k = targetKindByName("unpin")
		}
		return targetMain(ctx, c, d, k, ref, targetOpts{})
	case tui.PickActionRelease:
		return targetMain(ctx, c, d, targetKindByName("release"), ref, targetOpts{})
	}
	return cmdFail(c, "pick", fmt.Errorf("unknown action %v", a))
}
