package slots

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
)

// The files magnum writes into a per-PR worktree besides the local mise
// file it renders (which its marker tells apart: withoutRenderedMiseLocal)
// are magnum's too, never a person's: the [[repo]] copy_files, the main
// clone's own files (secrets among them), and whatever the setup wrote. A
// removal saves none of them (saveChanges); the worktree keeps a record of
// them in the kv store (ownFilesKey): the copy_files each render_mise
// copies, and the untracked files the setup step wrote (there after it,
// not before it: recordingSetup). A worktree made before the record
// existed has the copy_files of its next checkout alone.

// ownFilesKey is the kv key of the record of the files magnum wrote into
// slot id: a JSON list of paths relative to the worktree, a folder's with
// a trailing "/".
func ownFilesKey(id int64) string { return fmt.Sprintf("slot.%d.own_files", id) }

// ownFiles is slot sl's record of the files magnum wrote into it (nil: no
// record, or one that cannot be read, which the save then treats as a
// person's files).
func (m *Manager) ownFiles(ctx context.Context, sl store.Slot) []string {
	if sl.ID == 0 {
		return nil
	}
	v, ok, err := m.d.Store.GetKV(ctx, ownFilesKey(sl.ID))
	if err != nil || !ok {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(v), &out) != nil {
		return nil
	}
	return out
}

// recordOwnFiles adds paths to slot sl's record of the files magnum wrote
// into it (ownFiles), or with replace makes them the whole record, never
// either spelling of the local mise file, whose marker decides. Best
// effort: a record that fails is logged, and a removal then saves those
// files as a person's.
func (m *Manager) recordOwnFiles(ctx context.Context, sl store.Slot, paths []string, replace bool) {
	if sl.ID == 0 || m.d.DryRun {
		return
	}
	var all []string
	if !replace {
		all = m.ownFiles(ctx, sl)
	}
	for _, p := range paths {
		if base := strings.TrimSuffix(p, "/"); base != MiseLocal && base != MiseLocalDotted && p != "" {
			all = append(all, p)
		}
	}
	slices.Sort(all)
	all = slices.Compact(all)
	b, err := json.Marshal(all)
	if err == nil {
		err = m.d.Store.SetKV(ctx, ownFilesKey(sl.ID), string(b))
	}
	if err != nil {
		m.logErr(ctx, err, "slots: record the files magnum wrote into %s (a removal saves them as a person's): %v", sl.Name, err)
	}
}

// copiedFiles are the copy_files entries of files that the main clone
// holds, as recordOwnFiles names them: a file by its path, a folder with a
// trailing "/" (every file below it), never the local mise file.
func copiedFiles(mainClone string, files []string) []string {
	var out []string
	for _, f := range files {
		c := filepath.ToSlash(filepath.Clean(f))
		fi, err := os.Stat(filepath.Join(mainClone, c))
		switch {
		case err != nil:
		case fi.IsDir():
			out = append(out, c+"/")
		default:
			out = append(out, c)
		}
	}
	return out
}

// recordingSetup runs setup, the setup step of slot sl, then makes what it
// wrote, with copied (copiedFiles of the copy_files the step's render
// copied), the slot's record of the files magnum wrote into it: the
// untracked files there after it that were not before it. Setup also runs
// over a worktree kept from an earlier attempt (a per-PR row whose setup
// failed, revived; a pool slot provisioned again over its listed
// worktree), whose untracked files may be a person's by then: they stay
// theirs, and so does a file the setup rewrote over one that was there. A
// record a removed row of the same name left goes. Best effort, as
// recordOwnFiles: a listing that fails records copied alone and is
// logged, and a removal then saves what the setup wrote as a person's.
func (m *Manager) recordingSetup(ctx context.Context, sl store.Slot, copied []string, setup func() error) error {
	if sl.ID == 0 || m.d.DryRun {
		return setup()
	}
	before, listed := m.untrackedForRecord(ctx, sl, "before")
	if err := setup(); err != nil {
		return err
	}
	var wrote []string
	if listed {
		if after, ok := m.untrackedForRecord(ctx, sl, "after"); ok {
			had := map[string]bool{}
			for _, p := range before {
				had[p] = true
			}
			wrote = slices.DeleteFunc(after, func(p string) bool { return had[p] })
		}
	}
	m.recordOwnFiles(ctx, sl, append(wrote, copied...), true)
	return nil
}

// untrackedForRecord lists the untracked files of slot sl, when (before or
// after) its setup runs, for recordingSetup; false for a listing that
// fails, which is logged.
func (m *Manager) untrackedForRecord(ctx context.Context, sl store.Slot, when string) ([]string, bool) {
	files, err := m.git.UntrackedFiles(ctx, sl.Path)
	if errors.Is(err, gitx.ErrTreeTooLarge) {
		err = fmt.Errorf("more than the runner's output of names: %w", err)
	}
	if err != nil {
		m.logErr(ctx, err, "slots: list the untracked files of %s %s its setup (a removal saves what the setup wrote as a person's): %v",
			sl.Name, when, err)
		return nil, false
	}
	return files, true
}

// withoutOwn is untracked less the files of own (ownFiles): a path own
// names, or one below a folder it names.
func withoutOwn(untracked, own []string) []string {
	if len(own) == 0 {
		return untracked
	}
	return slices.DeleteFunc(untracked, func(p string) bool {
		for _, o := range own {
			if p == o || strings.HasSuffix(o, "/") && strings.HasPrefix(p, o) {
				return true
			}
		}
		return false
	})
}
