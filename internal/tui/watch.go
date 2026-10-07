package tui

import (
	"context"
	"errors"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// WatchFrame is one snapshot of the pane being mirrored.
type WatchFrame struct {
	Title  string // e.g. "talkable#11920"
	Role   string // the session's role name (config.Role.Name), e.g. codex-judge
	Pane   string // herdr pane id
	Status string // agent status as the caller reports it ("working", "idle", ...)
	Text   string // pane contents
	ANSI   bool   // Text carries ANSI styling to pass through; false strips escape sequences
}

// WatchOptions tunes RunWatch.
type WatchOptions struct {
	Interval time.Duration    // between fetches; default 2s, minimum 200ms
	Now      func() time.Time // clock for the header age; default time.Now
	// Tick schedules the next fetch; default tea.Tick.
	Tick func(time.Duration, func(time.Time) tea.Msg) tea.Cmd
}

// WatchFetch reads the current frame. It must honor ctx, which has a
// deadline (five intervals, at least 10s): a hung fetch then shows as an
// error instead of freezing the frame. An error shows in the header and
// the last good frame stays, unless it was wrapped with StopWatch, which
// ends the screen.
type WatchFetch func(ctx context.Context) (WatchFrame, error)

const (
	watchDefaultInterval = 2 * time.Second
	watchMinInterval     = 200 * time.Millisecond
	watchMinFetchTimeout = 10 * time.Second
	watchChrome          = 2 // header and footer lines around the viewport
)

// stopWatchError marks a fetch error that ends the watch.
type stopWatchError struct{ err error }

func (e *stopWatchError) Error() string { return e.err.Error() }
func (e *stopWatchError) Unwrap() error { return e.err }

// StopWatch wraps err so that RunWatch ends and returns err (for "the pane
// closed" or "herdr is gone"). StopWatch(nil) is nil.
func StopWatch(err error) error {
	if err == nil {
		return nil
	}
	return &stopWatchError{err: err}
}

// RunWatch mirrors fetch's frames full-screen, read-only, until the user
// quits (q, esc or ctrl+c), ctx ends (both return nil) or a fetch returns a
// StopWatch error (RunWatch returns the wrapped error).
func RunWatch(ctx context.Context, fetch WatchFetch, opts WatchOptions) error {
	if fetch == nil {
		return errors.New("tui: RunWatch needs a fetch function")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	final, err := runProgram(ctx, newWatchModel(ctx, fetch, opts))
	if err != nil {
		return err
	}
	if m, ok := final.(watchModel); ok {
		return m.stopError()
	}
	return nil
}

// watchResultMsg carries one fetch's outcome.
type watchResultMsg struct {
	frame WatchFrame
	err   error
}

// watchTickMsg asks for the next fetch once the interval has passed.
type watchTickMsg struct{}

// watchModel is the Bubble Tea model of the watch screen. Exactly one fetch
// is in flight at a time: a result schedules a tick, a tick starts a fetch.
type watchModel struct {
	ctx      context.Context
	fetch    WatchFetch
	interval time.Duration
	now      func() time.Time
	tick     func(time.Duration, func(time.Time) tea.Msg) tea.Cmd

	vp            viewport.Model
	width, height int
	follow        bool
	st            styles

	frame    WatchFrame
	have     bool      // a frame arrived
	lastOK   time.Time // when the last successful fetch finished
	content  string    // what the viewport holds
	fetchErr error     // last failed fetch, cleared by the next success
	stopped  error     // set by a StopWatch error
}

func newWatchModel(ctx context.Context, fetch WatchFetch, opts WatchOptions) watchModel {
	if ctx == nil {
		ctx = context.Background()
	}
	interval := opts.Interval
	switch {
	case interval <= 0:
		interval = watchDefaultInterval
	case interval < watchMinInterval:
		interval = watchMinInterval
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	tick := opts.Tick
	if tick == nil {
		tick = tea.Tick
	}
	m := watchModel{ctx: ctx, fetch: fetch, interval: interval, now: now, tick: tick, follow: true, st: defaultStyles, vp: viewport.New()}
	m.resize(80, 24)
	return m
}

// stopError is the error a StopWatch fetch ended the watch with.
func (m watchModel) stopError() error { return m.stopped }

// Init implements tea.Model.
func (m watchModel) Init() tea.Cmd { return tea.Batch(tea.RequestBackgroundColor, m.fetchCmd()) }

func (m watchModel) fetchCmd() tea.Cmd {
	ctx, fetch, limit := m.ctx, m.fetch, max(5*m.interval, watchMinFetchTimeout)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, limit)
		defer cancel()
		f, err := fetch(ctx)
		return watchResultMsg{frame: f, err: err}
	}
}

func (m watchModel) tickCmd() tea.Cmd {
	return m.tick(m.interval, func(time.Time) tea.Msg { return watchTickMsg{} })
}

// resize fits the viewport between the header and the footer.
func (m *watchModel) resize(w, h int) {
	m.width, m.height = w, h
	m.vp.SetWidth(w)
	m.vp.SetHeight(max(h-watchChrome, 1))
	m.settle()
}

// settle keeps the viewport at the bottom when following, else just inside
// the content.
func (m *watchModel) settle() {
	if m.follow {
		m.vp.GotoBottom()
		return
	}
	m.vp.SetYOffset(m.vp.YOffset())
}

// Update implements tea.Model.
func (m watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
	case tea.BackgroundColorMsg:
		m.st = newStyles(msg.IsDark())
	case tea.KeyPressMsg:
		return m.updateKey(msg)
	case watchTickMsg:
		return m, m.fetchCmd()
	case watchResultMsg:
		if m.ctx.Err() != nil {
			return m, tea.Quit
		}
		var stop *stopWatchError
		if errors.As(msg.err, &stop) {
			m.stopped = stop.err
			return m, tea.Quit
		}
		if msg.err != nil {
			m.fetchErr = msg.err // the last good frame stays
			return m, m.tickCmd()
		}
		m.fetchErr = nil
		m.have, m.frame, m.lastOK = true, msg.frame, m.now()
		if content := watchContent(msg.frame); content != m.content {
			m.content = content
			m.vp.SetContent(content)
		}
		m.settle()
		return m, m.tickCmd()
	}
	return m, nil
}

func (m watchModel) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "ctrl+c":
		return m, tea.Quit
	case "j", "down":
		m.vp.ScrollDown(1)
	case "pgdown":
		m.vp.PageDown()
	case "k", "up":
		m.follow = false
		m.vp.ScrollUp(1)
	case "pgup":
		m.follow = false
		m.vp.PageUp()
	case "g", "home":
		m.follow = false
		m.vp.GotoTop()
	case "G", "end":
		m.follow = true
		m.vp.GotoBottom()
	case "f":
		m.follow = !m.follow
		if m.follow {
			m.vp.GotoBottom()
		}
	}
	return m, nil
}

// watchContent is the text the viewport shows for f: styling stripped
// unless f.ANSI, and a final reset so styling cannot leak past the pane.
func watchContent(f WatchFrame) string {
	text := f.Text
	if !f.ANSI {
		text = ansi.Strip(text)
	}
	text = strings.ReplaceAll(text, "\r", "")
	text = strings.ReplaceAll(text, "\t", "    ")
	text = strings.TrimRight(text, "\n")
	if f.ANSI {
		text += "\x1b[0m"
	}
	return text
}

// View implements tea.Model.
func (m watchModel) View() tea.View { return newView(m.render()) }

func (m watchModel) render() string {
	body := m.vp.View()
	if m.frame.ANSI {
		body += "\x1b[0m"
	}
	footer := m.st.hints(m.width,
		hint{"j/k", "scroll"}, hint{"pgup/pgdn", "page"}, hint{"g/G", "top/bottom"},
		hint{"f", "follow"}, hint{"q", "quit"})
	return m.header() + "\n" + body + "\n" + footer
}

// header is the one-line title bar. The age, the follow state and a fetch
// failure are kept whole; the identity part gives up width first.
func (m watchModel) header() string {
	var ident []string
	age := "waiting…"
	if m.have {
		if m.frame.Title != "" {
			ident = append(ident, m.st.Title.Render(m.frame.Title))
		}
		if m.frame.Role != "" {
			ident = append(ident, m.frame.Role)
		}
		if m.frame.Pane != "" {
			ident = append(ident, "pane "+m.frame.Pane)
		}
		if m.frame.Status != "" {
			ident = append(ident, m.frame.Status)
		}
		age = "updated " + HumanDuration(m.now().Sub(m.lastOK))
	}
	sep := m.st.Dim.Render(" · ")
	state := m.st.OK.Render("follow")
	if !m.follow {
		state = m.st.Warn.Render("paused (f follows)")
	}
	tail := age + sep + state
	if m.fetchErr != nil {
		msg := "fetch failed: " + strings.Join(strings.Fields(m.fetchErr.Error()), " ")
		if m.width > 0 {
			// Under pressure the age goes before the failure notice does.
			if ansi.StringWidth(tail)+watchMinIdent+2*3+watchMinErr > m.width {
				tail = state
			}
			msg = truncate(msg, max(m.width-ansi.StringWidth(tail)-3-watchMinIdent, watchMinErr))
		}
		tail += sep + m.st.Err.Render(msg)
	}
	return watchJoinHeader(m.st, strings.Join(ident, sep), tail, m.width)
}

// watchMinIdent and watchMinErr are the widths the identity part and the
// fetch failure notice keep when they compete ("fetch failed:" fits).
const (
	watchMinIdent = 12
	watchMinErr   = 14
)

// watchJoinHeader puts tail after head, cutting head (not tail) to fit width.
func watchJoinHeader(st styles, head, tail string, width int) string {
	if head == "" {
		return truncate(tail, width)
	}
	sep := st.Dim.Render(" · ")
	if width <= 0 {
		return head + sep + tail
	}
	room := width - ansi.StringWidth(tail) - ansi.StringWidth(sep)
	if room < 1 {
		return truncate(tail, width)
	}
	return truncate(head, room) + sep + tail
}
