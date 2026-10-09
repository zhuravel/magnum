package cli

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// walkCommands calls fn for root and every command below it.
func walkCommands(cmd *cobra.Command, fn func(*cobra.Command)) {
	fn(cmd)
	for _, sub := range cmd.Commands() {
		walkCommands(sub, fn)
	}
}

// useBrackets matches an innermost [...] group of a usage line.
var useBrackets = regexp.MustCompile(`\[[^\[\]]*\]`)

// needsArgument reports whether a usage line names a <placeholder> outside
// its [optional] groups: the command cannot run without that argument.
func needsArgument(use string) bool {
	for {
		s := useBrackets.ReplaceAllString(use, "")
		if s == use {
			return strings.Contains(s, "<")
		}
		use = s
	}
}

// A command that needs an argument, run without one, is a usage error (exit
// 2) whose usage line names the command: approve, request-changes and
// unapprove printed "usage: magnum <ref> [...]", without their name.
func TestEveryCommandThatNeedsAnArgumentPrintsItsOwnUsageLine(t *testing.T) {
	h := newActHarness(t)
	var paths [][]string
	walkCommands(newRoot(h.c), func(cmd *cobra.Command) {
		if cmd.Runnable() && cmd.HasParent() && needsArgument(cmd.Use) {
			paths = append(paths, strings.Fields(cmd.CommandPath()))
		}
	})
	if len(paths) < 20 {
		t.Fatalf("found %d commands that need an argument: %v", len(paths), paths)
	}
	for _, p := range paths {
		h.out.Reset()
		h.errb.Reset()
		path := strings.Join(p, " ")
		if code := execute(h.c, p[1:]); code != 2 {
			t.Errorf("%s: exit %d, want 2 (stderr %s)", path, code, h.errb.String())
			continue
		}
		if !namesItsUsage(h.errb.String(), p) {
			t.Errorf("%s: stderr lacks %q:\n%s", path, "usage: "+path+" ", h.errb.String())
		}
	}
}

// namesItsUsage reports whether stderr carries the usage line of the command
// at path: "usage: <path> ...". A subcommand prints its own line, never its
// family's (the slots subcommands printed every form of `magnum slots`).
func namesItsUsage(stderr string, path []string) bool {
	return strings.Contains(stderr, "usage: "+strings.Join(path, " ")+" ")
}

// approve, request-changes and unapprove take one PR: more than one is "one
// PR at a time", never "which PR?".
func TestVerdictCommandsTakeOnePRAtATime(t *testing.T) {
	h := newActHarness(t)
	for _, name := range []string{"approve", "request-changes", "unapprove"} {
		h.errb.Reset()
		if code := h.cmd(name, "5", "6"); code != 2 {
			t.Errorf("%s 5 6: exit %d, want 2", name, code)
		}
		actContains(t, h.errb.String(), "magnum "+name+": one PR at a time", "usage: magnum "+name+" <ref>")
		if strings.Contains(h.errb.String(), "which PR?") {
			t.Errorf("%s 5 6: %s", name, h.errb.String())
		}
	}
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("queued %+v", reqs)
	}
}

// useFlag matches a flag a usage line names: --name or -x.
var useFlag = regexp.MustCompile(`--([a-z][a-z0-9-]*)|(?:^|[\s\[|(])-([a-zA-Z])\b`)

// Every command's usage line names the flags it defines and no other: review
// and open took --workspace and --cwd (plugin context) without saying so.
func TestEveryUsageLineNamesExactlyTheCommandsFlags(t *testing.T) {
	c, _, _ := bareContext(t)
	seen := 0
	walkCommands(newRoot(c), func(cmd *cobra.Command) {
		if !cmd.Runnable() || !cmd.HasParent() {
			return
		}
		seen++
		var named []string
		for _, m := range useFlag.FindAllStringSubmatch(cmd.Use, -1) {
			if m[1] != "" {
				named = append(named, m[1])
				if cmd.Flags().Lookup(m[1]) == nil && cmd.InheritedFlags().Lookup(m[1]) == nil {
					t.Errorf("%s: usage names --%s, which it does not define: %s", cmd.CommandPath(), m[1], cmd.Use)
				}
				continue
			}
			f := cmd.Flags().ShorthandLookup(m[2])
			if f == nil {
				t.Errorf("%s: usage names -%s, which it does not define: %s", cmd.CommandPath(), m[2], cmd.Use)
				continue
			}
			named = append(named, f.Name)
		}
		cmd.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
			if !f.Hidden && f.Name != "help" && !slices.Contains(named, f.Name) {
				t.Errorf("%s: usage omits --%s: %s", cmd.CommandPath(), f.Name, cmd.Use)
			}
		})
	})
	if seen < 40 {
		t.Fatalf("walked %d commands", seen)
	}
}
