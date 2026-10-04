package cleanup

import (
	"fmt"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"
)

// Render prints a plan for the CLI:
//
//	Plan: free ~2.1G disk, 800M MySQL (3 actions)
//	  release          talkable#11700 (merged 14m ago) review5
//	  drop_dbs         slug:review9 (orphan databases ...): 8 databases, 800M MySQL
//	Skipped:
//	  talkable#11701   grace_left (6m left)
func Render(p Plan) string {
	var b strings.Builder
	head := "Plan"
	if p.DryRun {
		head += " (dry run)"
	}
	if len(p.Actions) == 0 {
		fmt.Fprintf(&b, "%s: Nothing to clean up.\n", head)
	} else {
		n := "actions"
		if len(p.Actions) == 1 {
			n = "action"
		}
		fmt.Fprintf(&b, "%s: free ~%s disk, %s MySQL (%d %s)\n", head,
			humanBytes(p.Totals.DiskBytes), humanBytes(p.Totals.MySQLBytes), len(p.Actions), n)
	}
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, a := range p.Actions {
		fmt.Fprintf(w, "  %s\t%s\n", a.Kind, actionLine(a))
	}
	w.Flush()
	if len(p.Skipped) > 0 {
		b.WriteString("Skipped:\n")
		w = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, s := range p.Skipped {
			reason := s.Reason
			if s.Detail != "" {
				reason += " (" + s.Detail + ")"
			}
			fmt.Fprintf(w, "  %s\t%s\n", shortSubject(s.Subject), reason)
		}
		w.Flush()
	}
	if len(p.Warnings) > 0 {
		b.WriteString("Warnings:\n")
		for _, warn := range p.Warnings {
			fmt.Fprintf(&b, "  %s\n", warn)
		}
	}
	return b.String()
}

// actionLine is "<subject> (<why>) <target>: <what it frees>".
func actionLine(a Action) string {
	line := shortSubject(a.Subject)
	if a.Why != "" {
		line += " (" + a.Why + ")"
	}
	if a.Slot != "" && a.Kind != KindDropDBs && a.Subject != "slot:"+a.Slot && !strings.HasSuffix(a.Subject, a.Slot) {
		line += " " + a.Slot
	}
	var detail []string
	switch a.Kind {
	case KindRemoveSlot:
		detail = append(detail, "~"+humanBytes(a.Bytes)+" disk")
		if len(a.DBNames) > 0 {
			detail = append(detail, fmt.Sprintf("%d databases %s", len(a.DBNames), humanBytes(a.DBBytes)))
		}
	case KindRemoveWorktree:
		detail = append(detail, a.Path, "~"+humanBytes(a.Bytes)+" disk")
	case KindDropDBs:
		detail = append(detail, fmt.Sprintf("%d databases, %s MySQL", len(a.DBNames), humanBytes(a.DBBytes)))
	case KindResetExternal:
		detail = append(detail, fmt.Sprintf("switch -C %s origin/%s, databases kept", a.Branch, a.Base))
	}
	if len(detail) > 0 {
		line += ": " + strings.Join(detail, ", ")
	}
	if a.Force {
		line += " [force]"
	}
	if a.Confirm {
		line += " [needs typed confirmation]"
	}
	return line
}

// RenderReport prints the outcome of Apply, one line per action plus a summary.
func RenderReport(r Report) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, res := range r.Results {
		a := res.Action
		line := a.Kind + " " + shortSubject(a.Subject)
		if a.Slot != "" && a.Kind != KindDropDBs && a.Subject != "slot:"+a.Slot && !strings.HasSuffix(a.Subject, a.Slot) {
			line += " " + a.Slot
		}
		switch {
		case res.Error != "" && res.Status == StatusFailed:
			line += ": " + res.Error
		case res.Status == StatusUnconfirmed:
			line += " (needs typed confirmation)"
		}
		fmt.Fprintf(w, "  %s\t%s\n", res.Status, line)
	}
	w.Flush()
	parts := []string{fmt.Sprintf("%d done", r.Done), fmt.Sprintf("%d failed", r.Failed)}
	if r.Unconfirmed > 0 {
		parts = append(parts, fmt.Sprintf("%d unconfirmed", r.Unconfirmed))
	}
	if r.Planned > 0 {
		parts = append(parts, fmt.Sprintf("%d planned", r.Planned))
	}
	fmt.Fprintf(&b, "%s; freed ~%s disk, %s MySQL\n", strings.Join(parts, ", "), humanBytes(r.DiskBytes), humanBytes(r.MySQLBytes))
	return b.String()
}

var prSubjectRe = regexp.MustCompile(`^([^/#\s]+)/([^/#\s]+)#(\d+)$`)

// shortSubject turns owner/name#N into name#N when owner and name are equal
// (talkable/talkable#11700 → talkable#11700).
func shortSubject(s string) string {
	if m := prSubjectRe.FindStringSubmatch(s); m != nil && strings.EqualFold(m[1], m[2]) {
		return m[2] + "#" + m[3]
	}
	return s
}

// humanBytes formats a byte count: 0B, 512B, 2K, 5M, 2.1G.
func humanBytes(n int64) string {
	const k, m, g = 1 << 10, 1 << 20, 1 << 30
	switch {
	case n >= g:
		return fmt.Sprintf("%.1fG", float64(n)/g)
	case n >= m:
		return fmt.Sprintf("%.0fM", float64(n)/m)
	case n >= k:
		return fmt.Sprintf("%.0fK", float64(n)/k)
	case n > 0:
		return fmt.Sprintf("%dB", n)
	}
	return "0B"
}

// humanDuration formats an age or remaining time: <1m, 14m, 2h, 3d.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}
