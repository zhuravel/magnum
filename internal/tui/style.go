package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// styles are the named looks the screens share. They use the 16 basic ANSI
// colors only, so every terminal renders them, and come in a light and a
// dark variant: each model starts dark and switches when the terminal
// reports its background (tea.BackgroundColorMsg).
type styles struct {
	dark     bool
	Title    lipgloss.Style // screen title
	Section  lipgloss.Style // section headings (SLOTS, QUEUE, ...)
	Label    lipgloss.Style // "daemon:" style labels
	Header   lipgloss.Style // table column headings
	Dim      lipgloss.Style
	Accent   lipgloss.Style
	OK       lipgloss.Style
	Warn     lipgloss.Style
	Err      lipgloss.Style
	Selected lipgloss.Style // the cursor row
	Key      lipgloss.Style // a key in a hint
	Box      lipgloss.Style // bordered panels (help, preview)
}

// newStyles builds the palette for a dark or a light background.
func newStyles(dark bool) styles {
	pick := lipgloss.LightDark(dark)
	accent := pick(lipgloss.Blue, lipgloss.BrightBlue)
	dim := pick(lipgloss.BrightBlack, lipgloss.White)
	return styles{
		dark:     dark,
		Title:    lipgloss.NewStyle().Bold(true).Foreground(accent),
		Section:  lipgloss.NewStyle().Bold(true).Foreground(pick(lipgloss.Cyan, lipgloss.BrightCyan)),
		Label:    lipgloss.NewStyle().Bold(true),
		Header:   lipgloss.NewStyle().Bold(true).Foreground(dim),
		Dim:      lipgloss.NewStyle().Foreground(dim),
		Accent:   lipgloss.NewStyle().Foreground(accent),
		OK:       lipgloss.NewStyle().Foreground(pick(lipgloss.Green, lipgloss.BrightGreen)),
		Warn:     lipgloss.NewStyle().Bold(true).Foreground(pick(lipgloss.Yellow, lipgloss.BrightYellow)),
		Err:      lipgloss.NewStyle().Bold(true).Foreground(pick(lipgloss.Red, lipgloss.BrightRed)),
		Selected: lipgloss.NewStyle().Reverse(true),
		Key:      lipgloss.NewStyle().Bold(true).Foreground(accent),
		Box:      lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(dim).Padding(0, 1),
	}
}

// defaultStyles is what a model uses until the terminal reports its
// background; most terminals are dark.
var defaultStyles = newStyles(true)

// cursorMark prefixes the selected row so the selection shows without color.
const (
	cursorMark = "› "
	noMark     = "  "
)

// hint is one key and what it does, for footers and help overlays.
type hint struct{ key, desc string }

// hints joins hints as "key desc · key desc", cut to width (0 = no limit).
func (st styles) hints(width int, hs ...hint) string {
	parts := make([]string, 0, len(hs))
	for _, h := range hs {
		parts = append(parts, st.Key.Render(h.key)+" "+st.Dim.Render(h.desc))
	}
	return truncate(strings.Join(parts, st.Dim.Render(" · ")), width)
}

// truncate cuts s (which may hold ANSI styling) to width cells with an
// ellipsis; width <= 0 leaves it alone.
func truncate(s string, width int) string {
	if width <= 0 || ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

// truncateTail cuts s to width cells from the left, keeping its end behind
// an ellipsis.
func truncateTail(s string, width int) string {
	if width <= 0 || ansi.StringWidth(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return "…" + strings.TrimLeft(ansi.TruncateLeft(s, ansi.StringWidth(s)-width+1, ""), " ")
}

// fitTail is fit for a column that keeps the end of its cells.
func fitTail(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = truncateTail(s, width)
	if pad := width - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// renderRowCols is renderRow honouring each column's tail flag.
func renderRowCols(cols []column, cells []string, widths []int) string {
	var b strings.Builder
	for i, wd := range widths {
		cell := ""
		if i < len(cells) {
			cell = cells[i]
		}
		if i > 0 {
			b.WriteString(colGap)
		}
		tail := i < len(cols) && cols[i].tail
		switch {
		case tail && i == len(widths)-1:
			b.WriteString(truncateTail(cell, wd))
		case tail:
			b.WriteString(fitTail(cell, wd))
		case i == len(widths)-1:
			b.WriteString(truncate(cell, wd))
		default:
			b.WriteString(fit(cell, wd))
		}
	}
	return b.String()
}

// fit truncates s to width cells and pads it with spaces to exactly width.
func fit(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = truncate(s, width)
	if pad := width - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// orDash returns "-" for an empty cell.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// column is one table column: its heading, the narrowest it may get and
// whether it gives up width first when the table is too wide.
type column struct {
	title string
	min   int
	flex  bool
	// tail keeps the END of a cell that does not fit (an ellipsis replaces
	// the start): PR references such as owner/long-repo-name#1234 must keep
	// their number, not their owner.
	tail bool
}

// colGap separates table columns.
const colGap = "  "

// layoutColumns sizes columns to their content, then shrinks them (flex
// columns first, widest first) until the row fits width (0 = no limit).
// reserve is the width taken by a row prefix such as the cursor mark.
func layoutColumns(cols []column, rows [][]string, width, reserve int) []int {
	return layoutColumnsFixed(cols, rows, width, reserve, nil)
}

// layoutColumnsFixed is layoutColumns with the widths the user dragged:
// fixed[i] > 0 replaces column i's content width. A too wide row shrinks
// the flex columns first, then the other automatic ones, and only then
// the dragged ones (down to minColWidth).
func layoutColumnsFixed(cols []column, rows [][]string, width, reserve int, fixed []int) []int {
	user := func(i int) bool { return i < len(fixed) && fixed[i] > 0 }
	w := make([]int, len(cols))
	for i, c := range cols {
		if user(i) {
			w[i] = max(fixed[i], minColWidth)
			continue
		}
		w[i] = ansi.StringWidth(c.title)
		for _, r := range rows {
			if i < len(r) {
				w[i] = max(w[i], ansi.StringWidth(r[i]))
			}
		}
	}
	if width <= 0 {
		return w
	}
	total := reserve + len(colGap)*(len(cols)-1)
	for _, x := range w {
		total += x
	}
	for pass := range 3 { // flex columns, then the other automatic ones, then the dragged ones
		for total > width {
			best := -1
			for i, c := range cols {
				floor := max(c.min, 1)
				switch {
				case user(i) != (pass == 2), pass == 0 && !c.flex:
					continue
				case user(i):
					floor = minColWidth
				}
				if w[i] <= floor {
					continue
				}
				if best < 0 || w[i] > w[best] {
					best = i
				}
			}
			if best < 0 {
				break
			}
			w[best]--
			total--
		}
	}
	return w
}

// colAtWidths is the column at x on a heading line whose first column
// starts at start and whose columns are widths wide, colGap apart: i is
// its index, -1 for none; gap is true when x is on the gap right of
// column i (give or take sepGrab cells), which a drag resizes it from.
func colAtWidths(widths []int, start, x int) (i int, gap bool) {
	for i, w := range widths {
		end := start + w
		if i < len(widths)-1 && x >= end-sepGrab && x < end+len(colGap)+sepGrab {
			return i, true
		}
		if x >= start && x < end {
			return i, false
		}
		start = end + len(colGap)
	}
	return -1, false
}

// colStart is where column i starts on a line whose first column starts
// at start.
func colStart(widths []int, start, i int) int {
	for _, w := range widths[:min(i, len(widths))] {
		start += w + len(colGap)
	}
	return start
}

// renderRow lays cells out at the given widths; the last column is not
// padded so lines carry no trailing spaces.
func renderRow(cells []string, widths []int) string {
	var b strings.Builder
	for i, wd := range widths {
		cell := ""
		if i < len(cells) {
			cell = cells[i]
		}
		if i > 0 {
			b.WriteString(colGap)
		}
		if i == len(widths)-1 {
			b.WriteString(truncate(cell, wd))
		} else {
			b.WriteString(fit(cell, wd))
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// headerRow renders column headings at the given widths.
func (st styles) headerRow(cols []column, widths []int) string {
	titles := make([]string, len(cols))
	for i, c := range cols {
		titles[i] = c.title
	}
	return st.Header.Render(renderRow(titles, widths))
}

// wrapHints lays hints out over as many lines as width needs.
func (st styles) wrapHints(width int, hs ...hint) []string {
	var lines []string
	var cur []hint
	for _, h := range hs {
		next := append(append([]hint(nil), cur...), h)
		if len(cur) > 0 && width > 0 && ansi.StringWidth(st.hints(0, next...)) > width {
			lines = append(lines, st.hints(width, cur...))
			cur = []hint{h}
			continue
		}
		cur = next
	}
	if len(cur) > 0 {
		lines = append(lines, st.hints(width, cur...))
	}
	return lines
}

// fitHints renders the first hint set that fits width on one line, else
// the last set cut to width; sets go from fullest to shortest.
func (st styles) fitHints(width int, sets ...[]hint) string {
	for _, hs := range sets {
		if s := st.hints(0, hs...); width <= 0 || ansi.StringWidth(s) <= width {
			return s
		}
	}
	return st.hints(width, sets[len(sets)-1]...)
}
