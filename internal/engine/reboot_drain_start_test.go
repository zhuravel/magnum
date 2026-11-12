package engine

import (
	"testing"
)

// The start records the controls it knows right beside its pid, before any
// other record of its start (its build, its config, its PATH, the CLIs'
// versions): the CLI reads a daemon whose pid is recorded without its
// controls as an older build, so no gap may show that.
func TestTheStartRecordsItsControlsBesideItsPid(t *testing.T) {
	h := newHarness(t)
	for _, when := range []string{"INSERT", "UPDATE"} {
		if _, err := h.st.DB().ExecContext(h.ctx, `CREATE TRIGGER controls_at_build_`+when+` AFTER `+when+` ON kv
			WHEN NEW.key = 'daemon.build'
			BEGIN INSERT OR REPLACE INTO kv (key, value, updated_at) VALUES ('test.controls_at_build',
				COALESCE((SELECT value FROM kv WHERE key = 'daemon.controls'), 'missing'), NEW.updated_at); END`); err != nil {
			t.Fatal(err)
		}
	}
	h.startup()
	v, ok, err := h.st.GetKV(h.ctx, "test.controls_at_build")
	if err != nil || !ok {
		t.Fatalf("the start wrote no build (%v, %v)", ok, err)
	}
	if v == "missing" {
		t.Fatal("the start recorded its build before its controls: the CLI saw its pid without them")
	}
}
