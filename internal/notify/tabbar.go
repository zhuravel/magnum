package notify

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/zhuravel/magnum/internal/execx"
)

// WriteTabBar replaces the tab-bar file (Layout.TabBar(), state/tabbar.txt)
// with text, atomically: readers (herdr runs `cat` on it every few seconds)
// see either the old or the new content, never a partial write.
//
// herdr renders only the last output line, strips ESC bytes and drops the whole
// status area when the line does not fit, so text is flattened to a single
// plain line (newlines and tabs become spaces, other control characters are
// dropped, secrets redacted) and terminated by one newline. Empty text
// produces an empty file. Keeping the line short is the caller's job. When the
// file already holds exactly this content nothing is written, so calling it
// every daemon tick costs one small read.
func (n *Notifier) WriteTabBar(text string) error {
	if n.Layout.Home == "" {
		return fmt.Errorf("notify: tab bar: layout has no home directory")
	}
	path := n.Layout.TabBar()
	data := tabBarContent(text)
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("notify: tab bar: %w", err)
	}
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("notify: tab bar: %w", err)
	}
	return nil
}

func tabBarContent(text string) []byte {
	text = execx.Redact(text)
	text = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, text)
	text = strings.TrimSpace(text)
	if text == "" {
		return []byte{}
	}
	return []byte(text + "\n")
}

// writeFileAtomic writes data to path through a temp file in the same
// directory and a rename, so a crash or a concurrent reader never sees a
// partial file. The temp file is removed on failure.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(perm); err == nil {
		if _, err = f.Write(data); err == nil {
			err = f.Sync()
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
