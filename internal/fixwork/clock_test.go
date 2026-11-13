package fixwork

import (
	"testing"
	"time"
)

func TestWaitClock(t *testing.T) {
	now := time.Date(2026, 10, 5, 14, 0, 0, 0, time.Local)
	for at, want := range map[time.Time]string{
		now.Add(9 * time.Minute):                        "14:09",
		time.Date(2026, 10, 6, 0, 0, 0, 0, time.Local):  "00:00",
		time.Date(2026, 10, 7, 9, 30, 0, 0, time.Local): "Wed 09:30",
		time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local): "Oct 20 09:00",
		now.Add(-2 * 24 * time.Hour):                    "Oct 3 14:00",
	} {
		if got := WaitClock(at, now); got != want {
			t.Errorf("WaitClock(%s) = %q, want %q", at, got, want)
		}
	}
}
