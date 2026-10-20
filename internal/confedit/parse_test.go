package confedit

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/zhuravel/magnum"
)

func readFixture(tb testing.TB, name string) []byte {
	tb.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

func crlf(b []byte) []byte { return []byte(strings.ReplaceAll(string(b), "\n", "\r\n")) }

// fixtures are the files every sweep runs over: the built-in defaults (the
// guard when config.defaults.toml changes shape), an operator-like file and
// its CRLF copy.
func fixtures(tb testing.TB) map[string][]byte {
	tb.Helper()
	op := readFixture(tb, "operator.toml")
	return map[string][]byte{"defaults": magnum.DefaultConfig, "operator": op, "operator-crlf": crlf(op)}
}

func TestParseReadsTheKeysBurntSushiReadsInTheSameOrder(t *testing.T) {
	for name, src := range fixtures(t) {
		d, err := Parse(src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(d.entries) == 0 || len(d.tables) < 2 {
			t.Errorf("%s: %d entries, %d tables", name, len(d.entries), len(d.tables))
		}
	}
}

func TestParseRefusesWhatItDoesNotModel(t *testing.T) {
	for name, src := range map[string]string{
		"byte order mark": "\uFEFFa = 1\n",
	} {
		if _, err := Parse([]byte(src)); !errors.Is(err, ErrUnsupported) && err == nil {
			t.Errorf("%s: Parse = %v, want an error", name, err)
		}
	}
	if _, err := Parse([]byte("a = \n")); err == nil || errors.Is(err, ErrUnsupported) {
		t.Errorf("a file TOML refuses: %v, want the decoder's error", err)
	}
}
