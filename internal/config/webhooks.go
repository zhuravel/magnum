package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/webhooks"
)

// The [webhooks] modes. Off reads nothing. Shadow reads the relay's events
// (magnum-relay, which receives the GitHub App's webhook deliveries) and
// compares what each would refresh with what the poll finds, changing
// nothing the daemon does. Active, acting on the events, is a later phase:
// Validate refuses it.
const (
	WebhooksOff    = "off"
	WebhooksShadow = "shadow"
	WebhooksActive = "active"
)

// Webhooks is the [webhooks] table: the daemon's reader of the relay.
type Webhooks struct {
	// Mode is off ("" too) or shadow (WebhooksOff, WebhooksShadow).
	Mode string `toml:"mode"`
	// URL is the relay's base URL, https only; the events are read from
	// <url>/v1/events.
	URL string `toml:"url"`
	// TokenFile holds the relay's bearer token: a regular file of mode 0600
	// (~ and a path relative to the user config's directory resolved). The
	// token goes to curl on stdin, never in argv, a log or an error.
	TokenFile string `toml:"token_file"`
	// Owners are the watch owners the App is installed on, whose polls are
	// compared with the events; the others are never compared.
	Owners []string `toml:"owners"`
	// Wait is how long a page waits for an event (the relay's long poll).
	Wait Duration `toml:"wait"`
	// StaleAfter is how long the stream may go without a page before it is
	// unhealthy.
	StaleAfter Duration `toml:"stale_after"`
}

// Shadow reports whether the daemon reads the relay in shadow mode.
func (w Webhooks) Shadow() bool { return w.Mode == WebhooksShadow }

// Covers reports whether the App's events cover the watch owner (Owners,
// in any case).
func (w Webhooks) Covers(owner string) bool {
	for _, o := range w.Owners {
		if strings.EqualFold(o, owner) {
			return true
		}
	}
	return false
}

// validateWebhooks checks the [webhooks] table: the mode, and in shadow mode
// an https URL, a token file named, and owners that are owner names; the
// wait and stale_after whatever the mode. The token file itself is a
// warning's (webhooksWarnings): shadow mode must change nothing the daemon
// does, so a token file that is missing or open to others keeps the daemon
// from reading the relay (webhooks.ReadToken refuses it), never from
// starting.
func (c *Config) validateWebhooks() []error {
	w := c.Webhooks
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("webhooks."+format, args...)) }
	switch w.Mode {
	case "", WebhooksOff, WebhooksShadow:
	case WebhooksActive:
		bad(`mode %q is not available yet: this build runs "shadow" only, which compares the relay's events with the poll and changes nothing`, w.Mode)
	default:
		bad("mode must be off or shadow, got %q", w.Mode)
	}
	if w.URL != "" || w.Shadow() {
		if err := checkRelayURL(w.URL); err != nil {
			bad("url %v", err)
		}
	}
	if w.TokenFile == "" && w.Shadow() {
		bad("token_file must name the file holding the relay's token in shadow mode")
	}
	for _, o := range w.Owners {
		if strings.TrimSpace(o) == "" || strings.ContainsAny(o, "/ ") {
			bad("owners entry %q must be a watch owner (no repository, no spaces)", o)
		}
	}
	if d := w.Wait.Duration; d < time.Second || d > webhooks.MaxWait {
		bad("wait must be between 1s and %s (the relay waits at most %s), got %s", webhooks.MaxWait, webhooks.MaxWait, d)
	}
	if page := w.Wait.Duration + webhooks.PageSlack; w.StaleAfter.Duration <= page {
		bad("stale_after must be longer than %s (a page answers within wait and %s more), got %s", page, webhooks.PageSlack, w.StaleAfter.Duration)
	}
	return errs
}

// webhooksWarnings are the [webhooks] problems that do not make the config
// invalid, in shadow mode: a token file that does not exist, is not a
// regular file or is open to its group or others (the daemon never reads it
// then, and the stream says why), no owner (nothing is compared) and an
// owner no [[watch]] has (never compared).
func (c *Config) webhooksWarnings() []string {
	w := c.Webhooks
	if !w.Shadow() {
		return nil
	}
	var out []string
	if w.TokenFile != "" {
		switch err := webhooks.CheckTokenFile(w.TokenFile); {
		case errors.Is(err, fs.ErrNotExist):
			out = append(out, fmt.Sprintf("webhooks.token_file %s does not exist: the daemon cannot read the relay until it does", w.TokenFile))
		case err != nil:
			out = append(out, fmt.Sprintf("webhooks.token_file %v; the daemon does not read the relay with it", err))
		}
	}
	if len(w.Owners) == 0 {
		out = append(out, "webhooks.owners is empty: shadow mode counts the relay's events but compares no poll with them")
	}
	for _, o := range w.Owners {
		found := false
		for _, wa := range c.Watches {
			found = found || strings.EqualFold(wa.Owner, o)
		}
		if !found {
			out = append(out, fmt.Sprintf("webhooks.owners names %q, which no [[watch]] has: it is never compared", o))
		}
	}
	return out
}

// checkRelayURL accepts an https URL with a host and no credentials, query
// or fragment, made of characters that need no quoting in curl's config.
func checkRelayURL(raw string) error {
	if raw == "" {
		return errors.New("must be the relay's https:// URL in shadow mode")
	}
	if !strings.HasPrefix(raw, "https://") {
		return fmt.Errorf("must be an https:// URL, got %q", raw)
	}
	if strings.ContainsFunc(raw, func(r rune) bool { return r <= ' ' || r == '"' || r == '\\' || r == 0x7f }) {
		return fmt.Errorf("%q holds a space, a quote, a backslash or a control character", raw)
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Errorf("%q: %w", raw, err)
	case u.Host == "":
		return fmt.Errorf("%q names no host", raw)
	case u.User != nil:
		return fmt.Errorf("%q carries credentials: the token goes in token_file", raw)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#"):
		return fmt.Errorf("%q has a query or a fragment: name the relay's base URL", raw)
	}
	return nil
}
