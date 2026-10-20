package settings

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zhuravel/magnum/internal/confedit"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
)

// historyDirOf is where the saves of layout's user config keep the files
// they replace.
func historyDirOf(l paths.Layout) string { return filepath.Join(l.Data(), "config-history") }

// History lists the earlier versions of layout's user config the saves
// kept, newest first: Snapshot.History without loading the current file,
// which a hand edit may have broken.
func History(layout paths.Layout) ([]HistoryEntry, error) { return listHistory(historyDirOf(layout)) }

// RestoreFile is the save that puts e back over layout's user config as it
// is on disk, whether or not it loads: Snapshot.Restore for a file that no
// longer loads (a hand edit broke it). The version must load over base and
// render, as any save's file must; Commit writes it, refusing a file that
// changed since, and keeps the replaced file in the history.
func RestoreFile(layout paths.Layout, base string, e HistoryEntry) (*Plan, error) {
	dir := historyDirOf(layout)
	data, err := readHistory(dir, e.Path)
	if err != nil {
		return nil, err
	}
	if err := checkWritable(layout.UserConfig); err != nil {
		return nil, err
	}
	cur, _, err := readConfig(layout.UserConfig)
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfig(layout, base, data)
	if err != nil {
		return nil, fmt.Errorf("settings: magnum would not load the kept version: %w", err)
	}
	if _, err := engine.CheckPrompts(cfg); err != nil {
		return nil, fmt.Errorf("settings: the prompts do not render with the kept version:\n%w", err)
	}
	keys, _ := confedit.Changed(cur, data) // the keys, for logs; a file that does not parse names none
	p := &Plan{Path: layout.UserConfig, Old: cur, New: data, Sum: sha256.Sum256(cur), NewSum: sha256.Sum256(data),
		Warnings: cfg.Warnings(), Keys: keys, history: dir}
	p.Hunks = hunksOf(cur, data)
	p.Diff = unified(layout.UserConfig, p.Hunks)
	p.Count = len(p.Hunks)
	return p, nil
}

// hunksOf diffs old against out, each change with the table it lies in;
// an old file the scanner cannot read labels its hunks by the new file's
// tables, or "the file".
func hunksOf(old, out []byte) []Hunk {
	if doc, err := confedit.Parse(old); err == nil {
		return hunks(doc, out)
	}
	a, b := lines(old), lines(out)
	newDoc, _ := confedit.Parse(out)
	var hs []Hunk
	for _, op := range diffLines(a, b) {
		h := Hunk{Line: op.aStart + 1, Removed: a[op.aStart:op.aEnd], Added: b[op.bStart:op.bEnd], Table: "the file"}
		if newDoc != nil && len(h.Added) > 0 {
			first := 0
			for first < len(h.Added)-1 && strings.TrimSpace(h.Added[first]) == "" {
				first++
			}
			h.Table = tableLabel(newDoc, newDoc.TableAt(op.bStart+1+first))
		}
		hs = append(hs, h)
	}
	return hs
}
