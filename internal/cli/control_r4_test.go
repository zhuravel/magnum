package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/factory"
)

// A daemon whose pidfile is there before its record of the controls is
// starting, not an older build; a factory whose record cannot be read is
// unknown: neither line says "older build".
func TestStatusSaysStartingOrUnknownRatherThanAnOlderBuild(t *testing.T) {
	h, f := newControlHarness(t)
	if err := h.st.SetKV(h.ctx, engine.KVDaemonRebootDrain, engine.RebootDrain{Generation: 1, Since: h.now}.Value()); err != nil {
		t.Fatal(err)
	}
	review := reviewControlOf(storeKV(h.ctx, h.st), true, 3) // pid 3 recorded neither its pid nor its controls yet
	if line := review.line(h.now); strings.Contains(line, "older build") || !strings.Contains(line, "starting") {
		t.Errorf("review line %q, want it starting", line)
	}

	f.pid = 4300
	if err := os.MkdirAll(filepath.Dir(h.c.Layout.FactoryWork()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.c.Layout.FactoryWork(), []byte("{half a record"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := factory.WritePause(h.c.Layout, factory.Pause{Generation: 1, Since: h.now}.Value()); err != nil {
		t.Fatal(err)
	}
	if line := factoryControlOf(h.c.Layout, f.pid).line(h.now); strings.Contains(line, "older build") || !strings.Contains(line, "cannot be read") {
		t.Errorf("factory line %q, want its build unknown", line)
	}
}
