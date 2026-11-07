package factory

import (
	"strings"
	"testing"
)

// The known crashes of the environment are told by a whole line of stderr
// alone, whatever mise printed around it; only the locale is the
// machine's, and the same words inside a line of the branch's text, or an
// error of the command's own, are none.
func TestACrashOfTheEnvironmentIsToldByAWholeLineOfStderr(t *testing.T) {
	cases := []struct {
		stderr, lacks string
		machine       bool
	}{
		{"mise WARN untrusted\ninvalid byte sequence in US-ASCII\n", "a UTF-8 locale", true},
		{"/gems/x.rb:12:in 'match?': invalid byte sequence in US-ASCII (ArgumentError)\n", "a UTF-8 locale", true},
		{"Could not find rubocop-rails-2.30.0 in locally installed gems (Bundler::GemNotFound)\n", "a gem", false},
		{"Could not find gem 'rubocop-rails' in locally installed gems.\n", "a gem", false},
		{"/usr/lib/ruby/3.4/rubygems.rb:12:in 'require': cannot load such file -- rubocop-rspec (LoadError)\n", "a Ruby library", false},
		{"bin/lint: line 3: bundle: command not found\n", "a command", false},
		{"Error: .rubocop.yml names Style/Gone\n", "", false},
		{"bin/lint: echo 'yarn: command not found' in setup.sh, not an error\n", "", false},
		{"Warning: invalid byte sequence in US-ASCII is fixed in rubocop 1.70, see the changelog\n", "", false},
	}
	for _, tc := range cases {
		c, ok := crashOf("precheck.lint", "bin/lint {files}", "/tmp/shop.factory1", tc.stderr)
		switch {
		case tc.lacks == "" && ok:
			t.Errorf("%q: a crash of the environment (%v), want none", tc.stderr, c)
		case tc.lacks != "" && (!ok || c.sig.lacks != tc.lacks || c.sig.machine != tc.machine):
			t.Errorf("%q: crash %+v (%v), want one for lack of %s, the machine's %v", tc.stderr, c, ok, tc.lacks, tc.machine)
		case ok && strings.Contains(c.line, "mise WARN"):
			t.Errorf("the crash's line %q is mise's warning", c.line)
		}
	}
}

// A lint command names its output's format with --format, --out-format,
// --output-format, --formatter or --reporter, or with -f after a lint it
// is rubocop's or eslint's flag for; -f of make or docker compose is none.
func TestALintCommandNamesItsFormat(t *testing.T) {
	for cmd, want := range map[string]bool{
		"bundle exec rubocop --format emacs {files}":       true,
		"bin/lint --format=unix {files}":                   true,
		"yarn eslint -f unix {files}":                      true,
		"golangci-lint run --out-format=line-number":       true,
		"ruff check --output-format concise {files}":       true,
		"stylelint --formatter unix {files}":               true,
		"bin/lint {files}":                                 false,
		"make -f Makefile.lint lint FILES='{files}'":       false,
		"docker compose -f lint.yml run --rm lint {files}": false,
		"bundle exec rubocop --force-exclusion {files}":    false,
	} {
		if got := namesFormat(cmd); got != want {
			t.Errorf("namesFormat(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// A line is clipped to outputLineRunes, so a lint's minified source or a
// long dump never fills a NEEDS YOU.
func TestALineOfOutputIsClipped(t *testing.T) {
	if got := lastLine(strings.Repeat("x", 3*outputLineRunes) + "\n"); len([]rune(got)) > outputLineRunes {
		t.Fatalf("lastLine kept %d runes, want at most %d", len([]rune(got)), outputLineRunes)
	}
}

// The line of a command's output a NEEDS YOU names skips mise's notes, the
// stack's frames and the code frames of the branch's source, keeps Ruby's
// message line that starts with its place and a line that only starts with
// "at", and masks a token.
func TestTheLineThatSaysWhyACommandFailed(t *testing.T) {
	out := "mise WARN Config files are not trusted\n" +
		"/gems/x.rb:12:in 'match?': invalid byte sequence (ArgumentError)\n" +
		"\tfrom /gems/x.rb:3:in 'run'\n" +
		"    at Object.<anonymous> (/app/lint.js:1:2)\n" +
		"    at /app/node_modules/eslint/lib/cli.js:10:3\n" +
		"  12 | const branch = source;\n" +
		"     |       ^\n" +
		"[error]   3 | let x = ;\n" +
		"mise INFO done\n"
	if got := lastLine(out); !strings.Contains(got, "invalid byte sequence (ArgumentError)") {
		t.Fatalf("lastLine = %q, want Ruby's message line", got)
	}
	if got := lastLine("Error: the gems\nat least one gem is missing\n"); got != "at least one gem is missing" {
		t.Fatalf("lastLine = %q, want a line that starts with \"at \" and is no stack frame", got)
	}
	if got := firstLine("mise WARN untrusted\nsetup: token ghp_abcdefghijklmnopqrstuvwxyz0123456789 refused\n"); !strings.HasPrefix(got, "setup: token") ||
		strings.Contains(got, "ghp_abcdefghij") {
		t.Fatalf("firstLine = %q, want the setup's line, its token masked", got)
	}
}
