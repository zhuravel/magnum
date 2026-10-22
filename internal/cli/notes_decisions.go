package cli

// `magnum notes <repo> --decisions [N]`: who decided the repository's notes
// proposals, when and why, and the notes critic's record of each
// (store.NotesCritiques: when it ran, on which model, its outcome, decision,
// reason, checked and losses lists, and magnum's detail). Everything comes
// from the registry, which keeps every critique, so the record outlives the
// critiques' run directories (pruned after 30 days); a run directory is
// named only while it is still on disk. What the critic and the curator
// wrote goes through notes.Printable before it reaches the terminal.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// notesDecisionsShown caps the proposals `--decisions` lists on a terminal
// (--json prints them all).
const notesDecisionsShown = 20

// notesDecisionsAll is the value of a bare --decisions (pflag's NoOptDefVal):
// list the proposals.
const notesDecisionsAll = "all"

// critiqueRunning is the outcome key of a critique that still runs.
const critiqueRunning = "running"

// notesDecidedStates are the states of a proposal someone decided.
var notesDecidedStates = []string{store.ProposalApplied, store.ProposalRejected, store.ProposalExpired, store.ProposalInvalid,
	store.ProposalSuperseded}

// notesDecision is a proposal as --decisions shows it (and its --json):
// what it is, its state, who decided it (for the critic, the model of the
// critique that decided), when and why, and its critiques counted by
// outcome.
type notesDecision struct {
	ID          int64          `json:"id"`
	Kind        string         `json:"kind"`
	Trigger     string         `json:"trigger,omitempty"`
	What        string         `json:"what"` // "curation (over_limit)", "restore of version 12", "recovery"
	Model       string         `json:"model,omitempty"`
	State       string         `json:"state"`
	DecidedBy   string         `json:"decided_by,omitempty"`
	CriticModel string         `json:"critic_model,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	DecidedAt   *time.Time     `json:"decided_at,omitempty"`
	Reason      string         `json:"reason,omitempty"`
	Critiques   map[string]int `json:"critiques"` // by outcome; "running" for the one in flight
	CriticOn    bool           `json:"critic_running,omitempty"`
}

// notesDecisionsList is what `--decisions --json` prints.
type notesDecisionsList struct {
	Repo      string          `json:"repo"`
	Proposals []notesDecision `json:"proposals"`
}

// notesCritiqueView is a critique as `--decisions N --json` prints it.
type notesCritiqueView struct {
	store.NotesCritique
	RunDirExists bool `json:"run_dir_exists"`
}

// notesDecisionsOne is what `--decisions N --json` prints: the proposal and
// every critique of it, oldest first.
type notesDecisionsOne struct {
	Repo      string              `json:"repo"`
	Proposal  notesDecision       `json:"proposal"`
	Critiques []notesCritiqueView `json:"critiques"`
}

// notesDecisions lists the repository's proposals (which "all"), or shows
// proposal which with every critique of it.
func notesDecisions(ctx context.Context, c *Context, d *actDeps, f notesFlags, repo store.Repo) int {
	full := repo.FullName()
	mark := engine.ReadCritic(ctx, d.Store)
	if f.decisions != notesDecisionsAll {
		id, err := parsePositive(f.decisions)
		if err != nil {
			return inspUsage(c, "notes", fmt.Sprintf("--decisions %q: want a proposal id, or nothing to list them", f.decisions), notesUsage)
		}
		return notesDecisionsShow(ctx, c, d, f, repo, int64(id), mark)
	}
	ps, err := d.Store.NotesProposals(ctx, store.NotesProposalFilter{RepoID: repo.ID})
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	cs, err := d.Store.NotesCritiques(ctx, store.NotesCritiqueFilter{RepoID: repo.ID})
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	byProposal := map[int64][]store.NotesCritique{}
	for _, cr := range cs {
		byProposal[cr.ProposalID] = append(byProposal[cr.ProposalID], cr)
	}
	out := notesDecisionsList{Repo: full, Proposals: make([]notesDecision, 0, len(ps))}
	for _, p := range ps {
		out.Proposals = append(out.Proposals, notesDecisionOf(p, byProposal[p.ID], mark))
	}
	if f.json {
		if err := writeJSON(c.Stdout, out); err != nil {
			return cmdFail(c, "notes", err)
		}
		return 0
	}
	if len(out.Proposals) == 0 {
		fmt.Fprintf(c.Stderr, "no proposal for the notes of %s yet (`magnum notes %s --curate` asks for a curation)\n", full, full)
		return 0
	}
	shown := out.Proposals[:min(len(out.Proposals), notesDecisionsShown)]
	rows := make([][]string, 0, len(shown))
	for _, p := range shown {
		state, by, critiques, reason := p.State, "-", "-", "-"
		if p.CriticOn {
			state += " (the critic is on it)"
		}
		if p.DecidedBy != "" {
			by = notesDecider(p)
		}
		if len(p.Critiques) > 0 {
			critiques = notesCritiqueCounts(p.Critiques)
		}
		if p.Reason != "" {
			reason = textx.Clip(notesOneLine(p.Reason), 80)
		}
		when := p.CreatedAt
		if p.DecidedAt != nil {
			when = *p.DecidedAt
		}
		rows = append(rows, []string{fmt.Sprint(p.ID), p.What, state, by, when.Local().Format("2006-01-02 15:04"), critiques, reason})
	}
	actTable(c.Stdout, []string{"PROPOSAL", "WHAT", "STATE", "DECIDED BY", "WHEN", "CRITIQUES", "REASON"}, rows)
	if more := len(out.Proposals) - len(shown); more > 0 {
		fmt.Fprintf(c.Stderr, "%s not shown (--json prints them all)\n", textx.Count(more, "older proposal", "older proposals"))
	}
	fmt.Fprintf(c.Stderr, "`magnum notes %s --decisions <id>` shows one proposal with the critic's record of it\n", full)
	return 0
}

// notesDecisionsShow prints proposal id of repo and every critique of it,
// oldest first, from the registry.
func notesDecisionsShow(ctx context.Context, c *Context, d *actDeps, f notesFlags, repo store.Repo, id int64, mark *engine.CriticMark) int {
	full := repo.FullName()
	p, err := d.Store.NotesProposalByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && p.RepoID != repo.ID) {
		return cmdFail(c, "notes", fmt.Errorf("proposal %d is not a proposal of the notes of %s (`magnum notes %s --decisions` lists them)", id, full, full))
	}
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	cs, err := d.Store.NotesCritiques(ctx, store.NotesCritiqueFilter{ProposalID: p.ID})
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	out := notesDecisionsOne{Repo: full, Proposal: notesDecisionOf(p, cs, mark), Critiques: make([]notesCritiqueView, 0, len(cs))}
	for _, cr := range slices.Backward(cs) {
		out.Critiques = append(out.Critiques, notesCritiqueView{NotesCritique: cr, RunDirExists: cr.RunDir != "" && fsx.Exists(cr.RunDir)})
	}
	if f.json {
		if err := writeJSON(c.Stdout, out); err != nil {
			return cmdFail(c, "notes", err)
		}
		return 0
	}
	w := c.Stdout
	pd := out.Proposal
	what := "a " + pd.What
	if pd.Model != "" {
		what += " by " + notes.Printable(pd.Model)
	}
	fmt.Fprintf(w, "proposal %d for the notes of %s: %s, %s\n", pd.ID, full, what, pd.CreatedAt.Local().Format("2006-01-02 15:04"))
	fmt.Fprintf(w, "state: %s\n", notesDecisionState(pd))
	if len(out.Critiques) == 0 {
		fmt.Fprintln(w, "critiques: none")
		return 0
	}
	fmt.Fprintf(w, "critiques: %d (%s)\n", len(out.Critiques), notesCritiqueCounts(pd.Critiques))
	for _, cr := range out.Critiques {
		fmt.Fprintln(w)
		when := cr.StartedAt.Local().Format("2006-01-02 15:04")
		if cr.EndedAt != nil {
			when += " to " + inspClock(cr.StartedAt, *cr.EndedAt)
		}
		on := ""
		if cr.Model != "" {
			on = " on " + notes.Printable(cr.Model)
		}
		outcome := cmp.Or(cr.Outcome, critiqueRunning)
		if cr.Decision != "" {
			outcome += " (decision " + cr.Decision + ")"
		}
		fmt.Fprintf(w, "critique %d, %s%s: %s\n", cr.ID, when, on, outcome)
		if cr.Target != "" {
			fmt.Fprintf(w, "  target: %s\n", notes.Printable(cr.Target))
		}
		if cr.Reason != "" {
			fmt.Fprintf(w, "  reason: %s\n", notesOneLine(cr.Reason))
		}
		for _, list := range []struct {
			name  string
			items []string
		}{{"checked", cr.Checked}, {"losses", cr.Losses}} {
			if len(list.items) == 0 {
				continue
			}
			fmt.Fprintf(w, "  %s:\n", list.name)
			for _, item := range list.items {
				fmt.Fprintf(w, "    - %s\n", notesOneLine(item))
			}
		}
		if cr.Detail != "" {
			fmt.Fprintf(w, "  detail: %s\n", notesOneLine(cr.Detail))
		}
		if cr.RunDirExists {
			fmt.Fprintf(w, "  run directory: %s\n", cr.RunDir)
		}
	}
	return 0
}

// notesDecisionOf is proposal p with its critiques cs (newest first) and
// the critique running now (mark, nil when none). For a decision of the
// critic the model and, when the proposal keeps none, the reason come
// from the critique that decided it.
func notesDecisionOf(p store.NotesProposal, cs []store.NotesCritique, mark *engine.CriticMark) notesDecision {
	d := notesDecision{ID: p.ID, Kind: p.Kind, Trigger: p.Trigger, What: notesProposalWhat(p), Model: p.Model, State: p.State,
		DecidedBy: p.DecidedBy, CreatedAt: p.CreatedAt, DecidedAt: p.DecidedAt, Reason: p.Reason, Critiques: map[string]int{},
		CriticOn: mark != nil && mark.Proposal == p.ID}
	for _, cr := range cs {
		d.Critiques[cmp.Or(cr.Outcome, critiqueRunning)]++
	}
	if p.DecidedBy == store.DecidedByCritic {
		if i := slices.IndexFunc(cs, func(cr store.NotesCritique) bool {
			return cr.Outcome == store.CritiqueApplied || cr.Outcome == store.CritiqueRejected || cr.Outcome == store.CritiqueRecurated
		}); i >= 0 {
			d.CriticModel, d.Reason = cs[i].Model, cmp.Or(d.Reason, cs[i].Reason)
		}
	}
	return d
}

// notesProposalWhat says what a proposal is: "curation (over_limit)",
// "restore of version 12", "recovery".
func notesProposalWhat(p store.NotesProposal) string {
	switch {
	case p.Kind == store.ProposalRestore:
		return fmt.Sprintf("restore of version %d", store.Deref(p.VersionID))
	case p.Trigger == engine.CurateTriggerRecover:
		return "recovery"
	case p.Trigger != "":
		return "curation (" + p.Trigger + ")"
	}
	return "curation"
}

// notesDecider names who decided d: "the critic (fable)", "you", "magnum".
func notesDecider(d notesDecision) string {
	who := engine.DeciderName(d.DecidedBy)
	if d.DecidedBy == store.DecidedByCritic && d.CriticModel != "" {
		who += " (" + notes.Printable(d.CriticModel) + ")"
	}
	return who
}

// notesDecisionState says what became of d: "applied by the critic (fable)
// at 2026-10-22 15:04: <reason>", "rejected by you at ...", "expired at
// ...", "waits for a decision since ...".
func notesDecisionState(d notesDecision) string {
	switch d.State {
	case store.ProposalPending:
		s := "waits for a decision since " + d.CreatedAt.Local().Format("2006-01-02 15:04")
		if d.CriticOn {
			s += "; the critic is on it"
		}
		return s
	case store.ProposalApplying:
		return "being applied by " + notesDecider(d)
	}
	verb := d.State
	if d.State == store.ProposalSuperseded && d.DecidedBy == store.DecidedByCritic {
		verb = "re-curated"
	}
	s := verb
	if d.DecidedBy == store.DecidedByOperator || d.DecidedBy == store.DecidedByCritic {
		s += " by " + notesDecider(d)
	}
	if d.DecidedAt != nil {
		s += " at " + d.DecidedAt.Local().Format("2006-01-02 15:04")
	}
	if d.Reason != "" {
		s += ": " + notesOneLine(d.Reason)
	}
	return s
}

// notesDecisionLine is the summary's line for the last decision: "proposal
// 7 applied by the critic (fable) at 2026-10-22 15:04: <reason>".
func notesDecisionLine(d notesDecision) string {
	return fmt.Sprintf("last decision: proposal %d %s", d.ID, notesDecisionState(d))
}

// notesCritiqueCounts writes critiques counted by outcome: "1 applied, 1
// undecided".
func notesCritiqueCounts(n map[string]int) string {
	parts := make([]string, 0, len(n))
	for _, outcome := range slices.Sorted(maps.Keys(n)) {
		parts = append(parts, fmt.Sprintf("%d %s", n[outcome], outcome))
	}
	return strings.Join(parts, ", ")
}

// notesOneLine is agent-written text for one line of the terminal:
// control characters replaced (notes.Printable), newlines and tabs as
// spaces.
func notesOneLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(notes.Printable(strings.TrimSpace(s)))
}

// notesLastDecision is the decision of the repository's proposal decided
// last (nil when none is): its decision time, else its creation, among the
// newest proposals someone decided.
func notesLastDecision(ctx context.Context, st *store.Store, repoID int64) (*notesDecision, error) {
	ps, err := st.NotesProposals(ctx, store.NotesProposalFilter{RepoID: repoID, States: notesDecidedStates, Limit: notesDecisionsShown})
	if err != nil || len(ps) == 0 {
		return nil, err
	}
	at := func(p store.NotesProposal) time.Time {
		if p.DecidedAt != nil {
			return *p.DecidedAt
		}
		return p.CreatedAt
	}
	last := slices.MaxFunc(ps, func(a, b store.NotesProposal) int { return cmp.Or(at(a).Compare(at(b)), cmp.Compare(a.ID, b.ID)) })
	cs, err := st.NotesCritiques(ctx, store.NotesCritiqueFilter{ProposalID: last.ID})
	if err != nil {
		return nil, err
	}
	d := notesDecisionOf(last, cs, nil)
	return &d, nil
}
