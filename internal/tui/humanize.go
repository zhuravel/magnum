package tui

import (
	"fmt"
	"time"
)

// HumanBytes renders a byte count the way the plain-text status does:
// "0", "812K", "34M", "1.3G".
func HumanBytes(n int64) string {
	const k = 1024
	switch {
	case n <= 0:
		return "0"
	case n < k*k:
		return fmt.Sprintf("%dK", (n+k-1)/k)
	case n < k*k*k:
		return fmt.Sprintf("%dM", (n+k*k-1)/(k*k))
	}
	return fmt.Sprintf("%.1fG", float64(n)/float64(k*k*k))
}

// HumanDuration renders a duration compactly: "42s", "7m", "3h12m", "2d4h".
// A negative duration renders as its absolute value.
func HumanDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		h := int(d.Hours())
		if m := int(d.Minutes()) % 60; m != 0 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	}
	days := int(d.Hours()) / 24
	if h := int(d.Hours()) % 24; h != 0 {
		return fmt.Sprintf("%dd%dh", days, h)
	}
	return fmt.Sprintf("%dd", days)
}

// HumanAgo renders how long ago something happened: "12s ago". Zero or a
// negative duration means it never happened and renders as "never".
func HumanAgo(d time.Duration) string {
	if d <= 0 {
		return "never"
	}
	return HumanDuration(d) + " ago"
}
