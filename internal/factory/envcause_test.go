package factory

import (
	"strings"
	"testing"
)

// The known crashes of the environment are told by a line of the output,
// stderr or stdout, whatever mise printed around it; an error of the
// command's own is none.
func TestACrashOfTheEnvironmentIsToldByItsOutput(t *testing.T) {
	cases := []struct {
		stderr, stdout, lacks string
	}{
		{"mise WARN untrusted\ninvalid byte sequence in US-ASCII\n", "", "a UTF-8 locale"},
		{"", "Bundler::GemNotFound: Could not find rubocop-rails-2.30.0 in locally installed gems", "a gem"},
		{"/usr/lib/ruby/3.4/rubygems.rb:12:in 'require': cannot load such file -- rubocop-rspec (LoadError)\n", "", "a Ruby library"},
		{"bin/lint: line 3: bundle: command not found\n", "", "a command"},
		{"Error: .rubocop.yml names Style/Gone\n", "", ""},
	}
	for _, tc := range cases {
		c, ok := crashOf("precheck.lint", "bin/lint {files}", "/tmp/shop.factory1", tc.stderr, tc.stdout)
		switch {
		case tc.lacks == "" && ok:
			t.Errorf("%q: a crash of the environment (%v), want none", tc.stderr, c)
		case tc.lacks != "" && (!ok || c.sig.lacks != tc.lacks):
			t.Errorf("%q %q: crash %v (%v), want one for lack of %s", tc.stderr, tc.stdout, c, ok, tc.lacks)
		case ok && strings.Contains(c.line, "mise WARN"):
			t.Errorf("the crash's line %q is mise's warning", c.line)
		}
	}
}

// The line of a command's output a NEEDS YOU names skips mise's notes and
// the stack's frames, keeps Ruby's message line that starts with its place,
// and masks a token.
func TestTheLineThatSaysWhyACommandFailed(t *testing.T) {
	out := "mise WARN Config files are not trusted\n" +
		"/gems/x.rb:12:in 'match?': invalid byte sequence (ArgumentError)\n" +
		"\tfrom /gems/x.rb:3:in 'run'\n" +
		"    at Object.<anonymous> (/app/lint.js:1:2)\n" +
		"mise INFO done\n"
	if got := lastLine(out); !strings.Contains(got, "invalid byte sequence (ArgumentError)") {
		t.Fatalf("lastLine = %q, want Ruby's message line", got)
	}
	if got := firstLine("mise WARN untrusted\nsetup: token ghp_abcdefghijklmnopqrstuvwxyz0123456789 refused\n"); !strings.HasPrefix(got, "setup: token") ||
		strings.Contains(got, "ghp_abcdefghij") {
		t.Fatalf("firstLine = %q, want the setup's line, its token masked", got)
	}
}
