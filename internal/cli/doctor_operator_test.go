package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// doctor warns when a watch polls as an App and no gh identity is declared
// to stand for the operator (config.OperatorIdentity): review requests to
// his login start no round and his own PRs do not count as his. A gh poll
// identity, or a gh identity beside the App, warns nothing.
func TestDoctorWarnsWhenAnAppPollerLeavesNoOperatorLogin(t *testing.T) {
	const name = "identity of watch talkable"
	for _, tc := range []struct {
		name  string
		mod   func(cfg *config.Config)
		warns bool
	}{
		{"a gh poll identity", func(*config.Config) {}, false},
		{"an App polls, a gh identity is declared", func(cfg *config.Config) {
			cfg.Watches[0].PollIdentity = "talkable-app"
		}, false},
		{"an App polls, no gh identity is declared", func(cfg *config.Config) {
			cfg.Watches = cfg.Watches[:1]
			cfg.Watches[0].PollIdentity = "talkable-app"
			cfg.Identities = slices.DeleteFunc(cfg.Identities, func(id config.Identity) bool { return id.Kind == "gh" })
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, d, _, _ := doctorFixture(t)
			cfg := *d.Config
			cfg.Watches = slices.Clone(cfg.Watches)
			cfg.Identities = slices.Clone(cfg.Identities)
			tc.mod(&cfg)
			d.Config = &cfg
			c, ok := doctorByName(doctorIdentities(context.Background(), d))[name]
			if !tc.warns {
				if ok {
					t.Fatalf("%s = %+v, want no such check", name, c)
				}
				return
			}
			if !ok || c.Status != doctorWarn ||
				!strings.Contains(c.Detail, "watch talkable polls as the App talkable-app and no gh identity is declared") ||
				!strings.Contains(c.Detail, "review requests to your login start no round") ||
				!strings.Contains(c.Fix, `kind = "gh"`) {
				t.Fatalf("%s = %+v (found %v)", name, c, ok)
			}
		})
	}
}
