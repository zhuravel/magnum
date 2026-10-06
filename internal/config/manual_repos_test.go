package config

import "testing"

// manual_repos names repositories of its watch the way auto_approve does: a
// name as GitHub has it (any case), without owner or pattern, that the
// watch's include/exclude cover.
func TestManualReposValidatesItsRepositories(t *testing.T) {
	cases := map[string]struct {
		names []string
		want  string
	}{
		"uncovered":  {[]string{"app", "other"}, `watch acme: manual_repos names "other", which the watch does not cover (include/exclude)`},
		"excluded":   {[]string{"web-docs"}, `manual_repos names "web-docs"`},
		"owner/name": {[]string{"acme/app"}, `watch acme: manual_repos entry "acme/app" must be a repository name of the watch (no owner, no pattern)`},
		"a glob":     {[]string{"web-*"}, `manual_repos entry "web-*" must be a repository name`},
		"every one":  {[]string{"*"}, `manual_repos entry "*" must be a repository name`},
		"empty":      {[]string{" "}, `manual_repos entry " " must be a repository name`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := autoApproveConfig()
			cfg.Watches[0].ManualRepos = tc.names
			wantError(t, cfg.Validate(), tc.want)
		})
	}
	for _, names := range [][]string{nil, {"app"}, {"App"}, {"web-site", "app"}} {
		cfg := autoApproveConfig()
		cfg.Watches[0].ManualRepos = names
		if err := cfg.Validate(); err != nil {
			t.Errorf("manual_repos %q: %v", names, err)
		}
	}
}

// ManualRepo matches a repository name case-insensitively; a blank name
// (unknown) is never manual.
func TestManualRepoMatchesTheNamedRepositories(t *testing.T) {
	w := Watch{Owner: "acme", Include: []string{"*"}, ManualRepos: []string{"EXAMPLE"}}
	for name, want := range map[string]bool{"example": true, "EXAMPLE": true, "example-web": false, "app": false, "": false} {
		if got := w.ManualRepo(name); got != want {
			t.Errorf("ManualRepo(%q) = %v, want %v", name, got, want)
		}
	}
	if (Watch{}).ManualRepo("example") {
		t.Error("a watch without manual_repos has a manual repository")
	}
}

func TestManualReposKeyLoads(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"config.toml": minimalConfig + `manual_repos = ["example"]
`})
	if w := cfg.Watches[0]; len(w.ManualRepos) != 1 || w.ManualRepos[0] != "example" || !w.ManualRepo("example") {
		t.Fatalf("watch = %+v", w)
	}
}
