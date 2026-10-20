package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// [webhooks] is off by default, with a 25 s long poll and a stream that goes
// stale after 90 s, in Defaults() and in the committed defaults.
func TestWebhooksDefaultToOff(t *testing.T) {
	w := Defaults().Webhooks
	if w.Mode != WebhooksOff || w.URL != "" || w.TokenFile != "" || len(w.Owners) != 0 ||
		w.Wait.Duration != 25*time.Second || w.StaleAfter.Duration != 90*time.Second {
		t.Fatalf("webhooks defaults: %+v", w)
	}
	if w.Shadow() {
		t.Fatal("the default webhooks mode reads the relay")
	}
	cfg, err := loadCommittedWithLocal(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Webhooks.Mode != WebhooksOff || cfg.Webhooks.Wait.Duration != 25*time.Second || cfg.Webhooks.StaleAfter.Duration != 90*time.Second {
		t.Fatalf("the committed defaults: %+v", cfg.Webhooks)
	}
}

// webhooksConfig is a valid configuration whose [webhooks] table is w, with
// a token file of mode 0600 at the path it returns (w.TokenFile "token"
// names it).
func webhooksConfig(t *testing.T, w Webhooks) (*Config, string) {
	t.Helper()
	token := filepath.Join(t.TempDir(), "relay.token")
	if err := os.WriteFile(token, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.Herdr.Socket = "/tmp/herdr.sock"
	cfg.Identities = []Identity{{Name: "z", Kind: "gh", Login: "z"}}
	cfg.Watches = []Watch{{Owner: "talkable", Include: []string{"*"}, Identity: "z", PollIdentity: "z"}}
	if w.Wait.Duration == 0 {
		w.Wait = cfg.Webhooks.Wait
	}
	if w.StaleAfter.Duration == 0 {
		w.StaleAfter = cfg.Webhooks.StaleAfter
	}
	if w.TokenFile == "token" {
		w.TokenFile = token
	}
	cfg.Webhooks = w
	return cfg, token
}

// The mode is off ("" too) or shadow; active, the phase that acts on the
// events, is refused with what this build does instead, and anything else
// names the values.
func TestWebhooksModeValues(t *testing.T) {
	for _, mode := range []string{"", WebhooksOff, WebhooksShadow} {
		cfg, _ := webhooksConfig(t, Webhooks{Mode: mode, URL: "https://relay.example", TokenFile: "token", Owners: []string{"talkable"}})
		if err := cfg.Validate(); err != nil {
			t.Errorf("mode %q: %v", mode, err)
		}
	}
	for mode, want := range map[string]string{
		"active": `webhooks.mode "active" is not available yet`,
		"Shadow": `webhooks.mode must be off or shadow, got "Shadow"`,
		"on":     `webhooks.mode must be off or shadow, got "on"`,
	} {
		cfg, _ := webhooksConfig(t, Webhooks{Mode: mode, URL: "https://relay.example", TokenFile: "token", Owners: []string{"talkable"}})
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("mode %q: err = %v, want %q", mode, err, want)
		}
	}
}

// Shadow mode reads the relay over HTTPS only: a URL that is not https, has
// no host, carries credentials, a query or a fragment, or characters curl's
// config would have to quote, is refused, and so are no URL and no token
// file.
func TestWebhooksURLIsHTTPSOnly(t *testing.T) {
	for _, ok := range []string{"https://relay.example", "https://relay.example/", "https://relay.example:8443/magnum"} {
		cfg, _ := webhooksConfig(t, Webhooks{Mode: WebhooksShadow, URL: ok, TokenFile: "token", Owners: []string{"talkable"}})
		if err := cfg.Validate(); err != nil {
			t.Errorf("url %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "http://relay.example", "relay.example", "https://", "https://user:pw@relay.example",
		"https://relay.example/?after=1", "https://relay.example/#x", "https://relay.example/a b", `https://relay.example/"x`,
		`https://relay.example/a\b`, "ftp://relay.example", "HTTPS://relay.example"} {
		cfg, _ := webhooksConfig(t, Webhooks{Mode: WebhooksShadow, URL: bad, TokenFile: "token", Owners: []string{"talkable"}})
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "webhooks.url") {
			t.Errorf("url %q: err = %v, want a webhooks.url error", bad, err)
		}
	}
	cfg, _ := webhooksConfig(t, Webhooks{Mode: WebhooksShadow, URL: "https://relay.example", Owners: []string{"talkable"}})
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "webhooks.token_file") {
		t.Errorf("no token_file: err = %v", err)
	}
	cfg.Webhooks.Mode = WebhooksOff
	if err := cfg.Validate(); err != nil {
		t.Errorf("webhooks off without a token file: %v", err)
	}
}

// The token file must be a regular file only its owner can read or write
// (0600, or 0400): the bearer token reads every event of the App's
// installations. One open to its group or others, not a regular file, or
// missing is a warning naming it (the daemon never reads the relay with it,
// and the stream says why), never a config error: shadow mode must not keep
// the daemon from starting.
func TestWebhooksTokenFileMode(t *testing.T) {
	cfg, token := webhooksConfig(t, Webhooks{Mode: WebhooksShadow, URL: "https://relay.example", TokenFile: "token", Owners: []string{"talkable"}})
	for _, mode := range []os.FileMode{0o600, 0o400} {
		if err := os.Chmod(token, mode); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("mode %04o: %v", mode, err)
		}
		if w := cfg.Warnings(); len(w) != 0 {
			t.Errorf("mode %04o: warnings %q", mode, w)
		}
	}
	for _, mode := range []os.FileMode{0o640, 0o644, 0o606, 0o660} {
		if err := os.Chmod(token, mode); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("mode %04o: %v, want a warning only", mode, err)
		}
		if w := strings.Join(cfg.Warnings(), "\n"); !strings.Contains(w, "webhooks.token_file "+token) || !strings.Contains(w, "chmod 600") {
			t.Errorf("mode %04o: warnings %q, want the token file named", mode, w)
		}
	}
	dir := t.TempDir()
	for file, want := range map[string]string{dir: "not a regular file", filepath.Join(dir, "missing.token"): "does not exist"} {
		cfg.Webhooks.TokenFile = file
		if err := cfg.Validate(); err != nil {
			t.Errorf("token file %s: %v", file, err)
		}
		if w := strings.Join(cfg.Warnings(), "\n"); !strings.Contains(w, file) || !strings.Contains(w, want) {
			t.Errorf("token file %s: warnings %q, want %q", file, w, want)
		}
	}
}

// The long poll waits 1 s to 30 s (the relay waits at most 30 s), and the
// stream counts as stale only past the longest a page may take (the wait and
// curl's 10 s more), so a quiet healthy stream never reads as stale.
func TestWebhooksWaitAndStaleAfter(t *testing.T) {
	for _, tc := range []struct {
		wait, stale time.Duration
		want        string
	}{
		{wait: 25 * time.Second, stale: 90 * time.Second},
		{wait: time.Second, stale: 12 * time.Second},
		{wait: 30 * time.Second, stale: 41 * time.Second},
		{wait: 500 * time.Millisecond, stale: time.Minute, want: "webhooks.wait must be between 1s and 30s"},
		{wait: 31 * time.Second, stale: 2 * time.Minute, want: "webhooks.wait must be between 1s and 30s"},
		{wait: 25 * time.Second, stale: 35 * time.Second, want: "webhooks.stale_after must be longer than 35s"},
	} {
		cfg, _ := webhooksConfig(t, Webhooks{Mode: WebhooksShadow, URL: "https://relay.example", TokenFile: "token", Owners: []string{"talkable"},
			Wait: Duration{tc.wait}, StaleAfter: Duration{tc.stale}})
		err := cfg.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("wait %s, stale_after %s: %v", tc.wait, tc.stale, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("wait %s, stale_after %s: err = %v, want %q", tc.wait, tc.stale, err, tc.want)
		}
	}
}

// owners are watch owners: an empty entry or one with a slash is refused,
// one no [[watch]] has is a warning (it is never compared), and so is
// shadow mode with no owner at all.
func TestWebhooksOwners(t *testing.T) {
	cfg, _ := webhooksConfig(t, Webhooks{Mode: WebhooksShadow, URL: "https://relay.example", TokenFile: "token", Owners: []string{"Talkable"}})
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("warnings = %q, want none: an owner matches a watch in any case", w)
	}
	if !cfg.Webhooks.Covers("talkable") || cfg.Webhooks.Covers("example") {
		t.Errorf("Covers: talkable %v, example %v", cfg.Webhooks.Covers("talkable"), cfg.Webhooks.Covers("example"))
	}
	for _, bad := range []string{"", "talkable/talkable", " "} {
		cfg.Webhooks.Owners = []string{bad}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "webhooks.owners") {
			t.Errorf("owner %q: err = %v", bad, err)
		}
	}
	cfg.Webhooks.Owners = []string{"example"}
	if w := strings.Join(cfg.Warnings(), "\n"); !strings.Contains(w, `webhooks.owners names "example", which no [[watch]] has`) {
		t.Errorf("warnings = %q", w)
	}
	cfg.Webhooks.Owners = nil
	if w := strings.Join(cfg.Warnings(), "\n"); !strings.Contains(w, "webhooks.owners is empty") {
		t.Errorf("warnings = %q", w)
	}
	cfg.Webhooks.Mode = WebhooksOff
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("warnings with webhooks off = %q, want none", w)
	}
}

// The user config's [webhooks] keys override the base's key by key, and a
// relative token_file is read beside the user config, as a private key file
// is.
func TestWebhooksOverlayMergesKeyByKey(t *testing.T) {
	cfg, err := loadCommittedWithLocal(t, `
[webhooks]
mode = "shadow"
url = "https://relay.example"
token_file = "keys/relay.token"
owners = ["talkable"]
`)
	if err != nil {
		t.Fatal(err)
	}
	w := cfg.Webhooks
	if w.Mode != WebhooksShadow || w.URL != "https://relay.example" || len(w.Owners) != 1 || w.Wait.Duration != 25*time.Second ||
		w.StaleAfter.Duration != 90*time.Second {
		t.Fatalf("merged webhooks: %+v", w)
	}
	if want := filepath.Join(filepath.Dir(cfg.Sources[1]), "keys", "relay.token"); w.TokenFile != want {
		t.Fatalf("token_file = %q, want %q", w.TokenFile, want)
	}
}
