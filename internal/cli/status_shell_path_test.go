package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
)

// With [daemon] shell_path on, the daemon line of `magnum status` says how
// many directories the running daemon took from the login shell's PATH, or
// that the shell gave none; without a daemon, or with the key off, nothing.
func TestStatusShowsTheShellPATHOnTheDaemonLine(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	ctx := context.Background()
	d.DaemonPID = func() (int, error) { return 4242, nil }
	daemonLine := func() (statusReport, string) {
		t.Helper()
		r, err := statusGather(ctx, d, statusOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		statusRender(&b, r)
		for line := range strings.SplitSeq(b.String(), "\n") {
			if strings.HasPrefix(line, "daemon:") {
				return r, line
			}
		}
		t.Fatalf("no daemon line in\n%s", b.String())
		return r, ""
	}

	if r, line := daemonLine(); r.Daemon.ShellPATH != nil || strings.Contains(line, "PATH") {
		t.Fatalf("the key off: %+v, %q", r.Daemon.ShellPATH, line)
	}
	if err := st.SetKV(ctx, engine.KVDaemonShellPATH, `{"shell":"zsh","added":4}`); err != nil {
		t.Fatal(err)
	}
	r, line := daemonLine()
	if r.Daemon.ShellPATH == nil || r.Daemon.ShellPATH.Count != 4 || !strings.HasSuffix(line, ", PATH +4 directories from zsh") {
		t.Fatalf("4 added: %+v, %q", r.Daemon.ShellPATH, line)
	}
	if err := st.SetKV(ctx, engine.KVDaemonShellPATH, `{"shell":"zsh","added":0,"failed":"answered nothing in 20s"}`); err != nil {
		t.Fatal(err)
	}
	if _, line := daemonLine(); !strings.HasSuffix(line, ", no PATH from zsh (answered nothing in 20s)") {
		t.Fatalf("failed: %q", line)
	}

	d.DaemonPID = func() (int, error) { return 0, nil }
	if _, line := daemonLine(); strings.Contains(line, "PATH") {
		t.Fatalf("no daemon: %q", line)
	}
}
