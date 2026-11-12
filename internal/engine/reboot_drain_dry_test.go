package engine

import (
	"testing"

	"github.com/zhuravel/magnum/internal/store"
)

// A cleanup is a dry run by its plan when it carries one (cleanupAsync
// applies the plan): options marked dry with a plan that is not still start
// slot work, and are refused under the drain.
func TestACleanupIsDryByItsPlanWhenItHasOne(t *testing.T) {
	for _, c := range []struct {
		name    string
		payload string
		dry     bool
	}{
		{"options dry, no plan", `{"options":{"dry_run":true}}`, true},
		{"options dry, a plan that applies", `{"options":{"dry_run":true},"plan":{"dry_run":false}}`, false},
		{"a dry plan", `{"options":{},"plan":{"dry_run":true}}`, true},
		{"a staging deploy's dry run", `{"dry_run":true}`, true},
	} {
		if got := requestDry(store.Request{Payload: []byte(c.payload)}); got != c.dry {
			t.Errorf("%s: dry %v, want %v", c.name, got, c.dry)
		}
	}
}
