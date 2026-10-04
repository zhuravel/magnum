package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// PRBoardRow is one pull request on the PR board. Ref is what actions
// receive; Owner, Repo and Number label the row (Ref is parsed when they
// are empty).
type PRBoardRow struct {
	Ref, Owner, Repo   string
	Number             int
	Title, Author, URL string
	Draft              bool
	Labels, Assignees  []string
	// State is magnum's state: baseline, queued, reviewing, reviewed,
	// rereview_pending, needs_attention, paused, closed, released,
	// ineligible or ignored (magnum ignore).
	State          string
	GHState        string // GitHub's state: OPEN, CLOSED, MERGED
	UpdatedAt      time.Time
	HeadSHA        string
	LastReview     *ReviewInfo    // the latest review magnum knows of; nil when none
	Reviewers      []ReviewerInfo // everyone who reviewed or was asked to
	SinceReview    *ReviewDelta   // what changed since the last review; nil when unknown
	Slot           string         // folder of the review slot holding the PR, if any
	Pinned, Muted  bool
	Notes          bool      // the repository has reviewer notes (magnum notes)
	NextEligibleAt time.Time // earliest next automatic review; zero when not scheduled
	// LastError is the one-line explanation of the PR's stored error (for a
	// PR in needs_attention, why it needs you); ErrorFix the next step and
	// ErrorDetail the end of the failing command's output.
	LastError   string
	ErrorFix    string
	ErrorDetail []string
	RoundsToday int
	LastRound   *RoundTimings // the stages of the last review round; nil when none ran

	// Wait and WaitDetail say why a PR waiting for a round has none yet (the
	// daemon's account): the compact form the state cell shows ("re-review
	// · quiet → 14:09") and the sentence with the command that lifts it,
	// which the card shows. "" when the PR does not wait or no daemon said.
	Wait, WaitDetail string
	// Note is a one-line remark about the last review shown under LAST REVIEW
	// on the card (e.g. "comment-only push skipped (a7b3f8c → 602da9d)").
	Note string
}

// ReviewInfo is the latest review on a PR.
type ReviewInfo struct {
	Login, Event string // Event: APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED (any case)
	SubmittedAt  time.Time
	CommitSHA    string
	Stale        bool // the head moved since
	Mine         bool // Login is one of the self logins
}

// ReviewerInfo is one reviewer's latest verdict on a PR.
type ReviewerInfo struct {
	Login       string
	Verdict     string // approved | changes_requested | commented | dismissed | pending
	SubmittedAt time.Time
	CommitSHA   string
	Stale       bool // the head moved since the verdict
	Requested   bool // a review is requested from them
	Mine        bool
}

// ReviewDelta is what changed on a PR since Base: "reviewed" (the last
// reviewed head) or "base branch" (no review yet: the whole PR).
type ReviewDelta struct {
	Base                                 string
	BaseSHA                              string
	Commits, Files, Additions, Deletions int
	Truncated                            bool // the counts are lower bounds
}

// PRSort orders the board.
type PRSort string

// The board's sorts, in the order s cycles through them.
const (
	SortUpdated          PRSort = "updated"           // newest update first
	SortLastReview       PRSort = "last-review"       // latest review first
	SortReviewerActivity PRSort = "reviewer-activity" // latest verdict by anyone first
	SortChanges          PRSort = "changes"           // most lines changed since the review first
	SortState            PRSort = "state"             // most urgent state first
)

var prSortOrder = []PRSort{SortUpdated, SortLastReview, SortReviewerActivity, SortChanges, SortState}

// PRSorts lists the sorts in the order the s key cycles through them.
func PRSorts() []PRSort { return slices.Clone(prSortOrder) }

// ParsePRSort reads a sort name such as "updated" or "last-review" (case,
// "_" and spaces do not matter); empty means SortUpdated.
func ParsePRSort(s string) (PRSort, error) {
	n := strings.ToLower(strings.TrimSpace(s))
	n = strings.NewReplacer("_", "-", " ", "-").Replace(n)
	if n == "" {
		return SortUpdated, nil
	}
	for _, v := range prSortOrder {
		if string(v) == n {
			return v, nil
		}
	}
	names := make([]string, len(prSortOrder))
	for i, v := range prSortOrder {
		names[i] = string(v)
	}
	return "", fmt.Errorf("unknown sort %q (want %s)", s, strings.Join(names, ", "))
}

func (s PRSort) valid() bool { return slices.Contains(prSortOrder, s) }

// label names the sort in the title bar.
func (s PRSort) label() string { return strings.ReplaceAll(string(s), "-", " ") }

func (s PRSort) next() PRSort {
	i := slices.Index(prSortOrder, s)
	return prSortOrder[(i+1)%len(prSortOrder)]
}

// PRBoardSource supplies the board's rows; Rows is called once per
// refresh, never concurrently with itself.
type PRBoardSource interface {
	Rows(ctx context.Context) ([]PRBoardRow, error)
}

// PRBoardSourceFunc adapts a function to PRBoardSource.
type PRBoardSourceFunc func(ctx context.Context) ([]PRBoardRow, error)

// Rows calls f.
func (f PRBoardSourceFunc) Rows(ctx context.Context) ([]PRBoardRow, error) { return f(ctx) }

// PRBoardOptions tune the PR board.
type PRBoardOptions struct {
	Refresh time.Duration    // between Rows calls; default 5s, at least 200ms
	Title   string           // default "magnum · pull requests"
	Now     func() time.Time // clock for ages and the refresh time; default time.Now
	// SelfLogins are the logins that count as "me": the user and magnum's
	// own reviewer (e.g. "zhuravel", "talkable[bot]"). Case, a leading "@"
	// and a "[bot]" suffix do not matter.
	SelfLogins  []string
	DefaultSort PRSort // default SortUpdated
	DefaultView PRView // the view the board opens in; default ViewAll
	// ViewChanged, when set, hears every v, so the next board can open in
	// the same view.
	ViewChanged func(PRView)
	Repo        string // show only this repository ("name" or "owner/name"); empty shows all
	ASCII       bool   // ASCII glyphs ("+", "x", "pin") instead of ✔ ✗ 📌
	Judge       string // the judge role's name in the help ("open the <Judge> pane"); default "judge"
	// NoMouse starts with mouse support off ([terminal] mouse = false);
	// m turns it on and off either way.
	NoMouse bool
	// MouseToggled, when set, hears every m, so the next screen can start
	// the same way.
	MouseToggled func(on bool)
	// Widths keeps the column widths dragged with the mouse across runs;
	// nil keeps them for this run only.
	Widths ColumnWidths
}

// RunPRBoard shows the live PR board until the user quits or ctx ends. act
// may be nil (the action keys then say so). It returns ErrSwitchToDashboard
// when the user pressed tab for the status dashboard.
func RunPRBoard(ctx context.Context, src PRBoardSource, act DashboardActions, opts PRBoardOptions) error {
	if src == nil {
		return errors.New("tui: RunPRBoard needs a source")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // stops an action still running after the grace (or ctrl+c)
	final, err := runProgram(ctx, newPRBoardModel(ctx, src, act, opts))
	if m, ok := final.(prBoardModel); err == nil && ok && m.switching && ctx.Err() == nil {
		return ErrSwitchToDashboard
	}
	return err
}

// SortPRBoard returns a copy of rows in the given order; desc puts the
// largest key first (newest update, latest review, most recent verdict,
// most lines changed, most urgent state). Rows lacking the key (never
// reviewed, no delta) come last either way; ties put the newest update
// first, then order by ref. An unknown sort means SortUpdated.
func SortPRBoard(rows []PRBoardRow, by PRSort, desc bool) []PRBoardRow {
	out := slices.Clone(rows)
	key := prSortKey(by)
	slices.SortStableFunc(out, func(a, b PRBoardRow) int {
		va, oka := key(a)
		vb, okb := key(b)
		switch {
		case oka != okb:
			if oka {
				return -1
			}
			return 1
		case oka && va != vb:
			if desc {
				return cmp.Compare(vb, va)
			}
			return cmp.Compare(va, vb)
		}
		if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
			return c
		}
		return cmp.Compare(prRef(a), prRef(b))
	})
	return out
}

// prSortKey is the value a sort compares; false when the row lacks it.
func prSortKey(by PRSort) func(PRBoardRow) (int64, bool) {
	at := func(t time.Time) (int64, bool) { return t.UnixNano(), !t.IsZero() }
	switch by {
	case SortLastReview:
		return func(r PRBoardRow) (int64, bool) {
			if r.LastReview == nil {
				return 0, false
			}
			return at(r.LastReview.SubmittedAt)
		}
	case SortReviewerActivity:
		return func(r PRBoardRow) (int64, bool) { return at(latestVerdict(r)) }
	case SortChanges:
		return func(r PRBoardRow) (int64, bool) {
			d := r.SinceReview
			if d == nil {
				return 0, false
			}
			return int64(d.Additions+d.Deletions)*1_000_000 + int64(min(d.Commits, 999_999)), true
		}
	case SortState:
		return func(r PRBoardRow) (int64, bool) { return int64(stateUrgency(r.State)), true }
	}
	return func(r PRBoardRow) (int64, bool) { return at(r.UpdatedAt) }
}

// latestVerdict is when anyone last reviewed the PR.
func latestVerdict(r PRBoardRow) time.Time {
	var t time.Time
	if r.LastReview != nil {
		t = r.LastReview.SubmittedAt
	}
	for _, v := range r.Reviewers {
		if v.SubmittedAt.After(t) {
			t = v.SubmittedAt
		}
	}
	return t
}

// prStateOrder lists magnum's states from the most urgent; the summary
// line counts them in this order.
var prStateOrder = []string{
	"needs_attention", "paused", "reviewing", "rereview_pending", "queued",
	"reviewed", "baseline", "ineligible", "ignored", "closed", "released",
}

// stateUrgency ranks a state: higher is more urgent, unknown is 0.
func stateUrgency(state string) int {
	if i := slices.Index(prStateOrder, normState(state)); i >= 0 {
		return len(prStateOrder) - i
	}
	return 0
}

func normState(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
}

// prRefParts names the row's PR, from its fields or else its Ref or URL.
func prRefParts(r PRBoardRow) (owner, repo string, number int) {
	owner, repo, number = r.Owner, r.Repo, r.Number
	if repo != "" && number > 0 {
		return owner, repo, number
	}
	for _, s := range []string{r.Ref, r.URL} {
		if p, ok := parsePickRef(s); ok {
			if owner == "" {
				owner = p.owner
			}
			if repo == "" {
				repo = p.repo
			}
			if number <= 0 {
				number = p.number
			}
		}
	}
	return owner, repo, number
}

// prRef is what actions receive: Ref, else owner/repo#N built from the
// fields.
func prRef(r PRBoardRow) string {
	if ref := strings.TrimSpace(r.Ref); ref != "" {
		return ref
	}
	owner, repo, n := prRefParts(r)
	switch {
	case owner != "" && repo != "":
		return fmt.Sprintf("%s/%s#%d", owner, repo, n)
	case repo != "":
		return fmt.Sprintf("%s#%d", repo, n)
	case n > 0:
		return fmt.Sprintf("#%d", n)
	}
	return ""
}

// prURL is the PR's web page: URL, else github.com's page when the owner
// and repository are known.
func prURL(r PRBoardRow) string {
	if r.URL != "" {
		return r.URL
	}
	if owner, repo, n := prRefParts(r); owner != "" && repo != "" && n > 0 {
		return fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, n)
	}
	return ""
}

// inRepo reports whether the row belongs to repo ("name" or "owner/name").
func inRepo(r PRBoardRow, repo string) bool {
	repo = strings.ToLower(strings.TrimSpace(repo))
	if repo == "" {
		return true
	}
	owner, name, _ := prRefParts(r)
	if strings.Contains(repo, "/") {
		return repo == strings.ToLower(owner+"/"+name)
	}
	return repo == strings.ToLower(name)
}

// isOpen reports whether the PR is still open on GitHub (an unknown
// GitHub state counts as open unless magnum closed or released it).
func isOpen(r PRBoardRow) bool {
	switch strings.ToUpper(strings.TrimSpace(r.GHState)) {
	case "", "OPEN":
		s := normState(r.State)
		return s != "closed" && s != "released"
	}
	return false
}

// normLogin folds a login for comparison: case, "@" and "[bot]" dropped.
func normLogin(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "@")
	return strings.TrimSuffix(s, "[bot]")
}

// shortLogin is a login for the narrow columns ("talkable[bot]" →
// "talkable").
func shortLogin(s string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "@"), "[bot]")
}

// fuzzyContains reports whether term's runes appear in s in order and
// close together: the tightest such run may be at most about twice as
// long as term, so "rfrlwdgt" finds "referral widget" but "frank" does
// not find "Referral analytics: add the campaign breakdown".
func fuzzyContains(s, term string) bool {
	if strings.Contains(s, term) {
		return true
	}
	rs, ts := []rune(s), []rune(term)
	if len(ts) == 0 {
		return true
	}
	limit := 2*len(ts) + 2
	for start := range rs {
		if rs[start] != ts[0] {
			continue
		}
		i := 1
		for j := start + 1; j < len(rs) && i < len(ts) && j-start < limit; j++ {
			if rs[j] == ts[i] {
				i++
			}
		}
		if i == len(ts) {
			return true
		}
	}
	return false
}

const (
	defaultBoardRefresh = 5 * time.Second
	defaultBoardTitle   = "magnum · pull requests"
)

type (
	prbDataMsg struct {
		rows []PRBoardRow
		err  error
	}
	prbTickMsg struct{}
)

// scrollKey moves a scroll offset for a key: a line, a page, the top or
// the bottom (clamped later, once the content's length is known).
func scrollKey(k string, at, page int) int {
	switch k {
	case "j", "down":
		return at + 1
	case "k", "up":
		return max(at-1, 0)
	case "pgdown", "space", "ctrl+d":
		return at + page
	case "pgup", "ctrl+u":
		return max(at-page, 0)
	case "g", "home":
		return 0
	case "G", "end":
		return 1 << 20
	}
	return at
}

// scopeRows keeps the rows in repo, with their text made safe to draw:
// GitHub's strings may carry escape sequences or control characters.
func scopeRows(rows []PRBoardRow, repo string) []PRBoardRow {
	out := make([]PRBoardRow, 0, len(rows))
	for _, r := range rows {
		if inRepo(r, repo) {
			out = append(out, sanitizeRow(r))
		}
	}
	return out
}

func sanitizeRow(r PRBoardRow) PRBoardRow {
	r.Ref, r.Owner, r.Repo = cleanText(r.Ref), cleanText(r.Owner), cleanText(r.Repo)
	r.Title, r.Author, r.URL = cleanText(r.Title), cleanText(r.Author), cleanText(r.URL)
	r.State, r.GHState, r.Slot, r.LastError = cleanText(r.State), cleanText(r.GHState), cleanText(r.Slot), cleanText(r.LastError)
	r.HeadSHA = cleanText(r.HeadSHA)
	r.Wait, r.WaitDetail = cleanText(r.Wait), cleanText(r.WaitDetail)
	r.Note = cleanText(r.Note)
	r.ErrorFix, r.ErrorDetail = cleanText(r.ErrorFix), cleanAll(r.ErrorDetail)
	r.Labels, r.Assignees = cleanAll(r.Labels), cleanAll(r.Assignees)
	if r.LastReview != nil {
		li := *r.LastReview
		li.Login, li.Event, li.CommitSHA = cleanText(li.Login), cleanText(li.Event), cleanText(li.CommitSHA)
		r.LastReview = &li
	}
	if r.Reviewers != nil {
		revs := make([]ReviewerInfo, len(r.Reviewers))
		for i, v := range r.Reviewers {
			v.Login, v.Verdict, v.CommitSHA = cleanText(v.Login), cleanText(v.Verdict), cleanText(v.CommitSHA)
			revs[i] = v
		}
		r.Reviewers = revs
	}
	if r.SinceReview != nil {
		d := *r.SinceReview
		d.Base, d.BaseSHA = cleanText(d.Base), cleanText(d.BaseSHA)
		r.SinceReview = &d
	}
	if r.LastRound != nil {
		lr := *r.LastRound
		lr.Kind = cleanText(lr.Kind)
		lr.Stages = slices.Clone(lr.Stages)
		for i := range lr.Stages {
			lr.Stages[i].Name = cleanText(lr.Stages[i].Name)
		}
		r.LastRound = &lr
	}
	return r
}

// cleanText drops escape sequences and control characters and folds
// whitespace, so the text measures and draws as what it says.
func cleanText(s string) string {
	s = ansi.Strip(s)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, s)
	return oneLine(s)
}

func cleanAll(ss []string) []string {
	if ss == nil {
		return nil
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = cleanText(s)
	}
	return out
}

// prbMode is what the body shows.
type prbMode int

const (
	prbTable prbMode = iota
	prbDetail
	prbHelp
)

type prBoardModel struct {
	actionBar
	mouseState
	src  PRBoardSource
	opts PRBoardOptions
	st   styles
	pal  prbPalette
	g    prbGlyphs
	self map[string]bool

	width, height int

	all         []PRBoardRow // the rows in opts.Repo, as the source sent them
	view        []PRBoardRow // all, in boardView, filtered and sorted
	inView      int          // how many of all are in boardView (before the filter)
	haveData    bool
	loading     bool
	loadErr     error
	refreshedAt time.Time
	spin        spinner.Model
	spinning    bool

	sort      PRSort
	desc      bool
	boardView PRView // the preset subset of the rows (v cycles)
	filter    textinput.Model
	filtering bool // the filter input has the keyboard

	cursor, scroll int
	selKey         string // the cursor row's ref, kept across refreshes
	mode           prbMode
	detailScroll   int
	helpFrom       prbMode // where the help returns to
	helpScroll     int

	widths prbWidths   // dragged column widths
	saver  *widthSaver // keeps them; nil without opts.Widths

	gen   int64     // stamps all and view; a new stamp on every rebuild
	cache *prbCache // shared by every copy of the model
}

// prbCache is the board's rendering cache (see render_cache.go): the
// frame, the columns' content widths over every row, the column layout
// and each drawn row.
type prbCache struct {
	frame   frameCache[prbFrameKey]
	natural partCache[prbRowsKey, struct{}, prbNatural]
	layout  partCache[prbRowsKey, struct{}, prbLayout]
	summary partCache[prbRowsKey, struct{}, string]
	rows    partCache[prbRowsKey, prbRowKey, string]
}

// prbRowsKey is what the layout and every drawn row depend on besides
// the row itself.
type prbRowsKey struct {
	gen        int64
	width      int
	sort       PRSort
	desc, dark bool
	clock      int64
	widths     prbWidths // dragged widths; zero in the natural widths' key
}

// prbRowKey names one drawn row of a view.
type prbRowKey struct {
	index    int
	selected bool
}

// prbFrameKey is everything a frame of the board shows.
type prbFrameKey struct {
	rows                     prbRowsKey
	width, height            int
	cursor, scroll           int
	mode                     prbMode
	detailScroll, helpScroll int
	filter                   string // the input as drawn while filtering, else its text
	filtering                bool
	haveData, loading        bool
	loadErr                  string
	refreshedAt              int64
	spin                     string
	confirm, busy            string
	leaving, switching       bool
	flash                    string
	flashErr, flashInfo      bool
	mouseOn                  bool
	menu                     ctxMenu
}

func (m prBoardModel) rowsKey(w int) prbRowsKey {
	return prbRowsKey{gen: m.gen, width: w, sort: m.sort, desc: m.desc, dark: m.st.dark, clock: clockKey(m.opts.Now), widths: m.widths}
}

func (m prBoardModel) frameKey() prbFrameKey {
	k := prbFrameKey{
		rows: m.rowsKey(m.viewWidth()), width: m.width, height: m.height,
		cursor: m.cursor, scroll: m.scroll, mode: m.mode, detailScroll: m.detailScroll, helpScroll: m.helpScroll,
		filter: m.filter.Value(), filtering: m.filtering,
		haveData: m.haveData, loading: m.loading, loadErr: errText(m.loadErr), refreshedAt: m.refreshedAt.UnixNano(),
		busy: m.busy, leaving: m.leaving, switching: m.switching,
		flash: m.flash, flashErr: m.flashErr, flashInfo: m.flashInfo,
		mouseOn: m.mouseOn, menu: m.menu,
	}
	if m.loading || m.busy != "" || !m.haveData { // the spinner shows (drawing its frame costs)
		k.spin = m.spin.View()
	}
	if m.filtering {
		k.filter = m.filter.View() // the cursor and its blink too
	}
	if m.confirm != nil {
		k.confirm = m.confirm.question
	}
	return k
}

func newPRBoardModel(ctx context.Context, src PRBoardSource, act DashboardActions, opts PRBoardOptions) prBoardModel {
	if opts.Refresh <= 0 {
		opts.Refresh = defaultBoardRefresh
	}
	opts.Refresh = max(opts.Refresh, minRefresh)
	if opts.Title == "" {
		opts.Title = defaultBoardTitle
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if !opts.DefaultSort.valid() {
		opts.DefaultSort = SortUpdated
	}
	if !opts.DefaultView.valid() {
		opts.DefaultView = ViewAll
	}
	g := newPRBGlyphs(opts.ASCII)
	sp := spinner.New(spinner.WithSpinner(g.spinner), spinner.WithStyle(defaultStyles.Accent))
	in := textinput.New()
	in.Prompt = "/ "
	in.Placeholder = "ref, title, author, reviewer, label; state: assignee: author: review:requested"
	in.CharLimit = 120
	m := prBoardModel{
		actionBar: newActionBar(ctx, act), src: src, opts: opts, g: g, spin: sp, filter: in, cache: &prbCache{},
		self: selfSet(opts.SelfLogins), sort: opts.DefaultSort, desc: true, boardView: opts.DefaultView,
		loading: true, spinning: true, // Init starts the first load and the spinner
		saver: newWidthSaver(opts.Widths, widthsBoard),
	}
	m.mouseOn = !opts.NoMouse
	m.applyStyles(defaultStyles)
	return m
}

func selfSet(logins []string) map[string]bool {
	set := make(map[string]bool, len(logins))
	for _, l := range logins {
		if n := normLogin(l); n != "" {
			set[n] = true
		}
	}
	return set
}

func (m *prBoardModel) applyStyles(st styles) {
	m.st = st
	m.pal = newPRBPalette(st)
	m.spin.Style = st.Accent
	in := textinput.DefaultStyles(st.dark)
	in.Focused.Prompt, in.Blurred.Prompt = st.Key, st.Dim
	in.Focused.Placeholder, in.Blurred.Placeholder = st.Dim, st.Dim
	in.Cursor.Color = st.Accent.GetForeground()
	m.filter.SetStyles(in)
}

func (m prBoardModel) Init() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor, m.loadCmd(), m.spin.Tick, m.tickCmd(), m.saver.load(m.ctx))
}

func (m prBoardModel) loadCmd() tea.Cmd {
	ctx, src := m.ctx, m.src
	return func() tea.Msg {
		ctx, cancel := loadContext(ctx)
		defer cancel()
		rows, err := src.Rows(ctx)
		return prbDataMsg{rows: rows, err: err}
	}
}

func (m prBoardModel) tickCmd() tea.Cmd {
	return tea.Tick(m.opts.Refresh, func(time.Time) tea.Msg { return prbTickMsg{} })
}

// startLoad begins a load unless one is in flight.
func (m *prBoardModel) startLoad() tea.Cmd {
	if m.loading {
		return nil
	}
	m.loading = true
	return tea.Batch(m.loadCmd(), m.startSpinner())
}

func (m *prBoardModel) startSpinner() tea.Cmd {
	if m.spinning {
		return nil
	}
	m.spinning = true
	return m.spin.Tick
}

func (m prBoardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	next.fixScroll()
	return next, cmd
}

func (m prBoardModel) update(msg tea.Msg) (prBoardModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.filter.SetWidth(max(msg.Width-prbFilterRight(msg.Width)-6, 8))
	case tea.BackgroundColorMsg:
		m.applyStyles(newStyles(msg.IsDark()))
	case prbDataMsg:
		m.loading = false
		if msg.err != nil {
			m.loadErr = msg.err
			return m, nil
		}
		m.all = scopeRows(msg.rows, m.opts.Repo)
		m.haveData, m.loadErr, m.refreshedAt = true, nil, m.opts.Now()
		m.rebuild()
	case prbTickMsg:
		cmd := tea.Batch(m.startLoad(), m.tickCmd())
		return m, cmd
	case spinner.TickMsg:
		if !m.loading && m.busy == "" {
			m.spinning = false // let the tick chain end
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case actionDoneMsg:
		cmd, quit := m.done(msg)
		if quit {
			return m, cmd
		}
		cmd = tea.Batch(cmd, m.startLoad())
		return m, cmd
	case flashExpireMsg:
		m.expire(msg)
	case finishGraceMsg:
		return m, m.graceOver()
	case widthsLoadedMsg:
		return m.widthsLoaded(msg)
	case widthsSavedMsg:
		return m.fail("could not keep the column widths: " + oneLine(msg.err.Error()))
	case tea.KeyPressMsg:
		return m.updateKey(msg)
	case tea.MouseMsg:
		return m.updateMouse(msg)
	case tea.PasteMsg:
		if m.filtering {
			return m.editFilter(msg)
		}
	default:
		if m.filtering {
			return m.editFilter(msg) // cursor blinks
		}
	}
	return m, nil
}

// rebuild keeps the rows of the view that match the filter, sorts them and
// keeps the cursor on the same PR when it is still listed.
func (m *prBoardModel) rebuild() {
	q := parsePRQuery(m.filter.Value())
	rows := make([]PRBoardRow, 0, len(m.all))
	m.inView = 0
	for _, r := range m.all {
		if !m.boardView.has(r, m.self) {
			continue
		}
		m.inView++
		if q.match(r, m.self) {
			rows = append(rows, r)
		}
	}
	m.view = SortPRBoard(rows, m.sort, m.desc)
	m.gen = nextGen()
	if m.selKey != "" {
		for i, r := range m.view {
			if prRef(r) == m.selKey {
				m.cursor = i
				return
			}
		}
	}
	m.moveTo(m.cursor)
}

func (m *prBoardModel) moveTo(i int) {
	if len(m.view) == 0 {
		m.cursor = 0 // keep selKey: the PR comes back when the filter does
		return
	}
	m.cursor = min(max(i, 0), len(m.view)-1)
	m.selKey = prRef(m.view[m.cursor])
}

func (m prBoardModel) selected() (PRBoardRow, bool) {
	if m.cursor < 0 || m.cursor >= len(m.view) {
		return PRBoardRow{}, false
	}
	return m.view[m.cursor], true
}

func (m prBoardModel) updateKey(msg tea.KeyPressMsg) (prBoardModel, tea.Cmd) {
	k := msg.String()
	switch {
	case m.leaving:
		cmd := m.leavingKey(k)
		return m, cmd
	case k == "ctrl+c":
		cmd := m.leave(false)
		return m, cmd
	case m.confirm != nil:
		cmd, started := m.answer(k)
		return m.withSpinner(cmd, started)
	case m.menu.open:
		return m.menuKey(k)
	}
	if m.filtering {
		return m.filterKey(msg)
	}
	switch m.mode {
	case prbHelp:
		return m.helpKey(k)
	case prbDetail:
		if next, ok := m.detailKey(k); ok {
			return next, nil
		}
	}
	return m.tableKey(k)
}

// helpKey handles a key over the help: it scrolls, closes or quits.
func (m prBoardModel) helpKey(k string) (prBoardModel, tea.Cmd) {
	switch k {
	case "q":
		cmd := m.leave(false)
		return m, cmd
	case "?", "esc", "enter", "backspace":
		m.mode, m.helpScroll = m.helpFrom, 0
	default:
		m.helpScroll = scrollKey(k, m.helpScroll, max(m.bodyHeight()-3, 1))
	}
	return m, nil
}

// detailKey handles the keys the details card owns (back, help, scroll);
// ok is false for the rest, which act as on the table.
func (m prBoardModel) detailKey(k string) (next prBoardModel, ok bool) {
	switch k {
	case "esc", "enter", "backspace", "left", "h":
		m.mode, m.detailScroll = prbTable, 0
	case "?":
		m.helpFrom, m.mode = prbDetail, prbHelp
	case "j", "down", "k", "up", "g", "home", "G", "end", "pgdown", "space", "ctrl+d", "pgup", "ctrl+u":
		// The card scrolls; the cursor stays on its PR.
		m.detailScroll = scrollKey(k, m.detailScroll, max(m.bodyHeight()-3, 1))
	default:
		return m, false
	}
	return m, true
}

// tableKey handles a key on the table, and the action keys everywhere.
func (m prBoardModel) tableKey(k string) (prBoardModel, tea.Cmd) {
	switch k {
	case "q":
		cmd := m.leave(false)
		return m, cmd
	case "tab":
		cmd := m.leave(true)
		return m, cmd
	case "esc":
		if m.filter.Value() != "" {
			m.filter.SetValue("")
			m.rebuild()
			return m, nil
		}
		cmd := m.leave(false)
		return m, cmd
	case "j", "down":
		m.moveTo(m.cursor + 1)
	case "k", "up":
		m.moveTo(m.cursor - 1)
	case "g", "home":
		m.moveTo(0)
	case "G", "end":
		m.moveTo(len(m.view) - 1)
	case "pgdown", "ctrl+d", "space":
		m.moveTo(m.cursor + max(m.tableHeight()-1, 1))
	case "pgup", "ctrl+u":
		m.moveTo(m.cursor - max(m.tableHeight()-1, 1))
	case "enter":
		if _, ok := m.selected(); ok {
			m.mode, m.detailScroll = prbDetail, 0
		}
	case "?":
		m.helpFrom, m.mode = prbTable, prbHelp
	case "s":
		m.sort, m.desc = m.sort.next(), true
		m.rebuild()
	case "S":
		m.desc = !m.desc
		m.rebuild()
	case "v":
		m.boardView = m.boardView.next()
		m.rebuild()
		if m.opts.ViewChanged != nil {
			m.opts.ViewChanged(m.boardView)
		}
	case "/":
		m.filtering = true
		m.mode = prbTable
		cmd := m.filter.Focus()
		return m, cmd
	case "ctrl+r", "f5":
		cmd := m.startLoad()
		return m, cmd
	case "a":
		return m.run("attention", func(ctx context.Context, a DashboardActions) (string, error) { return a.Attention(ctx) })
	case "o":
		return m.onRow("open", func(ctx context.Context, a DashboardActions, ref string) (string, error) { return a.Open(ctx, ref) })
	case "r":
		return m.review("review", ReviewOpts{})
	case "R":
		return m.review("fresh review", ReviewOpts{Fresh: true})
	case "i":
		return m.review("simplify review", ReviewOpts{Simplify: true})
	case "p":
		return m.onRow("pin", func(ctx context.Context, a DashboardActions, ref string) (string, error) { return a.Pin(ctx, ref) })
	case "u":
		return m.onRow("unpin", func(ctx context.Context, a DashboardActions, ref string) (string, error) { return a.Unpin(ctx, ref) })
	case "M":
		return m.askOnRow("mute", func(r PRBoardRow, ref string) string { return muteQuestion(m.questionLabel(r, ref), true) },
			func(ctx context.Context, a DashboardActions, ref string) (string, error) { return a.Mute(ctx, ref) })
	case "U":
		return m.askOnRow("unmute", func(r PRBoardRow, ref string) string {
			if normState(r.State) == "ignored" {
				return unmuteIgnoredQuestion(m.questionLabel(r, ref))
			}
			return muteQuestion(m.questionLabel(r, ref), false)
		},
			func(ctx context.Context, a DashboardActions, ref string) (string, error) { return a.Unmute(ctx, ref) })
	case "K":
		return m.askOnRow("abort", func(r PRBoardRow, ref string) string { return abortQuestion(m.questionLabel(r, ref)) },
			func(ctx context.Context, a DashboardActions, ref string) (string, error) { return a.Abort(ctx, ref) })
	case "I":
		return m.askOnRow("ignore", func(r PRBoardRow, ref string) string { return ignoreQuestion(m.questionLabel(r, ref)) },
			func(ctx context.Context, a DashboardActions, ref string) (string, error) { return a.Ignore(ctx, ref) })
	case "x":
		return m.askOnRow("release", func(r PRBoardRow, ref string) string { return releaseQuestion(m.questionLabel(r, ref)) },
			func(ctx context.Context, a DashboardActions, ref string) (string, error) { return a.Release(ctx, ref) })
	case "b":
		return m.browse()
	case "m":
		m.toggleMouse()
		if m.opts.MouseToggled != nil {
			m.opts.MouseToggled(m.mouseOn)
		}
		cmd := m.note(mouseNote(m.mouseOn))
		return m, cmd
	case "W":
		return m.resetWidths()
	}
	return m, nil
}

// filterKey edits the filter: enter keeps it, esc clears it, the arrows
// still move the cursor.
func (m prBoardModel) filterKey(msg tea.KeyPressMsg) (prBoardModel, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.filtering = false
		m.filter.Blur()
		m.filter.SetValue("")
		m.rebuild()
		return m, nil
	case "enter":
		m.filtering = false
		m.filter.Blur()
		return m, nil
	case "down", "ctrl+n", "ctrl+j":
		m.moveTo(m.cursor + 1)
		return m, nil
	case "up", "ctrl+p", "ctrl+k":
		m.moveTo(m.cursor - 1)
		return m, nil
	}
	return m.editFilter(msg)
}

// editFilter hands msg to the filter input and refilters when it changed.
func (m prBoardModel) editFilter(msg tea.Msg) (prBoardModel, tea.Cmd) {
	before := m.filter.Value()
	var cmd tea.Cmd
	m.filter, cmd = m.filter.Update(msg)
	if m.filter.Value() != before {
		m.rebuild()
	}
	return m, cmd
}

// run starts an action unless one is already running.
func (m prBoardModel) run(what string, fn actionFunc) (prBoardModel, tea.Cmd) {
	cmd, started := m.start(what, fn)
	return m.withSpinner(cmd, started)
}

// withSpinner adds the spinner to an action that started.
func (m prBoardModel) withSpinner(cmd tea.Cmd, started bool) (prBoardModel, tea.Cmd) {
	if started {
		cmd = tea.Batch(cmd, m.startSpinner())
	}
	return m, cmd
}

// fail shows an error in the footer.
func (m prBoardModel) fail(text string) (prBoardModel, tea.Cmd) {
	cmd := m.setFlash(text, true)
	return m, cmd
}

// onRow runs fn on the cursor row's PR.
func (m prBoardModel) onRow(what string, fn refActionFunc) (prBoardModel, tea.Cmd) {
	r, ok := m.selected()
	if !ok {
		return m.fail("nothing selected")
	}
	ref := prRef(r)
	if ref == "" {
		return m.fail("this row names no PR")
	}
	return m.run(what+" "+ref, func(ctx context.Context, a DashboardActions) (string, error) { return fn(ctx, a, ref) })
}

// questionLabel names the row's PR in a question as the table does:
// repo#N, with the owner when the board spans several.
func (m prBoardModel) questionLabel(r PRBoardRow, ref string) string {
	if owner, repo, n := prRefParts(r); repo != "" && n > 0 {
		if m.painter().owners && owner != "" {
			return fmt.Sprintf("%s/%s#%d", owner, repo, n)
		}
		return fmt.Sprintf("%s#%d", repo, n)
	}
	return ref
}

// askOnRow puts fn on the cursor row's PR to the user: the footer asks
// question(row, ref) and y runs it. A missing PR, missing actions
// or an action still running fail at once instead of asking.
func (m prBoardModel) askOnRow(what string, question func(r PRBoardRow, ref string) string, fn refActionFunc) (prBoardModel, tea.Cmd) {
	r, ok := m.selected()
	if !ok {
		return m.fail("nothing selected")
	}
	ref := prRef(r)
	if ref == "" {
		return m.fail("this row names no PR")
	}
	cmd := m.ask(question(r, ref), what+" "+ref,
		func(ctx context.Context, a DashboardActions) (string, error) { return fn(ctx, a, ref) })
	return m, cmd
}

// review asks before a review round, saying what it will do and what
// changed since the last review.
func (m prBoardModel) review(what string, o ReviewOpts) (prBoardModel, tea.Cmd) {
	now := m.opts.Now()
	return m.askOnRow(what, func(r PRBoardRow, ref string) string {
		return reviewQuestion(m.questionLabel(r, ref), o, boardReviewFacts(r, now))
	},
		func(ctx context.Context, a DashboardActions, ref string) (string, error) {
			return a.Review(ctx, ref, o)
		})
}

func (m prBoardModel) browse() (prBoardModel, tea.Cmd) {
	r, ok := m.selected()
	if !ok {
		return m.fail("nothing selected")
	}
	url := prURL(r)
	if url == "" {
		return m.fail("this row has no URL")
	}
	return m.run("browser", func(ctx context.Context, a DashboardActions) (string, error) {
		if err := a.OpenBrowser(ctx, url); err != nil {
			return "", err
		}
		return "opened " + url, nil
	})
}

func (m prBoardModel) viewWidth() int {
	if m.width > 0 {
		return m.width
	}
	return 120
}

func (m prBoardModel) viewHeight() int {
	if m.height > 0 {
		return m.height
	}
	return 30
}

// The screen is the title bar and the summary line, the body, then the
// status rule and the key hints. The table spends two body lines on its
// heading and its rule.
const (
	prbChrome      = 4
	prbTableChrome = 2
)

// bodyHeight is the room between the summary line and the footer.
func (m prBoardModel) bodyHeight() int { return max(m.viewHeight()-prbChrome, 3) }

// tableHeight is how many rows fit on screen.
func (m prBoardModel) tableHeight() int { return max(m.bodyHeight()-prbTableChrome, 1) }

// fixScroll scrolls the table so the cursor row stays on screen, and
// keeps the card's and the help's scroll within their content.
func (m *prBoardModel) fixScroll() {
	if m.mode == prbHelp {
		n := len(m.painter().helpContent(m.viewWidth()))
		m.helpScroll = min(max(m.helpScroll, 0), max(n-(m.bodyHeight()-2), 0))
	}
	if m.mode == prbDetail {
		r, ok := m.selected()
		if !ok {
			m.detailScroll = 0
			return
		}
		n := len(m.painter().cardContent(r, cardInner(m.viewWidth())))
		m.detailScroll = min(max(m.detailScroll, 0), max(n-(m.bodyHeight()-2), 0))
	}
	avail := m.tableHeight()
	switch {
	case len(m.view) <= avail:
		m.scroll = 0
	case m.cursor < m.scroll:
		m.scroll = m.cursor
	case m.cursor >= m.scroll+avail:
		m.scroll = m.cursor - avail + 1
	}
	m.scroll = min(max(m.scroll, 0), max(len(m.view)-avail, 0))
}

// View draws the board, or returns the last frame when nothing it shows
// changed (a key storm at the bottom of the list changes nothing).
func (m prBoardModel) View() tea.View {
	return withMouse(newView(m.cache.frameFor(m.frameKey(), m.render)), m.mouseOn)
}

func (m prBoardModel) painter() prbPainter {
	p := newPRBPainter(m.st, m.pal, m.g, m.opts.Now(), m.self, m.all, m.sort, m.desc)
	p.judge = m.opts.Judge
	return p
}

func (m prBoardModel) render() string {
	w, h := m.viewWidth(), m.viewHeight()
	p := m.painter()
	right := m.st.Dim.Render("loading…")
	if m.haveData {
		right = m.st.Dim.Render(m.g.refresh + " " + m.refreshedAt.Local().Format("15:04:05"))
	}
	if m.loading || m.busy != "" {
		right = m.spin.View() + " " + right
	}
	out := []string{
		p.titleLine(w, m.opts.Title, m.opts.Repo, m.boardView, m.inView, m.filter.Value(), len(m.view), right),
		m.cache.summaryFor(m.rowsKey(w), func() string { return p.summaryLine(w) }),
	}

	var body []string
	below := 0
	switch m.mode {
	case prbHelp:
		body, below = p.help(w, m.bodyHeight(), m.helpScroll)
	case prbDetail:
		body, below = m.detailLines(p, w)
	default:
		body, below = m.tableLines(p, w)
	}
	for len(body) < m.bodyHeight() {
		body = append(body, "")
	}
	out = append(out, body[:m.bodyHeight()]...)
	out = append(out, m.statusRule(p, w, below), m.hintLine(w))
	if len(out) > h { // a tiny screen keeps the footer: it asks the y/n
		out = out[len(out)-h:]
	}
	for i, l := range out {
		out[i] = truncate(l, w)
	}
	if m.menu.open {
		m.drawMenu(out, w, h)
	}
	return strings.Join(out, "\n")
}

// tableLines are the column headings, a rule (marking the rows scrolled
// above) and the visible rows; below counts the rows under the screen.
func (m prBoardModel) tableLines(p prbPainter, w int) (lines []string, below int) {
	rk := m.rowsKey(w)
	lay := m.tableLayout(p, w)
	if !m.haveData {
		if m.loadErr != nil {
			return []string{"", "  " + m.st.Err.Render(truncate("could not load pull requests: "+oneLine(m.loadErr.Error()), w-2))}, 0
		}
		return []string{"", "  " + m.spin.View() + " " + m.st.Dim.Render("loading pull requests…")}, 0
	}
	avail := m.tableHeight()
	start := min(m.scroll, max(len(m.view)-1, 0))
	end := min(start+avail, len(m.view))
	below = len(m.view) - end
	lines = append(lines, p.headerLine(lay, w), p.rule(w, start, true))
	switch {
	case len(m.view) == 0 && m.filter.Value() != "":
		lines = append(lines, "", "  "+m.st.Dim.Render(truncate(fmt.Sprintf("no PR matches %q · esc clears the filter", m.filter.Value()), w-2)))
	case len(m.view) == 0 && m.boardView != ViewAll:
		lines = append(lines, "", "  "+m.st.Dim.Render(truncate(fmt.Sprintf("no PR in the %s view · v shows the next view", m.boardView), w-2)))
	case len(m.view) == 0:
		msg := "no open pull requests"
		if m.opts.Repo != "" {
			msg += " in " + m.opts.Repo
		}
		lines = append(lines, "", "  "+m.st.Dim.Render(msg))
	}
	for i := start; i < end; i++ {
		sel := i == m.cursor
		lines = append(lines, m.cache.row(rk, prbRowKey{i, sel}, func() string { return p.rowLine(m.view[i], lay, w, sel) }))
	}
	return lines, below
}

// detailLines is the cursor row's card, scrolled; below counts the card
// lines out of view.
func (m prBoardModel) detailLines(p prbPainter, w int) ([]string, int) {
	r, ok := m.selected()
	if !ok {
		return []string{"", "  " + m.st.Dim.Render("nothing selected · esc goes back")}, 0
	}
	return p.card(r, w, m.bodyHeight(), m.detailScroll)
}

// statusRule is the rule above the key hints; it carries the action
// result, the pending question, a load error or the scroll position.
func (m prBoardModel) statusRule(p prbPainter, w, below int) string {
	var msg string
	switch {
	case m.confirm != nil:
		// The question takes the whole rule: footerRule keeps 4 cells
		// for its lead and the space after the message.
		return p.footerRule(w, m.st.confirmLine(m.confirm.question, w-4), 0)
	case m.leaving:
		msg = m.spin.View() + " " + m.st.Warn.Render(m.busyText())
	case m.flash != "":
		switch {
		case m.flashErr:
			msg = m.st.Err.Render(m.g.fail + " " + m.flash)
		case m.flashInfo:
			msg = m.st.Dim.Render(m.flash)
		default:
			msg = m.st.OK.Render(m.g.ok + " " + m.flash)
		}
		msg += m.st.Dim.Render(m.busySuffix()) // the action in flight stays visible
	case m.busy != "":
		msg = m.spin.View() + " " + m.busyText()
	case m.loadErr != nil && m.haveData:
		text := "refresh failed: " + oneLine(m.loadErr.Error())
		if !m.refreshedAt.IsZero() {
			text += " (showing data from " + m.refreshedAt.Local().Format("15:04:05") + ")"
		}
		msg = m.st.Err.Render(m.g.fail + " " + text)
	}
	return p.footerRule(w, msg, below)
}

// prbFilterCount is the room for "999 of 999 match"; wide screens also
// show the filter's keys next to it.
const prbFilterCount = 18

// prbFilterRight is the room the filter line keeps right of the input.
func prbFilterRight(w int) int {
	if w >= 100 {
		return prbFilterCount + 40
	}
	return prbFilterCount
}

func (m prBoardModel) hintLine(w int) string {
	if m.filtering {
		right := m.st.Dim.Render(fmt.Sprintf("%d of %d match", len(m.view), m.inView))
		if prbFilterRight(w) > prbFilterCount {
			right += m.st.Dim.Render("  ·  ") + m.st.hints(0, hint{"enter", "keep"}, hint{"esc", "clear"}, hint{"↑/↓", "move"})
		}
		return spread(m.filter.View(), right+" ", w)
	}
	var left string
	off := ""
	if !m.mouseOn {
		off = m.st.Dim.Render("mouse off")
	}
	switch m.mode {
	case prbHelp:
		left = m.st.hints(w, hint{"j/k", "scroll"}, hint{"?", "close help"}, hint{"q", "quit"})
	case prbDetail:
		left = m.st.fitHints(w,
			[]hint{{"esc", "back"}, {"j/k", "scroll"}, {"r", "review"}, {"R", "fresh"}, {"i", "simplify"}, {"o", "open"},
				{"b", "browser"}, {"p/u", "pin"}, {"M/U", "mute"}, {"x", "release"}, {"K", "kill"}, {"I", "ignore"}, {"?", "help"}},
			[]hint{{"esc", "back"}, {"r", "review"}, {"R", "fresh"}, {"i", "simplify"}, {"o", "open"}, {"b", "browser"},
				{"p/u", "pin"}, {"M/U", "mute"}, {"x", "release"}, {"?", "help"}},
			[]hint{{"esc", "back"}, {"r", "review"}, {"o", "open"}, {"b", "browser"}, {"?", "help"}},
			[]hint{{"esc", "back"}, {"?", "help"}})
	default:
		pos := ""
		if len(m.view) > 0 {
			pos = m.st.Dim.Render(fmt.Sprintf("%d/%d", m.cursor+1, len(m.view)))
		}
		if off != "" {
			pos = strings.TrimSpace(off + "  " + pos)
		}
		room := max(w-ansi.StringWidth(pos)-2, 10)
		sets := [][]hint{
			{{"enter", "details"}, {"r", "review"}, {"R", "fresh"}, {"i", "simplify"}, {"o", "open"}, {"b", "browser"},
				{"p/u", "pin"}, {"x", "release"}, {"/", "filter"}, {"v", "view"}, {"s/S", "sort"}, {"tab", "overview"}, {"?", "help"}, {"q", "quit"}},
			{{"enter", "details"}, {"r", "review"}, {"o", "open"}, {"b", "browser"}, {"/", "filter"}, {"v", "view"}, {"s", "sort"},
				{"tab", "overview"}, {"?", "help"}, {"q", "quit"}},
			{{"enter", "details"}, {"/", "filter"}, {"v", "view"}, {"tab", "overview"}, {"?", "help"}, {"q", "quit"}},
			{{"enter", "details"}, {"/", "filter"}, {"?", "help"}, {"q", "quit"}},
		}
		if f := m.filter.Value(); f != "" {
			for i := range sets {
				sets[i] = append([]hint{{"esc", "clear " + m.st.Accent.Render(f)}}, sets[i]...)
			}
		}
		left = m.st.fitHints(room, sets...)
		return spread(left, pos, w)
	}
	return spread(left, off, w)
}
