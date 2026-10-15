package engine

// The PR's evidence map (DECISIONS "The judge says what it examined"):
// each round's judge writes what it read and probed (pipeline evidence.go),
// and when its review posts the engine merges that into one map per PR,
// evidence.json in the root of the PR's review reports
// (pipeline.EvidenceMapPath), keyed by path and behaviour: a newer entry
// replaces an older one, a file a round examined is no longer unknown, and
// when a round posts on a new head every entry whose file changed since the
// map's head (git's names of the files that differ between the two heads)
// goes stale, unless the round wrote it again. Each entry keeps the head
// and the round that wrote it. The next rounds' prompts name the map, and
// `magnum status <ref>` counts it.

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
	"time"

	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// maxEvidenceMapEntries bounds a PR's map: past it, the stale entries of
// the oldest rounds go first, then the oldest rounds' others.
const maxEvidenceMapEntries = 3000

// EvidenceMap is a PR's evidence map: what its rounds examined, merged.
type EvidenceMap struct {
	PR string `json:"pr"` // owner/repo#N
	// HeadSHA is the head of the last round merged in, from which the next
	// one measures what changed; Round is that round.
	HeadSHA   string             `json:"head_sha"`
	Round     int                `json:"round"`
	UpdatedAt time.Time          `json:"updated_at"`
	Entries   []EvidenceMapEntry `json:"entries"`
}

// EvidenceMapEntry is an entry of a judge's evidence file with the head and
// the round that wrote it. A stale entry (pipeline.EvidenceStale) keeps the
// state it was written with in Was.
type EvidenceMapEntry struct {
	pipeline.EvidenceEntry
	Was   string `json:"was,omitempty"`
	Head  string `json:"head"`
	Round int    `json:"round"`
	Run   string `json:"run,omitempty"` // the round's judge run
}

// ReadEvidenceMap reads the evidence map at path (pipeline.EvidenceMapPath);
// ok is false when there is none.
func ReadEvidenceMap(path string) (m EvidenceMap, ok bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return EvidenceMap{}, false, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &m)
	}
	if err != nil {
		return EvidenceMap{}, false, fmt.Errorf("evidence map %s: %w", path, err)
	}
	return m, true, nil
}

// EvidenceCounts counts a map's entries by state, and the paths no round
// ever read or probed (FilesNeverRead: only unknown or skipped entries, a
// stale entry counting as what it was).
type EvidenceCounts struct {
	Probed         int `json:"probed"`
	Read           int `json:"read"`
	Skipped        int `json:"skipped"`
	Unknown        int `json:"unknown"`
	Stale          int `json:"stale"`
	FilesNeverRead int `json:"files_never_read"`
}

// Counts counts m's entries (EvidenceCounts).
func (m EvidenceMap) Counts() EvidenceCounts {
	var c EvidenceCounts
	read := map[string]bool{}
	for _, e := range m.Entries {
		switch e.State {
		case pipeline.EvidenceProbed:
			c.Probed++
		case pipeline.EvidenceRead:
			c.Read++
		case pipeline.EvidenceSkipped:
			c.Skipped++
		case pipeline.EvidenceUnknown:
			c.Unknown++
		case pipeline.EvidenceStale:
			c.Stale++
		}
		was := cmp.Or(e.Was, e.State)
		read[e.Path] = read[e.Path] || was == pipeline.EvidenceRead || was == pipeline.EvidenceProbed
	}
	for _, r := range read {
		if !r {
			c.FilesNeverRead++
		}
	}
	return c
}

// Sentence is the card's line: "41 behaviours probed, 12 read, 9 unknown,
// 4 stale (3 files never read)", the skipped ones only when there are any.
func (c EvidenceCounts) Sentence() string {
	parts := []string{textx.Count(c.Probed, "behaviour", "behaviours") + " probed", fmt.Sprintf("%d read", c.Read)}
	if c.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", c.Skipped))
	}
	parts = append(parts, fmt.Sprintf("%d unknown", c.Unknown), fmt.Sprintf("%d stale", c.Stale))
	s := strings.Join(parts, ", ")
	if c.FilesNeverRead > 0 {
		s += " (" + textx.Count(c.FilesNeverRead, "file", "files") + " never read)"
	}
	return s
}

// evidenceRound is the round whose evidence a merge takes in: the head its
// judge examined, its number and its judge run.
type evidenceRound struct {
	head  string
	round int
	run   string
}

// mergeEvidence merges the evidence of a round that posted on target into
// PR pr's map (onPosted). When the map's head is another, the files that
// differ between the two (Git.ChangedBetween in the round's checkout) mark
// what the map held there stale; a diff git cannot read marks every such
// entry stale. A round without evidence still moves the map to its head.
// Failures only warn: the map is an input, never a gate.
func (e *Engine) mergeEvidence(ctx context.Context, job *roundJob, pr store.PR, target string, res pipeline.RoundResult) {
	path := pipeline.EvidenceMapPath(e.d.Layout, job.repo.Owner, job.repo.Name, pr.Number)
	old, ok, err := ReadEvidenceMap(path)
	if err != nil {
		e.log.Warn("evidence map unreadable; starting it over", "pr", pr.ID, "err", err)
	}
	if !ok && res.Evidence == nil {
		return
	}
	var changed []string
	known := true
	if old.HeadSHA != "" && old.HeadSHA != target {
		if changed, err = e.d.Git.ChangedBetween(ctx, job.slot.Path, old.HeadSHA, target); err != nil {
			known = false
			e.log.Info("evidence map: no diff of the heads; what the older head examined is stale", "pr", pr.ID,
				"from", textx.ShortSHA(old.HeadSHA), "to", textx.ShortSHA(target), "err", err)
		}
	}
	m, staled := mergeEvidenceMap(old, res.Evidence, evidenceRound{head: target, round: res.Round, run: res.JudgeRunID}, changed, known)
	m.PR, m.UpdatedAt = fmt.Sprintf("%s#%d", job.repo.FullName(), pr.Number), e.now().UTC()
	b, err := json.MarshalIndent(m, "", "  ")
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
			err = fsx.WriteFileAtomic(path, append(b, '\n'), 0o600)
		}
	}
	if err != nil {
		e.log.Warn("write the evidence map", "pr", pr.ID, "err", err)
		return
	}
	c := m.Counts()
	e.event(ctx, "info", prSubject(job.repo, pr.Number), "pr.evidence",
		fmt.Sprintf("evidence map on %s: %s; %d went stale", textx.ShortSHA(target), c.Sentence(), staled),
		map[string]any{"counts": c, "went_stale": staled, "round": res.Round, "entries": len(m.Entries)})
}

// mergeEvidenceMap is old with the round's evidence ev merged in (nil =
// none) and how many entries went stale: on a new head an entry read,
// probed or skipped on a file changed (every one when !known) goes stale;
// ev's entries replace those of the same path and behaviour (case aside);
// a file ev read, probed or skipped loses its file-level unknown entries
// (no behaviour). Entries are sorted by path and behaviour.
func mergeEvidenceMap(old EvidenceMap, ev *pipeline.Evidence, at evidenceRound, changed []string, known bool) (EvidenceMap, int) {
	moved := old.HeadSHA != "" && old.HeadSHA != at.head
	entries := make([]EvidenceMapEntry, 0, len(old.Entries))
	staled := map[string]bool{} // the keys that went stale now
	key := func(path, behaviour string) string { return path + "\x00" + strings.ToLower(behaviour) }
	for _, x := range old.Entries {
		switch x.State {
		case pipeline.EvidenceRead, pipeline.EvidenceProbed, pipeline.EvidenceSkipped:
			if moved && (!known || slices.Contains(changed, x.Path)) {
				x.Was, x.State = x.State, pipeline.EvidenceStale
				staled[key(x.Path, x.Behaviour)] = true
			}
		}
		entries = append(entries, x)
	}
	if ev != nil {
		index := map[string]int{}
		for i, x := range entries {
			index[key(x.Path, x.Behaviour)] = i
		}
		examined := map[string]bool{}
		for _, n := range ev.Entries {
			x := EvidenceMapEntry{EvidenceEntry: n, Head: at.head, Round: at.round, Run: at.run}
			if n.State != pipeline.EvidenceUnknown {
				examined[n.Path] = true
			}
			k := key(n.Path, n.Behaviour)
			if i, ok := index[k]; ok {
				delete(staled, k) // written again: fresh
				entries[i] = x
				continue
			}
			index[k] = len(entries)
			entries = append(entries, x)
		}
		entries = slices.DeleteFunc(entries, func(x EvidenceMapEntry) bool {
			return x.State == pipeline.EvidenceUnknown && x.Behaviour == "" && examined[x.Path]
		})
	}
	entries = capEvidence(entries)
	slices.SortStableFunc(entries, func(a, b EvidenceMapEntry) int {
		return cmp.Or(strings.Compare(a.Path, b.Path), strings.Compare(a.Behaviour, b.Behaviour))
	})
	return EvidenceMap{HeadSHA: at.head, Round: at.round, Entries: entries}, len(staled)
}

// capEvidence keeps at most maxEvidenceMapEntries entries: the stale ones
// of the oldest rounds go first, then the oldest rounds' others.
func capEvidence(entries []EvidenceMapEntry) []EvidenceMapEntry {
	over := len(entries) - maxEvidenceMapEntries
	if over <= 0 {
		return entries
	}
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	stale := func(i int) int {
		if entries[i].State == pipeline.EvidenceStale {
			return 0
		}
		return 1
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return cmp.Or(cmp.Compare(stale(a), stale(b)), cmp.Compare(entries[a].Round, entries[b].Round))
	})
	drop := map[int]bool{}
	for _, i := range order[:over] {
		drop[i] = true
	}
	kept := make([]EvidenceMapEntry, 0, maxEvidenceMapEntries)
	for i, x := range entries {
		if !drop[i] {
			kept = append(kept, x)
		}
	}
	return kept
}
