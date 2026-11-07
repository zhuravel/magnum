package factory

// The causes of a failure that are the machine's or the configuration's,
// never the change's: the line of a command's output that says why it
// failed, the crashes of a check's command that its environment caused
// (envCrash: the infrastructure hold takes them, never you), and the
// retry a NEEDS YOU for such a cause ends with.

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/textx"
)

// outputLineRunes bounds a line of a command's output that a NEEDS YOU, a
// hold or an event names.
const outputLineRunes = 200

// noiseRe matches a line of a command's output that never says why it
// failed: mise's own warnings and notes (a copy's untrusted config), and a
// stack's frames, whose message is on a line of its own (Ruby's
// `x.rb:12:in 'm'` and `from x.rb:12:in 'm'`, JavaScript's `at f (x.js:1:2)`).
var noiseRe = regexp.MustCompile("^(?:mise (?:WARN|INFO|DEBUG|TRACE)\\b|(?:from\\s+)?\\S+:\\d+:in\\s+[`'][^`']*'$|at\\s)")

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

// cleanLine is line as a NEEDS YOU, a hold or an event may name it:
// redacted (a token or a key is masked), its control characters turned into
// spaces, valid UTF-8, at most outputLineRunes runes.
func cleanLine(line string) string {
	s := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(execx.Redact(line), "�"))
	return textx.Clip(strings.TrimSpace(s), outputLineRunes)
}

// lastLine is the last line of a failed command's output out that may
// say why it failed (meaningfulLines), cleaned (cleanLine); "" for none.
func lastLine(out string) string {
	if lines := meaningfulLines(out); len(lines) > 0 {
		return cleanLine(lines[len(lines)-1])
	}
	return ""
}

// firstLine is the first such line of out, cleaned; "" for none.
func firstLine(out string) string {
	if lines := meaningfulLines(out); len(lines) > 0 {
		return cleanLine(lines[0])
	}
	return ""
}

// envSignature is a crash of a command that its environment caused: the
// words of its output that tell it (lower case), what it lacked, and the
// fix ({copy} the copy's path).
type envSignature struct{ match, lacks, fix string }

// envSignatures are the known crashes of the environment, first match
// first.
var envSignatures = []envSignature{
	{"invalid byte sequence in us-ascii", "a UTF-8 locale",
		"give the copies a UTF-8 locale (LANG = \"en_US.UTF-8\" in its [[factory.repo]] env, and no LC_ALL = \"C\"), then `magnum factory install`"},
	{"bundler::gemnotfound", "a gem", "`bundle install` in {copy}"},
	{"could not find gem", "a gem", "`bundle install` in {copy}"},
	{"cannot load such file", "a Ruby library", "`bundle install` in {copy}"},
	{"command not found", "a command",
		"install it, or put its directory on your login shell's PATH ([daemon] shell_path), then `magnum factory install`"},
}

// envCrash is a check's command that crashed on its environment (an
// envSignature in its output), not on the change nor on its configuration:
// the infrastructure hold takes it (infraCause), never you nor the writer.
// key and cmd are the command's configuration key and its command as
// configured, line the output line that tells the crash (cleaned), copy the
// copy it ran in.
type envCrash struct {
	key, cmd, line, copy string
	sig                  envSignature
}

func (c envCrash) Error() string {
	return fmt.Sprintf("%s `%s` crashed for lack of %s: %s", c.key, firstWord(c.cmd), c.sig.lacks, c.line)
}

// holdCause is the infrastructure hold's reason for c: what crashed, the
// line that says so and its fix.
func (c envCrash) holdCause() string {
	return fmt.Sprintf("the environment: %s crashed for lack of %s (%s); fix: %s", firstWord(c.cmd), c.sig.lacks, c.line,
		strings.ReplaceAll(c.sig.fix, "{copy}", c.copy))
}

// crashOf is the envCrash of command cmd of key that ran in copy and
// failed with stderr and stdout, when one of their lines tells a known
// crash of the environment (envSignatures).
func crashOf(key, cmd, copy, stderr, stdout string) (envCrash, bool) {
	lines := slices.Concat(meaningfulLines(stderr), meaningfulLines(stdout))
	for _, sig := range envSignatures {
		for _, line := range lines {
			if strings.Contains(strings.ToLower(line), sig.match) {
				return envCrash{key: key, cmd: cmd, line: cleanLine(line), copy: copy, sig: sig}, true
			}
		}
	}
	return envCrash{}, false
}

// retryHint ends a NEEDS YOU of task t whose cause is the machine's or the
// configuration's: once that is fixed, `magnum factory retry <id>` lets the
// task go on.
func retryHint(t registry.Task) string {
	return fmt.Sprintf("; then `magnum factory retry %d`", t.ID)
}

// formatRe matches a lint command that names its output's format
// (rubocop's and eslint's `--format emacs`, `--format=unix`, `-f emacs`).
var formatRe = regexp.MustCompile(`(?:^|\s)(?:--format(?:=|\s)|--formatter(?:=|\s)|-f\s)`)
