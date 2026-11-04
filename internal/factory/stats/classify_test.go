package stats_test

import (
	"os"
	"regexp"
	"testing"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/stats"
)

// The messages that only start a step are the ones the factory exists to
// make unnecessary: opening the PR, pushing, merging the parent in, merging
// into a staging branch, taking the recommendation.
func TestClassifyKnowsTheStepStartingMessages(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"open PR",
		"Open a PR",
		"please open the pull request now",
		"create a pull request",
		"push",
		"push it",
		"git push origin HEAD",
		"ok, push the branch",
		"merge master",
		"Merge the parent branch and run the specs",
		"merge into staging-void",
		"merge it into staging-void please",
		// stagings live in config, so any staging-<name> branch counts
		"merge into staging-zeta",
		"deploy this to staging-bastion",
		"do as recommended",
		"Do it as recommended",
		"go with your recommendation",
	} {
		if got := stats.Classify(text); got != registry.ClassStepStart {
			t.Errorf("Classify(%q) = %q, want %q", text, got, registry.ClassStepStart)
		}
	}
}

// Every class is told from the words that carry it, and a message that
// names a step but refuses it is a correction, not a step start.
func TestClassifyTellsTheOtherClasses(t *testing.T) {
	t.Parallel()
	for want, texts := range map[string][]string{
		registry.ClassApproval: {"yes", "Yes, that works", "ok", "lgtm", "looks good", "go ahead", "sounds good to me"},
		registry.ClassCorrection: {
			"no, that is the wrong table",
			"wrong file",
			"don't push yet",
			"no, do not open the PR",
			"that's not what I asked for",
			"you forgot the migration",
			"use the ledger table instead",
			"the spec still fails",
		},
		registry.ClassAccess: {
			"I gave you access to the sandbox",
			"the password is in the vault",
			"log in with my account",
			"here is the 2FA code: 000000",
			"your api key expired, use the new one",
		},
		registry.ClassInfo: {
			"the refund path only matters for expired points",
			"what does the sync job do?",
			"the push notification text lives in the mailer",
			"this table has 3 million rows",
			"",
			"   ",
		},
	} {
		for _, text := range texts {
			if got := stats.Classify(text); got != want {
				t.Errorf("Classify(%q) = %q, want %q", text, got, want)
			}
		}
	}
}

// Ruling: Classify names no environment. The stagings are the operator's
// config, and the defaults list none.
func TestClassifyHardcodesNoEnvironmentName(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("classify.go")
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`\b(void|bart|bastion)\b`).FindString(string(b)); m != "" {
		t.Errorf("classify.go names the environment %q", m)
	}
}

// The classes Classify returns are the ones a registry input can carry, so
// the engine can store them with ClassifyInput.
func TestClassifyReturnsOnlyClassesTheRegistryStores(t *testing.T) {
	t.Parallel()
	r := openRegistry(t)
	task := createTask(t, r, "PR-12301")
	id, err := r.RecordInput(t.Context(), registry.Input{TaskID: task.ID, Kind: registry.InputPaneTyped, Text: "open PR"})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"open PR", "yes", "wrong", "api key", "hello"} {
		if err := r.ClassifyInput(t.Context(), id, stats.Classify(text)); err != nil {
			t.Errorf("ClassifyInput(Classify(%q)): %v", text, err)
		}
	}
}
