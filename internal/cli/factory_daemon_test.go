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
