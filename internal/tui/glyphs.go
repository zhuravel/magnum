package tui

import (
	"strings"

	"charm.land/bubbles/v2/spinner"
)

// IconMode is the set of symbols the PR board and the status dashboard
// draw ([terminal] icons). A screen cannot tell which font the terminal
// uses, so the configuration decides; nothing is detected.
type IconMode string

const (
	// IconsUnicode draws Unicode symbols (✔ ✗ 💬 📌) that any font has: the
	// default, also for an empty or unknown mode.
	IconsUnicode IconMode = "unicode"
	// IconsNerd draws Nerd Font icons and emoji: a colored marker before
	// every state, verdict and finding priority, an icon in every state
	// pill and before the section headings. Needs a Nerd Font.
	IconsNerd IconMode = "nerd"
	// IconsASCII draws plain ASCII ("+", "x", "pin").
	IconsASCII IconMode = "ascii"
)

// normalized is m in lower case, IconsUnicode when empty or unknown.
func (m IconMode) normalized() IconMode {
	switch m := IconMode(strings.ToLower(strings.TrimSpace(string(m)))); m {
	case IconsNerd, IconsASCII:
		return m
	}
	return IconsUnicode
}

// glyphs are the symbols the screens draw in one IconMode.
//
// Widths: layout measures with ansi.StringWidth. Emoji are drawn without
// U+FE0F (terminals disagree on its width) and count two cells, as
// terminals draw them. Nerd Font icons sit in the Private Use Area and
// count one cell; most Nerd Fonts draw them one cell wide, some wider, so
// an icon is always followed by a space (gap where text would touch it):
// a wider icon covers that space and never shifts a column.
type glyphs struct {
	mode                                                    IconMode
	approved, changes, commented, pending, dismissed, other string
	nonBlocking, simplify, times                            string
	ciPass, ciFail, ciPending, ciMissing, ciSkip            string // a check's state: the CI column and card
	stale, mine, pin, errMark, fail, ok                     string
	bot                                                     string // before a bot's login in the narrow columns; "" = keep "[bot]"
	cursor, dash, minus, atLeast, rule, up, down, dot       string
	sortDesc, sortAsc, refresh, sep, chipSep                string
	gap                                                     string // after an icon that text follows directly
	pinAccent                                               bool   // the pin is a word or a monochrome icon: color it
	// rich marks what the other modes leave as text: states, slot states,
	// finding priorities, the card's decision and section headings.
	rich      bool
	stateIcon map[string]string // in a board state pill, before its label
	stateMark map[string]string // before a PR state elsewhere (summary, dashboard, legend)
	slotMark  map[string]string // before a slot state (dashboard)
	priority  [4]string         // before P0..P3
	ciMark    map[string]string // before a check state on the card
	heading   map[string]string // before a section heading (card, dashboard)
	running   [2]string         // before the daemon's state: stopped, running (dashboard)
	spinner   spinner.Spinner
}

func newGlyphs(mode IconMode) glyphs {
	switch mode.normalized() {
	case IconsASCII:
		return glyphs{
			mode:     IconsASCII,
			approved: "+", changes: "x", commented: "c", pending: "?", dismissed: "-", other: "*",
			stale: "~", mine: "*", pin: "pin", errMark: "!", fail: "x", ok: "+",
			nonBlocking: "~", simplify: "s", times: "x",
			ciPass: "+", ciFail: "x", ciPending: "o", ciMissing: "-", ciSkip: "/",
			cursor: ">", dash: "-", minus: "-", atLeast: ">=", rule: "-", up: "^", down: "v", dot: "",
			sortDesc: "v", sortAsc: "^", refresh: "at", sep: " | ", chipSep: ":",
			pinAccent: true,
			spinner:   spinner.Line,
		}
	case IconsNerd:
		return glyphs{
			mode: IconsNerd,
			// The review and check states are gh-dash's icons (v4.26
			// internal/tui/constants), so both boards read alike.
			approved:    "\U000F012C", // nf-md-check: gh-dash ApprovedIcon
			changes:     "\ueb43",     // codicon request-changes: gh-dash ChangesRequestedIcon
			commented:   "\uf27b",     // nf-fa-commenting_o: gh-dash CommentIcon
			pending:     "\ue641",     // gh-dash WaitingIcon
			dismissed:   "\uf468",     // nf-oct-circle_slash
			other:       "\uf444",     // nf-oct-dot_fill
			nonBlocking: "\uf27b",     // a comment verdict: as commented
			times:       "×",
			ciPass:      "\uf058",     // nf-fa-check_circle: gh-dash SuccessIcon
			ciFail:      "\U000F0159", // nf-md-close_circle: gh-dash FailureIcon
			ciPending:   "\ue641",     // gh-dash WaitingIcon
			ciMissing:   "\uf48b",     // nf-oct-dash
			ciSkip:      "\uf517",     // nf-oct-skip
			simplify:    "\uf0c4",     // nf-fa-scissors
			stale:       "\uf464",     // nf-oct-history
			mine:        "\uf51f",     // nf-oct-star_fill
			bot:         "\U000F06A9", // nf-md-robot
			pin:         "\uf435",     // nf-oct-pin
			errMark:     "\uf530",     // nf-oct-x_circle_fill
			fail:        "\U000F0159", // gh-dash FailureIcon
			ok:          "\uf058",     // gh-dash SuccessIcon
			cursor:      "▌", dash: "—", minus: "−", atLeast: "≥", rule: "─", up: "▲", down: "▼", dot: "●",
			sortDesc: "\uf51a", // nf-oct-sort_desc
			sortAsc:  "\uf519", // nf-oct-sort_asc
			refresh:  "\uf43a", // nf-oct-clock
			sep:      " · ", gap: " ", pinAccent: true, rich: true,
			stateIcon: map[string]string{
				"needs_attention":  "\uf421", // nf-oct-alert
				"paused":           "\uf04c", // nf-fa-pause
				"claiming":         "\uf441", // nf-oct-eye
				"reviewing":        "\uf441",
				"verifying":        "\uf441",
				"rereview_pending": "\uf46a", // nf-oct-sync
				"queued":           "\uf4e3", // nf-oct-hourglass
				"reviewed":         "\uf42e", // nf-oct-check
				"baseline":         "\uf4c5", // nf-oct-eye_closed
				"ineligible":       "\uf517", // nf-oct-skip
				"ignored":          "\uf466", // nf-oct-mute
				"closed":           "\uf4dc", // nf-oct-git_pull_request_closed
				"releasing":        "\uf52a", // nf-oct-unlock
				"released":         "\uf52a",
			},
			stateMark: map[string]string{
				"needs_attention": "🔴", "paused": "🛑",
				"claiming": "🔵", "reviewing": "🔵", "verifying": "🔵",
				"rereview_pending": "🟡", "queued": "🟡",
				"reviewed": "🟢", "baseline": "⚪", "ignored": "🚫",
				"ineligible": "⚫", "closed": "⚫", "releasing": "⚫", "released": "⚫",
			},
			slotMark: map[string]string{
				"free": "🟢", "claimed": "🔵", "busy": "🔵", "held": "🟣",
				"provisioning": "🟡", "releasing": "🟡", "dirty_schema": "🟠",
				"broken": "🔴", "lost": "🔴", "observed": "⚪", "removing": "⚫", "removed": "⚫",
			},
			priority: [4]string{"🔥", "🔴", "🟠", "⚪"},
			running:  [2]string{"🔴", "🟢"},
			heading: map[string]string{
				"WAITING":          "\uf4e3", // nf-oct-hourglass
				"SINCE REVIEW":     "\uf417", // nf-oct-git_commit
				"LAST REVIEW":      "\uf4af", // nf-oct-code_review
				"FINDINGS":         "\uf46f", // nf-oct-bug
				"CI":               "\uf52e", // nf-oct-workflow
				"NOT REVIEWED":     "\uf4c5", // nf-oct-eye_closed
				"SKIPPED":          "\uf517", // nf-oct-skip
				"LAST ROUND":       "\uf520", // nf-oct-stopwatch
				"REVIEWERS":        "\uf4fd", // nf-oct-people
				"NEEDS YOU":        "\uf421", // nf-oct-alert
				"LAST ERROR":       "\uf530", // nf-oct-x_circle_fill
				"ACTIONS":          "\uf427", // nf-oct-rocket
				"SLOTS":            "\uf413", // nf-oct-file_directory
				"QUEUE":            "\uf407", // nf-oct-git_pull_request
				"ATTENTION":        "\uf421", // nf-oct-alert
				"MANUAL WORKTREES": "\uf425", // nf-oct-tools
			},
			spinner: spinner.MiniDot,
		}
	}
	return glyphs{
		mode:     IconsUnicode,
		approved: "✔", changes: "✗", commented: "💬", pending: "◌", dismissed: "⊘", other: "•",
		stale: "⟳", mine: "★", bot: "🤖", pin: "📌", errMark: "!", fail: "✗", ok: "✔",
		nonBlocking: "●", simplify: "✂", times: "×",
		ciPass: "✓", ciFail: "✗", ciPending: "◌", ciMissing: "–", ciSkip: "⊘",
		cursor: "▌", dash: "—", minus: "−", atLeast: "≥", rule: "─", up: "▲", down: "▼", dot: "●",
		sortDesc: "↓", sortAsc: "↑", refresh: "↻", sep: " · ",
		spinner: spinner.MiniDot,
	}
}

// marked is text behind mark and a space; text alone when the mode has no
// mark there ("").
func marked(mark, text string) string {
	if mark == "" {
		return text
	}
	return mark + " " + text
}

// headed is a section heading behind its icon, when the mode has one.
func (g glyphs) headed(title string) string { return marked(g.heading[title], title) }
