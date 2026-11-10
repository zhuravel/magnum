package factory

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
)

// The judge's turn after your answers wrote report.md with what the answers
// changed ahead of its first section, and failed the report's end fact 3
// times of 3 (R52): the answers prompt said only "your first prompt still
// holds". A report end's answers prompt now names the report's sections in
// their order and that nothing comes before the first.
func TestTheAnswersPromptOfAReportEndNamesItsSectionsInOrder(t *testing.T) {
	dir := t.TempDir()
	outputs := []string{filepath.Join(dir, reportFile), filepath.Join(dir, acceptanceFile)}
	text := answersText(dir, outputs, config.FactoryEndReport)
	if !strings.HasPrefix(text, answersLead) {
		t.Fatalf("the prompt %q does not lead with %q", text, answersLead)
	}
	for _, want := range []string{"`## Architecture decisions` first", "`## Helpers`", "`## Files`", "nothing before"} {
		if !strings.Contains(text, want) {
			t.Errorf("the prompt %q does not say %q", text, want)
		}
	}
	if a, h, f := strings.Index(text, "Architecture decisions"), strings.Index(text, "Helpers"), strings.Index(text, "Files`"); !(a < h && h < f) {
		t.Errorf("the prompt %q names the sections out of order", text)
	}
}

// Another end's answers prompt is as it was: it names no report section.
func TestTheAnswersPromptOfAnotherEndNamesNoReportSection(t *testing.T) {
	dir := t.TempDir()
	text := answersText(dir, []string{filepath.Join(dir, proofFile)}, config.FactoryEndProof)
	if strings.Contains(text, "Architecture decisions") {
		t.Errorf("a proof end's answers prompt names the report's sections: %q", text)
	}
}
