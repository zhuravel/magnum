package config

import (
	"strings"
	"testing"
)

// [terminal] icons picks the screens' symbols: empty (unicode), unicode,
// nerd or ascii; anything else is an error naming the key.
func TestTerminalIconsValidation(t *testing.T) {
	for _, ok := range []string{"", "unicode", "nerd", "ascii"} {
		cfg := validPipelineConfig()
		cfg.Terminal.Icons = ok
		if err := cfg.Validate(); err != nil {
			t.Errorf("icons %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"emoji", "Nerd", "nerdfont"} {
		cfg := validPipelineConfig()
		cfg.Terminal.Icons = bad
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "terminal.icons") {
			t.Errorf("icons %q: err = %v, want one naming terminal.icons", bad, err)
		}
	}
}
