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
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/zhuravel/magnum/internal/textx"
)

// PRBoardOptions tune the PR board.
type PRBoardOptions struct {
	Refresh time.Duration    // between Rows calls; default 5s, at least 200ms
	Title   string           // default "magnum · pull requests"
	Now     func() time.Time // clock for ages and the refresh time; default time.Now
	// Tick schedules the board's timers (refresh, spinner and shimmer
	// frames, flash, the wait before closing); default tea.Tick.
	Tick func(time.Duration, func(time.Time) tea.Msg) tea.Cmd
	// SelfLogins are the logins that count as "me": the user and magnum's
	// own reviewer (e.g. "zhuravel", "talkable[bot]"). Case, a leading "@"
	// and a "[bot]" suffix do not matter.
	SelfLogins  []string
	DefaultSort PRSort // default SortUpdated
	DefaultView PRView // the view the board opens in; default ViewAll
	// DefaultOwner is the owner scope the board opens in; "" (the default)
	// shows every owner, as does an owner without PRs.
	DefaultOwner string
	// OwnerChanged, when set, hears every change of the owner scope (O, or
	// its owner's last PR leaving), so the next board can open in it.
	OwnerChanged func(owner string)
	// ViewChanged, when set, hears every v, so the next board can open in
	// the same view.
	ViewChanged func(PRView)
	Repo        string // show only this repository ("name" or "owner/name"); empty shows all
	// DefaultRepo is daemon.default_repo ("owner/name"): its owner comes
	// first when O cycles the owners.
	DefaultRepo string
	Icons       IconMode // the symbols: unicode (default), nerd (Nerd Font icons and emoji) or ascii
	Judge       string   // the judge role's name in the help ("open the <Judge> pane"); default "judge"
	// NoMouse starts with mouse support off ([terminal] mouse = false);
	// m turns it on and off either way.
	NoMouse bool
	// NoShimmer keeps the state cells of the PRs that need the operator
	// still ([board] shimmer = false); by default their colors slide.
	NoShimmer bool
	// HideSkipped starts the board with the ignored and skipped PRs hidden
	// (h toggles it); HideToggled, when set, hears every h so the choice
	// can be kept.
	HideSkipped bool
	HideToggled func(hide bool)
	// MouseToggled, when set, hears every m, so the next screen can start
	// the same way.
	MouseToggled func(on bool)
	// Widths keeps the column widths dragged with the mouse across runs;
	// nil keeps them for this run only.
	Widths ColumnWidths
	// RecentClosed is [board] recent_closed, the window the heading of the
	// recently closed section names ("merged or closed in the last 24h");
	// the source marks the rows in it (PRBoardRow.Recent).
	RecentClosed time.Duration
	// Log keeps the actions' outcomes (! shows them) and the requests still
	// pending; the dashboard shares it, so tab keeps both. nil = a log of
	// this screen's own.
	Log *ActionLog
	// Facts, when set, reads what the title says of the daemon (an older
	// build, a pause, a drain, the Codex budget's pace) with every load.
	Facts func(ctx context.Context) DaemonFacts
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

const (
	defaultBoardRefresh = 5 * time.Second
	defaultBoardTitle   = "magnum · pull requests"
)

type (
	prbDataMsg struct {
		rows  []PRBoardRow
		facts DaemonFacts
		err   error
	}
	prbTickMsg    struct{}
	prbAnimMsg    struct{} // the next frame of the reviewing pills' spinner
	prbShimmerMsg struct{} // the next frame of the needs-you cells' shimmer
)

// prbAnimEvery is the reviewing spinner's frame time: its eight frames turn
// once a second.
const prbAnimEvery = 125 * time.Millisecond

// prbShimmerEvery is the shimmer's frame time: four frames a second, the
// rainbow sliding one cell each.
const prbShimmerEvery = 250 * time.Millisecond

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

// prbMode is what the body shows.
type prbMode int

const (
	prbTable prbMode = iota
	prbDetail
	prbHelp
	prbLog // the action log (!)
)

type prBoardModel struct {
	actionBar
	mouseState
	src  PRBoardSource
	opts PRBoardOptions
	st   styles
	pal  prbPalette
	g    glyphs
	self map[string]bool

	width, height int

	loaded      []PRBoardRow // the rows in opts.Repo, as the source sent them
	all         []PRBoardRow // loaded, in the owner scope
	view        []PRBoardRow // all, in boardView, filtered and sorted
	inView      int          // how many of all are in boardView (before the filter)
	haveData    bool
	loading     bool
	loadErr     error
	refreshedAt time.Time
	facts       DaemonFacts // what the title says of the daemon (opts.Facts)
	spin        spinner.Model
	spinning    bool
	working     bool // some row in scope has a review round running: its pill spins
	anim        int  // the spinner's frame
	animating   bool // a prbAnimMsg is pending
	// shimmer is the needs-you cells' frame, which moves only while one of
	// them is on screen (shimmerOn); shimmering: a prbShimmerMsg is pending.
	// colorless: the terminal shows no colors (NO_COLOR, or none to show),
	// so those cells are drawn in reverse video and never shimmer.
	shimmer    int
	shimmering bool
	colorless  bool

	sort      PRSort
	desc      bool
	boardView PRView // the preset subset of the rows (v cycles)
	owner     string // the owner whose rows show (O cycles); "" shows every owner
	hide      bool   // ignored and skipped rows are hidden (h toggles)
	hidden    int    // how many rows hide hides in the owner scope
	filter    textinput.Model
	filtering bool // the filter input has the keyboard

	cursor, scroll int
	// section is the index in view of the first recently closed row, which
	// the section's heading line precedes; -1 when view has none.
	section      int
	selKey       string // the cursor row's ref, kept across refreshes
	mode         prbMode
	detailScroll int
	helpFrom     prbMode // where the help returns to
	helpScroll   int
	logFrom      prbMode // where the action log returns to
	logScroll    int

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
	anim       int // the reviewing spinner's frame; 0 while nothing spins
	sort       PRSort
	owner      string // the owner scope: the rows, their counts and refs follow it
	hide       bool   // ignored and skipped rows hidden: the rows and counts follow it
	desc, dark bool
	colorless  bool // the needs-you cells are drawn in reverse video
	clock      int64
	widths     prbWidths // dragged widths; zero in the natural widths' key
	queued     string    // the rows marked waiting for the daemon (ActionLog.pendingTargets)
}

// prbRowKey names one drawn row of a view. shimmer is the shimmer's frame
// for a row whose state cell shimmers, 0 for every other: only those rows
// are drawn again as it moves.
type prbRowKey struct {
	index    int
	selected bool
	shimmer  int
}

// prbFrameKey is everything a frame of the board shows.
type prbFrameKey struct {
	rows                     prbRowsKey
	width, height            int
	cursor, scroll           int
	mode                     prbMode
	detailScroll, helpScroll int
	logScroll                int
	logGen                   int64  // the action log's generation
	facts                    string // the title's facts as drawn (DaemonFacts.key)
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
	shimmer                  int // the shimmer's frame while a needs-you cell is on screen, else 0
}

func (m prBoardModel) rowsKey(w int) prbRowsKey {
	return prbRowsKey{gen: m.gen, width: w, anim: m.animFrame(), sort: m.sort, owner: m.owner, hide: m.hide, desc: m.desc, dark: m.st.dark,
		colorless: m.colorless, clock: clockKey(m.opts.Now), widths: m.widths, queued: strings.Join(m.log.pendingTargets(), "\n")}
}

func (m prBoardModel) frameKey() prbFrameKey {
	k := prbFrameKey{
		rows: m.rowsKey(m.viewWidth()), width: m.width, height: m.height,
		cursor: m.cursor, scroll: m.scroll, mode: m.mode, detailScroll: m.detailScroll, helpScroll: m.helpScroll,
		logScroll: m.logScroll, logGen: m.log.generation(), facts: m.facts.key(m.opts.Now()),
		filter: m.filter.Value(), filtering: m.filtering,
		haveData: m.haveData, loading: m.loading, loadErr: errText(m.loadErr), refreshedAt: m.refreshedAt.UnixNano(),
		busy: m.busy, leaving: m.leaving, switching: m.switching,
		flash: m.flash, flashErr: m.flashErr, flashInfo: m.flashInfo,
		mouseOn: m.mouseOn, menu: m.menu, shimmer: m.shimmerFrame(),
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
	if opts.Tick == nil {
		opts.Tick = tea.Tick
	}
	if !opts.DefaultSort.valid() {
		opts.DefaultSort = SortUpdated
	}
	if !opts.DefaultView.valid() {
		opts.DefaultView = ViewAll
	}
	g := newGlyphs(opts.Icons)
	sp := spinner.New(spinner.WithSpinner(g.spinner), spinner.WithStyle(defaultStyles.Accent))
	in := textinput.New()
	in.Prompt = "/ "
	in.Placeholder = "ref, title, author, reviewer, label; state: assignee: author: review:requested"
	in.CharLimit = 120
	m := prBoardModel{
		actionBar: newActionBar(ctx, act, opts.Log, opts.Now, opts.Tick), src: src, opts: opts, g: g, spin: sp, filter: in, cache: &prbCache{},
		self: selfSet(opts.SelfLogins), sort: opts.DefaultSort, desc: true, boardView: opts.DefaultView,
		owner: strings.TrimSpace(opts.DefaultOwner), hide: opts.HideSkipped, section: -1,
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
		if n := textx.FoldLogin(l); n != "" {
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
	ctx, src, facts := m.ctx, m.src, m.opts.Facts
	return func() tea.Msg {
		ctx, cancel := loadContext(ctx)
		defer cancel()
		rows, err := src.Rows(ctx)
		msg := prbDataMsg{rows: rows, err: err}
		if facts != nil && err == nil {
			msg.facts = facts(ctx)
		}
		return msg
	}
}

func (m prBoardModel) tickCmd() tea.Cmd {
	return m.opts.Tick(m.opts.Refresh, func(time.Time) tea.Msg { return prbTickMsg{} })
}

// startLoad begins a load unless one is in flight.
func (m *prBoardModel) startLoad() tea.Cmd {
	if m.loading {
		return nil
	}
	m.loading = true
	return tea.Batch(m.loadCmd(), m.startSpinner())
}

// startAnim starts the reviewing spinner's frames when a row needs them
// and none are pending; prbAnimMsg ends the chain once no row does.
func (m *prBoardModel) startAnim() tea.Cmd {
	if m.animating || !m.working || len(m.g.working) == 0 {
		return nil
	}
	m.animating = true
	return m.animCmd()
}

func (m prBoardModel) animCmd() tea.Cmd {
	return m.opts.Tick(prbAnimEvery, func(time.Time) tea.Msg { return prbAnimMsg{} })
}

// animFrame is the spinner's frame for the cache keys: 0 while nothing
// spins, so a still board keeps its cached frames.
func (m prBoardModel) animFrame() int {
	if !m.working || len(m.g.working) == 0 {
		return 0
	}
	return m.anim % len(m.g.working)
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
	if sh := next.startShimmer(); sh != nil {
		cmd = tea.Batch(cmd, sh)
	}
	return next, cmd
}

// startShimmer starts the shimmer's frames when a needs-you cell is on
// screen and none are pending; prbShimmerMsg ends the chain once none is.
// Every Update asks, so a scroll, a load, a mode or a resize that brings
// one on screen starts it.
func (m *prBoardModel) startShimmer() tea.Cmd {
	if m.shimmering || !m.shimmerOn() {
		return nil
	}
	m.shimmering = true
	return m.opts.Tick(prbShimmerEvery, func(time.Time) tea.Msg { return prbShimmerMsg{} })
}

// shimmerOn reports whether the needs-you cells shimmer now: on, colors
// shown, the table drawn and one of them among its visible rows.
func (m prBoardModel) shimmerOn() bool {
	if m.opts.NoShimmer || m.colorless || m.mode != prbTable || !m.haveData || len(m.view) == 0 {
		return false
	}
	start := min(m.scroll, max(len(m.view)-1, 0))
	end, _ := m.visible(start)
	for i := start; i < min(end, len(m.view)); i++ {
		if needsMeShown(m.view[i]) {
			return true
		}
	}
	return false
}

// shimmerFrame is the shimmer's frame for the painter and the cache keys:
// 0 while it does not move, so a board without a needs-you cell on screen
// keeps its cached frames.
func (m prBoardModel) shimmerFrame() int {
	if !m.shimmerOn() {
		return 0
	}
	return m.shimmer
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
		m.loaded = scopeRows(msg.rows, m.opts.Repo)
		m.haveData, m.loadErr, m.refreshedAt, m.facts = true, nil, m.opts.Now(), msg.facts
		var cmd tea.Cmd
		if m.owner != "" && len(ownedBy(m.loaded, m.owner)) == 0 { // its last PR left: back to every owner
			cmd = m.note("owner: all (" + m.owner + " has no pull requests now)")
			m.setOwner("")
		}
		m.rebuild()
		anim := m.startAnim() // before m is returned: it marks the chain started
		return m, tea.Batch(cmd, anim)
	case prbTickMsg:
		cmd := tea.Batch(m.startLoad(), m.follow(), m.tickCmd())
		return m, cmd
	case requestsMsg:
		cmd, settled := m.followed(msg)
		if settled { // the rows show what the daemon did
			cmd = tea.Batch(cmd, m.startLoad())
		}
		return m, cmd
	case tea.ColorProfileMsg:
		m.colorless = msg.Profile <= colorprofile.ASCII
	case prbShimmerMsg:
		m.shimmering = false // Update starts the next frame while one is on screen
		if m.shimmerOn() {
			m.shimmer = (m.shimmer + 1) % m.pal.shimmerCycle()
		}
		return m, nil
	case prbAnimMsg:
		if !m.working {
			m.animating = false // let the frame chain end
			return m, nil
		}
		m.anim++
		return m, m.animCmd()
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

// rebuild keeps the rows of the owner scope, the view and the filter,
// sorts them and keeps the cursor on the same PR when it is still listed.
func (m *prBoardModel) rebuild() {
	m.all = m.loaded
	if m.owner != "" {
		m.all = ownedBy(m.loaded, m.owner)
	}
	m.hidden = 0
	if m.hide {
		before := len(m.all)
		m.all = slices.DeleteFunc(slices.Clone(m.all), hiddenRow)
		m.hidden = before - len(m.all)
	}
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
	m.section = slices.IndexFunc(m.view, func(r PRBoardRow) bool { return r.Recent })
	m.working = slices.ContainsFunc(m.all, func(r PRBoardRow) bool { return workingState(r.State) })
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

// hiddenRow reports a row h hides: one `magnum ignore` muted or the
// configuration skips, but not one Codex flagged, which waits for a review
// by hand.
func hiddenRow(r PRBoardRow) bool {
	switch normState(r.State) {
	case "ignored":
		return true
	case "ineligible":
		return r.CodexFlag == ""
	}
	return false
}

// rowOwner is the user or organization owning the row's repository.
func rowOwner(r PRBoardRow) string {
	owner, _, _ := prRefParts(r)
	return owner
}

// owners are the owners with loaded rows (as the first of them spells
// it): the default repository's owner first, then alphabetically.
func (m prBoardModel) owners() []string {
	home := m.opts.DefaultRepo
	if home == "" {
		home = m.opts.Repo
	}
	home, _, _ = strings.Cut(home, "/")
	var out []string
	for _, r := range m.loaded {
		if o := rowOwner(r); o != "" && !slices.ContainsFunc(out, func(s string) bool { return strings.EqualFold(s, o) }) {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		if ha, hb := strings.EqualFold(a, home), strings.EqualFold(b, home); ha != hb {
			if ha {
				return -1
			}
			return 1
		}
		return cmp.Compare(strings.ToLower(a), strings.ToLower(b))
	})
	return out
}

// ownedBy are the rows of owner (case does not matter); none for "".
func ownedBy(rows []PRBoardRow, owner string) []PRBoardRow {
	if owner == "" {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(rows), func(r PRBoardRow) bool { return !strings.EqualFold(rowOwner(r), owner) })
}

// setOwner changes the owner scope and tells opts.OwnerChanged.
func (m *prBoardModel) setOwner(owner string) {
	m.owner = owner
	if m.opts.OwnerChanged != nil {
		m.opts.OwnerChanged(owner)
	}
}

// nextOwner moves the owner scope on: every owner, then each owner with
// rows (owners), then every owner again.
func (m prBoardModel) nextOwner() (prBoardModel, tea.Cmd) {
	owners := m.owners()
	if len(owners) == 0 {
		cmd := m.note("no owner to show alone")
		return m, cmd
	}
	switch i := slices.IndexFunc(owners, func(o string) bool { return strings.EqualFold(o, m.owner) }); {
	case m.owner == "":
		m.setOwner(owners[0])
	case i < 0 || i == len(owners)-1:
		m.setOwner("")
	default:
		m.setOwner(owners[i+1])
	}
	m.rebuild()
	cmd := m.note("owner: " + cmp.Or(m.owner, "all"))
	return m, cmd
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
	m.keyPressed()
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
	case prbLog:
		return m.logKey(k)
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

// logKey handles a key over the action log: it scrolls, closes or quits.
func (m prBoardModel) logKey(k string) (prBoardModel, tea.Cmd) {
	switch k {
	case "q":
		cmd := m.leave(false)
		return m, cmd
	case logKey, "esc", "enter", "backspace":
		m.mode, m.logScroll = m.logFrom, 0
	default:
		m.logScroll = scrollKey(k, m.logScroll, max(m.bodyHeight()-3, 1))
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
	case logKey:
		m.logFrom, m.mode, m.logScroll = m.mode, prbLog, 0
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
	case "O":
		return m.nextOwner()
	case "h":
		m.hide = !m.hide
		if m.opts.HideToggled != nil {
			m.opts.HideToggled(m.hide)
		}
		m.rebuild()
		msg := "showing ignored and skipped PRs (h hides them)"
		if m.hide {
			msg = fmt.Sprintf("hiding ignored and skipped PRs: %d (h shows them)", m.hidden)
		}
		cmd := m.note(msg)
		return m, cmd
	case "/":
		m.filtering = true
		m.mode = prbTable
		cmd := m.filter.Focus()
		return m, cmd
	case "ctrl+r", "f5":
		cmd := m.startLoad()
		return m, cmd
	case "a":
		return m.run("attention", func(ctx context.Context, a DashboardActions) (ActionResult, error) { return a.Attention(ctx) })
	case "m":
		m.toggleMouse()
		if m.opts.MouseToggled != nil {
			m.opts.MouseToggled(m.mouseOn)
		}
		cmd := m.note(mouseNote(m.mouseOn))
		return m, cmd
	case "W":
		return m.resetWidths()
	default:
		if a, ok := rowActFor(boardActs, k); ok {
			return m.rowAction(a)
		}
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

// run starts an action on no row unless one is already running.
func (m prBoardModel) run(what string, fn actionFunc) (prBoardModel, tea.Cmd) {
	cmd, started := m.start(what, "", fn)
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

// rowAction runs the row action a on the cursor row (rowacts.go): refused
// at once with the reason, asked y/N first, or started.
func (m prBoardModel) rowAction(a rowAct) (prBoardModel, tea.Cmd) {
	cmd, started := m.actionBar.rowAction(a, m.actRow())
	return m.withSpinner(cmd, started)
}

// actRow is what the row actions know of the cursor row; questions name
// its PR as the table does.
func (m prBoardModel) actRow() actRow {
	r, ok := m.selected()
	if !ok {
		return actRow{}
	}
	return boardActRow(r, m.questionLabel(r, prRef(r)), m.opts.Now())
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

// tableHeight is how many table lines fit on screen: rows, and the
// recently closed section's heading when it shows.
func (m prBoardModel) tableHeight() int { return max(m.bodyHeight()-prbTableChrome, 1) }

// visible is the rows the table draws from row start, [start, end), and
// whether the recently closed section's heading is among them: it takes a
// line before row m.section, or the last line with the section's rows
// below (a table of one line keeps it for the row).
func (m prBoardModel) visible(start int) (end int, heading bool) {
	avail := m.tableHeight()
	if s := m.section; s >= start && s < start+avail && s < len(m.view) && avail > 1 {
		return min(start+avail-1, len(m.view)), true
	}
	return min(start+avail, len(m.view)), false
}

// maxScroll is the first row of the last screenful: the smallest start
// from which the table draws every row to the end.
func (m prBoardModel) maxScroll() int {
	s := max(len(m.view)-m.tableHeight(), 0)
	for s < len(m.view)-1 {
		if end, _ := m.visible(s); end >= len(m.view) {
			break
		}
		s++
	}
	return s
}

// fixScroll scrolls the table so the cursor row stays on screen, and
// keeps the card's and the help's scroll within their content.
func (m *prBoardModel) fixScroll() {
	if m.mode == prbHelp {
		n := len(m.painter().helpContent(m.viewWidth()))
		m.helpScroll = min(max(m.helpScroll, 0), max(n-(m.bodyHeight()-2), 0))
	}
	if m.mode == prbLog {
		n := len(m.logContent(m.viewWidth()))
		m.logScroll = min(max(m.logScroll, 0), max(n-(m.bodyHeight()-2), 0))
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
	case m.maxScroll() == 0:
		m.scroll = 0
	case m.cursor < m.scroll:
		m.scroll = m.cursor
	case m.cursor >= m.scroll+avail:
		m.scroll = m.cursor - avail + 1
	}
	for m.scroll < m.cursor { // the heading may push the cursor row off
		if end, _ := m.visible(m.scroll); m.cursor < end {
			break
		}
		m.scroll++
	}
	m.scroll = min(max(m.scroll, 0), m.maxScroll())
}

// View draws the board, or returns the last frame when nothing it shows
// changed (a key storm at the bottom of the list changes nothing).
func (m prBoardModel) View() tea.View {
	return withMouse(newView(m.cache.frameFor(m.frameKey(), m.render)), m.mouseOn)
}

func (m prBoardModel) painter() prbPainter {
	p := newPRBPainter(m.st, m.pal, m.g, m.opts.Now(), m.self, m.all, m.sort, m.desc)
	p.judge, p.frame = m.opts.Judge, m.animFrame()
	p.shimmer, p.colorless = m.shimmerFrame(), m.colorless
	return p
}

func (m prBoardModel) render() string {
	w, h := m.viewWidth(), m.viewHeight()
	p := m.painter()
	clock, spin := m.st.Dim.Render("loading…"), ""
	if m.haveData {
		clock = m.st.Dim.Render(m.g.refresh + " " + m.refreshedAt.Local().Format("15:04:05"))
	}
	if m.loading || m.busy != "" {
		spin = m.spin.View()
	}
	out := []string{
		p.titleLine(w, m.opts.Title, m.opts.Repo, m.owner, m.boardView, m.inView, m.filter.Value(), len(m.view), m.hidden,
			spin, clock, m.facts.list(m.opts.Now())),
		m.cache.summaryFor(m.rowsKey(w), func() string { return p.summaryLine(w) }),
	}

	var body []string
	below := 0
	switch m.mode {
	case prbHelp:
		body, below = p.help(w, m.bodyHeight(), m.helpScroll)
	case prbLog:
		body, below = p.box(m.logContent(w), w, m.bodyHeight(), m.logScroll)
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
	queued := m.log.pendingTargets()
	if !m.haveData {
		if m.loadErr != nil {
			return []string{"", "  " + m.st.Err.Render(truncate("could not load pull requests: "+oneLine(m.loadErr.Error()), w-2))}, 0
		}
		return []string{"", "  " + m.spin.View() + " " + m.st.Dim.Render("loading pull requests…")}, 0
	}
	start := min(m.scroll, max(len(m.view)-1, 0))
	end, heading := m.visible(start)
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
		if heading && i == m.section {
			lines = append(lines, p.recentHeading(w, m.opts.RecentClosed, len(m.view)-m.section))
		}
		sel := i == m.cursor
		key := prbRowKey{index: i, selected: sel}
		if needsMeShown(m.view[i]) {
			key.shimmer = p.shimmer
		}
		lines = append(lines, m.cache.row(rk, key, func() string {
			return p.rowLine(m.view[i], lay, w, sel, queuedFor(prRef(m.view[i]), queued))
		}))
	}
	if heading && m.section == end { // the last line: the section's rows are below
		lines = append(lines, p.recentHeading(w, m.opts.RecentClosed, len(m.view)-m.section))
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
		suffix := m.busySuffix() // the action in flight stays visible
		room := w - 4 - ansi.StringWidth(suffix)
		if below > 0 {
			room -= ansi.StringWidth(fmt.Sprintf(" %s %d more ", p.g.down, below)) + 2
		}
		switch {
		case m.flashErr:
			msg = m.st.Err.Render(m.g.fail + " " + m.flashFit(room-ansi.StringWidth(m.g.fail)-1))
		case m.flashInfo:
			msg = m.st.Dim.Render(m.flashFit(room))
		default:
			msg = m.st.OK.Render(m.g.ok + " " + m.flashFit(room-ansi.StringWidth(m.g.ok)-1))
		}
		msg += m.st.Dim.Render(suffix)
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
	case prbLog:
		left = m.st.hints(w, hint{"j/k", "scroll"}, hint{logKey, "close log"}, hint{"q", "quit"})
	case prbDetail:
		back, help := []hint{{"esc", "back"}}, []hint{{"?", "help"}}
		left = m.st.fitHints(w,
			withHints(back, []hint{{"j/k", "scroll"}}, actionHints(actReview, actFresh, actSimplify, actOpen, actBrowser, actTracker,
				actPin, actUnpin, actMute, actUnmute, actRelease, actAbort, actIgnore), help),
			withHints(back, actionHints(actReview, actFresh, actSimplify, actOpen, actBrowser, actPin, actUnpin, actMute, actUnmute, actRelease), help),
			withHints(back, actionHints(actReview, actOpen, actBrowser), help),
			withHints(back, help))
	default:
		pos := ""
		if len(m.view) > 0 {
			pos = m.st.Dim.Render(fmt.Sprintf("%d/%d", m.cursor+1, len(m.view)))
		}
		if off != "" {
			pos = strings.TrimSpace(off + "  " + pos)
		}
		room := max(w-ansi.StringWidth(pos)-2, 10)
		details := []hint{{"enter", "details"}}
		sets := [][]hint{
			withHints(details, actionHints(actReview, actFresh, actSimplify, actOpen, actBrowser, actTracker, actPin, actUnpin, actRelease),
				[]hint{{"/", "filter"}, {"v", "view"}, {"O", "owner"}, {"s/S", "sort"}, {"tab", "overview"}, {logKey, "log"}, {"?", "help"}, {"q", "quit"}}),
			withHints(details, actionHints(actReview, actFresh, actSimplify, actOpen, actBrowser, actPin, actUnpin, actRelease),
				[]hint{{"/", "filter"}, {"v", "view"}, {"s/S", "sort"}, {"tab", "overview"}, {"?", "help"}, {"q", "quit"}}),
			withHints(details, actionHints(actReview, actOpen, actBrowser),
				[]hint{{"/", "filter"}, {"v", "view"}, {"s", "sort"}, {"tab", "overview"}, {"?", "help"}, {"q", "quit"}}),
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
