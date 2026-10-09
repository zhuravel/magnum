package pipeline

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// A round's events reach the log at their level with the round's subject
// and the event's kind: a round that ends in error logs at Error, its start
// at Info. Every line used to arrive at Info.
func TestARoundsEventsLogAtTheirLevel(t *testing.T) {
	e := newEnv(t)
	id := e.r.Identity.(fakeIdentity)
	id.envErr = errors.New("no private key")
	e.r.Identity = id

	if _, err := e.r.RunRound(e.ctx, e.ownInput(KindInitial)); err == nil {
		t.Fatal("a round whose identity has no env must fail")
	}
	for kind, want := range map[string]slog.Level{"round.start": slog.LevelInfo, "round.end": slog.LevelError} {
		r, ok := e.log.record(kind)
		switch {
		case !ok:
			t.Errorf("no %s record in %q", kind, e.log.lines)
		case r.level != want || r.attrs["subject"] != "pr:talkable/talkable#11920" ||
			!strings.HasPrefix(r.line, "pipeline: pr:talkable/talkable#11920 "+kind+": "):
			t.Errorf("%s record = %+v, want level %v with the round's subject", kind, r, want)
		}
	}
}
