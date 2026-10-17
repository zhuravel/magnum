package execx

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// stderrLogLine runs a shell that writes stderr to its stderr (fed on its
// stdin, so the command line in the log does not carry it) and exits with
// code, and returns the one line it logged.
func stderrLogLine(t *testing.T, stderr string, code string, probe bool) string {
	t.Helper()
	var log levelBuf
	r := &Real{Log: &log}
	_, err := r.Run(t.Context(), Cmd{Name: "sh", Args: []string{"-c", "cat >&2; exit " + code}, Stdin: []byte(stderr), Probe: probe})
	if (err == nil) != (code == "0") {
		t.Fatalf("exit %s: err = %v", code, err)
	}
	if len(log.lines) != 1 {
		t.Fatalf("lines = %q, want one", log.lines)
	}
	return log.lines[0]
}

// A failed command's log line says why it failed: gh api exits 1 for a 502
// and for a bad query alike, and the daemon log held 96 of those lines that
// could not be told apart. The line carries the first non-empty line of
// stderr, redacted and bounded to 200 runes on one line; a command that
// succeeds, or fails with nothing on stderr, logs no stderr.
func TestAFailedCommandLogsItsFirstStderrLine(t *testing.T) {
	const secret = "ghs_0123456789abcdefghijklmnopqrstuvwxyz"
	for _, tc := range []struct {
		name   string
		stderr string
		code   string
		probe  bool
		level  string
		want   string // the text after " stderr=", "" for no stderr part
	}{
		{"the first of two lines", "gh: HTTP 502 Bad Gateway\nsecond line\n", "1", false, "WARN", "gh: HTTP 502 Bad Gateway"},
		{"blank lines first", "\n  \n\tgh: Could not resolve to a node\nsecond line\n", "1", false, "WARN", "gh: Could not resolve to a node"},
		{"a carriage return ends the line", "progress 10%\rprogress 20%\nsecond line\n", "1", false, "WARN", "progress 10%"},
		{"a token is redacted", "bad credentials for " + secret + " today\nsecond line\n", "1", false, "WARN", "bad credentials for <redacted> today"},
		{"a key block is redacted", "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkq\n-----END PRIVATE KEY-----\n", "1", false, "WARN", "<redacted>"},
		{"a key block after text is redacted", "key: -----BEGIN PRIVATE KEY----- MIIEvQIBADANBgkq\nsecond line\n", "1", false, "WARN", "key: <redacted>"},
		{"control characters become spaces", "one\ttwo\x1b[31mred\nsecond line\n", "1", false, "WARN", "one two [31mred"},
		{"a probe's answer carries it too", "fatal: not a git repository\n", "128", true, "DEBUG", "fatal: not a git repository"},
		{"nothing on stderr", "", "1", false, "WARN", ""},
		{"blank stderr", "\n \n", "1", false, "WARN", ""},
		{"success with stderr", "warning: LF will be replaced\n", "0", false, "DEBUG", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := stderrLogLine(t, tc.stderr, tc.code, tc.probe)
			if !strings.HasPrefix(line, tc.level+" exec sh -c ") {
				t.Fatalf("line = %q, want a %s exec line", line, tc.level)
			}
			_, got, found := strings.Cut(line, " stderr=")
			switch {
			case tc.want == "" && found:
				t.Errorf("line carries stderr: %q", line)
			case tc.want != "" && got != tc.want:
				t.Errorf("line = %q, want it to end with ` stderr=%s`", line, tc.want)
			}
			for _, leak := range []string{secret, secret[4:16], "MIIEvQ", "second line", "END PRIVATE"} {
				if strings.Contains(line, leak) {
					t.Errorf("line leaks %q: %q", leak, line)
				}
			}
		})
	}

	t.Run("bounded to 200 runes", func(t *testing.T) {
		for _, unit := range []string{"x", "é", "日"} {
			line := stderrLogLine(t, strings.Repeat(unit, 5000)+"\nlater\n", "1", false)
			_, got, ok := strings.Cut(line, " stderr=")
			if !ok || !utf8.ValidString(got) || utf8.RuneCountInString(got) != 200 || !strings.HasSuffix(got, "…") || strings.Contains(got, "later") ||
				strings.Trim(got, unit+"…") != "" {
				t.Errorf("unit %q: stderr part = %d runes %q", unit, utf8.RuneCountInString(got), got)
			}
		}
		// A line of 200 runes is whole, 201 is cut to 199 and an ellipsis.
		for n, want := range map[int]int{120: 120, 200: 200, 201: 200} {
			line := stderrLogLine(t, strings.Repeat("y", n)+"\n", "1", false)
			_, got, _ := strings.Cut(line, " stderr=")
			if utf8.RuneCountInString(got) != want || strings.HasSuffix(got, "…") != (n > 200) {
				t.Errorf("%d runes in: stderr part = %d runes, want %d", n, utf8.RuneCountInString(got), want)
			}
		}
	})

	// The line is one log record: a stderr with an invalid byte still logs
	// valid text, and the format string never reads a percent sign.
	t.Run("invalid bytes and percent signs", func(t *testing.T) {
		line := stderrLogLine(t, "bad \xff byte 100%s %d\n", "1", false)
		_, got, _ := strings.Cut(line, " stderr=")
		if !utf8.ValidString(got) || !strings.HasSuffix(got, " byte 100%s %d") {
			t.Errorf("stderr part = %q", got)
		}
	})
}
