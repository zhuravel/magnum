package config

import (
	"testing"
	"time"
)

// The factory reads its panes every 5 s unless the config says otherwise
// (R50 slice 1: two idle readings 5 s apart see a turn's end about 8 s
// sooner than at 10 s), in the Go defaults and the embedded file alike, and
// validation still refuses an observe interval that is not positive.
func TestTheFactoryObservesItsPanesEveryFiveSecondsByDefault(t *testing.T) {
	if got := DefaultFactory().Observe.Duration; got != 5*time.Second {
		t.Errorf("DefaultFactory().Observe = %s, want 5s", got)
	}
	cfg, err := loadCommittedWithLocal(t, testLocalConfig)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Factory.Observe.Duration; got != 5*time.Second {
		t.Errorf("the embedded defaults' [factory] observe = %s, want 5s", got)
	}
	for _, d := range []time.Duration{0, -5 * time.Second} {
		cfg := validFactoryConfig()
		cfg.Factory.Observe = Duration{d}
		wantError(t, cfg.Validate(), "factory.observe must be positive, got "+d.String())
	}
}
