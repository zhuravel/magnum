package launchd

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

const (
	// bootoutWait bounds how long Install waits, after bootout, for launchd to
	// finish tearing the old job down (bootout returns before the daemon has
	// exited; its own shutdown and any child it kills can take tens of seconds,
	// and the CLI budgets 30 s for a SIGTERMed daemon). bootoutPoll separates
	// the Status polls.
	bootoutWait = 30 * time.Second
	bootoutPoll = time.Second

	// bootstrapAttempts is how many times Install runs `launchctl bootstrap`
	// on the transient "Input/output error" before giving up; bootstrapDelay
	// separates the attempts. It is only a brief fallback: the real wait is
	// the Status poll above.
	bootstrapAttempts = 3
	bootstrapDelay    = time.Second

	launchctlTimeout = 30 * time.Second
)

// timing is Install's injectable clock: tests replace it so nothing really
// sleeps.
type timing struct {
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// realTiming is the wall clock with a ctx-aware sleep.
var realTiming = timing{now: time.Now, sleep: sleepCtx}

// target is the launchctl service target "gui/<uid>/<label>".
func target(uid int, label string) string { return fmt.Sprintf("gui/%d/%s", uid, label) }

func domain(uid int) string { return fmt.Sprintf("gui/%d", uid) }

func launchctl(label string, mutates bool, args ...string) execx.Cmd {
	return execx.Cmd{
		Name:    "launchctl",
		Args:    args,
		Timeout: launchctlTimeout,
		Mutates: mutates,
		Label:   label,
	}
}

// Install (re)loads the job described by plist into gui/<uid>.
//
// It writes plist to path (0644, atomically), then runs
//
//	launchctl bootout   gui/<uid>/<label>   (errors ignored: not loaded yet)
//	launchctl print     gui/<uid>/<label>   (polled until the job is gone)
//	launchctl enable    gui/<uid>/<label>   (best effort: undoes an old disable)
//	launchctl bootstrap gui/<uid> <path>
//
// bootout does not wait for the old job to finish tearing down, and
// bootstrapping into that window fails with "Input/output error". Install
// therefore polls Status once a second until launchd no longer knows the job
// (at most 30 s; a job that is still loaded then is reported as such), and
// only then bootstraps. The transient error can still show up, so bootstrap is
// retried up to three times, one second apart, on that error only; any other
// failure is returned immediately. When the retries run out, the error blames
// a missing gui/<uid> domain (no console login session) only if
// `launchctl print gui/<uid>` itself fails. The label is read from the plist
// itself.
func Install(ctx context.Context, run execx.Runner, uid int, path string, plist []byte) error {
	return install(ctx, run, uid, path, plist, realTiming)
}

func install(ctx context.Context, run execx.Runner, uid int, path string, plist []byte, tm timing) error {
	label, err := labelFromPlist(plist)
	if err != nil {
		return fmt.Errorf("launchd install: %w", err)
	}
	if err := writeFileAtomic(path, plist); err != nil {
		return fmt.Errorf("launchd install: write %s: %w", path, err)
	}

	// Both are best effort; bootstrap below is the arbiter.
	_, _ = run.Run(ctx, launchctl("launchctl bootout", true, "bootout", target(uid, label)))
	stillLoaded, err := waitUnloaded(ctx, run, uid, label, tm)
	if err != nil {
		return fmt.Errorf("launchd install: waiting for %s to unload: %w", label, err)
	}
	_, _ = run.Run(ctx, launchctl("launchctl enable", true, "enable", target(uid, label)))

	var lastErr error
	for attempt := 1; attempt <= bootstrapAttempts; attempt++ {
		res, err := run.Run(ctx, launchctl("launchctl bootstrap", true, "bootstrap", domain(uid), path))
		if err == nil {
			return nil
		}
		lastErr = err
		if !isEIO(res, err) {
			return fmt.Errorf("launchctl bootstrap %s: %w", domain(uid), err)
		}
		if attempt < bootstrapAttempts {
			if serr := tm.sleep(ctx, bootstrapDelay); serr != nil {
				return fmt.Errorf("launchctl bootstrap %s: %w", domain(uid), serr)
			}
		}
	}
	return fmt.Errorf("launchctl bootstrap %s failed after %d attempts%s: %w",
		domain(uid), bootstrapAttempts, bootstrapEvidence(ctx, run, uid, label, stillLoaded), lastErr)
}

// waitUnloaded polls Status until launchd no longer knows the job, for at most
// bootoutWait. stillLoaded is true when the deadline passed with the job
// loaded. A Status failure ends the wait with stillLoaded false (the state is
// unknown, and bootstrap reports the real problem); only a done ctx is an
// error.
func waitUnloaded(ctx context.Context, run execx.Runner, uid int, label string, tm timing) (stillLoaded bool, err error) {
	deadline := tm.now().Add(bootoutWait)
	for {
		st, serr := Status(ctx, run, uid, label)
		if serr != nil {
			return false, ctx.Err()
		}
		if !st.Loaded() {
			return false, nil
		}
		remaining := deadline.Sub(tm.now())
		if remaining <= 0 {
			return true, nil
		}
		if err := tm.sleep(ctx, min(bootoutPoll, remaining)); err != nil {
			return false, err
		}
	}
}

// bootstrapEvidence explains an exhausted EIO retry with what was observed,
// never with a guess: the old job still loaded after the wait, or
// `launchctl print gui/<uid>` failing (the domain, which exists only during a
// console login session, may be missing). Otherwise it adds nothing and the
// caller's error stands alone.
func bootstrapEvidence(ctx context.Context, run execx.Runner, uid int, label string, stillLoaded bool) string {
	if stillLoaded {
		return fmt.Sprintf("; the previous %s job was still loaded %s after bootout", label, bootoutWait)
	}
	_, err := run.Run(ctx, execx.Cmd{
		Name:    "launchctl",
		Args:    []string{"print", domain(uid)},
		Timeout: launchctlTimeout,
		Label:   "launchctl print",
	})
	var ee *execx.ExitError
	if errors.As(err, &ee) {
		return fmt.Sprintf("; `launchctl print %s` also fails, so the domain may not exist "+
			"(is there a console login session? log in on the console, then re-install)", domain(uid))
	}
	return ""
}

// Uninstall unloads the job and deletes its plist at path. It is idempotent: a
// job that is not loaded and a missing file are not errors. If bootout fails
// while the job is still loaded, the error is returned and the file is kept so
// the daemon is not orphaned without its definition.
func Uninstall(ctx context.Context, run execx.Runner, uid int, label, path string) error {
	if err := Bootout(ctx, run, uid, label); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("launchd uninstall: %w", err)
	}
	return nil
}

// Bootout unloads the job (`launchctl bootout`) but keeps its plist, so
// `launchctl bootstrap` or `magnum install` can load it again. A bootout
// error is ignored only when Status confirms the job is not loaded; if Status
// itself fails, the job's state is unknown and the bootout error is returned.
func Bootout(ctx context.Context, run execx.Runner, uid int, label string) error {
	_, err := run.Run(ctx, launchctl("launchctl bootout", true, "bootout", target(uid, label)))
	if err != nil {
		st, serr := Status(ctx, run, uid, label)
		if serr != nil {
			// Not knowing whether the job is gone is not "gone".
			return fmt.Errorf("launchctl bootout %s: %w (and its state is unknown: %w)", target(uid, label), err, serr)
		}
		if st.Loaded() {
			return fmt.Errorf("launchctl bootout %s: %w", target(uid, label), err)
		}
	}
	return nil
}

// Kickstart restarts the job now (`launchctl kickstart -k`), killing the
// running instance first if there is one.
func Kickstart(ctx context.Context, run execx.Runner, uid int, label string) error {
	if _, err := run.Run(ctx, launchctl("launchctl kickstart", true, "kickstart", "-k", target(uid, label))); err != nil {
		return fmt.Errorf("launchctl kickstart %s: %w", target(uid, label), err)
	}
	return nil
}

// isEIO reports whether a failed launchctl call was the transient
// "Input/output error" (exit 5).
func isEIO(res execx.Result, err error) bool {
	text := string(res.Stderr)
	if err != nil {
		text += "\n" + err.Error()
	}
	return strings.Contains(strings.ToLower(text), "input/output error")
}

// sleepCtx waits for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// writeFileAtomic writes data to path with mode 0644 via a temp file in the
// same directory, flushed to disk before the rename, so a reader (launchd)
// never sees a partial plist, even after a crash.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".magnum-plist-*")
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Chmod(0o644); err != nil { // CreateTemp makes 0600; ignore umask
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	done = true
	return nil
}

// labelFromPlist extracts the top-level Label string from plist XML.
func labelFromPlist(plist []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(plist))
	depth := 0 // plist=1, top-level dict=2, its keys and values=3
	lastKey := ""
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", errors.New("plist has no top-level Label string")
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth != 3 {
				continue
			}
			switch t.Name.Local {
			case "key", "string":
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return "", fmt.Errorf("parse plist: %w", err)
				}
				depth--
				if t.Name.Local == "key" {
					lastKey = s
				} else {
					if lastKey == "Label" && s != "" {
						return s, nil
					}
					lastKey = ""
				}
			default:
				lastKey = ""
			}
		case xml.EndElement:
			depth--
		}
	}
}
