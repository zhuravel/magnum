package config

import "testing"

// A login list entry written as an App's URL form ("app/helper") never matched anything:
// github.NormalizeLogin trims only "[bot]", so the entry sat in allow_bot_authors, skip_authors or
// auto_approve_skip_authors and did nothing, with no word of why. A "/" has no place in a login, so the
// config refuses it in those lists and in auto_approve_authors, on a [[watch]] and on a [[repo]] block,
// saying how to write the login; the plain login and its "[bot]" form pass, and so does a TOML file that
// loads.
func TestAnAppPrefixedLoginIsRefused(t *testing.T) {
	const hint = `entry "app/helper" must be a GitHub login: write helper or helper[bot], not app/helper`
	autoApprove := func(w *Watch) { w.AutoApprove, w.AutoApproveAs = []string{"*"}, "z" }
	for _, c := range []struct {
		list  string
		watch func(w *Watch, logins []string)
		repo  func(r *Repo, logins []string) // nil: the list is a watch's alone
	}{
		{"allow_bot_authors", func(w *Watch, l []string) { w.AllowBotAuthors = l }, func(r *Repo, l []string) { r.AllowBotAuthors = l }},
		{"skip_authors", func(w *Watch, l []string) { w.SkipAuthors = l }, nil},
		{"auto_approve_skip_authors", func(w *Watch, l []string) { autoApprove(w); w.AutoApproveSkipAuthors = l },
			func(r *Repo, l []string) { r.AutoApproveSkipAuthors = l }},
		{"auto_approve_authors", func(w *Watch, l []string) { autoApprove(w); w.AutoApproveAuthors = l },
			func(r *Repo, l []string) { r.AutoApproveAuthors = l }},
	} {
		cfg := autoApproveConfig()
		c.watch(&cfg.Watches[0], []string{"helper", "app/helper"})
		wantError(t, cfg.Validate(), "watch acme: "+c.list+" "+hint)

		for _, ok := range [][]string{{"helper"}, {"helper[bot]"}, {"Helper", "HELPER[bot]", "other"}} {
			cfg = autoApproveConfig()
			c.watch(&cfg.Watches[0], ok)
			if err := cfg.Validate(); err != nil {
				t.Errorf("watch %s %q: %v", c.list, ok, err)
			}
		}
		if c.repo == nil {
			continue
		}
		cfg = autoApproveConfig()
		autoApprove(&cfg.Watches[0])
		cfg.Repos = []Repo{{Repo: "acme/app"}}
		c.repo(&cfg.Repos[0], []string{"app/helper"})
		wantError(t, cfg.Validate(), "repo acme/app: "+c.list+" "+hint)

		for _, ok := range [][]string{{"helper"}, {"helper[bot]"}} {
			cfg = autoApproveConfig()
			autoApprove(&cfg.Watches[0])
			cfg.Repos = []Repo{{Repo: "acme/app"}}
			c.repo(&cfg.Repos[0], ok)
			if err := cfg.Validate(); err != nil {
				t.Errorf("repo %s %q: %v", c.list, ok, err)
			}
		}
	}

	// The message names the entry as written and the login after its last "/".
	cfg := autoApproveConfig()
	cfg.Watches[0].SkipAuthors = []string{"Apps/Dependabot"}
	wantError(t, cfg.Validate(), `skip_authors entry "Apps/Dependabot" must be a GitHub login: write Dependabot or Dependabot[bot], not Apps/Dependabot`)
	cfg.Watches[0].SkipAuthors = []string{"app/"}
	wantError(t, cfg.Validate(), `skip_authors entry "app/" must be a GitHub login (no "/")`)

	// skip_authors has always read an entry with spaces around it or an empty one (the eligibility check
	// trims it and skips the blank), so only the "/" is new there.
	cfg = autoApproveConfig()
	cfg.Watches[0].SkipAuthors = []string{" dependabot ", ""}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a padded or empty skip_authors entry: %v", err)
	}

	// The loader refuses it too, with the block named.
	_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": minimalConfig + "skip_authors = [\"app/helper\"]\n"})
	wantError(t, err, "watch acme: skip_authors "+hint)
}
