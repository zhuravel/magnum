package config

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var (
	overlaySectionCall = regexp.MustCompile(`overlaySection\(md, "([a-z_]+)"`)
	// headerTable matches a table name of a header comment ("[daemon]"; the
	// test skips a [[watch]] block, and a [kinds.<name>] table has a dot).
	headerTable = regexp.MustCompile(`\[([a-z_]+)\]`)
)

// The two example files open with the tables the user's file overrides key by
// key; the loader's overlay (applyOverlay) merges the same sections, and the
// list in both comments was one short ([webhooks]) after the webhooks landed.
func TestTheExampleHeadersListTheTablesTheOverlayMerges(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	var merged []string
	for _, m := range overlaySectionCall.FindAllStringSubmatch(string(src), -1) {
		merged = append(merged, m[1])
	}
	slices.Sort(merged)
	if len(merged) < 10 { // the pattern must find the calls, not silently skip them
		t.Fatalf("found only %d overlaySection calls in config.go: %v", len(merged), merged)
	}
	for _, doc := range []string{"config.defaults.toml", "config.full.example.toml"} {
		var header []string
		for line := range strings.Lines(readDoc(t, doc)) {
			if !strings.HasPrefix(line, "#") {
				break
			}
			header = append(header, strings.TrimPrefix(line, "#"))
		}
		// A comment wraps lines, so a name may stand at the end of one.
		text := " " + strings.Join(strings.Fields(strings.Join(header, " ")), " ") + " "
		var named []string
		for _, m := range headerTable.FindAllStringSubmatchIndex(text, -1) {
			if m[0] > 0 && text[m[0]-1] == '[' { // a [[watch]] block
				continue
			}
			named = append(named, text[m[2]:m[3]])
		}
		slices.Sort(named)
		named = slices.Compact(named)
		if !slices.Equal(named, merged) {
			t.Errorf("%s: the header names the tables %v, but applyOverlay merges %v", doc, named, merged)
		}
	}
}
