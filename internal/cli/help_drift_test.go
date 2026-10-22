package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/tui"
)

// keyAtoms splits a help overlay's key label into the keys it names:
// "j/k ↑/↓" is j, k, ↑ and ↓, "ctrl+r / F5" is ctrl+r and F5, "enter, o" is
// enter and o, and "/" and "," alone are those keys.
func keyAtoms(label string) []string {
	if label == "/" || label == "," {
		return []string{label}
	}
	var out []string
	for f := range strings.FieldsSeq(label) {
		if f == "/" { // the separator of "g / G"
			continue
		}
		if f != "," {
			f = strings.TrimSuffix(f, ",")
		}
		for a := range strings.SplitSeq(f, "/") {
			if a != "" {
				out = append(out, a)
			}
		}
	}
	return out
}

// helpWords are the words of a help text as a reader finds a key in prose:
// "(R fresh," is R, "A/C approve" is A and C, "`,` opens" is ",", "/ filters" is "/".
func helpWords(help string) map[string]bool {
	words := map[string]bool{}
	for f := range strings.FieldsSeq(help) {
		w := strings.Trim(f, "(),.;:`\"'")
		if w == "" {
			w = strings.Trim(f, "`")
		}
		if w == "/" {
			words[w] = true
			continue
		}
		for part := range strings.SplitSeq(w, "/") {
			words[part] = true
		}
	}
	return words
}

// keysTheHelpLeavesToTheOverlay are keys the --help prose does not spell
// out: the arrows and paging that every list has, and esc.
var keysTheHelpLeavesToTheOverlay = []string{"↑", "↓", "pgup", "pgdn", "esc"}

// commandHelp is the text of `magnum <args> --help` on one line: cobra wraps
// the help, and a phrase may break between two of its words.
func commandHelp(t *testing.T, args ...string) string {
	t.Helper()
	c, out, errb := bareContext(t)
	if code := execute(c, append(args, "--help")); code != 0 {
		t.Fatalf("magnum %s --help: exit %d\n%s%s", strings.Join(args, " "), code, out, errb)
	}
	return strings.Join(strings.Fields(out.String()), " ")
}

// The help of `magnum status` and `magnum tui` names every key their
// screens' help overlays list: the `,` that opens the settings screen was in
// the overlays, the board's hint line and the README, and in neither help.
// A lowercase `a` is also an article, so a help that loses that key keeps
// passing; every other key is a word of its own.
func TestStatusAndTuiHelpNameEveryKeyOfTheirOverlays(t *testing.T) {
	for _, c := range []struct {
		command string
		labels  []string
	}{
		{"status", tui.DashboardHelpKeys()},
		{"tui", tui.BoardHelpKeys()},
	} {
		words := helpWords(commandHelp(t, c.command))
		if len(c.labels) < 20 {
			t.Fatalf("%s: the overlay lists %d keys: %v", c.command, len(c.labels), c.labels)
		}
		for _, label := range c.labels {
			for _, key := range keyAtoms(label) {
				if !words[key] && !slices.Contains(keysTheHelpLeavesToTheOverlay, key) {
					t.Errorf("magnum %s --help does not name the key %q (the overlay lists %q)", c.command, key, label)
				}
			}
		}
	}
	if got := keyAtoms("j/k ↑/↓"); !slices.Equal(got, []string{"j", "k", "↑", "↓"}) {
		t.Errorf("keyAtoms(j/k ↑/↓) = %q", got)
	}
	if got := keyAtoms("? / !"); !slices.Equal(got, []string{"?", "!"}) {
		t.Errorf("keyAtoms(? / !) = %q", got)
	}
	if got := keyAtoms("enter, o"); !slices.Equal(got, []string{"enter", "o"}) {
		t.Errorf("keyAtoms(enter, o) = %q", got)
	}
	if got := keyAtoms(","); !slices.Equal(got, []string{","}) {
		t.Errorf("keyAtoms(,) = %q", got)
	}
}

// doctorHelpEntry says which words of `magnum doctor --help` cover the
// checks that start with name.
type doctorHelpEntry struct{ name, words string }

// doctorHelpEntries covers every check doctor prints, by the words its help
// uses for them. The agent kinds' checks ("codex", "codex login", ...) are
// added from the config. A check with no entry fails the test, and so does
// an entry whose words the help lost.
var doctorHelpEntries = []doctorHelpEntry{
	{"config", "config"}, {"binary", "config"}, // doctorConfig's checks
	{"herdr", "herdr (version and socket methods)"},
	{"codex session hook", "Codex session hook"}, {"codex session ids", "Codex session ids"},
	{"skill", "judge skill"}, {"fix role", "the fix role"}, {"learn editor", "skill editor"},
	{"whiteboard", "Whiteboard CLI and server"},
	{"prompt", "role prompts and prompts_dir"}, {"prompts", "role prompts and prompts_dir"},
	{"gh", "gh and each identity"}, {"identity", "gh and each identity"}, {"jira", "Jira token file"},
	{"mysql", "MySQL"},
	{"clone", "main clones and their SSH origins"}, {"origin", "main clones and their SSH origins"},
	{"schema", "reset_db"},
	{"mise exec", "mise ("}, {"login shell", "login shell"},
	{"disk", "free disk"}, {"memory", "memory pressure and swap"}, {"codex staging", "codex staging"},
	{"launchd", "launchd agent"}, {"launchd mise", "the mise it starts the daemon through"},
	{"terminal", "Automation permission"}, {"plugin", "herdr plugin link"},
	{"registry", "registry"},
}

// `magnum doctor --help` lists what doctor checks: the memory check was in
// the doctor, the README and the status line, and not in the help.
func TestDoctorHelpNamesEveryCheck(t *testing.T) {
	help := strings.ToLower(commandHelp(t, "doctor"))
	_, d, _, _ := doctorFixture(t)
	entries := slices.Clone(doctorHelpEntries)
	for kind := range d.Config.Kinds {
		entries = append(entries, doctorHelpEntry{kind, "every agent CLI"})
	}
	for _, e := range entries {
		if !strings.Contains(help, strings.ToLower(e.words)) {
			t.Errorf("magnum doctor --help lacks %q, which covers the %q checks", e.words, e.name)
		}
	}
	seen := 0
	for _, c := range doctorRun(context.Background(), d) {
		seen++
		if !slices.ContainsFunc(entries, func(e doctorHelpEntry) bool {
			return c.Name == e.name || strings.HasPrefix(c.Name, e.name+" ") || strings.HasPrefix(c.Name, e.name+"_")
		}) {
			t.Errorf("doctor prints the check %q, which no entry of doctorHelpEntries covers: name it in the help and the table", c.Name)
		}
	}
	if seen < 25 {
		t.Fatalf("doctor ran %d checks", seen)
	}
}

// Every command that waits for a GitHub budget below [github] budget_floor
// says so in its help; only `magnum fix --help` did.
func TestHelpOfTheJobsTheGitHubFloorHoldsNamesTheFloor(t *testing.T) {
	for _, command := range []string{"fix", "retro", "notes", "whiteboard", "review"} {
		if help := commandHelp(t, command); !strings.Contains(help, "[github] budget_floor") {
			t.Errorf("magnum %s --help does not name [github] budget_floor", command)
		}
	}
}
