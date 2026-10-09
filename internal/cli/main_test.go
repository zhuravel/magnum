package cli

import (
	"fmt"
	"os"
	"testing"
)

// TestMain keeps every test of the package away from the user's real magnum
// home, config and sockets: the variables config.Load, app.New and the
// command helpers read are cleared, and HOME points at an empty directory.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "magnum-cli-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// MAGNUM_REPORT_DIR and MAGNUM_PR_URL mark a review agent's pane, where
	// the commands that act for the operator refuse to run (refuseInAgentPane).
	for _, k := range []string{"MAGNUM_CONFIG", "MAGNUM_HOME", "MAGNUM_MYSQL_DSN", "MAGNUM_PICK_QUERY", "MAGNUM_BIN",
		"MAGNUM_REPORT_DIR", "MAGNUM_PR_URL",
		"HERDR_SOCKET_PATH", "HERDR_BIN_PATH", "HERDR_PLUGIN_ID", "HERDR_PLUGIN_CONTEXT_JSON", "HERDR_ENV"} {
		os.Unsetenv(k)
	}
	os.Setenv("HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
