package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/paths"
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

// magnum's review panes stay in your herdr session unless your config names
// another: review_session is "" in the Go defaults and the built-in file, so
// the panes' session is [terminal] session on [herdr] socket.
func TestTheReviewPanesStayInYourSessionByDefault(t *testing.T) {
	for _, cfg := range []*Config{Defaults(), mustLoadDefaults(t)} {
		yours := cfg.YourHerdr()
		if yours.Name != "default" || yours.Socket != cfg.Herdr.Socket || yours.Socket == "" {
			t.Errorf("your session %+v, want [terminal] session default on [herdr] socket %q", yours, cfg.Herdr.Socket)
		}
		if got := cfg.ReviewHerdr(); got != yours {
			t.Errorf("the review panes' session %+v, want yours %+v", got, yours)
		}
	}
	cfg := Defaults()
	cfg.Terminal.Session, cfg.Herdr.Socket = "work", "/run/herdr/work.sock"
	if got, want := cfg.ReviewHerdr(), (herdr.Session{Name: "work", Socket: "/run/herdr/work.sock"}); got != want {
		t.Errorf("the review panes' session %+v, want yours %+v", got, want)
	}
	cfg.Terminal.Session = ""
	if got := cfg.YourHerdr(); got.Name != "default" {
		t.Errorf("your session %+v with no [terminal] session, want herdr's default", got)
	}
}

// A named review session is herdr's: its own server on its own socket. The
// name "default" is herdr's default session, on herdr's default socket, so
// with [herdr] socket there too the two are one session.
func TestANamedReviewSessionHasItsOwnSocket(t *testing.T) {
	cfg := mustLoadDefaults(t)
	cfg.Herdr.ReviewSession = "reviews"
	if got, want := cfg.ReviewHerdr(), (herdr.Session{Name: "reviews", Socket: herdr.SessionSocket("reviews")}); got != want ||
		!strings.HasSuffix(got.Socket, "/.config/herdr/sessions/reviews/herdr.sock") {
		t.Fatalf("the review panes' session %+v, want %+v", got, want)
	}
	if got := cfg.YourHerdr(); got.Name != "default" || got.Socket != cfg.Herdr.Socket {
		t.Fatalf("your session %+v moved with the review panes'", got)
	}
	cfg.Herdr.ReviewSession = "default"
	if got := cfg.ReviewHerdr(); got != cfg.YourHerdr() {
		t.Fatalf("review_session default is %+v, want your session %+v", got, cfg.YourHerdr())
	}
}

// review_socket names the review session's socket when it is not herdr's
// own for the name; ~ expands as in [herdr] socket.
func TestAReviewSocketWinsOverTheSessionsOwn(t *testing.T) {
	cfg := Defaults()
	cfg.Herdr.ReviewSession, cfg.Herdr.ReviewSocket = "reviews", "/run/herdr/reviews.sock"
	if got, want := cfg.ReviewHerdr(), (herdr.Session{Name: "reviews", Socket: "/run/herdr/reviews.sock"}); got != want {
		t.Fatalf("the review panes' session %+v, want %+v", got, want)
	}
	loaded, _, err := loadUser(t, "[herdr]\nreview_session = \"reviews\"\nreview_socket = \"~/herdr/reviews.sock\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.ReviewHerdr().Socket; got != paths.Expand("~/herdr/reviews.sock") || !filepath.IsAbs(got) {
		t.Fatalf("review_socket %q, want ~ expanded", got)
	}
}

func TestAReviewSessionMustBeASessionName(t *testing.T) {
	cfg := validPipelineConfig()
	for _, name := range []string{"", "reviews", "default", "review.2"} {
		cfg.Herdr.ReviewSession = name
		if err := cfg.Validate(); err != nil {
			t.Fatalf("review_session %q: %v", name, err)
		}
	}
	for _, name := range []string{"two words", "-reviews", "a/b"} {
		cfg.Herdr.ReviewSession = name
		wantError(t, cfg.Validate(), "herdr.review_session", name)
	}
}

func TestAReviewSocketMustBeAbsolute(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Herdr.ReviewSession, cfg.Herdr.ReviewSocket = "reviews", "run/reviews.sock"
	wantError(t, cfg.Validate(), "herdr.review_socket", "absolute", "run/reviews.sock")
	cfg.Herdr.ReviewSocket = "/run/reviews.sock"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The reveal opens the review panes' session by its name, so a socket
// alone names no session to open.
func TestAReviewSocketNeedsAReviewSession(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Herdr.ReviewSocket = "/run/reviews.sock"
	wantError(t, cfg.Validate(), "herdr.review_socket", "herdr.review_session")
}

// The factory's copies count every pane of the factory's herdr session in
// a copy that no session of its own is in as someone's: magnum's review
// panes there would hold its copies. So the review session is never the
// factory's, by name or by socket.
func TestTheReviewSessionCannotBeTheFactorys(t *testing.T) {
	cfg := validPipelineConfig()
	cfg.Herdr.ReviewSession = cfg.Factory.HerdrSession
	wantError(t, cfg.Validate(), "herdr.review_session", "factory.herdr_session", cfg.Factory.HerdrSession)

	cfg.Herdr.ReviewSession, cfg.Herdr.ReviewSocket = "reviews", "/run/herdr/factory.sock"
	cfg.Factory.HerdrSocket = "/run/herdr/factory.sock"
	wantError(t, cfg.Validate(), "herdr.review_session", "the factory's socket", "/run/herdr/factory.sock")

	cfg.Herdr.ReviewSocket, cfg.Factory.HerdrSocket = "", herdr.SessionSocket("reviews")
	wantError(t, cfg.Validate(), "herdr.review_session", "the factory's socket")

	cfg.Factory.HerdrSocket = ""
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

// Your config names the review session the way the operator's does: the
// key alone, which leaves [herdr]'s other keys and your session as they were.
func TestAUserConfigSetsTheReviewSession(t *testing.T) {
	defaults := mustLoadDefaults(t)
	cfg, _, err := loadUser(t, "[herdr]\nreview_session = \"reviews\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ReviewHerdr(); got.Name != "reviews" || got.Socket != herdr.SessionSocket("reviews") {
		t.Fatalf("the review panes' session %+v, want reviews on its own socket", got)
	}
	if cfg.Herdr.Socket != defaults.Herdr.Socket || cfg.Herdr.Notify != defaults.Herdr.Notify || cfg.YourHerdr() != defaults.YourHerdr() {
		t.Fatalf("[herdr] %+v, want the defaults' socket and notify, your session as it was (%+v)", cfg.Herdr, defaults.YourHerdr())
	}
}
