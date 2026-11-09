package evalrun

import (
	"strings"
	"testing"
)

// Round 11 of a task, which a second opinion past the loop's ten rounds runs, takes a number of its own:
// rounds 1 to 10 keep the numbers they ran under (900000 + task × 10 + round, which round 1 of task 2's
// 900021 would be for round 11 of task 1), rounds 11 to 99 take 9000000 + task × 100 + round, and
// LocalRoundOf names both back and refuses a number neither form makes.
func TestRoundElevenTakesANumberNoOtherRoundHas(t *testing.T) {
	if LocalNumber(1, 1) != 900011 || LocalNumber(7, 10) != 900080 || LocalNumber(2, 1) != 900021 {
		t.Fatalf("rounds 1 to 10 moved: %d %d %d", LocalNumber(1, 1), LocalNumber(7, 10), LocalNumber(2, 1))
	}
	if n := LocalNumber(1, 11); n == LocalNumber(2, 1) || n != 9000111 {
		t.Fatalf("round 11 of task 1 = %d, round 1 of task 2 = %d", n, LocalNumber(2, 1))
	}
	if n := LocalNumber(123, 12); n != 9012312 {
		t.Fatalf("round 12 of task 123 = %d, want 9012312", n)
	}
	if task, round, ok := LocalRoundOf(9000111); !ok || task != 1 || round != 11 {
		t.Fatalf("LocalRoundOf(9000111) = %d, %d, %v", task, round, ok)
	}
	for _, n := range []int{9000105, 9000110, 9000010, 1900001, 8999999, 19000000} {
		if task, round, ok := LocalRoundOf(n); ok {
			t.Errorf("LocalRoundOf(%d) = task %d, round %d; want none", n, task, round)
		}
	}
	// Every pair numbered and named back is a pair no other shares (LocalRoundOf undoes LocalNumber), and
	// a number LocalRoundOf names is one LocalNumber makes.
	for task := int64(1); task <= MaxLocalTask; task++ {
		for round := 1; round <= MaxLocalRound; round++ {
			n := LocalNumber(task, round)
			if gt, gr, ok := LocalRoundOf(n); !ok || gt != task || gr != round || n < localNumberBase {
				t.Fatalf("LocalRoundOf(LocalNumber(%d, %d) = %d) = %d, %d, %v", task, round, n, gt, gr, ok)
			}
		}
	}
	for n := range 20_000_000 {
		if task, round, ok := LocalRoundOf(n); ok && LocalNumber(task, round) != n {
			t.Fatalf("LocalRoundOf(%d) = task %d, round %d, whose number is %d", n, task, round, LocalNumber(task, round))
		}
	}
}

// LocalNumber has no number for a task or a round it cannot keep apart from another's: task 0 or past
// MaxLocalTask, round 0 or past MaxLocalRound.
func TestLocalNumberIsZeroOutsideItsDomain(t *testing.T) {
	for _, tc := range []struct {
		task  int64
		round int
	}{{0, 1}, {-1, 1}, {1, 0}, {1, -1}, {1, MaxLocalRound + 1}, {MaxLocalTask + 1, 1}, {MaxLocalTask + 1, 11}} {
		if n := LocalNumber(tc.task, tc.round); n != 0 {
			t.Errorf("LocalNumber(%d, %d) = %d, want 0", tc.task, tc.round, n)
		}
	}
	if LocalNumberFormula(10) != "900000 + task × 10 + round" || LocalNumberFormula(11) != "9000000 + task × 100 + round" {
		t.Fatalf("formulas %q, %q", LocalNumberFormula(10), LocalNumberFormula(11))
	}
}

// RunLocal runs a round past the loop's ten under its own number, and refuses a round or a task no
// number is kept for.
func TestRunLocalRunsRoundElevenUnderItsOwnNumber(t *testing.T) {
	d, runs := localFixture(t, cleanResult)
	if _, err := RunLocal(t.Context(), d, localCase(t, 11)); err != nil {
		t.Fatal(err)
	}
	if r := (*runs)[0]; r.round.Number != 9000711 || !strings.HasSuffix(r.round.URL, "/pull/9000711") {
		t.Fatalf("round 11 of task 7: %+v", r.round.EvalCase)
	}
	if _, err := RunLocal(t.Context(), d, localCase(t, MaxLocalRound+1)); err == nil || !strings.Contains(err.Error(), "round of 1 to 99") {
		t.Fatalf("round 100: %v", err)
	}
	c := localCase(t, 1)
	c.TaskID = MaxLocalTask + 1
	if _, err := RunLocal(t.Context(), d, c); err == nil || !strings.Contains(err.Error(), "1 to 99999") {
		t.Fatalf("task 100000: %v", err)
	}
	if len(*runs) != 1 {
		t.Fatalf("rounds run: %d", len(*runs))
	}
}
