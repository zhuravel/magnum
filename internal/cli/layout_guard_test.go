package cli

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/zhuravel/magnum/internal/store/storetest"
)

// When paths.Resolve fails, the root refuses every command with its error:
// without the guard a registry-opening command ran on the zero layout, and
// app.New created ./state/magnum.db in whatever directory the command ran in.
func TestABrokenLayoutRefusesACommandBeforeItOpensTheRegistry(t *testing.T) {
	storetest.Serial(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("MAGNUM_CONFIG", "")
	layoutErr := errors.New("MAGNUM_HOME=/nowhere is not a magnum checkout")
	out, errb := &bytes.Buffer{}, &bytes.Buffer{}
	c := &Context{Version: "test", Stdout: out, Stderr: errb, layoutErr: layoutErr}
	if code := execute(c, []string{"where", "5"}); code != 1 {
		t.Fatalf("exit %d, want 1; stderr %s", code, errb)
	}
	if got := errb.String(); got != "magnum: "+layoutErr.Error()+"\n" || out.Len() != 0 {
		t.Errorf("stderr %q stdout %q", got, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("state/ in the working directory: %v", err)
	}
}
