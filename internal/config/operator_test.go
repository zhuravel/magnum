package config

import "testing"

// operatorConfig declares the operator's gh identity me, a second gh
// identity me-too and the App reviewer-app, and one watch posting as the
// App.
func operatorConfig(poll, approveAs string, ids ...Identity) *Config {
	cfg := Defaults()
	cfg.Identities = ids
	cfg.Watches = []Watch{{Owner: "example", Include: []string{"*"}, Identity: "reviewer-app", PollIdentity: poll, AutoApproveAs: approveAs}}
	return cfg
}

var (
	opMe    = Identity{Name: "me", Kind: "gh", Login: "your-login"}
	opMeToo = Identity{Name: "me-too", Kind: "gh", Login: "your-other-login"}
	opApp   = Identity{Name: "reviewer-app", Kind: "app", Login: "your-app[bot]"}
)

// The operator's own login (review requests, own PRs, the footer) is the
// watch's poll identity when that is a gh identity, as before; under an App
// poll identity it is auto_approve_as, else the first gh identity declared,
// else none: an App's login is never the operator's.
func TestTheOperatorIdentityIsTheGhPollerElseAutoApproveAsElseTheFirstGhIdentity(t *testing.T) {
	for _, tc := range []struct {
		name            string
		poll, approveAs string
		ids             []Identity
		want            string
	}{
		{"a gh poll identity", "me-too", "", []Identity{opApp, opMe, opMeToo}, "me-too"},
		{"a gh poll identity wins over auto_approve_as", "me-too", "me", []Identity{opApp, opMe, opMeToo}, "me-too"},
		{"an App polls: auto_approve_as", "reviewer-app", "me-too", []Identity{opApp, opMe, opMeToo}, "me-too"},
		{"an App polls without auto_approve_as: the first gh identity", "reviewer-app", "", []Identity{opApp, opMeToo, opMe}, "me-too"},
		{"no poll identity set: the posting identity, an App", "", "", []Identity{opApp, opMe}, "me"},
		{"an App polls and no gh identity is declared", "reviewer-app", "", []Identity{opApp}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := operatorConfig(tc.poll, tc.approveAs, tc.ids...)
			got := cfg.OperatorIdentity(cfg.Watches[0])
			switch {
			case tc.want == "" && got != nil:
				t.Fatalf("OperatorIdentity = %+v, want none", *got)
			case tc.want != "" && (got == nil || got.Name != tc.want):
				t.Fatalf("OperatorIdentity = %+v, want %s", got, tc.want)
			}
			wantLogin := ""
			if got != nil {
				wantLogin = got.Login
			}
			if login := cfg.OperatorLogin(cfg.Watches[0]); login != wantLogin {
				t.Fatalf("OperatorLogin = %q, want %q", login, wantLogin)
			}
		})
	}
}

// For a watch that polls as a gh identity, which every config did before
// an App could poll, the operator's login is the poll login it always was.
func TestAGhPollersOperatorLoginIsItsPollLogin(t *testing.T) {
	cfg := operatorConfig("me", "me-too", opApp, opMeToo, opMe)
	cfg.Watches = append(cfg.Watches, Watch{Owner: "your-login", Include: []string{"*"}, Identity: "me-too", PollIdentity: "me-too"})
	for _, w := range cfg.Watches {
		if got, want := cfg.OperatorLogin(w), cfg.IdentityByName(w.PollIdentity).Login; got != want {
			t.Errorf("watch %s: OperatorLogin = %q, want its poll login %q", w.Owner, got, want)
		}
	}
}
