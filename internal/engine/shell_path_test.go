package engine

import (
	"strings"
	"testing"
)

// The daemon's start records what it took from the login shell's PATH
// ([daemon] shell_path): one info event that names the count, never a
// directory, and the count in kv for `magnum status`; a shell that gave no
// PATH a warn event; with the key off an earlier start's row goes.
func TestTheDaemonRecordsItsShellPATHAtStartup(t *testing.T) {
	h := newHarness(t)
	h.e.shellPATH = &ShellPATH{Shell: "zsh", Added: []string{"/opt/dbngin/mysql-8.0/bin", "/opt/dbngin/redis-7.2/bin"}, Count: 2}
	h.startup()
	evs := h.events("daemon.shell_path")
	if len(evs) != 1 || evs[0].Level != "info" || evs[0].Message != "PATH from zsh: 2 directories added" {
		t.Fatalf("events = %+v", evs)
	}
	got, ok := ReadShellPATH(h.ctx, h.st)
	if !ok || got.Shell != "zsh" || got.Count != 2 || got.Failed != "" || got.Added != nil {
		t.Fatalf("ReadShellPATH = %+v, %v", got, ok)
	}
	if v, _, _ := h.st.GetKV(h.ctx, KVDaemonShellPATH); strings.Contains(v, "dbngin") {
		t.Fatalf("the kv row names a directory: %s", v)
	}

	h.e.shellPATH = &ShellPATH{Shell: "zsh", Failed: "answered nothing in 20s"}
	h.startup()
	evs = h.events("daemon.shell_path")
	if len(evs) != 2 || evs[1].Level != "warn" || evs[1].Message != "the login shell (zsh) answered nothing in 20s. The launchd PATH stays" {
		t.Fatalf("events after a failed shell = %+v", evs)
	}
	if got, ok := ReadShellPATH(h.ctx, h.st); !ok || got.Failed != "answered nothing in 20s" || got.Count != 0 {
		t.Fatalf("ReadShellPATH after a failed shell = %+v, %v", got, ok)
	}

	h.e.shellPATH = nil
	h.startup()
	if _, ok := ReadShellPATH(h.ctx, h.st); ok {
		t.Fatal("the key off kept the earlier start's row")
	}
	if evs := h.events("daemon.shell_path"); len(evs) != 2 {
		t.Fatalf("the key off recorded an event: %+v", evs)
	}
}
