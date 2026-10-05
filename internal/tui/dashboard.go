package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// StatusData is one snapshot of what the dashboard shows. Table cells are
// display strings the caller formats (refs, folders, sizes); the header
// numbers are formatted by the dashboard.
type StatusData struct {
	Daemon      DaemonInfo
	Activity    ActivityInfo
	GitHub      GitHubInfo
	Rounds      RoundsInfo
	Agents      AgentsInfo
	Disk        DiskInfo
	Pauses      []Pause
	Slots       []SlotRow
	Queue       []PRRow // open PRs, then closed ones pending release
	Attention   []AttentionRow
	Manual      []ManualRow // manual worktrees, shown when toggled on
	Warnings    []string    // sources that could not be read
	GeneratedAt time.Time
}

// DaemonInfo is the daemon process and its launchd job.
type DaemonInfo struct {
	Running bool
	PID     int
	Uptime  string // preformatted ("3h12m"); empty when unknown
	Launchd string // launchd job state ("running", "not loaded")
	// Skew says the daemon runs an older build than the CLI's or the one on
	// disk ("daemon runs v1 since …; v2 is built: `magnum daemon-restart`");
	// "" when it does not.
	Skew string
}

// ActivityInfo is how long ago the daemon last polled GitHub, ticked and
// reconciled; zero (or negative) means never.
type ActivityInfo struct {
	LastPoll, LastTick, LastReconcile time.Duration
}

// GitHubInfo is the GraphQL rate budget; Limit 0 means unknown (no poll yet).
type GitHubInfo struct {
	Remaining, Limit int
	ResetIn          time.Duration
}

// RoundsInfo is the review rounds in progress.
type RoundsInfo struct {
	Active, Max int
	ActivePRs   []string
}

// AgentsInfo counts working agent panes; a non-empty Error ("herdr
// unreachable") replaces the counts.
type AgentsInfo struct {
	CodexWorking, CodexMax, ClaudeWorking int
	Other                                 []KindCount // other agent kinds the roles use, shown after claude
	Error                                 string
}

// KindCount is the number of working panes of one agent kind.
type KindCount struct {
	Kind    string
	Working int
}

// DiskInfo is the free space where slots live; FreeGB <= 0 means unknown.
type DiskInfo struct {
	FreeGB float64
	MinGB  int
}

// Pause is one reason automation (or part of it) is held back.
type Pause struct {
	Key    string // scope: daemon, codex, claude, watch:<owner>, identity:<name>, github
	Reason string // why, with "until" already folded in when it applies
	Fix    string // command or step that lifts it; optional
}

// SlotRow is one review slot. Actions on a slot row target PRRef, or the
// slot Name for pin/unpin/release when it holds no PR.
type SlotRow struct {
	Name, Folder, PRRef, PRState, SlotState, DBs, Disk string
	URL                                                string // PR URL for b; optional (looked up in Queue by PRRef)
	PRGHState                                          string // GitHub's state of the slot's PR: OPEN, CLOSED or MERGED; "" when unknown
	// PRMergedUnreviewed and PRFlagDismissed are PRRow's MergedUnreviewed and
	// FlagDismissed for the slot's PR; read with PRGHState.
	PRMergedUnreviewed, PRFlagDismissed bool
}

// PRRow is one queued (or closing) PR; Ref is what actions receive.
type PRRow struct {
	Ref, Title, Author, State, Next, Age, URL string
	Review                                    *ReviewFacts // for the y/N question before a review; nil when unknown
	GHState                                   string       // GitHub's state: OPEN, CLOSED or MERGED; "" when unknown
	// MergedUnreviewed: GitHub merged the PR before magnum reviewed its last
	// push; FlagDismissed: the PR was muted after that, so it is not flagged
	// (store.IsMergedUnreviewed, store.IsFlagDismissed). M dismisses the one
	// and restores the other.
	MergedUnreviewed, FlagDismissed bool
}

// AttentionRow is something that needs the user.
type AttentionRow struct {
	Subject, Kind, Message string
	Fix                    string // optional
}

// ManualRow is a manual worktree (read-only, not selectable).
type ManualRow struct {
	Folder, Branch, PRRef, GitHub, DBs, Agents, Disk string
}

// DashboardSource supplies the dashboard's data; Gather is called once per
// refresh, never concurrently with itself.
type DashboardSource interface {
	Gather(ctx context.Context) (StatusData, error)
}

// SourceFunc adapts a function to DashboardSource.
type SourceFunc func(ctx context.Context) (StatusData, error)

// Gather calls f.
func (f SourceFunc) Gather(ctx context.Context) (StatusData, error) { return f(ctx) }

// ReviewOpts are the review variants the screens ask for. A forced round
// reviews the head even when it was reviewed already, so there is no
// "again" variant.
type ReviewOpts struct {
	Fresh    bool // new agent sessions instead of resuming
	Simplify bool // run the role aliased simplify this round (magnum review --simplify)
}

// DashboardActions run what the dashboard's keys ask for. ref is a row's
// PR reference (or a slot name, for pin/unpin/release of an empty slot).
// The returned text is shown in the footer for a few seconds (only its
// last non-empty line when it has several); an error is shown instead.
// One action runs at a time; the data refreshes right after it returns.
type DashboardActions interface {
	Open(ctx context.Context, ref string) (string, error)
	Review(ctx context.Context, ref string, opts ReviewOpts) (string, error)
	Pin(ctx context.Context, ref string) (string, error)
	Unpin(ctx context.Context, ref string) (string, error)
	Release(ctx context.Context, ref string) (string, error)
	Mute(ctx context.Context, ref string) (string, error)
	Unmute(ctx context.Context, ref string) (string, error)
	Abort(ctx context.Context, ref string) (string, error)  // kill the PR's running review
	Ignore(ctx context.Context, ref string) (string, error) // abort, mute and free the slot
	// Approve and RequestChanges post the reviewer's own verdict on the head
	// magnum reviewed (magnum approve / request-changes).
	Approve(ctx context.Context, ref string) (string, error)
	RequestChanges(ctx context.Context, ref string) (string, error)
	Attention(ctx context.Context) (string, error)
	OpenBrowser(ctx context.Context, url string) error
}

// DashboardOptions tune the dashboard.
type DashboardOptions struct {
	Refresh    time.Duration    // between Gather calls; default 2s, at least 200ms
	ShowManual bool             // start with the manual worktrees section shown
	Title      string           // default "magnum status"
	Now        func() time.Time // clock for "updated Xs ago"; default time.Now
	Judge      string           // the judge role's name in the help ("open the PR's <Judge> pane"); default "judge"
	Icons      IconMode         // the symbols: unicode (default), nerd (Nerd Font icons and emoji) or ascii
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

// RunDashboard shows the live status dashboard until the user quits or ctx
// ends. act may be nil (the action keys then say so). It returns
// ErrSwitchToBoard when the user pressed tab (or t) for the PR board.
func RunDashboard(ctx context.Context, src DashboardSource, act DashboardActions, opts DashboardOptions) error {
	if src == nil {
		return errors.New("tui: RunDashboard needs a source")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // stops an action still running after the grace (or ctrl+c)
	final, err := runProgram(ctx, newDashboardModel(ctx, src, act, opts))
	if m, ok := final.(dashboardModel); err == nil && ok && m.switching && ctx.Err() == nil {
		return ErrSwitchToBoard
	}
	return err
}

const (
	defaultRefresh = 2 * time.Second
	minRefresh     = 200 * time.Millisecond
	maxPauseLines  = 4
	dashFooter     = 2 // the status line and the key hints
	dashMinBody    = 3 // body lines kept before the header is cut
)

type (
	dashDataMsg struct {
		data StatusData
		err  error
	}
	dashTickMsg struct{}
)

// dashRow is one selectable row: a slot or a queued PR.
type dashRow struct {
	slot *SlotRow
	pr   *PRRow
}

func (r dashRow) key() string {
	if r.slot != nil {
		return "slot:" + r.slot.Name
	}
	return "pr:" + r.pr.Ref
}

// prRef is the PR the row names, "" for an empty slot.
func (r dashRow) prRef() string {
	if r.slot != nil {
		return strings.TrimSpace(r.slot.PRRef)
	}
	return r.pr.Ref
}

type dashboardModel struct {
	actionBar
	mouseState
	src  DashboardSource
	opts DashboardOptions
	st   styles
	pal  prbPalette // the state colors of the marks; follows st
	g    glyphs     // fixed for the model's life, so no cache key names it

	width, height int

	data     StatusData
	haveData bool
	loading  bool
	loadErr  error
	spin     spinner.Model
	spinning bool

	rows   []dashRow
	cursor int
	selKey string
	scroll int

	showManual bool
	showHelp   bool
	helpScroll int

	// widths are the dragged column widths by name ("queue.title"); the
	// map is replaced, never changed, as copies of the model share it.
	// widthsKey spells it for the cache keys.
	widths    map[string]int
	widthsKey string
	saver     *widthSaver // keeps them; nil without opts.Widths

	gen   int64      // stamps data; a new stamp on every refresh
	cache *dashCache // shared by every copy of the model
}

// dashCache is the dashboard's rendering cache (see render_cache.go): the
// frame and the body with no row selected.
type dashCache struct {
	frame  frameCache[dashFrameKey]
	body   partCache[dashBodyKey, struct{}, dashBody]
	header partCache[dashHeaderKey, struct{}, []string]
}

// dashHeaderKey is what the header depends on.
type dashHeaderKey struct {
	gen                     int64
	width                   int
	clock                   int64
	dark, haveData, loading bool
	spin                    string
}

// dashBodyKey is what the body depends on besides the cursor.
type dashBodyKey struct {
	gen              int64
	width            int
	showManual, dark bool
	widths           string // dashboardModel.widthsKey
}

// dashFrameKey is everything a frame of the dashboard shows.
type dashFrameKey struct {
	body                 dashBodyKey
	width, height, clock int64
	cursor, scroll       int
	showHelp             bool
	helpScroll           int
	haveData, loading    bool
	loadErr              string
	spin                 string
	confirm, busy        string
	leaving, switching   bool
	flash                string
	flashErr, flashInfo  bool
	mouseOn              bool
	menu                 ctxMenu
}

// bodyCacheKey is the key of the body at width w.
func (m dashboardModel) bodyCacheKey(w int) dashBodyKey {
	return dashBodyKey{gen: m.gen, width: w, showManual: m.showManual, dark: m.st.dark, widths: m.widthsKey}
}

func (m dashboardModel) frameKey() dashFrameKey {
	k := dashFrameKey{
		body:  m.bodyCacheKey(m.viewWidth()),
		width: int64(m.width), height: int64(m.height), clock: clockKey(m.opts.Now),
		cursor: m.cursor, scroll: m.scroll, showHelp: m.showHelp, helpScroll: m.helpScroll,
		haveData: m.haveData, loading: m.loading, loadErr: errText(m.loadErr), spin: m.spinFrame(),
		busy: m.busy, leaving: m.leaving, switching: m.switching,
		flash: m.flash, flashErr: m.flashErr, flashInfo: m.flashInfo,
		mouseOn: m.mouseOn, menu: m.menu,
	}
	if m.confirm != nil {
		k.confirm = m.confirm.question
	}
	return k
}

func newDashboardModel(ctx context.Context, src DashboardSource, act DashboardActions, opts DashboardOptions) dashboardModel {
	if opts.Refresh <= 0 {
		opts.Refresh = defaultRefresh
	}
	opts.Refresh = max(opts.Refresh, minRefresh)
	if opts.Title == "" {
		opts.Title = "magnum status"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	g := newGlyphs(opts.Icons)
	sp := spinner.New(spinner.WithSpinner(g.spinner), spinner.WithStyle(defaultStyles.Accent))
	m := dashboardModel{
		actionBar: newActionBar(ctx, act), src: src, opts: opts, st: defaultStyles, pal: newPRBPalette(defaultStyles), g: g, spin: sp, cache: &dashCache{},
		showManual: opts.ShowManual,
		loading:    true, spinning: true, // Init starts the first gather and the spinner
		saver: newWidthSaver(opts.Widths, widthsDashboard),
	}
	m.mouseOn = !opts.NoMouse
	return m
}

func (m dashboardModel) Init() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor, m.gatherCmd(), m.spin.Tick, m.tickCmd(), m.saver.load(m.ctx))
}

func (m dashboardModel) gatherCmd() tea.Cmd {
	ctx, src := m.ctx, m.src
	return func() tea.Msg {
		ctx, cancel := loadContext(ctx)
		defer cancel()
		d, err := src.Gather(ctx)
		return dashDataMsg{data: d, err: err}
	}
}

func (m dashboardModel) tickCmd() tea.Cmd {
	return tea.Tick(m.opts.Refresh, func(time.Time) tea.Msg { return dashTickMsg{} })
}

// startLoad begins a gather unless one is in flight.
func (m *dashboardModel) startLoad() tea.Cmd {
	if m.loading {
		return nil
	}
	m.loading = true
	cmds := []tea.Cmd{m.gatherCmd()}
	if !m.spinning {
		m.spinning = true
		cmds = append(cmds, m.spin.Tick)
	}
	return tea.Batch(cmds...)
}

func (m dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	next.fixScroll()
	return next, cmd
}

func (m dashboardModel) update(msg tea.Msg) (dashboardModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.BackgroundColorMsg:
		m.st = newStyles(msg.IsDark())
		m.pal = newPRBPalette(m.st)
		m.spin.Style = m.st.Accent
	case dashDataMsg:
		m.loading = false
		if msg.err != nil {
			m.loadErr = msg.err
			return m, nil
		}
		m.data, m.haveData, m.loadErr = sanitizeStatus(msg.data), true, nil
		m.rebuildRows()
	case dashTickMsg:
		return m, tea.Batch(m.startLoad(), m.tickCmd())
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
		return m, tea.Batch(cmd, m.startLoad())
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
	}
	return m, nil
}

func (m dashboardModel) updateKey(msg tea.KeyPressMsg) (dashboardModel, tea.Cmd) {
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
	if m.showHelp {
		switch k {
		case "q":
			cmd := m.leave(false)
			return m, cmd
		case "?", "esc", "enter":
			m.showHelp, m.helpScroll = false, 0
		default:
			m.helpScroll = scrollKey(k, m.helpScroll, max(m.bodyHeight()-3, 1))
		}
		return m, nil
	}
	return m.listKey(k)
}

// listKey handles a key on the slots and the queue: moving, the action
// keys (on the selected row) and the screen's own keys.
func (m dashboardModel) listKey(k string) (dashboardModel, tea.Cmd) {
	half := max(m.bodyHeight()/2, 1)
	switch k {
	case "q", "esc":
		cmd := m.leave(false)
		return m, cmd
	case "tab", "t":
		cmd := m.leave(true)
		return m, cmd
	case "j", "down":
		m.moveOrScroll(1)
	case "k", "up":
		m.moveOrScroll(-1)
	case "home":
		m.moveTo(0)
		m.scroll = 0
	case "end":
		m.moveTo(len(m.rows) - 1)
		m.scroll = 1 << 20 // fixScroll shows what follows the last row
	case "pgdown":
		m.moveOrScroll(half)
	case "pgup":
		m.moveOrScroll(-half)
	case "?":
		m.showHelp, m.helpScroll = true, 0
	case "w":
		m.showManual = !m.showManual
	case "m":
		m.toggleMouse()
		if m.opts.MouseToggled != nil {
			m.opts.MouseToggled(m.mouseOn)
		}
		cmd := m.note(mouseNote(m.mouseOn))
		return m, cmd
	case "W":
		return m.resetWidths()
	case "g", "ctrl+r", "f5":
		cmd := m.startLoad()
		return m, cmd
	case "a":
		return m.run("attention", func(ctx context.Context, a DashboardActions) (string, error) { return a.Attention(ctx) })
	case "enter":
		return m.rowAction(actOpen)
	default:
		if a, ok := rowActFor(dashActs, k); ok {
			return m.rowAction(a)
		}
	}
	return m, nil
}

// rowAction runs the row action a on the selected row (rowacts.go):
// refused at once with the reason, asked y/N first, or started.
func (m dashboardModel) rowAction(a rowAct) (dashboardModel, tea.Cmd) {
	cmd, started := m.actionBar.rowAction(a, m.actRow())
	return m.withSpinner(cmd, started)
}

// actRow is what the row actions know of the selected row: its PR's state,
// GitHub state and heads (a slot row reads its PR's queue row), and its slot.
// The dashboard lists every slot, so a PR no slot row holds has none; a
// slot pinned is the PR's pin too, while a slot that is not pinned says
// nothing of a PR pinned elsewhere. An empty slot row offers pin, unpin and
// release of the slot.
func (m dashboardModel) actRow() actRow {
	r, ok := m.selected()
	if !ok {
		return actRow{}
	}
	ref := r.prRef()
	row := actRow{ok: true, ref: ref, label: ref, state: m.rowState(r), url: m.urlOf(r), facts: m.reviewFacts(r), slotKnown: true}
	gh, flagged, dismissed := m.mergeFacts(r)
	row.ghState, row.mergedUnreviewed, row.flagDismissed = strings.ToUpper(strings.TrimSpace(gh)), flagged, dismissed
	if f := m.reviewHeads(r); f != nil {
		row.head, row.reviewed = f.HeadSHA, f.ReviewedSHA
	}
	if s := m.slotRow(r); s != nil {
		row.inSlot = true
		row.pinned = strings.Contains(s.SlotState, "pinned")
		row.pinKnown = row.pinned || ref == ""
	}
	if ref == "" {
		row.slot, row.label, row.none = r.slot.Name, "slot "+r.slot.Name, "slot "+r.slot.Name+" holds no PR"
	}
	if row.ignored() {
		row.muted, row.mutedKnown = true, true
	}
	return row
}

// slotRow is the slot row of r: r itself, or the slot holding its PR; nil
// when no slot holds it.
func (m dashboardModel) slotRow(r dashRow) *SlotRow {
	if r.slot != nil {
		return r.slot
	}
	for i := range m.data.Slots {
		if s := &m.data.Slots[i]; r.pr.Ref != "" && strings.EqualFold(s.PRRef, r.pr.Ref) {
			return s
		}
	}
	return nil
}

// reviewHeads is the review facts of the row's PR (heads, last review); a
// slot row reads its queue row. nil when unknown.
func (m dashboardModel) reviewHeads(r dashRow) *ReviewFacts {
	if r.pr != nil {
		return r.pr.Review
	}
	if q, ok := m.queueRow(r.prRef()); ok {
		return q.Review
	}
	return nil
}

// run starts an action unless one is already running.
func (m dashboardModel) run(what string, fn actionFunc) (dashboardModel, tea.Cmd) {
	cmd, started := m.start(what, fn)
	return m.withSpinner(cmd, started)
}

// fail shows an error in the footer.
func (m dashboardModel) fail(text string) (dashboardModel, tea.Cmd) {
	cmd := m.setFlash(text, true)
	return m, cmd
}

// withSpinner adds the spinner to an action that started.
func (m dashboardModel) withSpinner(cmd tea.Cmd, started bool) (dashboardModel, tea.Cmd) {
	if started && !m.spinning {
		m.spinning = true
		cmd = tea.Batch(cmd, m.spin.Tick)
	}
	return m, cmd
}

// reviewFacts is what the question says about the row's PR; a slot row
// reads its PR's queue row, which carries the heads and what is next.
func (m dashboardModel) reviewFacts(r dashRow) string {
	now := m.opts.Now()
	if r.pr != nil {
		return reviewFacts(r.pr.State, r.pr.Next, r.pr.Review, now)
	}
	if q, ok := m.queueRow(r.prRef()); ok {
		return reviewFacts(q.State, q.Next, q.Review, now)
	}
	return stateReviewFacts(r.slot.PRState, "")
}

// mergeFacts is GitHub's state of the row's PR with whether it is flagged
// merged unreviewed and whether that flag is dismissed; a slot row without
// its own state reads them from its PR's queue row.
func (m dashboardModel) mergeFacts(r dashRow) (gh string, flagged, dismissed bool) {
	if r.pr != nil {
		return r.pr.GHState, r.pr.MergedUnreviewed, r.pr.FlagDismissed
	}
	if r.slot.PRGHState != "" {
		return r.slot.PRGHState, r.slot.PRMergedUnreviewed, r.slot.PRFlagDismissed
	}
	if q, ok := m.queueRow(r.prRef()); ok {
		return q.GHState, q.MergedUnreviewed, q.FlagDismissed
	}
	return "", false, false
}

// ghStateMerged reports whether a GitHub PR state (case aside) is MERGED.
func ghStateMerged(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "MERGED")
}

// queueRow is the queue's row for ref.
func (m dashboardModel) queueRow(ref string) (PRRow, bool) {
	if ref == "" {
		return PRRow{}, false
	}
	for _, q := range m.data.Queue {
		if strings.EqualFold(q.Ref, ref) {
			return q, true
		}
	}
	return PRRow{}, false
}

// urlOf is the row's PR URL; a slot row without one borrows its queue row's.
func (m dashboardModel) urlOf(r dashRow) string {
	if r.pr != nil {
		return r.pr.URL
	}
	if r.slot.URL != "" {
		return r.slot.URL
	}
	if q, ok := m.queueRow(r.prRef()); ok {
		return q.URL
	}
	return ""
}

func (m dashboardModel) selected() (dashRow, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return dashRow{}, false
	}
	return m.rows[m.cursor], true
}

// moveOrScroll moves the cursor n rows. Past the last row (or with no
// rows) it scrolls the body instead, so the sections under the queue
// (attention, manual worktrees, warnings) can be read; back up, it
// scrolls until the cursor row shows before moving it.
func (m *dashboardModel) moveOrScroll(n int) {
	_, cur := m.bodySize(m.viewWidth())
	switch {
	case n > 0 && m.cursor >= len(m.rows)-1:
		m.scroll += n
	case n < 0 && (cur < 0 || cur < m.scroll):
		m.scroll = max(m.scroll+n, 0)
	default:
		m.moveTo(m.cursor + n)
	}
}

func (m *dashboardModel) moveTo(i int) {
	if len(m.rows) == 0 {
		m.cursor, m.selKey = 0, ""
		return
	}
	m.cursor = min(max(i, 0), len(m.rows)-1)
	m.selKey = m.rows[m.cursor].key()
}

// rebuildRows lists the selectable rows of fresh data and keeps the cursor
// on the same slot or PR when it is still there.
func (m *dashboardModel) rebuildRows() {
	m.gen = nextGen()
	m.rows = make([]dashRow, 0, len(m.data.Slots)+len(m.data.Queue))
	for i := range m.data.Slots {
		m.rows = append(m.rows, dashRow{slot: &m.data.Slots[i]})
	}
	for i := range m.data.Queue {
		m.rows = append(m.rows, dashRow{pr: &m.data.Queue[i]})
	}
	for i, r := range m.rows {
		if m.selKey != "" && r.key() == m.selKey {
			m.cursor = i
			return
		}
	}
	m.moveTo(m.cursor)
}

// fixScroll scrolls the body so the cursor row stays on screen, unless
// the user scrolled past the last row (or there are no rows), and keeps
// the help's scroll within its content.
func (m *dashboardModel) fixScroll() {
	avail := m.bodyHeight()
	if m.showHelp {
		room := max(avail-2, 1) // the help box's border
		m.helpScroll = min(max(m.helpScroll, 0), max(len(m.helpContent())-room, 0))
		return
	}
	n, cur := m.bodySize(m.viewWidth())
	switch {
	case cur < 0:
	case m.cursor == len(m.rows)-1 && cur < m.scroll:
		// scrolled past the last row to read what follows it
	case cur < m.scroll:
		m.scroll = max(cur-min(2, avail-1), 0) // keep the section heading in view
	case cur >= m.scroll+avail:
		m.scroll = cur - avail + 1
	}
	m.scroll = min(max(m.scroll, 0), max(n-avail, 0))
}

func (m dashboardModel) viewWidth() int {
	if m.width > 0 {
		return m.width
	}
	return 80
}

func (m dashboardModel) viewHeight() int {
	if m.height > 0 {
		return m.height
	}
	return 24
}

// header is the header cut to leave room for the footer (the status line
// asks the y/N) and dashMinBody body lines: pauses can make it long. The
// lines are shared with the cache: clone them before changing them.
func (m dashboardModel) header(w int) []string {
	k := dashHeaderKey{gen: m.gen, width: w, clock: clockKey(m.opts.Now), dark: m.st.dark, haveData: m.haveData, loading: m.loading, spin: m.spinFrame()}
	lines := m.cache.headerFor(k, func() []string { return m.headerLines(w) })
	return lines[:min(len(lines), max(m.viewHeight()-dashFooter-dashMinBody, 1))]
}

// spinFrame is the spinner as drawn while it shows (loading, an action
// running, no data yet), else "": drawing its frame costs, and keys only
// need it when it is on screen.
func (m dashboardModel) spinFrame() string {
	if m.loading || m.busy != "" || !m.haveData {
		return m.spin.View()
	}
	return ""
}

// bodyHeight is the room between the header and the footer.
func (m dashboardModel) bodyHeight() int {
	return max(m.viewHeight()-len(m.header(m.viewWidth()))-dashFooter, 1)
}

// View draws the dashboard, or returns the last frame when nothing it
// shows changed.
func (m dashboardModel) View() tea.View {
	return withMouse(newView(m.cache.frameFor(m.frameKey(), m.render)), m.mouseOn)
}

func (m dashboardModel) render() string {
	w, h := m.viewWidth(), m.viewHeight()
	header := slices.Clone(m.header(w)) // cached lines: render changes the first
	avail := m.bodyHeight()
	var body []string
	var pos string
	if m.showHelp {
		body, pos = m.helpLines(w, avail)
	} else {
		lines, _ := m.bodyLines(w)
		start := min(m.scroll, max(len(lines)-1, 0))
		end := min(start+avail, len(lines))
		body = lines[start:end]
		if len(lines) > avail {
			pos = fmt.Sprintf(" · lines %d-%d of %d", start+1, end, len(lines))
		}
	}
	if pos != "" {
		header[0] = truncate(header[0]+m.st.Dim.Render(pos), w)
	}
	status := m.statusLine(w)
	out := append(header, body...)
	out = append(out, status, m.hintLine(w))
	if len(out) > h { // a tiny screen keeps the footer: it asks the y/N
		if h == 1 && status != "" {
			return status
		}
		out = out[len(out)-h:]
	}
	if m.menu.open {
		out = m.drawMenu(out, w, h)
	}
	return strings.Join(out, "\n")
}

func (m dashboardModel) headerLines(w int) []string {
	title := m.st.Title.Render(m.opts.Title)
	if m.haveData && !m.data.GeneratedAt.IsZero() {
		title += m.st.Dim.Render(" · updated " + HumanAgo(max(m.opts.Now().Sub(m.data.GeneratedAt), time.Nanosecond)))
	}
	if m.loading {
		title += " " + m.spin.View()
	}
	lines := []string{truncate(title, w)}
	if !m.haveData {
		return lines
	}
	d := m.data
	label := func(s string) string { return m.st.Label.Render(fmt.Sprintf("%-10s", s)) }

	dot := func(st lipgloss.Style) string {
		if m.g.markDot == "" {
			return ""
		}
		return st.Render(m.g.markDot)
	}
	daemon := marked(dot(m.st.Err), m.st.Err.Render("not running"))
	if d.Daemon.Running {
		daemon = marked(dot(m.st.OK), m.st.OK.Render("running")) + fmt.Sprintf(" (pid %d", d.Daemon.PID)
		if d.Daemon.Uptime != "" {
			daemon += ", up " + d.Daemon.Uptime
		}
		daemon += ")"
	}
	if d.Daemon.Launchd != "" {
		daemon += " · launchd " + d.Daemon.Launchd
	}
	lines = append(lines, label("daemon:")+daemon)

	act := "last poll " + HumanAgo(d.Activity.LastPoll) + " · last tick " + HumanAgo(d.Activity.LastTick)
	if d.Activity.LastReconcile > 0 {
		act += " · last reconcile " + HumanAgo(d.Activity.LastReconcile)
	}
	lines = append(lines, label("activity:")+act)

	gh := m.st.Dim.Render("rate budget unknown (no poll yet)")
	if d.GitHub.Limit > 0 {
		gh = fmt.Sprintf("%d/%d points left", d.GitHub.Remaining, d.GitHub.Limit)
		if d.GitHub.Remaining*10 < d.GitHub.Limit {
			gh = m.st.Warn.Render(gh)
		}
		if d.GitHub.ResetIn > 0 {
			gh += " · resets in " + HumanDuration(d.GitHub.ResetIn)
		}
	}
	lines = append(lines, label("github:")+gh)

	rounds := fmt.Sprintf("%d/%d active", d.Rounds.Active, d.Rounds.Max)
	if len(d.Rounds.ActivePRs) > 0 {
		rounds += " (" + strings.Join(d.Rounds.ActivePRs, ", ") + ")"
	}
	if d.Agents.Error != "" {
		rounds += " · agents: " + m.st.Warn.Render(d.Agents.Error)
	} else {
		rounds += fmt.Sprintf(" · agents: codex %d/%d, claude %d", d.Agents.CodexWorking, d.Agents.CodexMax, d.Agents.ClaudeWorking)
		for _, k := range d.Agents.Other {
			rounds += fmt.Sprintf(", %s %d", k.Kind, k.Working)
		}
		rounds += " working"
	}
	lines = append(lines, label("rounds:")+rounds)

	disk := m.st.Dim.Render("unknown")
	if d.Disk.FreeGB > 0 {
		disk = fmt.Sprintf("%.1f GB free (min %d GB)", d.Disk.FreeGB, d.Disk.MinGB)
		if d.Disk.MinGB > 0 && d.Disk.FreeGB < float64(d.Disk.MinGB) {
			disk = m.st.Warn.Render(disk + "  LOW: provisioning refused")
		}
	}
	lines = append(lines, label("disk:")+disk)

	if len(d.Pauses) == 0 {
		lines = append(lines, label("pauses:")+m.st.Dim.Render("none"))
	} else {
		for i, p := range d.Pauses {
			if i == maxPauseLines {
				lines = append(lines, m.st.Warn.Render(fmt.Sprintf("          … %d more pauses", len(d.Pauses)-i)))
				break
			}
			lead := "          "
			if i == 0 {
				lead = label("pauses:")
			}
			line := marked(stateMark(m.g, m.pal, "paused"), m.st.Warn.Render("PAUSED "+p.Key+": ")) + p.Reason
			if p.Fix != "" {
				line += m.st.Dim.Render(" · fix: " + p.Fix)
			}
			lines = append(lines, lead+line)
		}
	}
	for i := range lines {
		lines[i] = truncate(lines[i], w)
	}
	return append(lines, "")
}

var (
	slotCols   = []column{{title: "SLOT", min: 6, tail: true}, {title: "FOLDER", min: 10, flex: true}, {title: "PR", min: 8}, {title: "STATE", min: 6, flex: true}, {title: "DATABASES", min: 4}, {title: "DISK", min: 4}}
	queueCols  = []column{{title: "PR", min: 8, tail: true}, {title: "STATE", min: 6}, {title: "NEXT", min: 6, flex: true}, {title: "AGE", min: 3}, {title: "AUTHOR", min: 4}, {title: "TITLE", min: 8, flex: true}}
	manualCols = []column{{title: "FOLDER", min: 10, flex: true}, {title: "BRANCH", min: 8, flex: true}, {title: "PR", min: 4}, {title: "GITHUB", min: 4}, {title: "DATABASES", min: 4}, {title: "AGENTS", min: 4, flex: true}, {title: "DISK", min: 4}}
)

// bodyLines renders the scrollable part and reports the cursor's line (-1
// when no row is selectable).
func (m dashboardModel) bodyLines(w int) ([]string, int) {
	if !m.haveData {
		if m.loadErr != nil {
			return []string{m.st.Err.Render(truncate("could not load status: "+oneLine(m.loadErr.Error()), w))}, -1
		}
		return []string{m.spin.View() + " loading status…"}, -1
	}
	b := m.body(w)
	if m.cursor < 0 || m.cursor >= len(b.rowAt) {
		return b.lines, -1
	}
	at := b.rowAt[m.cursor]
	lines := slices.Clone(b.lines)
	lines[at] = truncate(cursorMark+m.st.Selected.Render(b.rowText[m.cursor]), w)
	return lines, at
}

// bodySize is how many lines the body has and the cursor's line (-1 when
// no row is selectable), without drawing the cursor row.
func (m dashboardModel) bodySize(w int) (n, cur int) {
	if !m.haveData {
		return 1, -1
	}
	b := m.body(w)
	if m.cursor < 0 || m.cursor >= len(b.rowAt) {
		return len(b.lines), -1
	}
	return len(b.lines), b.rowAt[m.cursor]
}

// body is the body with no row selected, from the cache.
func (m dashboardModel) body(w int) dashBody {
	return m.cache.bodyFor(m.bodyCacheKey(w), func() dashBody { return m.drawBody(w) })
}

// dashBody is the dashboard's body with no row selected, and where each
// selectable row is, so moving the cursor draws one row, not the body.
type dashBody struct {
	lines   []string
	rowAt   []int    // the line of each selectable row
	rowText []string // each selectable row's cells, for drawing it selected
	tables  []dashTable
}

// dashTable is where a table's heading line is and how wide its columns
// are, for the mouse.
type dashTable struct {
	name   string // slots, queue, manual
	line   int
	widths []int
}

// drawBody draws the body of fresh data with no row selected.
func (m dashboardModel) drawBody(w int) dashBody {
	var out []string
	var b dashBody
	section := func(title string, n int) {
		out = append(out, m.st.Section.Render(fmt.Sprintf("%s (%d)", m.g.headed(title), n)))
	}
	addRows := func(table string, cols []column, cells [][]string) {
		widths := layoutColumnsFixed(cols, cells, w, len(cursorMark), m.fixedWidths(table, cols))
		b.tables = append(b.tables, dashTable{name: table, line: len(out), widths: widths})
		out = append(out, noMark+m.st.headerRow(cols, widths))
		for _, c := range cells {
			line := renderRowCols(cols, c, widths)
			// The selected row draws its text plain in the selection's
			// style: a mark's color reset would end the highlight.
			b.rowAt, b.rowText = append(b.rowAt, len(out)), append(b.rowText, ansi.Strip(line))
			out = append(out, noMark+line)
		}
	}

	d := m.data
	section("SLOTS", len(d.Slots))
	if len(d.Slots) == 0 {
		out = append(out, m.st.Dim.Render("  none (`magnum slots provision` creates the pool)"))
	} else {
		cells := make([][]string, len(d.Slots))
		for i, s := range d.Slots {
			pr := "-"
			if s.PRRef != "" {
				pr = strings.TrimSpace(s.PRRef + " " + m.stateText(s.PRState))
			}
			state := orDash(s.SlotState)
			if f := strings.Fields(s.SlotState); len(f) > 0 { // "busy [pinned]"
				state = marked(slotMark(m.g, m.pal, f[0]), state)
			}
			cells[i] = []string{s.Name, orDash(s.Folder), pr, state, orDash(s.DBs), orDash(s.Disk)}
		}
		addRows(dashSlots, slotCols, cells)
	}

	out = append(out, "")
	section("QUEUE", len(d.Queue))
	if len(d.Queue) == 0 {
		out = append(out, m.st.Dim.Render("  empty"))
	} else {
		cells := make([][]string, len(d.Queue))
		for i, q := range d.Queue {
			cells[i] = []string{q.Ref, orDash(m.stateText(q.State)), orDash(q.Next), orDash(q.Age), orDash(q.Author), oneLine(q.Title)}
		}
		addRows(dashQueue, queueCols, cells)
	}

	if len(d.Attention) > 0 {
		out = append(out, "")
		section("ATTENTION", len(d.Attention))
		for _, a := range d.Attention {
			subj := a.Subject
			if a.Kind != "" {
				subj += " [" + a.Kind + "]"
			}
			out = append(out, truncate(noMark+marked(stateMark(m.g, m.pal, "needs_attention"), m.st.Warn.Render(subj))+": "+oneLine(a.Message), w))
			if a.Fix != "" {
				out = append(out, truncate(noMark+m.st.Dim.Render("  fix: "+a.Fix), w))
			}
		}
	}

	if len(d.Manual) > 0 {
		out = append(out, "")
		if !m.showManual {
			out = append(out, m.st.Dim.Render(fmt.Sprintf("%s (%d hidden, w shows them)", m.g.headed("MANUAL WORKTREES"), len(d.Manual))))
		} else {
			section("MANUAL WORKTREES", len(d.Manual))
			cells := make([][]string, len(d.Manual))
			for i, x := range d.Manual {
				cells[i] = []string{orDash(x.Folder), orDash(x.Branch), orDash(x.PRRef), orDash(x.GitHub), orDash(x.DBs), orDash(x.Agents), orDash(x.Disk)}
			}
			widths := layoutColumnsFixed(manualCols, cells, w, len(noMark), m.fixedWidths(dashManual, manualCols))
			b.tables = append(b.tables, dashTable{name: dashManual, line: len(out), widths: widths})
			out = append(out, noMark+m.st.headerRow(manualCols, widths))
			for _, c := range cells {
				out = append(out, noMark+renderRow(c, widths))
			}
		}
	}

	for _, warn := range d.Warnings {
		out = append(out, m.st.Warn.Render("warning: ")+warn)
	}
	for i := range out { // narrow screens: column minimums can add up past w
		out[i] = truncate(out[i], w)
	}
	b.lines = out
	return b
}

// stateText is a PR state behind its mark, when the mode has one: the
// mark's color is the cell's only styling (the selected row draws its
// text plain, see addRows).
func (m dashboardModel) stateText(state string) string {
	if strings.TrimSpace(state) == "" {
		return state
	}
	return marked(stateMark(m.g, m.pal, state), state)
}

func (m dashboardModel) statusLine(w int) string {
	busy := m.busyText()
	switch {
	case m.confirm != nil:
		return m.st.confirmLine(m.confirm.question, w)
	case m.leaving:
		return truncate(m.spin.View()+" "+m.st.Warn.Render(busy), w)
	case m.flash != "":
		line := m.st.OK.Render(m.flash)
		switch {
		case m.flashErr:
			line = m.st.Err.Render(m.flash)
		case m.flashInfo:
			line = m.st.Dim.Render(m.flash)
		}
		line += m.st.Dim.Render(m.busySuffix()) // the action in flight stays visible
		return truncate(line, w)
	case busy != "":
		return truncate(m.spin.View()+" "+busy, w)
	case m.loadErr != nil && m.haveData:
		msg := "refresh failed: " + oneLine(m.loadErr.Error())
		if !m.data.GeneratedAt.IsZero() {
			msg += " (showing data from " + m.data.GeneratedAt.Local().Format("15:04:05") + ")"
		}
		return truncate(m.st.Err.Render(msg), w)
	}
	return ""
}

func (m dashboardModel) hintLine(w int) string {
	off := ""
	if !m.mouseOn {
		off = m.st.Dim.Render("mouse off")
	}
	if m.showHelp {
		return spread(m.st.hints(w, hint{"j/k", "scroll"}, hint{"?", "close help"}, hint{"q", "quit"}), off, w)
	}
	room := w
	if off != "" {
		room = max(w-ansi.StringWidth(off)-2, 10)
	}
	open, attention := []hint{{"enter", "open"}}, []hint{{"a", "attention"}}
	return spread(m.st.fitHints(room,
		withHints([]hint{{"j/k", "move"}}, open, actionHints(actReview, actPin, actUnpin, actRelease), attention,
			actionHints(actBrowser), []hint{{"w", "manual"}, {"tab", "PRs"}, {"?", "help"}, {"q", "quit"}}),
		withHints(open, actionHints(actReview, actRelease), attention, []hint{{"tab", "PRs"}, {"?", "help"}, {"q", "quit"}}),
		[]hint{{"tab", "PRs"}, {"?", "help"}, {"q", "quit"}},
		[]hint{{"?", "help"}, {"q", "quit"}}), off, w)
}

// dashboardKeys is the help overlay's content; judge names the judge role.
func dashboardKeys(judge string) []hint {
	return []hint{
		{"j/k ↑/↓", "move (slots, then queue)"},
		{"pgup/pgdn", "move half a page"},
		{"enter, o", "open the PR's " + judgeName(judge) + " pane"},
		{"r", "review now (asks y/N)"},
		{"R", "fresh review in new agent sessions (asks y/N)"},
		{"i", "review with /simplify (asks y/N)"},
		{"y", "answer yes; any other key, enter too, cancels"},
		{"p / u", "pin / unpin (the slot when it holds no PR)"},
		{"x", "release (asks y/N)"},
		{"M / U", "mute / unmute the PR (asks y/N)"},
		{"M", "on a merged PR: dismiss / restore its merged-unreviewed flag (asks y/N)"},
		{"K", "kill the PR's running review, or drop its queued one (asks y/N)"},
		{"I", "ignore the PR: kill its review, mute it, free its slot (asks y/N)"},
		{"a", "jump to the pane that needs attention"},
		{"b", "open the PR in the browser"},
		{"w", "show or hide manual worktrees"},
		{"W", "reset the column widths"},
		{"g, ctrl+r", "refresh now (F5 too)"},
		{"tab, t", "switch to the PR board"},
		{"?", "close this help"},
		{"q, esc", "quit"},
	}
}

// judgeName is the judge role's name for help lines ("judge" when unknown).
func judgeName(s string) string {
	if s == "" {
		return "judge"
	}
	return s
}

// helpContent is the help's lines: the title, then one line per key.
func (m dashboardModel) helpContent() []string {
	lines := []string{m.st.Title.Render("Keys")}
	for _, h := range dashboardKeys(m.opts.Judge) {
		lines = append(lines, m.st.Key.Render(fmt.Sprintf("%-11s", h.key))+" "+h.desc)
	}
	lines = append(lines, "", m.st.Title.Render("Mouse"))
	for _, h := range mouseHelp(false, "opens the pane") {
		lines = append(lines, m.st.Key.Render(fmt.Sprintf("%-16s", h.key))+" "+h.desc)
	}
	lines = append(lines, strings.Split(m.st.Dim.Width(64).Render(mouseSelectNote), "\n")...)
	return append(lines, m.legend(64)...)
}

// legend explains the marks before PR and slot states (nerd mode); the
// other modes mark nothing.
func (m dashboardModel) legend(width int) []string {
	if !m.g.rich {
		return nil
	}
	var prs, slots []string
	for _, s := range prStateOrder {
		if mk := stateMark(m.g, m.pal, s); mk != "" {
			prs = append(prs, mk+" "+stateLabel(s))
		}
	}
	for _, s := range dashSlotStateOrder {
		if mk := slotMark(m.g, m.pal, s); mk != "" {
			slots = append(slots, mk+" "+strings.ReplaceAll(s, "_", " "))
		}
	}
	lines := []string{"", m.st.Title.Render("Legend"), m.st.Label.Render("PRs")}
	lines = append(lines, flow(prs, "   ", width)...)
	lines = append(lines, m.st.Label.Render("Slots"))
	return append(lines, flow(slots, "   ", width)...)
}

// dashSlotStateOrder lists the slot states for the legend, healthy first.
var dashSlotStateOrder = []string{
	"free", "claimed", "busy", "held", "provisioning", "releasing", "dirty_schema", "broken", "lost", "observed", "removing", "removed",
}

// helpLines is the help box in height lines, its content scrolled by
// helpScroll; pos says which lines show when not all of them fit.
func (m dashboardModel) helpLines(w, height int) (lines []string, pos string) {
	content := m.helpContent()
	room := max(height-2, 1) // the box's border
	start := min(m.helpScroll, max(len(content)-room, 0))
	end := min(start+room, len(content))
	if end-start < len(content) {
		pos = fmt.Sprintf(" · help lines %d-%d of %d", start+1, end, len(content))
	}
	box := m.st.Box
	if w > 4 {
		box = box.MaxWidth(w)
	}
	return strings.Split(box.Render(strings.Join(content[start:end], "\n")), "\n"), pos
}

// sanitizeStatus is d with its text safe to draw, as the board does with
// its rows: pause reasons, attention messages and warnings may quote git,
// GitHub or agent output carrying escape sequences or control characters.
// d's slices are copied, not changed.
func sanitizeStatus(d StatusData) StatusData {
	d.Daemon.Uptime, d.Daemon.Launchd = cleanText(d.Daemon.Uptime), cleanText(d.Daemon.Launchd)
	d.Rounds.ActivePRs = cleanAll(d.Rounds.ActivePRs)
	d.Agents.Error = cleanText(d.Agents.Error)
	d.Agents.Other = cleanEach(d.Agents.Other, func(k *KindCount) { k.Kind = cleanText(k.Kind) })
	d.Pauses = cleanEach(d.Pauses, func(p *Pause) {
		p.Key, p.Reason, p.Fix = cleanText(p.Key), cleanText(p.Reason), cleanText(p.Fix)
	})
	d.Slots = cleanEach(d.Slots, func(r *SlotRow) {
		r.Name, r.Folder, r.PRRef, r.PRState = cleanText(r.Name), cleanText(r.Folder), cleanText(r.PRRef), cleanText(r.PRState)
		r.SlotState, r.DBs, r.Disk, r.URL = cleanText(r.SlotState), cleanText(r.DBs), cleanText(r.Disk), cleanText(r.URL)
	})
	d.Queue = cleanEach(d.Queue, func(r *PRRow) {
		r.Ref, r.Title, r.Author, r.State = cleanText(r.Ref), cleanText(r.Title), cleanText(r.Author), cleanText(r.State)
		r.Next, r.Age, r.URL = cleanText(r.Next), cleanText(r.Age), cleanText(r.URL)
	})
	d.Attention = cleanEach(d.Attention, func(a *AttentionRow) {
		a.Subject, a.Kind, a.Message, a.Fix = cleanText(a.Subject), cleanText(a.Kind), cleanText(a.Message), cleanText(a.Fix)
	})
	d.Manual = cleanEach(d.Manual, func(r *ManualRow) {
		r.Folder, r.Branch, r.PRRef, r.GitHub = cleanText(r.Folder), cleanText(r.Branch), cleanText(r.PRRef), cleanText(r.GitHub)
		r.DBs, r.Agents, r.Disk = cleanText(r.DBs), cleanText(r.Agents), cleanText(r.Disk)
	})
	d.Warnings = cleanAll(d.Warnings)
	return d
}

// cleanEach is a copy of xs with clean applied to each element.
func cleanEach[T any](xs []T, clean func(*T)) []T {
	xs = slices.Clone(xs)
	for i := range xs {
		clean(&xs[i])
	}
	return xs
}

// oneLine folds whitespace (newlines included) into single spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// lastLine is the last non-empty line of s, folded to one line.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := oneLine(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
