package config

import (
	"cmp"

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
