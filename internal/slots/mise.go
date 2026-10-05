package slots

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
)

// MiseLocal is the per-checkout mise file magnum renders into every slot.
const MiseLocal = ".mise.local.toml"

// ErrMiseLocal: the main clone's .mise.local.toml cannot be rendered safely.
var ErrMiseLocal = errors.New("slots: cannot render .mise.local.toml")

// miseMarker is the first line of every rendered file.
const miseMarker = "# magnum: rendered from the main clone's .mise.local.toml (secret keys stripped, slot env set); rewritten on every render"

// RenderMiseLocal renders a checkout's .mise.local.toml from the main
// clone's file src. src is parsed as TOML: its `env` value must be absent or
// a table, however it is spelled ([env], [env.KEY] sub-tables, dotted keys,
// a root-level `env = { … }` inline table); every key in strip and every key
// in set is deleted from that table whatever its value (a string, an inline
// table, a sub-table such as { value = "…" }), then each key of set is set
// to its string value. The whole tree is encoded back after miseMarker with
// sorted keys, so the output is deterministic and rendering it again with
// the same arguments yields the same bytes. Comments of src are dropped,
// commented-out secrets with them, and so is the order of its keys. A src
// that does not parse, or whose `env` is not a table ([[env]], a string), is
// ErrMiseLocal. An empty src renders the marker and an [env] table with set.
func RenderMiseLocal(src []byte, strip []string, set map[string]string) ([]byte, error) {
	tree := map[string]any{}
	if _, err := toml.Decode(string(src), &tree); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMiseLocal, err)
	}
	env := map[string]any{}
	if v, ok := tree["env"]; ok {
		if env, ok = v.(map[string]any); !ok {
			return nil, fmt.Errorf("%w: env is a %T, not a table", ErrMiseLocal, v)
		}
	}
	for _, k := range strip {
		delete(env, k)
	}
	for k, v := range set {
		env[k] = v
	}
	tree["env"] = env
	keepLocalTimes(tree)

	var b bytes.Buffer
	b.WriteString(miseMarker + "\n")
	enc := toml.NewEncoder(&b)
	enc.Indent = ""
	if err := enc.Encode(tree); err != nil {
		return nil, fmt.Errorf("%w: encode: %w", ErrMiseLocal, err)
	}
	return b.Bytes(), nil
}

// localTOMLTime is a TOML local date, time or date-time encoded with its
// wall clock: toml.Encoder converts those to UTC first, shifting them by the
// machine's UTC offset on every render.
type localTOMLTime struct {
	t      time.Time
	layout string
}

func (l localTOMLTime) MarshalTOML() ([]byte, error) { return []byte(l.t.Format(l.layout)), nil }

// localTOMLLayouts maps the zones toml.Decode gives local dates, times and
// date-times to their TOML layouts.
var localTOMLLayouts = map[string]string{
	"datetime-local": "2006-01-02T15:04:05.999999999",
	"date-local":     "2006-01-02",
	"time-local":     "15:04:05.999999999",
}

// keepLocalTimes replaces, in place, every local date, time or date-time in
// a decoded TOML value with a localTOMLTime and returns v.
func keepLocalTimes(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			v[k] = keepLocalTimes(e)
		}
	case []map[string]any:
		for _, e := range v {
			keepLocalTimes(e)
		}
	case []any:
		for i, e := range v {
			v[i] = keepLocalTimes(e)
		}
	case time.Time:
		if layout, ok := localTOMLLayouts[v.Location().String()]; ok {
			return localTOMLTime{t: v, layout: layout}
		}
	}
	return v
}

// renderMise writes the slot's .mise.local.toml from the main clone's
// (DefaultStripEnv and the pool's strip_env removed, slot env set), copies
// the pool's other copy_files verbatim, and trusts the rendered file with
// mise. Every write stays inside the checkout (WriteCheckoutFile).
func (m *Manager) renderMise(ctx context.Context, sl store.Slot, pool config.Pool) error {
	src, err := os.ReadFile(filepath.Join(sl.MainClone, MiseLocal))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("slots: read main %s: %w", MiseLocal, err)
	}
	out, err := RenderMiseLocal(src, stripKeys(pool.StripEnv), pool.SlotEnv(sl.Name))
	if err != nil {
		return err
	}
	if err := WriteCheckoutFile(sl.Path, MiseLocal, out, 0o600); err != nil {
		return err
	}
	if err := copyIntoCheckout(sl.MainClone, sl.Path, pool.CopyFiles); err != nil {
		return err
	}
	_, err = m.d.Run.Run(ctx, execx.Cmd{
		Name: m.mise, Args: []string{"-C", sl.Path, "trust", filepath.Join(sl.Path, MiseLocal)}, Dir: sl.Path,
		Mutates: true, Label: "mise trust " + sl.Name,
	})
	if err != nil {
		return fmt.Errorf("slots: mise trust %s: %w", sl.Name, err)
	}
	return nil
}

// copyIntoCheckout copies the copy_files entries files from the main clone
// into checkout (CopyIntoCheckout), skipping .mise.local.toml, which is
// rendered instead.
func copyIntoCheckout(mainClone, checkout string, files []string) error {
	for _, f := range files {
		if filepath.Clean(f) == MiseLocal {
			continue
		}
		if err := CopyIntoCheckout(filepath.Join(mainClone, f), checkout, f); err != nil {
			return err
		}
	}
	return nil
}

// WriteCheckoutFile writes data to rel inside checkout atomically (temp file
// in the same directory, then rename), with permission perm, creating parent
// directories, through an os.Root on checkout: a rel that is absolute or
// escapes (.., or a symlink in a parent pointing outside) is an error and
// nothing is written outside checkout. A symlink at rel itself is replaced,
// not followed.
//
// The checkout holds PR-controlled files (tracked symlinks included), so
// every write magnum makes into one goes through here.
func WriteCheckoutFile(checkout, rel string, data []byte, perm fs.FileMode) (err error) {
	clean := filepath.Clean(rel)
	if !filepath.IsLocal(clean) || clean == "." {
		return fmt.Errorf("slots: write %q in %s: not a path inside the checkout", rel, checkout)
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("slots: write %s in %s: %w", clean, checkout, err)
		}
	}()
	root, err := os.OpenRoot(checkout)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
		return err
	}
	return fsx.WriteFileAtomicIn(root, clean, data, perm)
}

// CopyIntoCheckout copies src (a file outside the checkout, e.g. in the main
// clone) to rel inside checkout through WriteCheckoutFile, keeping src's
// permission bits; a missing src is skipped (nil).
func CopyIntoCheckout(src, checkout, rel string) error {
	fi, err := os.Stat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("slots: copy %s: %w", src, err)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("slots: copy %s: %w", src, err)
	}
	return WriteCheckoutFile(checkout, rel, b, fi.Mode().Perm())
}
