package pipeline

// The evidence file (DECISIONS "The judge says what it examined"): a
// finding locates a problem and says nothing about the areas that passed,
// and a Checks line is a command in prose, so nobody could tell which files
// and behaviours of a long PR a round examined and which none ever did; on
// talkable#11483 three latent defects sat in areas 38 rounds never probed.
// The judge writes evidence.json beside its result file (`evidence_file`):
// one entry per behaviour it read or probed in the round, a probe with its
// command and whether it ran a control, and one `unknown` entry per file of
// the diff it reviewed that it did not read. The round reads the file at
// its end, drops what it cannot use and hands the rest to the engine
// (RoundResult.Evidence), which merges it into the PR's evidence map
// (EvidenceMapPath, engine evidence.go). The map is an input to the next
// rounds: the judge and claude-review prompts name it, its unknown and
// stale areas being leads to read first. Paths and behaviours are data in
// the files, never in a prompt or an event.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/textx"
)

// EvidenceFile is the judge's evidence file in its report directory, and
// the name of the PR's evidence map (EvidenceMapPath).
const EvidenceFile = agents.EvidenceFile

// The states of an evidence entry. The judge writes the first four;
// EvidenceStale is the engine's, for an entry of the map whose file changed
// since its round wrote it.
const (
	EvidenceRead    = "read"    // the judge read the code of the behaviour
	EvidenceProbed  = "probed"  // it ran a probe (Command), with a control or not
	EvidenceSkipped = "skipped" // it left the behaviour alone on purpose (Result says why)
	EvidenceUnknown = "unknown" // nobody read it: a file of the diff the judge did not read
	EvidenceStale   = "stale"   // the map's: read, probed or skipped on a head its file changed since
)

// evidenceStates are the states a judge's entry may carry.
var evidenceStates = []string{EvidenceRead, EvidenceProbed, EvidenceSkipped, EvidenceUnknown}

// maxEvidenceEntries bounds the entries kept from one round's file (the
// prompt asks for 30 to 80 for 30 files); the rest are dropped and counted.
const maxEvidenceEntries = 400

// Caps on an entry's free text, in characters.
const (
	maxEvidenceText    = 300 // symbol, behaviour, result
	maxEvidenceCommand = 500
)

// EvidenceEntry is one behaviour the judge read or probed, or one file it
// did not read (State EvidenceUnknown, no Behaviour).
type EvidenceEntry struct {
	Path      string `json:"path"`                // relative to the repository's top
	Symbol    string `json:"symbol,omitempty"`    // e.g. Restorer#apply_incentives
	Behaviour string `json:"behaviour,omitempty"` // what holds, one clause
	State     string `json:"state"`               // EvidenceRead, EvidenceProbed, EvidenceSkipped, EvidenceUnknown
	Command   string `json:"command,omitempty"`   // a probe's command
	Control   bool   `json:"control,omitempty"`   // the probe ran a control
	Result    string `json:"result,omitempty"`    // a probe's result; a skip's reason
}

// Evidence is the judge's evidence file as the round kept it.
type Evidence struct {
	HeadSHA string          `json:"head_sha"` // the head the judge examined ("" = the round's)
	Entries []EvidenceEntry `json:"entries"`
}

// EvidenceMapPath is PR owner/repo#number's evidence map: EvidenceFile in
// the root of its review reports, above the per-head directories.
func EvidenceMapPath(l paths.Layout, owner, repo string, number int) string {
	return filepath.Join(l.PRReviews(owner, repo, number), EvidenceFile)
}

// evidenceMap is the PR's evidence map for the prompts: its path when it
// exists, "" in a blind replay (whose judge reads nothing after its head),
// in a continued turn (its paused turn had it) and when the PR has none.
func (rd *round) evidenceMap() string {
	if rd.in.Blind || rd.in.Kind == KindContinue {
		return ""
	}
	p := EvidenceMapPath(rd.r.Layout, rd.owner, rd.name, rd.in.PR.Number)
	if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return p
}

// clearEvidence removes file, the judge's evidence file, before the
// judge's first prompt of a round that is not a continue: one an earlier
// round on this head left is not this round's. A continued turn keeps what
// its paused turn wrote.
func (rd *round) clearEvidence(ctx context.Context, file string) {
	if rd.in.Kind == KindContinue || file == "" {
		return
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
		rd.logErr(ctx, err, "pipeline: remove %s: %v", file, err)
	}
}

// readEvidence reads the judge's evidence file at the round's end into the
// round's result (RoundResult.Evidence; nil when there is none or it
// cannot be read) and warns about what it dropped (validEvidence).
func (rd *round) readEvidence(ctx context.Context, file string) {
	if file == "" {
		return
	}
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	var ev Evidence
	if err == nil {
		err = json.Unmarshal(b, &ev)
	}
	if err != nil {
		rd.warn(ctx, "ignoring the judge's %s: %v", EvidenceFile, err)
		return
	}
	dropped := validEvidence(&ev, rd.in.SlotPath)
	rd.mu.Lock()
	ev.HeadSHA = cmp.Or(strings.TrimSpace(ev.HeadSHA), rd.res.TargetSHA)
	rd.mu.Unlock()
	if dropped > 0 {
		rd.warn(ctx, "the judge's %s: %s dropped (no path, a state other than %s, or more than %d)",
			EvidenceFile, textx.Count(dropped, "entry", "entries"), strings.Join(evidenceStates, ", "), maxEvidenceEntries)
	}
	rd.mu.Lock()
	rd.res.Evidence = &ev
	rd.mu.Unlock()
	rd.event(ctx, "info", "round.evidence", fmt.Sprintf("the judge's %s: %s", EvidenceFile, evidenceSummary(&ev)),
		map[string]any{"entries": len(ev.Entries), "dropped": dropped, "file": file})
}

// validEvidence keeps ev's usable entries and returns how many it dropped:
// one without a path, one whose state is not a judge's (case aside), and
// those past maxEvidenceEntries. A path is made relative to the checkout
// it names, a probe without a command is kept as read, and free text is
// cut to its cap.
func validEvidence(ev *Evidence, checkout string) int {
	kept := ev.Entries[:0]
	dropped := 0
	for _, e := range ev.Entries {
		e.Path = evidencePath(e.Path, checkout)
		e.State = strings.ToLower(strings.TrimSpace(e.State))
		if e.Path == "" || !slices.Contains(evidenceStates, e.State) || len(kept) == maxEvidenceEntries {
			dropped++
			continue
		}
		e.Symbol = textx.Clip(strings.TrimSpace(e.Symbol), maxEvidenceText)
		e.Behaviour = textx.Clip(strings.TrimSpace(e.Behaviour), maxEvidenceText)
		e.Result = textx.Clip(strings.TrimSpace(e.Result), maxEvidenceText)
		e.Command = textx.Clip(strings.TrimSpace(e.Command), maxEvidenceCommand)
		if e.State == EvidenceProbed && e.Command == "" {
			e.State, e.Control = EvidenceRead, false
		}
		kept = append(kept, e)
	}
	ev.Entries = kept
	return dropped
}

// evidencePath is p relative to the repository's top: trimmed, without a
// leading "./" or the checkout's own path.
func evidencePath(p, checkout string) string {
	p = strings.TrimSpace(p)
	if checkout != "" {
		p = strings.TrimPrefix(p, strings.TrimSuffix(checkout, "/")+"/")
	}
	return strings.TrimPrefix(p, "./")
}

// evidenceSummary counts ev's entries by state for the round's event.
func evidenceSummary(ev *Evidence) string {
	n := map[string]int{}
	for _, e := range ev.Entries {
		n[e.State]++
	}
	return fmt.Sprintf("%d probed, %d read, %d skipped, %d unknown", n[EvidenceProbed], n[EvidenceRead], n[EvidenceSkipped], n[EvidenceUnknown])
}
