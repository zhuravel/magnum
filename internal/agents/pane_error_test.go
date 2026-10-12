package agents

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A pane's error line is the one a CLI prints for itself, at the start of
// the line: a TUI renders what a tool printed (which a PR's code can steer)
// indented or boxed, and that text never reaches the round's error, the
// attention reason or the toasts. The line is cut at paneErrorMax runes,
// never in the middle of one.
func TestPaneErrorTakesOnlyALineAtColumnZero(t *testing.T) {
	const indented = "    Error: expected 2 got 3"
	cases := map[string]struct {
		pane string
		want string
	}{
		"the CLI's own line wins over an indented tool line": {
			"$ codex --search\n" + indented + "\nerror: the argument '--search' cannot be used multiple times\n  " + indented + "\n$ ",
			"error: the argument '--search' cannot be used multiple times",
		},
		"only an indented tool line":  {"$ pytest\n" + indented + "\n│ Error: boxed\n$ ", ""},
		"fatal and panic at column 0": {"fatal: not a git repository\n$ ", "fatal: not a git repository"},
		"capitals count":              {"Error: model not found\n$ ", "Error: model not found"},
		"a word that only starts so":  {"errors were found above\nerror_code=2\n$ ", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			ws := e.started()
			pane := ws.Panes[RoleJudge]
			e.h.reads[pane] = tc.pane
			if got := e.m.paneError(e.ctx, pane); got != tc.want {
				t.Fatalf("paneError = %q, want %q", got, tc.want)
			}
		})
	}
}

// The cut is by runes: a line of multibyte characters over the limit ends
// in an ellipsis, paneErrorMax runes long in all, with no rune broken.
func TestPaneErrorCutsALongLineByRunes(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	pane := ws.Panes[RoleJudge]
	e.h.reads[pane] = "error: " + strings.Repeat("ö界", 150) + "\n$ " // 307 runes, 2 and 3 bytes each
	got := e.m.paneError(e.ctx, pane)
	if n := utf8.RuneCountInString(got); n != paneErrorMax || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) || !strings.HasPrefix(got, "error: ö界ö界") {
		t.Fatalf("got %d runes (valid %v): %q", n, utf8.ValidString(got), got)
	}
}
