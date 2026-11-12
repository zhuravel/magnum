package factory

// The causes of a failure that are the machine's, the copy's or the
// configuration's, never the change's: the line of a command's stderr that
// says why it failed, the crashes of a lint that its environment caused
// (envCrash: the machine's go to the infrastructure hold, the copy's are
// healed or the task's), and the retry a NEEDS YOU for such a cause ends
// with.

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/scrub"
	"github.com/zhuravel/magnum/internal/textx"
)

// outputLineRunes bounds a line of a command's output that a NEEDS YOU, a
// hold or an event names.
const outputLineRunes = 200

// envCrashTries is how many times a lint may crash on the machine's
// environment at one head of a task before the task needs you (and the
// infrastructure hold lets the other tasks go): a crash that stays while
// the head does is not one a wait ends.
const envCrashTries = 3

// noiseRe matches a line of a command's output that never says why it
// failed: mise's own warnings and notes (a copy's untrusted config); a
// stack's frames, whose message is on a line of its own (Ruby's
// `x.rb:12:in 'm'` and `from x.rb:12:in 'm'`, JavaScript's
// `at f (x.js:1:2)` and `at x.js:1:2`); and the code frames that quote the
// branch's source (`> 3 | code`, `    | ^`, prettier's `[error] 3 | code`,
// Ruby's `^~~~`).
var noiseRe = regexp.MustCompile("^(?:mise (?:WARN|INFO|DEBUG|TRACE)\\b" +
	"|(?:from\\s+)?\\S+:\\d+:in\\s+[`'][^`']*'$" +
	"|at\\s+.*\\(\\S*:\\d+:\\d+\\)$|at\\s+\\S+:\\d+:\\d+$" +
	"|(?:\\[\\w+\\]\\s*)?>?\\s*\\d*\\s*\\|" +
	"|[\\^~]+$)")

// meaningfulLines are the lines of out that may say why a command failed:
// trimmed, none blank and none noise (noiseRe).
func meaningfulLines(out string) []string {
	var lines []string
	for line := range strings.FieldsFuncSeq(out, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if line = strings.TrimSpace(line); line != "" && !noiseRe.MatchString(line) {
			lines = append(lines, line)
		}
	}
	return lines
}

// rubyPlaceRe is the place a Ruby error's line starts with
// (`/gems/x.rb:12:in 'require': `), before its message.
var rubyPlaceRe = regexp.MustCompile("^(?:from\\s+)?\\S+:\\d+:in\\s+[`'][^`']*':\\s*")

// cleanLine is line as a NEEDS YOU, a hold or an event may name it: the
// error's message without the place a Ruby error starts with, scrubbed
// (scrub.Text: a token, a key, a password, an email or a host is masked, as
// in all the text the factory keeps), its control characters turned into
// spaces, valid UTF-8, at most outputLineRunes runes.
func cleanLine(line string) string { return cleanLineMax(line, outputLineRunes) }

// cleanLineMax is cleanLine at most n runes long.
func cleanLineMax(line string, n int) string {
	if msg := rubyPlaceRe.ReplaceAllString(line, ""); msg != "" {
		line = msg
	}
	s := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(scrub.Text(line), "�"))
	return textx.Clip(strings.TrimSpace(s), n)
}

// lastLine is the last line of a failed command's stderr out that may say
// why it failed (meaningfulLines), cleaned (cleanLine); "" for none. Only
// stderr is read: a lint's stdout is what it says about the branch.
func lastLine(out string) string {
	if lines := meaningfulLines(out); len(lines) > 0 {
		return cleanLine(lines[len(lines)-1])
	}
	return ""
}

// firstLine is the first such line of out, cleaned; "" for none.
func firstLine(out string) string { return firstLineMax(out, outputLineRunes) }

// firstLineMax is firstLine at most n runes long.
func firstLineMax(out string, n int) string {
	if lines := meaningfulLines(out); len(lines) > 0 {
		return cleanLineMax(lines[0], n)
	}
	return ""
}

// envSignature is a crash of a command that its environment caused: the
// whole line of stderr that tells it (re, matched on the trimmed line, so
// the same words inside a line of the branch's text tell nothing), what it
// lacked, and the fix ({copy} the copy's path). machine: the daemon's
// environment, every copy's alike (the infrastructure hold takes it); else
// the copy's or the task's own. heal: the repository's installs in the copy
// may bring back what it lacked. scriptOnly: the change can be its cause
// only through the lint's own script (a lint that echoes the files it
// lints names them on stderr, and they lack no command).
type envSignature struct {
	re                        *regexp.Regexp
	lacks, fix                string
	machine, heal, scriptOnly bool
}

// envSignatures are the known crashes of the environment, first match
// first.
var envSignatures = []envSignature{
	{re: regexp.MustCompile(`(?:^|: )invalid byte sequence in US-ASCII(?: \([\w:]+\))?$`), lacks: "a UTF-8 locale", machine: true,
		fix: "give the copies a UTF-8 locale (LANG = \"en_US.UTF-8\" in its [[factory.repo]] env, and no LC_ALL = \"C\"), " +
			"then `magnum factory install`"},
	{re: regexp.MustCompile(`\(Bundler::GemNotFound\)$|^Bundler::GemNotFound:|^Could not find gem '[^']+'`), lacks: "a gem", heal: true,
		fix: "`bundle install` in {copy}"},
	// rubocop rescues the LoadError of a plugin or a require itself and
	// prints its bare message; Ruby's own uncaught one ends in (LoadError).
	{re: regexp.MustCompile(`^cannot load such file -- \S+$|cannot load such file -- \S+ \(LoadError\)$`), lacks: "a Ruby library",
		heal: true, fix: "`bundle install` in {copy}"},
	// Node's own (and jest's "… from '<file>'") of a package the copy's
	// node_modules lack; a relative path ("./x") is the change's.
	{re: regexp.MustCompile(`^(?:Error: )?Cannot find module '[^./'][^']*'`), lacks: "a Node module", heal: true,
		fix: "the repository's installs (`pnpm install`, `yarn install`) in {copy}"},
	{re: regexp.MustCompile(`^(?:\S+: )*(?:line \d+: )?\S+: command not found$|^(?:\S+: )*command not found: \S+$`), lacks: "a command",
		scriptOnly: true, fix: "install it, or put its directory on your login shell's PATH ([daemon] shell_path), then `magnum factory install`"},
}

// envCrash is a lint that crashed on its environment (an envSignature on a
// line of its stderr), not on the change nor on its configuration. key and
// cmd are the command's configuration key and its command as configured,
// line the stderr line that tells the crash (cleaned), copy the copy it ran
// in.
type envCrash struct {
	key, cmd, line, copy string
	sig                  envSignature
}

func (c envCrash) Error() string {
	return fmt.Sprintf("%s `%s` crashed for lack of %s: %s", c.key, firstWord(c.cmd), c.sig.lacks, c.line)
}

// fix is what ends c, in the copy it ran in.
func (c envCrash) fix() string { return strings.ReplaceAll(c.sig.fix, "{copy}", c.copy) }

// holdCause is the infrastructure hold's reason for c, a crash of the
// machine's: what crashed, the line that says so and its fix.
func (c envCrash) holdCause() string {
	return fmt.Sprintf("the environment: %s crashed for lack of %s (%s); fix: %s", firstWord(c.cmd), c.sig.lacks, c.line, c.fix())
}

// crashOf is the envCrash of command cmd of key that ran in the copy at
// dir and failed with stderr, when a whole line of it tells a known crash
// of the environment (envSignatures). The command's stdout tells none: it
// is what the lint says about the branch (its source, a script's echo).
func crashOf(key, cmd, dir, stderr string) (envCrash, bool) {
	lines := meaningfulLines(stderr)
	for _, sig := range envSignatures {
		for _, line := range lines {
			if sig.re.MatchString(line) {
				return envCrash{key: key, cmd: cmd, line: cleanLine(line), copy: dir, sig: sig}, true
			}
		}
	}
	return envCrash{}, false
}

// lintInputRe matches a changed file a lint loads (its gems and packages,
// the tools' versions, a lint's configuration or its own cops): a crash
// for lack of something, in a branch that changed one, is the branch's.
var lintInputRe = regexp.MustCompile(`(?i)(?:^|/)(?:Gemfile(?:\.lock)?|gems\.(?:rb|locked)|package(?:-lock)?\.json|yarn\.lock|` +
	`pnpm-lock\.yaml|\.tool-versions|\.ruby-version|\.?mise\.toml)$|rubocop|eslint|stylelint|prettier|(?:^|/)cops?/`)

// scriptDefRe matches a changed file that defines the scripts a command runs
// (package.json's scripts, a Makefile's recipes, a Rakefile's and
// lib/tasks' tasks, bin's scripts): a command it names that is missing is
// the change's.
var scriptDefRe = regexp.MustCompile(`(?:^|/)(?:package\.json|[Mm]akefile|GNUmakefile|Rakefile|Justfile|Taskfile\.ya?ml)$|(?:^|/)lib/tasks/|(?:^|/)bin/`)

// branchCaused are the files of the change (files, as the copy names them)
// that crash c of a lint can come from: the lint's own script (its
// command's first word), and, unless c's signature is the script's alone
// (scriptOnly: a missing command), one the lint loads (lintInputRe) or one
// its stderr names.
func branchCaused(c envCrash, stderr string, files []string) []string {
	var out []string
	for _, f := range files {
		if f == firstWord(c.cmd) || c.sig.scriptOnly && scriptDefRe.MatchString(f) ||
			!c.sig.scriptOnly && (lintInputRe.MatchString(f) || strings.Contains(stderr, f)) {
			out = append(out, f)
		}
	}
	return out
}

// retryHint ends a NEEDS YOU of task t whose cause is the machine's or the
// configuration's: once that is fixed, `magnum factory retry <id>` lets the
// task go on.
func retryHint(t registry.Task) string {
	return fmt.Sprintf("; then `magnum factory retry %d`", t.ID)
}

// formatRe matches a lint command that names its output's format with a
// long flag (rubocop's and eslint's `--format emacs`, `--format=unix`,
// golangci-lint's `--out-format`, ruff's `--output-format`, `--formatter`,
// `--reporter`), and lintShortFormatRe one that names it with `-f` after a
// lint whose short flag that is (rubocop, eslint, stylelint and the like):
// make's and docker compose's `-f` name a file.
var (
	formatRe          = regexp.MustCompile(`(?:^|\s)--(?:(?:out|output)-)?format(?:ter)?(?:=|\s)|(?:^|\s)--reporter(?:=|\s)`)
	lintShortFormatRe = regexp.MustCompile(`(?:^|[\s/])(?:rubocop|standardrb|eslint|stylelint|erb_lint|erblint|haml-lint|scss-lint)\s(?:[^|;&]*\s)?-f(?:\s|=)`)
)

// namesFormat reports whether lint command cmd names its output's format.
func namesFormat(cmd string) bool {
	return formatRe.MatchString(cmd) || lintShortFormatRe.MatchString(cmd)
}
