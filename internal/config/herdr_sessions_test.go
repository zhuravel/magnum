package config

import (
	"strings"
	"testing"
)

// The factory's panes live in its own herdr session: the socket [factory]
// herdr_socket names, else the named session's own, never the review panes'
// default one.
func TestTheFactorySessionHasItsOwnHerdrSocket(t *testing.T) {
	cfg := Defaults()
	if got := cfg.FactoryHerdr(); got.Name != "factory" || !strings.HasSuffix(got.Socket, "/.config/herdr/sessions/factory/herdr.sock") {
		t.Fatalf("session %+v, want the factory session on its own socket", got)
	}
	cfg.Factory.HerdrSocket = "/run/herdr/factory.sock"
	if got := cfg.FactoryHerdr(); got.Name != "factory" || got.Socket != "/run/herdr/factory.sock" {
		t.Fatalf("session %+v, want the factory session on the configured socket", got)
	}
}
