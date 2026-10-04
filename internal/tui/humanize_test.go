package tui

import (
	"os"
	"testing"
	"time"
)

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0", -5: "0", 1: "1K", 812 * 1024: "812K", 34 << 20: "34M", 1395864371: "1.3G"} {
		if got := HumanBytes(n); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		42 * time.Second: "42s", -7 * time.Minute: "7m", 3*time.Hour + 12*time.Minute: "3h12m",
		5 * time.Hour: "5h", 52 * time.Hour: "2d4h", 72 * time.Hour: "3d",
	} {
		if got := HumanDuration(d); got != want {
			t.Errorf("HumanDuration(%v) = %q, want %q", d, got, want)
		}
	}
	if got := HumanAgo(0); got != "never" {
		t.Errorf("HumanAgo(0) = %q, want never", got)
	}
	if got := HumanAgo(90 * time.Second); got != "1m ago" {
		t.Errorf("HumanAgo(90s) = %q, want 1m ago", got)
	}
}

func TestIsTerminalRejectsFilesAndPipes(t *testing.T) {
	if IsTerminal(nil) {
		t.Error("nil file counted as a terminal")
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if IsTerminal(null) {
		t.Error("/dev/null counted as a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if IsTerminal(r) || IsTerminal(w) {
		t.Error("a pipe counted as a terminal")
	}
}

func TestLayoutColumnsShrinksFlexFirst(t *testing.T) {
	cols := []column{{title: "A", min: 3}, {title: "TITLE", min: 5, flex: true}, {title: "B", min: 2}}
	rows := [][]string{{"abcdef", "a very long title that will not fit", "xy"}}
	w := layoutColumns(cols, rows, 30, 2)
	if w[0] != 6 || w[2] != 2 {
		t.Fatalf("fixed columns shrank before the flex one: %v", w)
	}
	line := noMark + renderRow(rows[0], w)
	if got := len([]rune(line)); got > 30 {
		t.Fatalf("row is %d cells wide, want <= 30: %q", got, line)
	}
	mustContain(t, line, "abcdef", "…", "xy")
}
