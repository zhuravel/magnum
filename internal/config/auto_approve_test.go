package config

import (
	"strings"
	"testing"
)

// autoApproveConfig is validPipelineConfig with the operator's gh identity
// "z", an App "app" and a watch of acme's "app" and "web-*" repositories
// (not "web-docs") that posts as the App.
func autoApproveConfig() *Config {
	cfg := validPipelineConfig()
	cfg.Identities = append(cfg.Identities, Identity{Name: "app", Kind: "app", Login: "acme[bot]", AppID: 1, InstallationID: 2,
		ClientID: "Iv-x", PrivateKeyEnv: "KEY"})
	cfg.Watches[0].Include = []string{"app", "web-*"}
	cfg.Watches[0].Exclude = []string{"web-docs"}
	cfg.Watches[0].Identity = "app"
	return cfg
}

func strPtr(s string) *string { return &s }

// Auto-approval is off unless a watch names its repositories: no
// repository gets an identity to approve as.
func TestAutoApproveIsOffByDefault(t *testing.T) {
	cfg := autoApproveConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if id := cfg.AutoApproveFor("acme/app"); id != nil || cfg.AutoApproves() {
		t.Fatalf("AutoApproveFor = %+v, AutoApproves = %v without auto_approve", id, cfg.AutoApproves())
	}
	cfg.Watches[0].AutoApproveAs = "z" // the identity alone opts nothing in
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if id := cfg.AutoApproveFor("acme/app"); id != nil || cfg.AutoApproves() {
		t.Fatalf("auto_approve_as alone: AutoApproveFor = %+v", id)
	}
}

// auto_approve names repositories of its watch (a name as GitHub has it,
// any case, or "*") and needs auto_approve_as, an identity of kind gh: the
// operator's own account, whose approval GitHub counts (an App's does not).
func TestAutoApproveValidatesItsRepositoriesAndIdentity(t *testing.T) {
	cases := map[string]struct {
		edit func(w *Watch)
		want string
	}{
		"no identity":      {func(w *Watch) { w.AutoApprove = []string{"app"} }, "watch acme: auto_approve needs auto_approve_as"},
		"unknown identity": {func(w *Watch) { w.AutoApprove, w.AutoApproveAs = []string{"app"}, "ghost" }, `watch acme: unknown auto_approve_as "ghost"`},
		"an App":           {func(w *Watch) { w.AutoApprove, w.AutoApproveAs = []string{"app"}, "app" }, `auto_approve_as "app" must be an identity of kind gh`},
		"uncovered":        {func(w *Watch) { w.AutoApprove, w.AutoApproveAs = []string{"app", "other"}, "z" }, `auto_approve names "other", which the watch does not cover`},
		"excluded":         {func(w *Watch) { w.AutoApprove, w.AutoApproveAs = []string{"web-docs"}, "z" }, `auto_approve names "web-docs"`},
		"owner/name":       {func(w *Watch) { w.AutoApprove, w.AutoApproveAs = []string{"acme/app"}, "z" }, `auto_approve entry "acme/app" must be a repository name`},
		"a glob":           {func(w *Watch) { w.AutoApprove, w.AutoApproveAs = []string{"web-*"}, "z" }, `auto_approve entry "web-*" must be a repository name`},
		"empty":            {func(w *Watch) { w.AutoApprove, w.AutoApproveAs = []string{" "}, "z" }, `auto_approve entry " " must be a repository name`},
		"body": {func(w *Watch) {
			w.AutoApprove, w.AutoApproveAs, w.AutoApproveBody = []string{"app"}, "z", strPtr("{{.Nope}}")
		},
			"watch acme: auto_approve_body does not render"},
		"body lines": {func(w *Watch) {
			w.AutoApprove, w.AutoApproveAs, w.AutoApproveBody = []string{"app"}, "z", strPtr("one\ntwo")
		},
			"watch acme: auto_approve_body must be one line"},
		"unknown as alone": {func(w *Watch) { w.AutoApproveAs = "ghost" }, `unknown auto_approve_as "ghost"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := autoApproveConfig()
			tc.edit(&cfg.Watches[0])
			wantError(t, cfg.Validate(), tc.want)
		})
	}
	for _, names := range [][]string{{"app"}, {"App"}, {"web-site", "app"}, {"*"}} {
		cfg := autoApproveConfig()
		cfg.Watches[0].AutoApprove, cfg.Watches[0].AutoApproveAs = names, "z"
		if err := cfg.Validate(); err != nil {
			t.Errorf("auto_approve %q: %v", names, err)
		}
	}
}

// AutoApproveFor is the identity of the watch whose auto_approve names the
// repository ("*" names every repository the watch covers).
func TestAutoApproveForPicksTheNamedRepositories(t *testing.T) {
	cfg := autoApproveConfig()
	cfg.Watches[0].AutoApprove, cfg.Watches[0].AutoApproveAs = []string{"App"}, "z"
	if id := cfg.AutoApproveFor("Acme/app"); id == nil || id.Name != "z" || !cfg.AutoApproves() {
		t.Fatalf("acme/app: %+v", id)
	}
	for _, full := range []string{"acme/web-site", "acme/web-docs", "other/app", "app"} {
		if id := cfg.AutoApproveFor(full); id != nil {
			t.Errorf("%s: %+v", full, id)
		}
	}
	cfg.Watches[0].AutoApprove = []string{"*"}
	if id := cfg.AutoApproveFor("acme/web-site"); id == nil {
		t.Error(`"*" leaves out acme/web-site`)
	}
	if id := cfg.AutoApproveFor("acme/web-docs"); id != nil {
		t.Errorf(`"*" names an excluded repository: %+v`, id)
	}
}

// The approval's body is one line: the default names the reviewed commit
// in seven characters and links magnum's review; auto_approve_body replaces
// it with a template of the same data. The marker magnum recognises its
// approvals by is not part of it.
func TestAutoApproveBodyRendersTheTemplate(t *testing.T) {
	d := AutoApproveData{SHA: "0123456789abcdef0123456789abcdef01234567", ReviewURL: "https://github.com/acme/app/pull/7#pullrequestreview-9",
		Repo: "acme/app", Number: 7}
	cfg := autoApproveConfig()
	cfg.Watches[0].AutoApprove, cfg.Watches[0].AutoApproveAs = []string{"app"}, "z"
	got, err := RenderAutoApproveBody(cfg.AutoApproveBodyFor("acme/app"), d)
	want := "Auto-approved: magnum's review of `0123456` found no blocking problems ([review](https://github.com/acme/app/pull/7#pullrequestreview-9))."
	if err != nil || got != want {
		t.Fatalf("default body = %q, %v\nwant %q", got, err, want)
	}
	d.ReviewURL = ""
	if got, _ := RenderAutoApproveBody(DefaultAutoApproveBody, d); got != "Auto-approved: magnum's review of `0123456` found no blocking problems." {
		t.Errorf("without a review URL: %q", got)
	}
	cfg.Watches[0].AutoApproveBody = strPtr("LGTM per magnum ({{.Repo}}#{{.Number}} at {{.Short}})")
	if got, err := RenderAutoApproveBody(cfg.AutoApproveBodyFor("acme/app"), d); err != nil || got != "LGTM per magnum (acme/app#7 at 0123456)" {
		t.Errorf("template body = %q, %v", got, err)
	}
	if strings.Contains(DefaultAutoApproveBody, "<!--") {
		t.Error("the default body carries the marker")
	}
}

// The keys load from the user's config.toml.
func TestAutoApproveKeysLoad(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig + `auto_approve = ["*"]
auto_approve_as = "z"
auto_approve_body = "Approved after magnum's review of {{.Short}}."
`})
	w := cfg.Watches[0]
	if len(w.AutoApprove) != 1 || w.AutoApprove[0] != "*" || w.AutoApproveAs != "z" || w.AutoApproveBody == nil {
		t.Fatalf("watch = %+v", w)
	}
}
