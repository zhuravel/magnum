package app

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/github"
)

// appPollConfig is testConfig with the talkable watch polling as the App it
// posts as; the gh identity zhuravel stays declared.
var appPollConfig = strings.Replace(testConfig, `poll_identity = "zhuravel"`, `poll_identity = "talkable-app"`, 1)

// Under an App poll identity the App's rounds still know the operator's own
// login (config.OperatorIdentity), so a PR the gh login authored counts as
// self-authored and its judge gets COMMENT both ways
// (pipeline's TestSelfAuthoredPRGetsCommentBothWays); before, SelfLogin was
// the App's login and the App could approve or request changes on it.
func TestUnderAnAppPollerTheRoundsSelfLoginIsTheOperators(t *testing.T) {
	for _, tc := range []struct{ name, cfg string }{{"a gh poll identity", testConfig}, {"an App poll identity", appPollConfig}} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := testAppWith(t, tc.cfg, Options{})
			if got := a.Pipeline["talkable-app"].SelfLogin; got != "zhuravel" {
				t.Fatalf("the App's rounds' self login = %q, want zhuravel", got)
			}
			if got := a.Pipeline["zhuravel"].SelfLogin; got != "zhuravel" {
				t.Fatalf("the gh identity's rounds' self login = %q, want zhuravel", got)
			}
		})
	}
}

// The inventory reads as a gh identity when one is declared, though
// the first watch polls as an App: the operator's login sees every
// repository he does, an installation only the ones it covers.
func TestTheInventoryReadsAsTheOperatorsGhIdentity(t *testing.T) {
	for _, tc := range []struct{ name, cfg string }{{"a gh poll identity", testConfig}, {"an App poll identity", appPollConfig}} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := testAppWith(t, tc.cfg, Options{})
			if got, ok := a.Inventory.GitHub.(*github.Client); !ok || got != a.GitHub("zhuravel") {
				t.Fatalf("the inventory's client is the App's: %v, want zhuravel's", got == a.GitHub("talkable-app"))
			}
		})
	}
}
