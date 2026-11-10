package runtime

import (
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// The heal stops a herdr server only when the session is the factory's own:
// never herdr's default session, never yours ([terminal] session), never the
// review daemon's ([herdr] review_session), by name or by socket, even when a
// config names one of them as the factory's.
func TestTheHealStopsOnlyTheFactorysOwnHerdrSession(t *testing.T) {
	cfg := func(factory, factorySocket, terminal, review, yourSocket string) *config.Config {
		c := &config.Config{}
		c.Factory.HerdrSession, c.Factory.HerdrSocket = factory, factorySocket
		c.Terminal.Session, c.Herdr.ReviewSession, c.Herdr.Socket = terminal, review, yourSocket
		return c
	}
	for _, tc := range []struct {
		name string
		c    *config.Config
		own  bool
	}{
		{"the factory's own session", cfg("factory", "", "", "reviews", ""), true},
		{"herdr's default session", cfg("", "", "", "reviews", ""), false},
		{"named default", cfg("default", "", "", "reviews", ""), false},
		{"the review daemon's session", cfg("reviews", "", "", "reviews", ""), false},
		{"your terminal's session", cfg("work", "", "work", "reviews", ""), false},
		{"your session's socket", cfg("factory", "/tmp/x/herdr.sock", "", "reviews", "/tmp/x/herdr.sock"), false},
		{"the review session's socket", func() *config.Config {
			c := cfg("factory", "/tmp/r/herdr.sock", "", "reviews", "")
			c.Herdr.ReviewSocket = "/tmp/r/herdr.sock"
			return c
		}(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownSession(tc.c); got != tc.own {
				t.Errorf("ownSession = %v, want %v", got, tc.own)
			}
		})
	}
}
