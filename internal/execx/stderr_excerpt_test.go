package execx

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// BenchmarkExitErrorOfALongStderr builds the error of a command that wrote 8
// MiB to stderr, and renders a wrap of it, as a step's fail row, a PR's
// last_error and a log line each do.
func BenchmarkExitErrorOfALongStderr(b *testing.B) {
	stderr := []byte(strings.Repeat("remote: Counting objects: 100% (12/12), done.\n", (8<<20)/47))
	c := Cmd{Name: "git", Args: []string{"fetch", "origin"}}
	b.Run("build", func(b *testing.B) {
		for b.Loop() {
			if exitError(c, 1, stderr).Stderr == "" {
				b.Fatal("empty")
			}
		}
	})
	err := error(exitError(c, 1, stderr))
	b.Run("wrap", func(b *testing.B) {
		for b.Loop() {
			if fmt.Errorf("slots: fetch: %w", err).Error() == "" {
				b.Fatal("empty")
			}
		}
	})
}

// A failed command's stderr can run to megabytes (a seed that prints its
// SQL, git's progress): Result.Stderr keeps it whole, while its ExitError
// keeps the first and last 32 KiB around a "[N bytes cut]" line, redacted
// once when the command ends, so no wrap carries megabytes into a step's
// fail row, a PR's last_error or a log line. A token the cut would split is
// redacted whole first.
func TestAnExitErrorKeepsAnExcerptOfALongStderr(t *testing.T) {
	const secret = "ghs_0123456789abcdefghijklmnopqrstuvwxyzABCDEFGH"
	filler := func(n int) string { return strings.Repeat("x", n) }
	// "head" starts the output and a token straddles the head's end; a
	// second token straddles the tail's start, in the last line, which ends
	// with "fatal: the end".
	head := "head\n" + filler(stderrKeep-5-10) + secret + "\n"
	middle := strings.Repeat("progress line\n", 20000)
	tail := filler(3) + secret + filler(stderrKeep-40) + " fatal: the end\n"
	whole := head + middle + tail
	r := &Real{}
	res, err := r.Run(t.Context(), Cmd{Name: "sh", Args: []string{"-c", `cat >&2; exit 2`}, Stdin: []byte(whole)})
	ee, ok := errors.AsType[*ExitError](err)
	if !ok || ee.Code != 2 {
		t.Fatalf("want an ExitError with code 2, got %v", err)
	}
	if string(res.Stderr) != whole {
		t.Fatalf("Result.Stderr = %d bytes, want the whole %d", len(res.Stderr), len(whole))
	}
	msg := err.Error()
	if len(ee.Stderr) > 2*stderrKeep+64 || len(msg) > 2*stderrKeep+256 {
		t.Errorf("ExitError keeps %d bytes of stderr and its text is %d bytes, want about %d", len(ee.Stderr), len(msg), 2*stderrKeep)
	}
	for _, want := range []string{"head\n", "fatal: the end", " bytes cut]\n"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error text lacks %q", want)
		}
	}
	if strings.Contains(msg, secret[4:12]) || strings.Contains(msg, secret[len(secret)-12:]) {
		t.Errorf("error text leaks a part of a token")
	}
	// The excerpt is a prefix and a suffix of the redacted stderr around the
	// cut line, which counts the bytes between them.
	redacted := Redact(whole)
	m := regexp.MustCompile(`\[(\d+) bytes cut\]\n`).FindStringSubmatchIndex(ee.Stderr)
	if m == nil {
		t.Fatalf("no cut line in %d bytes", len(ee.Stderr))
	}
	prefix, suffix := ee.Stderr[:m[0]], ee.Stderr[m[1]:]
	if len(prefix) > stderrKeep || !strings.HasPrefix(redacted, prefix) {
		prefix = strings.TrimSuffix(prefix, "\n") // the line end the cut line starts with
	}
	cut, _ := strconv.Atoi(ee.Stderr[m[2]:m[3]])
	if !strings.HasPrefix(redacted, prefix) || !strings.HasSuffix(redacted, suffix) || cut != len(redacted)-len(prefix)-len(suffix) ||
		len(prefix) > stderrKeep || len(suffix) > stderrKeep || len(prefix) < stderrKeep-4 || len(suffix) < stderrKeep-64 {
		t.Errorf("excerpt = %d bytes, a %d-byte cut and %d bytes, of %d", len(prefix), cut, len(suffix), len(redacted))
	}
	if again := err.Error(); again != msg {
		t.Error("Error() changed between calls")
	}

	// A short stderr stays whole, as before.
	_, err = r.Run(t.Context(), Cmd{Name: "sh", Args: []string{"-c", `echo "fatal: no such ref $0" >&2; exit 1`, secret}})
	if err == nil || !strings.HasSuffix(err.Error(), "exited 1: fatal: no such ref <redacted>") {
		t.Errorf("short stderr: %v", err)
	}
}
