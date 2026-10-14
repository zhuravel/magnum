package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// The docs name the config's keys and count its built-in roles in prose, and
// both drifted before (a key renamed in a table, "four built-in roles" after
// the fix and whiteboard roles joined the list). These tests read the docs
// from the checkout, so a doc that disagrees with the loader fails the gate.

func readDoc(t *testing.T, elems ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, elems...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var tomlBlock = regexp.MustCompile("(?s)```toml\n(.*?)```")

// Every ```toml block of README.md and prompts/README.md is a configuration
// the loader's decoder reads with no key it does not know: a key renamed in
// the code and not in the docs fails here. (Decoding only: a block may name
// an identity or a role declared elsewhere, which validation would refuse.)
func TestEveryTomlBlockInTheDocsDecodesWithoutUnknownKeys(t *testing.T) {
	blocks := 0
	for _, doc := range []string{"README.md", filepath.Join("prompts", "README.md")} {
		for i, m := range tomlBlock.FindAllStringSubmatch(readDoc(t, doc), -1) {
			blocks++
			var cfg Config
			md, err := toml.Decode(m[1], &cfg)
			if err != nil {
				t.Errorf("%s: toml block %d does not decode: %v", doc, i+1, err)
				continue
			}
			if undecoded := md.Undecoded(); len(undecoded) > 0 {
				t.Errorf("%s: toml block %d has keys the config does not know: %v", doc, i+1, undecoded)
			}
		}
	}
	if blocks < 10 { // the pattern must find the blocks, not silently skip them
		t.Fatalf("found only %d toml blocks in the docs; the pattern is broken", blocks)
	}
}

var numberWords = []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve"}

// The docs write the count of built-in roles as a word ("the seven built-in
// roles", "the seven [[role]] blocks below"); it is len(DefaultRoles()).
func TestTheDocsCountTheBuiltInRoles(t *testing.T) {
	n := len(DefaultRoles())
	if n >= len(numberWords) {
		t.Fatalf("DefaultRoles has %d roles; numberWords stops at %d", n, len(numberWords)-1)
	}
	want := numberWords[n]
	words := strings.Join(numberWords[1:], "|")
	count := regexp.MustCompile(`(?i)\b(` + words + `) (built-in roles|\[\[role\]\] blocks below)|the (` + words + `) roles below are the built-in`)
	seen := 0
	for _, doc := range [][]string{{"README.md"}, {"prompts", "README.md"}, {"config.defaults.toml"}, {"config.full.example.toml"}, {"config.example.toml"}} {
		// A comment wraps lines, so a phrase may stand across a line break.
		text := strings.Join(strings.Fields(readDoc(t, doc...)), " ")
		for _, m := range count.FindAllStringSubmatch(text, -1) {
			seen++
			if got := strings.ToLower(m[1] + m[3]); got != want {
				t.Errorf("%s says %q, but DefaultRoles has %d (%s) roles", filepath.Join(doc...), m[0], n, want)
			}
		}
	}
	if seen < 4 { // the pattern must find the phrases, not silently skip them
		t.Fatalf("found only %d counts of the built-in roles in the docs; the pattern is broken", seen)
	}
}
