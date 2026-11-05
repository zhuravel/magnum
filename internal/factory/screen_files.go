package factory

// The screen's small files beside factory.db: when you last saw home
// (seen.json, the screen's, under a lock of its own) and the screen's
// latest focus report (focus.json, the daemon's, which the alerts' delivery
// reads). Each is one record, written atomically.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
)

// seenRecord is seen.json (Layout.FactorySeen).
type seenRecord struct {
	At time.Time `json:"at"`
}

// ReadSeen is when you last saw the factory screen's home, as SaveSeen
// kept it in layout l; the zero time before the first.
func ReadSeen(l paths.Layout) (time.Time, error) {
	b, err := os.ReadFile(l.FactorySeen())
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	var r seenRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return time.Time{}, fmt.Errorf("%s: %w", filepath.Base(l.FactorySeen()), err)
	}
	return r.At, nil
}

// SaveSeen keeps that you saw home at at in seen.json of layout l, so it
// survives a restart of the screen and of the daemon. The time only moves
// forward: an earlier time than the one kept changes nothing, and two
// screens saving at once take turns on a lock of the file's own, so the
// later time stays.
func SaveSeen(l paths.Layout, at time.Time) error {
	path := l.FactorySeen()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	if cur, err := ReadSeen(l); err == nil && !at.After(cur) {
		return nil
	}
	b, err := json.Marshal(seenRecord{At: at.Round(0)})
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, append(b, '\n'), 0o600)
}

// ReadScreenFocus is the screen's latest focus as the daemon keeps it
// (Layout.FactoryFocus); none yet is the zero FocusPayload, which is not
// Live.
func ReadScreenFocus(l paths.Layout) (FocusPayload, error) {
	var f FocusPayload
	b, err := os.ReadFile(l.FactoryFocus())
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return FocusPayload{}, fmt.Errorf("%s: %w", filepath.Base(l.FactoryFocus()), err)
	}
	return f, nil
}
