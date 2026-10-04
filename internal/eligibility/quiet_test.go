package eligibility

import (
	"strings"
	"testing"
	"time"
)

func at(h, m, s int) time.Time { return time.Date(2026, 1, 14, h, m, s, 0, time.UTC) }

func TestQuietHours(t *testing.T) {
	tests := []struct {
		name string
		spec string
		now  time.Time
		want bool
	}{
		{"empty spec never quiet", "", at(3, 0, 0), false},
		{"blank spec never quiet", "   ", at(3, 0, 0), false},

		// same-day window, start inclusive and end exclusive
		{"day window: before start", "01:00-07:00", at(0, 59, 59), false},
		{"day window: at start", "01:00-07:00", at(1, 0, 0), true},
		{"day window: inside", "01:00-07:00", at(3, 0, 0), true},
		{"day window: last second", "01:00-07:00", at(6, 59, 59), true},
		{"day window: at end", "01:00-07:00", at(7, 0, 0), false},
		{"day window: afternoon", "01:00-07:00", at(15, 0, 0), false},
		{"day window: midnight", "01:00-07:00", at(0, 0, 0), false},
		{"day window: before midnight", "01:00-07:00", at(23, 59, 59), false},
		{"daytime window", "09:00-17:30", at(17, 29, 59), true},
		{"daytime window end", "09:00-17:30", at(17, 30, 0), false},

		// window wrapping past midnight
		{"wrap: before start", "22:00-06:00", at(21, 59, 59), false},
		{"wrap: at start", "22:00-06:00", at(22, 0, 0), true},
		{"wrap: late evening", "22:00-06:00", at(23, 59, 59), true},
		{"wrap: midnight", "22:00-06:00", at(0, 0, 0), true},
		{"wrap: early morning", "22:00-06:00", at(5, 59, 59), true},
		{"wrap: at end", "22:00-06:00", at(6, 0, 0), false},
		{"wrap: midday", "22:00-06:00", at(12, 0, 0), false},
		{"wrap with minutes: before start", "23:30-00:30", at(23, 29, 0), false},
		{"wrap with minutes: at start", "23:30-00:30", at(23, 30, 0), true},
		{"wrap with minutes: after midnight", "23:30-00:30", at(0, 29, 59), true},
		{"wrap with minutes: at end", "23:30-00:30", at(0, 30, 0), false},
		{"starting at midnight", "00:00-06:00", at(0, 0, 0), true},
		{"ending at 23:59", "12:00-23:59", at(23, 58, 59), true},
		{"ending at 23:59 excludes the end", "12:00-23:59", at(23, 59, 0), false},

		// parsing tolerance
		{"spaces around the dash", "01:00 - 07:00", at(3, 0, 0), true},
		{"surrounding whitespace", "  01:00-07:00 ", at(3, 0, 0), true},
		{"single digit hour", "1:00-7:00", at(3, 0, 0), true},

		// the HH:MM are read from now's own wall clock
		{"uses the wall clock of now's location", "01:00-07:00", time.Date(2026, 1, 14, 3, 0, 0, 0, time.FixedZone("UTC+9", 9*3600)), true},
		{"does not convert now to another zone", "01:00-07:00", time.Date(2026, 1, 14, 3, 0, 0, 0, time.FixedZone("UTC-9", -9*3600)), true},
		{"wall clock outside the window in its own zone", "01:00-07:00", time.Date(2026, 1, 14, 12, 0, 0, 0, time.FixedZone("UTC+9", 9*3600)), false},

		// invalid specs are never quiet
		{"invalid: hour out of range", "25:00-07:00", at(3, 0, 0), false},
		{"invalid: minute out of range", "01:00-07:60", at(3, 0, 0), false},
		{"invalid: missing end", "01:00", at(3, 0, 0), false},
		{"invalid: empty end", "01:00-", at(3, 0, 0), false},
		{"invalid: empty start", "-07:00", at(3, 0, 0), false},
		{"invalid: three parts", "01:00-07:00-08:00", at(3, 0, 0), false},
		{"invalid: words", "night", at(3, 0, 0), false},
		{"invalid: empty window", "01:00-01:00", at(1, 0, 0), false},
		{"invalid: 24:00", "22:00-24:00", at(23, 0, 0), false},
		{"invalid: seconds", "01:00:00-07:00:00", at(3, 0, 0), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := QuietHours(tc.spec, tc.now); got != tc.want {
				t.Fatalf("QuietHours(%q, %v) = %v, want %v", tc.spec, tc.now, got, tc.want)
			}
		})
	}
}

func TestValidateQuietHours(t *testing.T) {
	valid := []string{"", "  ", "01:00-07:00", "22:00-06:00", "00:00-23:59", " 1:00 - 7:00 "}
	for _, spec := range valid {
		if err := ValidateQuietHours(spec); err != nil {
			t.Errorf("ValidateQuietHours(%q) = %v, want nil", spec, err)
		}
	}
	invalid := []string{"25:00-07:00", "01:00-07:60", "01:00", "01:00-", "-07:00", "01:00-07:00-08:00", "night", "01:00-01:00", "22:00-24:00", "01:00:00-07:00:00"}
	for _, spec := range invalid {
		err := ValidateQuietHours(spec)
		if err == nil {
			t.Errorf("ValidateQuietHours(%q) = nil, want error", spec)
			continue
		}
		if !strings.Contains(err.Error(), spec) {
			t.Errorf("error %q should quote the spec %q", err, spec)
		}
	}
}
