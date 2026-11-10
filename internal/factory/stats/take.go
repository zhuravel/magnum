package stats

// The operator's explicit signals, read from the records the factory keeps:
// whether the answers took the recommendations (questions.took_recommendation,
// decisions.recommendation_followed) and the ratings the task card's + and −
// give. Reading only: Compute writes nothing.

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/factory/registry"
)

// Take counts the cases Of, out of which the operator Took the recommendation:
// the answered questions that carried one, or the decisions on a card that
// did (and followed it).
type Take struct {
	Of   int `json:"of"`
	Took int `json:"took"`
}

func (t *Take) add(took bool) {
	t.Of++
	if took {
		t.Took++
	}
}

// share is the share that took it, "67%", or "-" when none was counted.
func (t Take) share() string {
	if t.Of == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(t.Took)/float64(t.Of))
}

// TakeTotals are the take rates over a report's tasks: the answered questions
// with a recommendation in all (Questions), by the source that raised them
// (ByRole: the sources the stats count a question under, several joined with
// "+") and by the kind of the task (ByKind); and the decisions on a card that
// carried a recommendation (Decisions, whose Took is the decisions that
// followed it). A question counts once per task, by its text, with its
// current answer.
type TakeTotals struct {
	Questions Take            `json:"questions"`
	ByRole    map[string]Take `json:"by_role"`
	ByKind    map[string]Take `json:"by_kind"`
	Decisions Take            `json:"decisions"`
}

// RatingTotals are the ratings the operator gave with + and −: how many
// (every rating is kept, so one task rated twice counts twice) on how many
// tasks, how many of them + and −, by the key of the reason (NoReason: given
// with none), and how many were given without the diff opened first, which is
// noise.
type RatingTotals struct {
	Count    int            `json:"count"`
	Tasks    int            `json:"tasks"`
	Plus     int            `json:"plus"`
	Minus    int            `json:"minus"`
	Reasons  map[string]int `json:"reasons"`
	NoReason int            `json:"no_reason"`
	Noise    int            `json:"noise"`
}

// signals gathers the take rates and the ratings of the tasks it is given.
type signals struct {
	take    TakeTotals
	ratings RatingTotals
}

func newSignals() *signals {
	return &signals{
		take:    TakeTotals{ByRole: map[string]Take{}, ByKind: map[string]Take{}},
		ratings: RatingTotals{Reasons: map[string]int{}},
	}
}

// add reads task t's answered questions that carried a recommendation, its
// decisions on a card that carried one and its ratings.
func (s *signals) add(ctx context.Context, reg *registry.Registry, t registry.Task) error {
	qs, err := reg.Questions(ctx, t.ID)
	if err != nil {
		return err
	}
	last := map[string]registry.Question{} // by text: the newest answered one that carried a recommendation
	var order []string
	for _, q := range qs {
		if q.State != registry.QuestionAnswered || q.Recommended < 0 {
			continue
		}
		if _, ok := last[q.Text]; !ok {
			order = append(order, q.Text)
		}
		last[q.Text] = q
	}
	if len(order) > 0 {
		relays, err := relayRoles(ctx, reg, t.ID)
		if err != nil {
			return err
		}
		kind := cmp.Or(t.Kind, "unknown")
		for _, text := range order {
			q := last[text]
			role := cmp.Or(strings.Join(sourcesOf(q, relays), "+"), "unknown")
			s.take.Questions.add(q.TookRecommendation)
			byRole, byKind := s.take.ByRole[role], s.take.ByKind[kind]
			byRole.add(q.TookRecommendation)
			byKind.add(q.TookRecommendation)
			s.take.ByRole[role], s.take.ByKind[kind] = byRole, byKind
		}
	}
	ds, err := reg.Decisions(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, d := range ds {
		if d.RecommendationShown {
			s.take.Decisions.add(d.RecommendationFollowed)
		}
	}
	rs, err := reg.Ratings(ctx, t.ID)
	if err != nil {
		return err
	}
	if len(rs) > 0 {
		s.ratings.Tasks++
	}
	for _, rt := range rs {
		s.ratings.Count++
		if rt.Value > 0 {
			s.ratings.Plus++
		} else {
			s.ratings.Minus++
		}
		if rt.ReasonKey == "" {
			s.ratings.NoReason++
		} else {
			s.ratings.Reasons[rt.ReasonKey]++
		}
		if !rt.DiffOpenedFirst {
			s.ratings.Noise++
		}
	}
	return nil
}

// totals are the take rates (nil when no answered question carried a
// recommendation and no decision recorded one) and the ratings (nil when
// there are none).
func (s *signals) totals() (*TakeTotals, *RatingTotals) {
	var take *TakeTotals
	var ratings *RatingTotals
	if s.take.Questions.Of > 0 || s.take.Decisions.Of > 0 {
		take = &s.take
	}
	if s.ratings.Count > 0 {
		ratings = &s.ratings
	}
	return take, ratings
}

// takeMarkdown is the section of the recommendations taken: the questions in
// all, by the role that raised them and by the kind of task, and the
// decisions. Without a take rate, no section.
func (r Report) takeMarkdown(b *strings.Builder) {
	take := r.Take
	if take == nil {
		return
	}
	b.WriteString("\n## Recommendations taken\n\n")
	if take.Questions.Of == 0 {
		b.WriteString("No answered question carried a recommendation.\n")
	} else {
		fmt.Fprintf(b, "Questions that carried a recommendation and were answered: %d of %d took it (%s).\n", take.Questions.Took, take.Questions.Of,
			take.Questions.share())
		table := func(head string, by map[string]Take) {
			fmt.Fprintf(b, "\n| %s | Answered | Took it | Share |\n|---|---|---|---|\n", head)
			for _, k := range slices.Sorted(maps.Keys(by)) {
				fmt.Fprintf(b, "| %s | %d | %d | %s |\n", k, by[k].Of, by[k].Took, by[k].share())
			}
		}
		table("Raised by", take.ByRole)
		table("Kind of task", take.ByKind)
	}
	if take.Decisions.Of == 0 {
		b.WriteString("\nNo decision recorded a recommendation on its card.\n")
	} else {
		fmt.Fprintf(b, "\nDecisions on a card that carried a recommendation: %d of %d followed it (%s).\n", take.Decisions.Took, take.Decisions.Of,
			take.Decisions.share())
	}
	b.WriteString("\nA question counts once per task, by its text, under the sources \"Questions by source\" counts it under (several roles " +
		"joined with `+`); one without a recommendation, or not answered yet, is not counted. Took it: your answer, as it stands after an " +
		"edit, is the recommended option.\n")
}

// ratingsMarkdown is the section of the ratings; none, no section.
func (r Report) ratingsMarkdown(b *strings.Builder) {
	rt := r.Ratings
	if rt == nil {
		return
	}
	var reasons []string
	for _, k := range slices.Sorted(maps.Keys(rt.Reasons)) {
		reasons = append(reasons, fmt.Sprintf("%s %d", k, rt.Reasons[k]))
	}
	if rt.NoReason > 0 {
		reasons = append(reasons, fmt.Sprintf("none given %d", rt.NoReason))
	}
	were := "were"
	if rt.Noise == 1 {
		were = "was"
	}
	fmt.Fprintf(b, "\n## Ratings\n\n%s on %s: %d +, %d −. Reasons: %s. %d %s given without the diff opened first (noise, not signal).\n",
		plural(rt.Count, "rating"), plural(rt.Tasks, "task"), rt.Plus, rt.Minus, strings.Join(reasons, ", "), rt.Noise, were)
	b.WriteString("\nThe task card's + and − store a rating each; a task rated again keeps both. A rating given without the diff opened " +
		"first, on the screen or as your newest push on the ready card recorded, counts as noise.\n")
}
