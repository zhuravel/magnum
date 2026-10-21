package magnum

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var codeSpan = regexp.MustCompile("`([^`]+)`")

// AGENTS.md's layout table names every package under internal/, and no other:
// the settings screen's two packages (internal/settings and internal/confedit)
// landed with no row in it.
func TestAgentsLayoutTableNamesEveryInternalPackage(t *testing.T) {
	doc, err := os.ReadFile("AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	_, layout, ok := strings.Cut(string(doc), "\n## Layout\n")
	if !ok {
		t.Fatal("AGENTS.md has no Layout section")
	}
	layout, _, _ = strings.Cut(layout, "\n## ")

	// A row's first cell lists code spans: "`internal/slots`, `cleanup`" names
	// internal/slots and internal/cleanup; a span with a path of its own
	// ("cmd/magnum-relay") stands for itself.
	var named []string
	for row := range strings.Lines(layout) {
		if !strings.HasPrefix(row, "|") {
			continue
		}
		cell := strings.Split(row, "|")[1]
		inInternal := false
		for _, m := range codeSpan.FindAllStringSubmatch(cell, -1) {
			switch span := m[1]; {
			case strings.HasPrefix(span, "internal/"):
				inInternal = true
				named = append(named, span)
			case !strings.Contains(span, "/") && inInternal:
				named = append(named, "internal/"+span)
			}
		}
	}

	entries, err := os.ReadDir("internal")
	if err != nil {
		t.Fatal(err)
	}
	var packages []string
	for _, e := range entries {
		if e.IsDir() {
			packages = append(packages, "internal/"+e.Name())
		}
	}
	if len(packages) < 30 { // the listing must find the packages, not silently skip them
		t.Fatalf("found only %d packages under internal/: %v", len(packages), packages)
	}
	for _, p := range packages {
		if !slices.Contains(named, p) {
			t.Errorf("AGENTS.md's layout table has no row for %s", p)
		}
	}
	for _, n := range named {
		if !slices.Contains(packages, n) {
			t.Errorf("AGENTS.md's layout table names %s, which is not a package", n)
		}
	}
}
