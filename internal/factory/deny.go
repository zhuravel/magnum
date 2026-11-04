package factory

// The deny list (spec 8.4 and 12): no command the factory runs itself opens
// a remote console, runs a command in a cluster's pod (`kubectl exec`) or
// runs gh-ost, whatever wraps it (mise exec, env, bundle exec, a shell's -c
// script, a nested shell). Every command of the factory takes one path,
// Guard's runner: the precheck's lint, specs, jest and migration runs, the
// proof's spec runs and the git calls. The agents' own commands are bound by
// their prompts' rules, not by this list. It stops mistakes, not a
// determined process (section 12).

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
)

// ErrDenied marks a command the deny list refused (Guard): it never
// started.
var ErrDenied = errors.New("denied")

// kubectlRemote are the kubectl verbs that run a command in a cluster's pod
// or reach into one: a remote console by another name.
var kubectlRemote = []string{"exec", "attach", "debug", "run", "cp", "port-forward"}

// maxShellDepth bounds how deep Denied reads shells inside shells.
const maxShellDepth = 4

// Denied reports whether the factory may not run argv, and why, in plain
// words: gh-ost with any argument, kubectl exec (and the other verbs that
// reach into a pod: attach, debug, run, cp, port-forward), and each of
// repo's consoles at a command's place. It reads through the wrappers a
// command runs another under (mise exec, env and its assignments, bundle
// exec, exec, command, nohup, time, nice, timeout, xargs, sudo) and into a
// shell's -c script, each command of it (; && || | & and newlines, $( and
// backticks); gh-ost and kubectl's verbs are also found as words anywhere in
// a script, quoted or not, so a substitution inside quotes does not hide
// them. A console's own file as an argument (lint of bin/remote-console) is
// no console run.
func Denied(argv []string, repo config.FactoryRepo) (why string, denied bool) {
	for _, cmd := range commandsOf(argv, 0) {
		if why, ok := deniedCommand(cmd.words, repo, !cmd.loose); ok {
			return why, true
		}
	}
	return "", false
}

// simpleCommand is one command a command line runs: its words, and whether
// they are a script's loose words (quotes undone), not a command's place.
type simpleCommand struct {
	words []string
	loose bool
}

// deniedCommand is Denied for one simple command; atStart says cmd[0] is a
// command's place (a console matches there only), false for the loose words
// of a script.
func deniedCommand(cmd []string, repo config.FactoryRepo, atStart bool) (string, bool) {
	for i, w := range cmd {
		switch filepath.Base(w) {
		case "gh-ost":
			return "gh-ost changes a shared database's table, and only you run it (the runbook card): the factory never runs gh-ost", true
		case "kubectl":
			for _, v := range cmd[i+1:] {
				if slices.Contains(kubectlRemote, v) {
					return "kubectl " + v + " reaches into a cluster's pod: the factory never runs a remote console", true
				}
			}
		}
	}
	for _, entry := range repo.Consoles {
		ew := strings.Fields(entry)
		if len(ew) == 0 {
			continue
		}
		if (atStart && matchesAt(cmd, 0, ew)) || (len(ew) > 1 && slices.ContainsFunc(indexes(cmd), func(i int) bool { return matchesAt(cmd, i, ew) })) {
			return entry + " is a remote console of " + repo.Repo + " (its consoles): the factory never runs one", true
		}
	}
	return "", false
}

// indexes are the indexes of s.
func indexes[T any](s []T) []int {
	out := make([]int, len(s))
	for i := range s {
		out[i] = i
	}
	return out
}

// matchesAt reports whether cmd holds the words of a console entry from i:
// its first word by path (./bin/rc and /copy/bin/rc are bin/rc; a bare
// name matches a path's base), the rest as they are.
func matchesAt(cmd []string, i int, entry []string) bool {
	if i+len(entry) > len(cmd) || !samePath(cmd[i], entry[0]) {
		return false
	}
	return slices.Equal(cmd[i+1:i+len(entry)], entry[1:])
}

// samePath reports whether word runs the program entry names.
func samePath(word, entry string) bool {
	w, e := strings.TrimPrefix(filepath.Clean(word), "./"), strings.TrimPrefix(filepath.Clean(entry), "./")
	if !strings.Contains(e, "/") {
		return filepath.Base(w) == e
	}
	return w == e || strings.HasSuffix(w, "/"+e)
}

// commandsOf are the simple commands argv runs: argv itself without its
// wrappers, or, for a shell with a -c script, each command of the script
// (read again, depth bounded, for a shell inside it), and the script's
// loose words (quotes and substitutions undone), in which a console counts
// only as a run of two or more words.
func commandsOf(argv []string, depth int) []simpleCommand {
	words := unwrap(argv)
	if len(words) == 0 {
		return nil
	}
	script, ok := shellScript(words)
	if !ok || depth >= maxShellDepth {
		return []simpleCommand{{words: words}}
	}
	var out []simpleCommand
	for _, c := range shellCommands(script) {
		out = append(out, commandsOf(c, depth+1)...)
	}
	for _, c := range looseCommands(script) {
		out = append(out, simpleCommand{words: c, loose: true})
	}
	return out
}

// shellScript is the -c script of a shell command (sh, bash, zsh, dash).
func shellScript(words []string) (string, bool) {
	switch filepath.Base(words[0]) {
	case "sh", "bash", "zsh", "dash", "ksh":
	default:
		return "", false
	}
	for i := 1; i < len(words); i++ {
		w := words[i]
		if !strings.HasPrefix(w, "-") {
			return "", false
		}
		if strings.Contains(strings.TrimLeft(w, "-"), "c") && !strings.HasPrefix(w, "--") && i+1 < len(words) {
			return words[i+1], true
		}
	}
	return "", false
}

// optionArgs are the options of a wrapper that take the next word as their
// value.
var optionArgs = map[string][]string{
	"env":     {"-u", "-C", "-S", "-P", "--unset", "--chdir", "--split-string"},
	"nice":    {"-n", "--adjustment"},
	"sudo":    {"-u", "-g", "-C", "-D", "-h", "-p", "-U", "--user", "--group"},
	"timeout": {"-s", "-k", "--signal", "--kill-after"},
	"xargs":   {"-I", "-n", "-P", "-L", "-s", "-d", "-E", "-a"},
	"mise":    {"-C", "--cd", "-E", "--env", "-j", "--jobs"},
	"exec":    {"-a"},
}

// unwrap drops the wrappers in front of a command: leading NAME=value
// assignments, env, mise exec (up to its --), bundle exec, exec, command,
// nohup, time, nice, timeout (and its duration), xargs and sudo, with their
// options.
func unwrap(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		if isAssignment(w) {
			words = words[1:]
			continue
		}
		name := filepath.Base(w)
		switch name {
		case "env", "exec", "command", "nohup", "time", "nice", "sudo", "xargs", "caffeinate", "stdbuf":
			words = skipOptions(words[1:], optionArgs[name])
		case "timeout":
			if words = skipOptions(words[1:], optionArgs[name]); len(words) > 0 {
				words = words[1:] // the duration
			}
		case "bundle", "bundler":
			if len(words) < 2 || words[1] != "exec" {
				return words
			}
			words = skipOptions(words[2:], nil)
		case "mise", "rtx":
			rest := skipOptions(words[1:], optionArgs["mise"])
			if len(rest) == 0 || (rest[0] != "exec" && rest[0] != "x") {
				return words
			}
			if i := slices.Index(rest, "--"); i >= 0 {
				words = rest[i+1:]
			} else {
				words = skipOptions(rest[1:], optionArgs["mise"])
			}
		default:
			return words
		}
	}
	return words
}

// skipOptions drops the leading options of words (withArg take the next
// word too) and a "--" that ends them.
func skipOptions(words []string, withArg []string) []string {
	for len(words) > 0 && strings.HasPrefix(words[0], "-") {
		if words[0] == "--" {
			return words[1:]
		}
		if slices.Contains(withArg, words[0]) && len(words) > 1 {
			words = words[1:]
		}
		words = words[1:]
	}
	return words
}

// isAssignment reports whether w is a shell assignment, NAME=value.
func isAssignment(w string) bool {
	name, _, ok := strings.Cut(w, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// shellCommands splits a shell script into its simple commands, as words:
// quotes ('…', "…") and backslashes are undone, and ; & | ( ) newlines, `
// and $( end a command. A "$(…)" or `…` inside double quotes is read as the
// commands it holds. A # at a word's start ends the line.
func shellCommands(script string) [][]string {
	var cmds [][]string
	var cur []string
	var word strings.Builder
	inWord := false
	flushWord := func() {
		if inWord {
			cur = append(cur, word.String())
		}
		word.Reset()
		inWord = false
	}
	flushCmd := func() {
		flushWord()
		if len(cur) > 0 {
			cmds = append(cmds, cur)
		}
		cur = nil
	}
	s := script
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			if s[i] != '\n' {
				word.WriteByte(s[i])
				inWord = true
			}
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				end = len(s) - i - 1
			}
			word.WriteString(s[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case c == '"':
			j := i + 1
			for ; j < len(s) && s[j] != '"'; j++ {
				switch {
				case s[j] == '\\' && j+1 < len(s):
					j++
					word.WriteByte(s[j])
				case s[j] == '$' && j+1 < len(s) && s[j+1] == '(':
					inner, n := balanced(s[j+2:])
					cmds = append(cmds, shellCommands(inner)...)
					j += 1 + n
				case s[j] == '`':
					end := strings.IndexByte(s[j+1:], '`')
					if end < 0 {
						end = len(s) - j - 1
					}
					cmds = append(cmds, shellCommands(s[j+1:j+1+end])...)
					j += end + 1
				default:
					word.WriteByte(s[j])
				}
			}
			i = j
			inWord = true
		case c == ' ' || c == '\t':
			flushWord()
		case c == '#' && !inWord:
			for i < len(s) && s[i] != '\n' {
				i++
			}
			flushCmd()
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			flushCmd()
			i++
		case strings.IndexByte(";&|()\n`", c) >= 0:
			flushCmd()
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	flushCmd()
	return cmds
}

// balanced is the text of s up to the ")" that closes a "$(" before it,
// and how many bytes it took with that ")".
func balanced(s string) (string, int) {
	depth := 1
	for i := range len(s) {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return s[:i], i + 1
			}
		}
	}
	return s, len(s)
}

// looseCommands splits a script into commands of words with every quote
// undone (a quote is a space): the words a quoted substitution would run,
// looked at for gh-ost and kubectl's verbs only.
func looseCommands(script string) [][]string {
	var out [][]string
	for part := range strings.FieldsFuncSeq(script, func(r rune) bool { return strings.ContainsRune(";&|()\n`$", r) }) {
		if words := strings.FieldsFunc(part, func(r rune) bool { return r == ' ' || r == '\t' || r == '\'' || r == '"' || r == '\\' }); len(words) > 0 {
			out = append(out, words)
		}
	}
	return out
}

// guarded is a runner behind the deny list.
type guarded struct {
	next execx.Runner
	cfg  *config.Config
}

// Guard puts the deny list (Denied) in front of next, with the consoles of
// every [[factory.repo]] of cfg: a command it denies never starts, and its
// error (ErrDenied) says why. A runner Guard made is returned as it is; nil
// stays nil. The factory daemon runs every command of its own through one.
func Guard(next execx.Runner, cfg *config.Config) execx.Runner {
	if next == nil {
		return nil
	}
	if g, ok := next.(*guarded); ok {
		return g
	}
	return &guarded{next: next, cfg: cfg}
}

// Run runs c unless the deny list refuses it.
func (g *guarded) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	argv := append([]string{c.Name}, c.Args...)
	repos := []config.FactoryRepo{{}}
	if g.cfg != nil && len(g.cfg.Factory.Repos) > 0 {
		repos = g.cfg.Factory.Repos
	}
	for _, r := range repos {
		if why, ok := Denied(argv, r); ok {
			what := c.Label
			if what == "" {
				what = c.Name
			}
			return execx.Result{Code: -1}, fmt.Errorf("factory: %s: %w: %s", what, ErrDenied, why)
		}
	}
	return g.next.Run(ctx, c)
}
