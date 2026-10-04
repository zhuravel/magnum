package cli

// What of mise doctor checks depends on what uses it: pool slots run their
// tools through `mise exec`, the LaunchAgent starts the daemon through mise
// (`magnum install`), and agents' tool shells see mise's shims only when
// ~/.zprofile activates them. A machine without any of these has nothing to
// check.

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/launchd"
)

// doctorPlistPath is the installed LaunchAgent's plist; "" without a home.
func doctorPlistPath(d doctorDeps) string {
	if d.UserHome == "" {
		return ""
	}
	return launchd.AgentPath(d.UserHome, launchd.DefaultLabel)
}

// doctorPlistProgram reads the installed LaunchAgent's program (the first
// of its ProgramArguments); found is false when no plist is installed.
func doctorPlistProgram(d doctorDeps) (program string, found bool, err error) {
	path := doctorPlistPath(d)
	if path == "" {
		return "", false, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", true, err
	}
	args, err := plistProgramArguments(b)
	if err != nil {
		return "", true, err
	}
	if len(args) == 0 {
		return "", true, errors.New("no ProgramArguments")
	}
	return args[0], true, nil
}

// plistProgramArguments extracts the ProgramArguments array of an XML
// property list.
func plistProgramArguments(b []byte) ([]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	key := ""
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch {
		case se.Name.Local == "key":
			if err := dec.DecodeElement(&key, &se); err != nil {
				return nil, err
			}
			continue
		case se.Name.Local == "array" && key == "ProgramArguments":
			var arr struct {
				Strings []string `xml:"string"`
			}
			if err := dec.DecodeElement(&arr, &se); err != nil {
				return nil, err
			}
			return arr.Strings, nil
		}
		key = ""
	}
}

// isMiseProgram reports whether a program path is mise.
func isMiseProgram(p string) bool { return filepath.Base(strings.TrimSpace(p)) == "mise" }

// doctorMiseOnPath is where `command -v mise` finds mise; "" when it does
// not.
func doctorMiseOnPath(ctx context.Context, d doctorDeps) string {
	if d.Run == nil {
		return ""
	}
	res, err := d.Run.Run(ctx, execx.Cmd{Name: "/bin/sh", Args: []string{"-c", "command -v mise"}, Timeout: 10 * time.Second, Label: "doctor"})
	if err != nil {
		return ""
	}
	if p := res.Out(); filepath.IsAbs(p) && !strings.ContainsAny(p, "\n") {
		return p
	}
	return ""
}

// doctorMiseUse says why this machine needs mise: a [[pool]], a
// LaunchAgent that starts the daemon through it, or mise on PATH; "" when
// nothing does.
func doctorMiseUse(ctx context.Context, d doctorDeps) string {
	if d.Config != nil && len(d.Config.Pools) > 0 {
		return "pool slots run their tools through mise"
	}
	if prog, found, err := doctorPlistProgram(d); found && err == nil && isMiseProgram(prog) {
		return "the LaunchAgent starts the daemon through mise"
	}
	if p := doctorMiseOnPath(ctx, d); p != "" {
		return "mise is on PATH (" + inspTilde(p) + ")"
	}
	return ""
}

// doctorLaunchdMise checks that the mise the LaunchAgent starts the daemon
// through exists: without it launchd cannot start magnum at all.
func doctorLaunchdMise(_ context.Context, d doctorDeps) []doctorCheck {
	const name = "launchd mise"
	path := doctorPlistPath(d)
	prog, found, err := doctorPlistProgram(d)
	switch {
	case !found:
		return []doctorCheck{doctorSkipped(name, "no LaunchAgent installed (magnum install writes one)")}
	case err != nil:
		return []doctorCheck{doctorWarned(name, "could not read the program of "+inspTilde(path)+": "+err.Error(),
			"magnum install (rewrites the LaunchAgent)")}
	case !isMiseProgram(prog):
		return []doctorCheck{doctorSkipped(name, "the LaunchAgent starts "+inspTilde(prog)+" without mise")}
	}
	fix := "install mise (https://mise.jdx.dev), then `magnum install` writes its path into the LaunchAgent"
	fi, err := os.Stat(prog)
	switch {
	case err != nil:
		return []doctorCheck{doctorFailed(name, "the LaunchAgent starts the daemon through "+inspTilde(prog)+", which does not exist: launchd cannot start magnum", fix)}
	case fi.IsDir() || fi.Mode().Perm()&0o111 == 0:
		return []doctorCheck{doctorFailed(name, "the LaunchAgent starts the daemon through "+inspTilde(prog)+", which is not executable", fix)}
	}
	return []doctorCheck{doctorOK(name, "the LaunchAgent starts the daemon through "+inspTilde(prog))}
}
