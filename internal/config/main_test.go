package config

import (
	"os"
	"testing"
)

// TestMain clears $MAGNUM_CONFIG: Load reads it before the layout's files,
// and a test that passes no file must load those, never the operator's.
func TestMain(m *testing.M) {
	os.Unsetenv("MAGNUM_CONFIG")
	os.Exit(m.Run())
}
