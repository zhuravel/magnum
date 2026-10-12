package agents

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// codex-review's sandbox still reaches the databases on localhost: it ran
// specs on the slot at the same time as the other roles and lost their
// fixture rows. Its line, from the command and from the full-line template,
// runs `codex review` through the checkout's db-lock (db-lock execs codex
// from PATH, so the operator's shell function stays out as `command` kept
// it out), and the checkout is the slot the round runs on.
func TestCodexReviewRunsUnderTheDBLock(t *testing.T) {
	e := newEnv(t)
	const slot = "/slots/talkable review3"
	d := ShellData{BaseRef: "origin/master", ReportPath: "/r/codex.md", Marker: DoneMarker("r-1"), RunID: "r-1", Checkout: slot}
	role := e.spec(RoleCodexReview)
	sh := role
	sh.Command, sh.Prompt = "", "codex-review.sh"
	lock := DBLockLine(e.cfg.Layout.Binary(), slot, string(RoleCodexReview))
	if want := "db-lock --checkout '/slots/talkable review3' --role codex-review --"; !strings.HasSuffix(lock, want) {
		t.Fatalf("DBLockLine = %s, want it to end with %s", lock, want)
	}
	// The lock is the first word of the command, inside the braces that tee
	// the report, after the report's run marker.
	want := "{ printf '" + ReportMarker("r-1") + `\n'; ` + lock + " codex review -c model_reasoning_effort=high "
	for name, r := range map[string]config.Role{"command": role, "codex-review.sh": sh} {
		got, err := e.m.ShellLine(e.ctx, e.pr.ID, r, d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(got, want) {
			t.Errorf("%s: line = %s\nwant it to contain %s", name, got, want)
		}
		if !strings.Contains(got, " --base origin/master; } | tee /r/codex.md;") || strings.Contains(got, "command codex") {
			t.Errorf("%s: line = %s\nwant the review's end unchanged and no bare codex", name, got)
		}
	}
}

// A shell line's DBLockCommand is the caller's when set (the fix job does
// the same for a role's), else derived from the executable, the checkout
// and the role, which the package-level ShellLine does without a Manager.
func TestShellDataDBLockCommandIsKeptOrDerived(t *testing.T) {
	role := defaultRole(t, RoleCodexReview)
	base := ShellData{BaseRef: "origin/master", ReportPath: "/r/codex.md", Marker: DoneMarker("r-1")}

	own := base
	own.DBLockCommand = "/opt/magnum/bin/magnum db-lock --checkout /slots/x --role mine --timeout 5m --"
	if got, err := ShellLine(role, own); err != nil || !strings.Contains(got, "; "+own.DBLockCommand+" codex review -c model_reasoning_effort=high --base origin/master; } |") {
		t.Errorf("a DBLockCommand set by the caller: line = %q, %v", got, err)
	}

	derived := base
	derived.Magnum, derived.Checkout = "/opt/magnum dir/bin/magnum", "/slots/talkable.review3"
	if got, err := ShellLine(role, derived); err != nil || !strings.Contains(got,
		`; '/opt/magnum dir/bin/magnum' db-lock --checkout /slots/talkable.review3 --role codex-review -- codex review -c`) {
		t.Errorf("a derived DBLockCommand: line = %q, %v", got, err)
	}

	// Without a checkout db-lock takes the git work tree it runs in; the
	// check of the configuration (engine skew) renders the line this way.
	if got, err := ShellLine(role, base); err != nil || !strings.Contains(got, "; magnum db-lock --role codex-review -- codex review -c") {
		t.Errorf("a derived DBLockCommand without a checkout: line = %q, %v", got, err)
	}
}

// The lock must not change what the done marker reports: db-lock exits with
// the command's own status (75 when the wait timed out, which is no
// review), the report holds the command's stdout only (db-lock's notes go
// to stderr, which stays in the pane), and the marker still follows the
// status of the group, not tee's. Run by a real shell, with a stand-in for
// magnum that does what `magnum db-lock` does to its command.
func TestCodexReviewLineKeepsTheCommandsStatusUnderTheLock(t *testing.T) {
	sh, err := exec.LookPath("zsh")
	if err != nil {
		if sh, err = exec.LookPath("bash"); err != nil {
			t.Skip("no zsh or bash")
		}
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	writeFile(t, filepath.Join(bin, "magnum"), `#!/bin/sh
[ "$1" = db-lock ] || exit 125
shift
while [ "$#" -gt 0 ] && [ "$1" != -- ]; do shift; done
shift
if [ -n "$LOCK_TIMEOUT" ]; then echo "db-lock: waited 20m0s for judge (rspec); the check did not run" >&2; exit 75; fi
echo "db-lock: waiting for judge (rspec)" >&2
exec "$@"
`, 0o755)
	writeFile(t, filepath.Join(bin, "codex"), "#!/bin/sh\necho \"verdict from codex $*\"\necho transcript >&2\nexit ${CODEX_STATUS:-0}\n", 0o755)

	role := defaultRole(t, RoleCodexReview)
	marker := DoneMarker("r-1")
	waitRe := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(marker) + `(?:[ \t]+\d+)?[ \t]*$`)
	verdict := "verdict from codex review -c model_reasoning_effort=high --base origin/master\n"
	for name, tc := range map[string]struct {
		env    []string
		status int
		report string
	}{
		"codex passes":   {nil, 0, verdict},
		"codex fails":    {[]string{"CODEX_STATUS=3"}, 3, verdict},
		"lock timed out": {[]string{"LOCK_TIMEOUT=1"}, 75, ""},
	} {
		t.Run(name, func(t *testing.T) {
			report := filepath.Join(t.TempDir(), "codex-review.md")
			line, err := ShellLine(role, ShellData{BaseRef: "origin/master", ReportPath: report, Marker: marker, RunID: "r-1",
				Magnum: filepath.Join(bin, "magnum"), Checkout: dir})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(sh, "-c", line)
			// A home of its own: zsh reads ~/.zshenv on every start, and the
			// operator's must not run in a test.
			cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + dir, "ZDOTDIR=" + dir, "LC_ALL=C"}, tc.env...)
			out, _ := cmd.CombinedOutput()
			m := waitRe.FindString(string(out))
			if m == "" {
				t.Fatalf("no marker line in %q", out)
			}
			if got := doneStatus(m, marker); got != tc.status {
				t.Errorf("marker status %d, want %d (output %q)", got, tc.status, out)
			}
			b, _ := os.ReadFile(report)
			if want := ReportMarker("r-1") + "\n" + tc.report; string(b) != want {
				t.Errorf("report = %q, want %q", b, want)
			}
		})
	}
}
