package pipeline

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
)

// stackInstruction opens the judge prompts' one line on a PR stacked below
// this one.
const stackInstruction = "A finding that sits only on lines a PR stacked below this one introduced ("

// One PR's review (#752, whose head was #751's head plus 41 commits) posted
// a P2 on a line #751 introduced, and its author answered that the line came
// unchanged from #751: related.json listed #751 among 40 shared paths but
// not that #752 was stacked on it, so the skill's lower-layer rule never
// applied. An open related PR whose head is in the history of the head
// under review is stacked below it, one whose history holds that head
// above it, as git answers in the checkout (never GitHub); related.json
// and related-all.json carry the stack, the related_prs line says it, a
// PR below gets the prompt's one line, and the round.related event names
// both. A head the checkout never fetched, the same head and a merged PR
// get none.
func TestARelatedPRWhoseHeadIsAnAncestorIsStackedBelow(t *testing.T) {
	e := newEnv(t)
	e.ownFiles(target, "app/models/order.rb")
	e.otherPR(751, false, nil, "app/models/order.rb")                        // head751, under target
	e.otherPR(753, true, nil, "app/models/order.rb")                         // head753, a draft on top of target
	e.otherPR(754, false, nil, "app/models/order.rb")                        // head754, never fetched
	e.pushed(755, target, "app/models/order.rb")                             // the same head
	e.otherPR(756, false, new(t0.Add(-24*time.Hour)), "app/models/order.rb") // merged, under target
	e.git.graph = map[string][]string{target: {"head751"}, "head751": {"head756"}, "head756": nil, "head753": {target}}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(811, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.Related, in.Roles = defaultRelated, judgeAlone(e)
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}

	path := filepath.Join(res.ReportDir, RelatedFile)
	got, raw := readRelated(t, path)
	stacks := map[int]string{}
	for _, r := range got.Related {
		stacks[r.Number] = r.Stack
	}
	if want := map[int]string{751: "below", 753: "above", 754: "", 755: "", 756: ""}; !maps.Equal(stacks, want) {
		t.Fatalf("stacks = %v, want %v:\n%s", stacks, want, raw)
	}
	mustContain(t, RelatedFile, raw, `"stack": "below"`, `"stack": "above"`)
	all, err := os.ReadFile(filepath.Join(res.ReportDir, relatedAllFile))
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, relatedAllFile, string(all), `"stack": "below"`)
	for _, call := range e.git.ancestry {
		for _, never := range []string{"head754", "head755", "head756"} {
			if strings.Contains(call, never) {
				t.Errorf("is-ancestor asked about %s: %q", never, call)
			}
		}
	}

	prompts := e.ag.submitsFor(agents.RoleJudge)
	if len(prompts) != 1 {
		t.Fatalf("judge prompts = %d, want 1", len(prompts))
	}
	mustContain(t, "judge prompt", prompts[0].Text,
		"\nrelated_prs: "+path+" (#751 stacked below this PR; #753 above it)\n", stackInstruction+"#751) is that PR's: `outside_diff`")

	evs := e.eventsOfKind("round.related")
	if len(evs) != 1 {
		t.Fatalf("round.related events = %+v, want one", evs)
	}
	mustContain(t, "round.related", evs[0].Message, "#751 stacked below this PR; #753 above it")
	var data struct {
		Below []int `json:"stacked_below"`
		Above []int `json:"stacked_above"`
	}
	if err := json.Unmarshal(evs[0].Data, &data); err != nil || !slices.Equal(data.Below, []int{751}) || !slices.Equal(data.Above, []int{753}) {
		t.Errorf("round.related data = %s (%v), want stacked_below [751] and stacked_above [753]", evs[0].Data, err)
	}
}

// The stack decides a finding in every round, so a re-review whose
// related.json leaves out the stacked PR (an earlier posted review named
// it in its state and paths) still gives the judge the line on the PR
// below, and its event names it; the related_prs field stays away with the
// file. Without a PR below there is no such line.
func TestAStackedPRNamedBeforeStillGetsItsLineInTheJudgePrompt(t *testing.T) {
	e := newEnv(t)
	e.otherPR(751, false, nil, "app/models/order.rb")
	e.git.graph = map[string][]string{target: {prevSHA}, prevSHA: {"head751"}, "head751": nil}
	dir, prompts := e.relatedRound(1, KindInitial, prevSHA, "", "app/models/order.rb")
	path := filepath.Join(dir, RelatedFile)
	if got, raw := readRelated(t, path); len(got.Related) != 1 || got.Related[0].Number != 751 || got.Related[0].Stack != "below" {
		t.Fatalf("round 1 related.json, want #751 below:\n%s", raw)
	}
	mustContain(t, "round 1's judge prompt", prompts[0].Text, "\nrelated_prs: "+path+" (#751 stacked below this PR)\n", stackInstruction+"#751)")

	dir, prompts = e.relatedRound(2, KindRereview, target, prevSHA, "app/models/order.rb")
	if got := relatedNumbers(t, dir, prompts); got != nil {
		t.Fatalf("round 2 related = %v, want no file: #751 is as round 1 named it", got)
	}
	mustContain(t, "round 2's judge prompt", prompts[0].Text, stackInstruction+"#751) is that PR's")
	evs := e.eventsOfKind("round.related")
	mustContain(t, "round 2's round.related", evs[len(evs)-1].Message, "named before", "#751 stacked below this PR")

	e.git.graph = nil
	_, prompts = e.relatedRound(3, KindRereview, "3333333333333333333333333333333333333333", target, "app/models/order.rb")
	if strings.Contains(prompts[0].Text, stackInstruction) {
		t.Errorf("round 3's judge prompt gives a line on a stacked PR without one:\n%s", prompts[0].Text)
	}
}
