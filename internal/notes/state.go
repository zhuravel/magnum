package notes

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/fsx"
)

// State is the whole content of a repository's notes: the notes text and
// every harness file with its body. It is what a version in the registry
// holds (store.NotesVersion) and what an applied proposal writes.
type State struct {
	Exists bool // the notes file exists (false: Notes is empty)
	Notes  []byte
	Files  []Blob // sorted by Path
}

// Blob is one harness file with its content.
type Blob struct {
	Path   string // slash-separated, relative to the harness directory
	SHA256 string
	Body   []byte
}

// Caps on what ReadState reads: a harness this large is not notes, and a
// version must fit the registry.
const (
	MaxFileBytes  = 8 << 20
	MaxStateBytes = 64 << 20
)

// ErrTooLarge is ReadState's error for a harness past MaxFileBytes or
// MaxStateBytes.
var ErrTooLarge = errors.New("notes: the harness is too large to record")

// ReadState reads r's notes and harness: regular files only, as List lists
// them. A missing notes file or harness reads empty.
func ReadState(r Repo) (State, error) {
	text, exists, err := readNotes(r.Notes())
	if err != nil {
		return State{}, err
	}
	s := State{Exists: exists, Notes: text}
	files, err := List(r.Harness())
	if err != nil {
		return State{}, err
	}
	if len(files) == 0 {
		return s, nil
	}
	root, err := os.OpenRoot(r.Harness())
	if err != nil {
		return State{}, err
	}
	defer root.Close()
	total := int64(len(text))
	for _, f := range files {
		if f.Size > MaxFileBytes {
			return State{}, fmt.Errorf("%w: %s is %d bytes", ErrTooLarge, f.Name, f.Size)
		}
		body, err := root.ReadFile(f.Name)
		if err != nil {
			return State{}, err
		}
		if total += int64(len(body)); total > MaxStateBytes {
			return State{}, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, MaxStateBytes)
		}
		// Hashed again: the file may have changed since it was listed.
		s.Files = append(s.Files, Blob{Path: f.Name, SHA256: TextSHA(body), Body: body})
	}
	return s, nil
}

// Listing is s's harness as a listing (names, sizes, hashes).
func (s State) Listing() []File {
	out := make([]File, 0, len(s.Files))
	for _, b := range s.Files {
		out = append(out, File{Name: b.Path, Size: int64(len(b.Body)), SHA256: b.SHA256})
	}
	return out
}

// Fingerprint identifies s (Fingerprint of its notes and listing).
func (s State) Fingerprint() string { return Fingerprint(s.Notes, s.Listing()) }

// Size measures s against maxLine (Size.Over compares it with the limits).
func (s State) Size(maxLine int) Size {
	var out Size
	if s.Exists {
		out = MeasureText(s.Notes, maxLine)
	}
	out.HarnessFiles = len(s.Files)
	for _, b := range s.Files {
		out.HarnessBytes += int64(len(b.Body))
	}
	return out
}

// Same reports whether s and o hold the same notes and harness.
func (s State) Same(o State) bool { return s.Fingerprint() == o.Fingerprint() }

// WriteState makes r's notes and harness s: the notes file is replaced
// atomically (removed when s has none), and the harness by a complete new
// directory renamed into place, the old one removed after. The caller holds
// r's lock (Lock). A harness path that is not clean (cleanName) is refused
// before anything is written.
func WriteState(r Repo, s State) error {
	for _, b := range s.Files {
		if !cleanName(b.Path) {
			return fmt.Errorf("notes: harness path %q is not a clean relative path", b.Path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(r.Notes()), 0o700); err != nil {
		return err
	}
	stage := r.Harness() + ".new-" + rand.Text()[:8]
	if err := os.Mkdir(stage, 0o700); err != nil {
		return err
	}
	staged := false
	defer func() {
		if !staged {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := writeTree(stage, s.Files); err != nil {
		return err
	}
	if s.Exists {
		if err := fsx.WriteFileAtomic(r.Notes(), s.Notes, 0o600); err != nil {
			return err
		}
	} else if err := os.Remove(r.Notes()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	old := r.Harness() + ".old-" + rand.Text()[:8]
	switch err := os.Rename(r.Harness(), old); {
	case errors.Is(err, fs.ErrNotExist):
		old = ""
	case err != nil:
		return err
	}
	if err := os.Rename(stage, r.Harness()); err != nil {
		if old != "" {
			_ = os.Rename(old, r.Harness())
		}
		return err
	}
	staged = true
	if old != "" {
		return os.RemoveAll(old)
	}
	return nil
}

// writeTree writes files under dir, confined to it (os.Root).
func writeTree(dir string, files []Blob) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, b := range files {
		if d := filepath.Dir(filepath.FromSlash(b.Path)); d != "." {
			if err := root.MkdirAll(d, 0o700); err != nil {
				return err
			}
		}
		// Scripts stay runnable: the judge runs them as they are.
		if err := root.WriteFile(filepath.FromSlash(b.Path), b.Body, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// The lock protocol of the judges (agents.NotesLockLine): a directory made
// with mkdir, waited for, and taken over when older than ten minutes (its
// holder died).
const (
	lockPoll  = 2 * time.Second
	lockStale = 10 * time.Minute
)

// ErrBusy is Lock's error when another holder kept the lock past the wait.
var ErrBusy = errors.New("notes busy: another writer holds the notes lock")

// lockSleep waits between attempts; tests replace it.
var lockSleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Lock takes the notes lock at path (Repo.Lock) as the judges do, retrying
// until wait has passed (ErrBusy); it returns the release.
func Lock(ctx context.Context, path string, wait time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := os.Mkdir(path, 0o700)
		if err == nil {
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if fi, serr := os.Stat(path); serr == nil && time.Since(fi.ModTime()) > lockStale {
			_ = os.Remove(path)
			continue
		}
		if !time.Now().Before(deadline) {
			return nil, ErrBusy
		}
		if err := lockSleep(ctx, min(lockPoll, max(time.Until(deadline), time.Millisecond))); err != nil {
			return nil, err
		}
	}
}

// sortBlobs orders blobs by path.
func sortBlobs(bs []Blob) {
	slices.SortFunc(bs, func(a, b Blob) int { return strings.Compare(a.Path, b.Path) })
}
