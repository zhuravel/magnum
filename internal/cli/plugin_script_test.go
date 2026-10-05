package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	magnum "github.com/zhuravel/magnum"
)

// pluginScript runs the herdr plugin's script with a fake magnum (it prints
// what the test says per verb) and a fake herdr that records the toasts.
type pluginScript struct {
	t                  *testing.T
	dir, calls, toasts string
}

func newPluginScript(t *testing.T) *pluginScript {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	p := &pluginScript{t: t, dir: dir, calls: filepath.Join(dir, "calls"), toasts: filepath.Join(dir, "toasts")}
	files := map[string]string{
		"scripts/magnum-ctl.sh": string(magnum.PluginScript),
		// The fake magnum: $OUT_<verb> on stdout, $ERR_<verb> on stderr,
		// exit $RC_<verb>.
		"fake-magnum": `#!/bin/sh
echo "$*" >> "$CALLS"
v=$(echo "$1" | tr '-' '_')
eval "out=\${OUT_$v:-}; err=\${ERR_$v:-}; rc=\${RC_$v:-0}"
[ -n "$out" ] && printf '%b\n' "$out"
[ -n "$err" ] && printf '%b\n' "$err" >&2
exit "$rc"
`,
		"fake-herdr": `#!/bin/sh
shift 3 # notification show magnum
[ "$1" = "--body" ] && printf '%s\n' "$2" >> "$TOASTS"
`,
	}
	for rel, body := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// run runs the script with args and env (KEY=value) and returns the toasts
// it sent.
func (p *pluginScript) run(env []string, args ...string) []string {
	p.t.Helper()
	_ = os.Remove(p.toasts)
	cmd := exec.Command("bash", append([]string{filepath.Join(p.dir, "scripts", "magnum-ctl.sh")}, args...)...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + p.dir, "MAGNUM_BIN=" + filepath.Join(p.dir, "fake-magnum"),
		"HERDR_BIN_PATH=" + filepath.Join(p.dir, "fake-herdr"), "CALLS=" + p.calls, "TOASTS=" + p.toasts}, env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		p.t.Fatalf("magnum-ctl.sh %v: %v\n%s", args, err, out)
	}
	b, _ := os.ReadFile(p.toasts)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// The restart action used to toast only a success, so a restart refused
// while rounds ran said nothing: it waits with --when-idle and toasts its
// last line either way.
func TestPluginDaemonRestartToastsItsOutcome(t *testing.T) {
	p := newPluginScript(t)
	got := p.run([]string{"OUT_daemon_restart=waiting for 1 round(s) in flight to end\\nrestarted zhuravel.magnum via launchd"}, "daemon-restart")
	if len(got) != 1 || got[0] != "restarted zhuravel.magnum via launchd" {
		t.Fatalf("success toasts = %q", got)
	}
	if b, _ := os.ReadFile(p.calls); !strings.Contains(string(b), "daemon-restart --when-idle") {
		t.Fatalf("calls = %s", b)
	}
	got = p.run([]string{"RC_daemon_restart=1", "ERR_daemon_restart=magnum daemon-restart: rounds were still in flight after 2h0m0s\\nfix: retry with a longer --timeout"}, "daemon-restart")
	if len(got) != 1 || got[0] != "fix: retry with a longer --timeout" {
		t.Fatalf("failure toasts = %q", got)
	}
}

// Any action that fails toasts its last stderr line (else its last output
// line); temporary files are removed.
func TestPluginActionFailuresToastTheirLastLine(t *testing.T) {
	p := newPluginScript(t)
	if got := p.run([]string{"RC_attention=1", "OUT_attention=nothing needs you"}, "attention"); len(got) != 1 || got[0] != "nothing needs you" {
		t.Fatalf("attention toasts = %q", got)
	}
	if got := p.run([]string{"RC_ui=1", "ERR_ui=herdr is not running\\nfix: start herdr"}, "popup", "status"); len(got) != 1 || got[0] != "fix: start herdr" {
		t.Fatalf("popup toasts = %q", got)
	}
	if got := p.run([]string{"RC_pin=1", "ERR_pin=magnum pin: no magnum PR in herdr workspace w1"}, "here", "pin"); len(got) != 1 || got[0] != "magnum pin: no magnum PR in herdr workspace w1" {
		t.Fatalf("here toasts = %q", got)
	}
	if got := p.run([]string{"OUT_pin=pinned talkable/talkable#5"}, "here", "pin"); len(got) != 1 || got[0] != "pinned talkable/talkable#5" {
		t.Fatalf("here success toasts = %q", got)
	}
	left, _ := filepath.Glob(filepath.Join(p.dir, "magnum-ctl.*"))
	if len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}
