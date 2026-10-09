package notes

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// printableOK reports whether s is what Printable promises: valid UTF-8 with
// no control character but newline and tab.
func printableOK(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return r != '\n' && r != '\t' && unicode.IsControl(r) })
}

// Printable replaces control characters and invalid UTF-8 alike: a raw C1
// byte such as 0x9b (an 8-bit CSI) is invalid UTF-8 and became U+FFFD only
// when the text also held a control character; alone it went through.
func TestPrintable(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain text stays", "a note\n\twith a tab", "a note\n\twith a tab"},
		{"valid multi-byte stays", "café → ok", "café → ok"},
		{"C0 escape", "red\x1b[31m", "red [31m"},
		{"DEL", "a\x7fb", "a b"},
		{"C1 as a rune", "a\u009b31mb", "a 31mb"},
		{"raw C1 byte alone", "a\x9b31mb", "a�31mb"},
		{"raw C1 byte with a control", "\x1b\x9b", " �"},
		{"truncated sequence", "caf\xc3", "caf�"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Printable(tc.in)
			if got != tc.want {
				t.Fatalf("Printable(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !printableOK(got) {
				t.Fatalf("Printable(%q) = %q holds a control character or invalid UTF-8", tc.in, got)
			}
		})
	}
}

// Whatever an agent writes, Printable's output is valid UTF-8 without a
// control character other than newline and tab, and text that already is
// passes unchanged.
func FuzzPrintable(f *testing.F) {
	for _, s := range []string{"", "plain\n", "\x1b[2J", "\x9b2J", "caf\xc3", "\u009b", "a\x00b", "\xff\xfe", "tab\tnl\n"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out := Printable(in)
		if !printableOK(out) {
			t.Fatalf("Printable(%q) = %q holds a control character or invalid UTF-8", in, out)
		}
		if printableOK(in) && out != in {
			t.Fatalf("Printable(%q) = %q changed printable text", in, out)
		}
		if again := Printable(out); again != out {
			t.Fatalf("Printable is not idempotent: %q then %q", out, again)
		}
	})
}
