package agents

import (
	"context"
	"regexp"
	"strings"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/textx"
)

// paneErrorLines is how much of a pane's recent output paneError reads;
// paneErrorMax is the most of the error line it keeps, in runes.
const (
	paneErrorLines = 40
	paneErrorMax   = 200
)

// paneErrorRe matches a CLI's own error line in a pane ("error: the argument
// '--search' cannot be used multiple times", "Error: …", "fatal: …"): it
// starts the line. What a TUI renders of a tool's output (a test's
// "    Error: expected 2 got 3", which the PR's code can steer) is indented
// or boxed, so it never matches.
var paneErrorRe = regexp.MustCompile(`(?i)^(error|fatal|panic)\b`)

// paneError is the last error line of a pane's recent output, for a prompt
// herdr refused because the agent is gone (agent_not_ready after the wait,
// agent_not_found): a CLI that died at its launch printed why in the pane
// (a Codex flag its launch repeated, a config key it rejects), and magnum's
// events saw only herdr's code. Only a line that starts at column 0 counts
// (paneErrorRe); it is redacted and cut to paneErrorMax runes; "" when the
// pane is unknown, the read fails or no line looks like an error (the shell
// prompt is never taken for one).
func (m *Manager) paneError(ctx context.Context, paneID string) string {
	if paneID == "" {
		return ""
	}
	r, err := m.d.Herdr.PaneRead(ctx, paneID, herdr.ReadOptions{Source: herdr.SourceRecent, Lines: paneErrorLines, Format: herdr.FormatText})
	if err != nil {
		return ""
	}
	var last string
	for line := range strings.SplitSeq(r.Text, "\n") {
		if paneErrorRe.MatchString(line) {
			last = strings.TrimSpace(line)
		}
	}
	if last == "" {
		return ""
	}
	return textx.Clip(execx.Redact(last), paneErrorMax)
}
