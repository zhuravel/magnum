package confedit

import (
	"errors"
	"testing"

	"github.com/BurntSushi/toml"
)

// The cross-check refuses a scan that disagrees with BurntSushi's key list
// in number, order or name.
func TestTheCrossCheckRefusesAnyDifferenceFromMetaDataKeys(t *testing.T) {
	keys := []toml.Key{{"pipeline"}, {"pipeline", "spend"}}
	if err := crossCheck([][]string{{"pipeline"}, {"pipeline", "spend"}}, keys); err != nil {
		t.Errorf("the same keys: %v", err)
	}
	for name, scanned := range map[string][][]string{
		"one fewer":        {{"pipeline"}},
		"one more":         {{"pipeline"}, {"pipeline", "spend"}, {"pipeline", "fix_cycles"}},
		"in another order": {{"pipeline", "spend"}, {"pipeline"}},
		"another name":     {{"pipeline"}, {"pipeline", "level"}},
	} {
		if err := crossCheck(scanned, keys); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v, want ErrUnsupported", name, err)
		}
	}
}

// A lone carriage return is not a line ending: the decoder refuses it
// before the scanner sees it.
func TestParseRefusesALoneCarriageReturn(t *testing.T) {
	if _, err := Parse([]byte("[pipeline]\rspend = \"high\"\n")); err == nil || errors.Is(err, ErrUnsupported) {
		t.Errorf("Parse = %v, want the decoder's error", err)
	}
}

// The text check passes an edit inside its lines and an insertion, and
// refuses any change to another line, a line ending or a blank line
// included.
func TestTheTextCheckRefusesAChangeOutsideTheEditedLines(t *testing.T) {
	old := []byte("[a]\n# note\nx = 1\n\ny = 2\n")
	for name, c := range map[string]struct {
		new    string
		lo, hi int
		want   bool
	}{
		"the edited line":         {"[a]\n# note\nx = 5\n\ny = 2\n", 3, 4, true},
		"a line added":            {"[a]\n# note\nx = 1\nz = 3\n\ny = 2\n", 4, 4, true},
		"the edited line removed": {"[a]\n# note\n\ny = 2\n", 3, 4, true},
		"a comment changed":       {"[a]\n# Note\nx = 5\n\ny = 2\n", 3, 4, false},
		"a blank line dropped":    {"[a]\n# note\nx = 5\ny = 2\n", 3, 4, false},
		"a line ending changed":   {"[a]\n# note\nx = 5\n\ny = 2\r\n", 3, 4, false},
		"a space added elsewhere": {"[a]\n# note\nx = 5\n\ny = 2 \n", 3, 4, false},
	} {
		if got := confined(old, []byte(c.new), c.lo, c.hi); got != c.want {
			t.Errorf("%s: confined = %v, want %v", name, got, c.want)
		}
	}
}
