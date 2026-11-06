package config

import (
	"cmp"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/zhuravel/magnum/internal/herdr"
)

// FactoryHerdr is the herdr session of the factory's panes: [factory]
// herdr_session, on [factory] herdr_socket, else on that session's own
// socket (herdr.SessionSocket; herdr's default session, "default", keeps
// herdr's default socket).
func (c *Config) FactoryHerdr() herdr.Session {
	name := cmp.Or(c.Factory.HerdrSession, "default")
	return herdr.Session{Name: name, Socket: cmp.Or(c.Factory.HerdrSocket, herdr.SessionSocket(name))}
}

// YourHerdr is your herdr session: [terminal] session ("" = herdr's
// default session) on [herdr] socket. Toasts show there, and the slot
// guards read your agents there.
func (c *Config) YourHerdr() herdr.Session {
	return herdr.Session{Name: cmp.Or(c.Terminal.Session, "default"), Socket: c.Herdr.Socket}
}

// ReviewHerdr is the herdr session of magnum's review panes: yours while
// [herdr] review_session is "", else that session, on [herdr] review_socket
// or the session's own socket (herdr.SessionSocket: "default" is herdr's
// default session on its default socket).
func (c *Config) ReviewHerdr() herdr.Session {
	name := c.Herdr.ReviewSession
	if name == "" {
		return c.YourHerdr()
	}
	return herdr.Session{Name: name, Socket: cmp.Or(c.Herdr.ReviewSocket, herdr.SessionSocket(name))}
}

// validateReviewHerdr checks [herdr] review_session and review_socket: a
// session name, an absolute socket that comes with a name (the reveal opens
// the session by its name), and never the factory's session, by name or by
// socket: the factory's copies take a pane of its session in a copy that no
// session of the factory's is in for someone's, so magnum's review panes
// there would hold them.
func (c *Config) validateReviewHerdr() []error {
	h := c.Herdr
	var errs []error
	if h.ReviewSession != "" && !herdrSessionRe.MatchString(h.ReviewSession) {
		errs = append(errs, fmt.Errorf("herdr.review_session %q must be a session name (letters, digits, . _ -), or \"\" for "+
			"your session", h.ReviewSession))
	}
	if h.ReviewSocket != "" && !filepath.IsAbs(h.ReviewSocket) {
		errs = append(errs, fmt.Errorf("herdr.review_socket must be absolute after expansion (or empty for the session's own "+
			"socket): %s", h.ReviewSocket))
	}
	if h.ReviewSocket != "" && h.ReviewSession == "" {
		errs = append(errs, errors.New("herdr.review_socket needs herdr.review_session: `magnum open` opens the review "+
			"panes' session by its name"))
	}
	if h.ReviewSession == "" || len(errs) > 0 {
		return errs
	}
	review, factory := c.ReviewHerdr(), c.FactoryHerdr()
	switch {
	case review.Name == factory.Name:
		errs = append(errs, fmt.Errorf("herdr.review_session %q is factory.herdr_session too: magnum's review panes would "+
			"hold the factory's copies", review.Name))
	case review.Socket == factory.Socket:
		errs = append(errs, fmt.Errorf("herdr.review_session %q is on the factory's socket %s: magnum's review panes would "+
			"hold the factory's copies", review.Name, review.Socket))
	}
	return errs
}
