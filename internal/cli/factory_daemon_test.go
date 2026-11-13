package cli

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// The factory daemon runs the copies' setup, reset and precheck commands
// itself, so it needs the login shell's PATH as the review daemon does:
// under launchd's PATH alone, talkable's test setup found no DBngin
// redis-server and every copy's setup failed.
func TestTheFactoryDaemonTakesItsPATHFromTheLoginShell(t *testing.T) {
	st := newShellPATHTest(t, "echo '/usr/bin:/Applications/DBngin.app/redis-7.2.4/bin:/bin'")
	c := &Context{Version: "test", Layout: paths.Layout{Home: t.TempDir()}, Config: st.cfg, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	storetest.Seed(t, c.Layout.FactoryAgentsDB())
	registrytest.Seed(t, c.Layout.FactoryDB())

	d, err := startFactoryDaemon(context.Background(), c, slog.LevelInfo)
	if err != nil {
		t.Fatalf("the factory daemon does not start: %v", err)
	}
	d.Close()

	if want := launchdTestPATH + ":/Applications/DBngin.app/redis-7.2.4/bin"; st.env["PATH"] != want {
		t.Fatalf("PATH = %s\nwant   %s", st.env["PATH"], want)
	}
	log, err := os.ReadFile(c.Layout.FactoryLog())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "PATH from zsh: 1 directory added") {
		t.Fatalf("factory.log does not say what the shell added:\n%s", log)
	}
}

// The factory daemon runs rubocop in its copies, which crashes on
// "invalid byte sequence in US-ASCII" without a locale: under launchd's
// environment, which names none, it sets LANG=en_US.UTF-8 and says so.
func TestTheFactoryDaemonSetsAUTF8LocaleWhenLaunchdGivesNone(t *testing.T) {
	st := newShellPATHTest(t, "echo '/usr/bin:/bin'")
	st.cfg.Daemon.ShellPath = false
	c := &Context{Version: "test", Layout: paths.Layout{Home: t.TempDir()}, Config: st.cfg, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	storetest.Seed(t, c.Layout.FactoryAgentsDB())
	registrytest.Seed(t, c.Layout.FactoryDB())

	d, err := startFactoryDaemon(context.Background(), c, slog.LevelInfo)
	if err != nil {
		t.Fatalf("the factory daemon does not start: %v", err)
	}
	d.Close()

	if st.env["LANG"] != "en_US.UTF-8" {
		t.Fatalf("LANG = %q, want en_US.UTF-8", st.env["LANG"])
	}
	log, err := os.ReadFile(c.Layout.FactoryLog())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "LANG=en_US.UTF-8") {
		t.Fatalf("factory.log does not say it set the locale:\n%s", log)
	}
}

// The factory daemon records the PATH it runs with, in its agents store,
// once its environment is ready: launchd's, with the login shell's
// directories under [daemon] shell_path, or launchd's alone with the key
// off. `magnum doctor` weighs a missing redis-cli against it instead of
// rebuilding it from today's configuration, which a daemon started before a
// change of the key, or whose shell did not answer, does not share (C3 of
// T3b's third check). The log never names the directories.
func TestTheFactoryDaemonRecordsThePATHItRunsWith(t *testing.T) {
	for _, tc := range []struct {
		name      string
		shellPath bool
		want      string
	}{
		{"with the login shell's directories", true, launchdTestPATH + ":/Applications/DBngin.app/redis-7.2.4/bin"},
		{"with shell_path off", false, launchdTestPATH},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newShellPATHTest(t, "echo '/usr/bin:/Applications/DBngin.app/redis-7.2.4/bin:/bin'")
			st.cfg.Daemon.ShellPath = tc.shellPath
			c := &Context{Version: "test", Layout: paths.Layout{Home: t.TempDir()}, Config: st.cfg, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
			storetest.Seed(t, c.Layout.FactoryAgentsDB())
			registrytest.Seed(t, c.Layout.FactoryDB())

			d, err := startFactoryDaemon(context.Background(), c, slog.LevelInfo)
			if err != nil {
				t.Fatalf("the factory daemon does not start: %v", err)
			}
			d.Close()

			got, ok := readFactoryDaemonPATH(context.Background(), c.Layout.FactoryAgentsDB())
			if !ok || got.PID != os.Getpid() || got.Path != tc.want {
				t.Fatalf("the recorded PATH = %+v (found %v), want pid %d and PATH %s", got, ok, os.Getpid(), tc.want)
			}
			log, err := os.ReadFile(c.Layout.FactoryLog())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(log), "DBngin") {
				t.Fatalf("factory.log names a directory of the PATH:\n%s", log)
			}
		})
	}
}
