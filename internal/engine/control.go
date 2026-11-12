package engine

// The records of `magnum drain` and of the controls a daemon knows, which
// both daemons share (docs/superpowers/specs/2026-11-12-control-commands-
// design.md). A drain for a reboot (RebootDrain) holds new work on a daemon
// until the first start of that daemon on another boot of the Mac (until
// "boot"), or until `magnum resume` (until "resume", `magnum drain
// --keep`). It names no command: Ctrl-C or a closed terminal leaves it in
// force. The review daemon keeps it in the kv row KVDaemonRebootDrain, the
// factory daemon in reboot-drain.json beside its drain.json. Each daemon
// acknowledges it (DrainAck) once its gates hold, says what still runs, and
// whether it is quiet and parked its idle agents; the command says "safe to
// reboot" only from both acknowledgements of its generation. A restart's
// drain (KVDaemonDraining, drain.json) is another record: neither lifts the
// other.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// The kv rows of the controls on the review daemon's registry.
const (
	// KVDaemonRebootDrain is the drain for a reboot (RebootDrain.Value),
	// which `magnum drain` writes and `magnum resume` deletes; a daemon's
	// start on another boot ends a drain until "boot".
	KVDaemonRebootDrain = "daemon.reboot_drain"
	// KVDaemonDrainAck is the review daemon's acknowledgement of it
	// (DrainAck.Value), rewritten while the drain holds.
	KVDaemonDrainAck = "daemon.reboot_drain_ack"
	// KVDaemonControls is the controls the running daemon knows
	// (Controls.Value), written at its start: the CLI refuses a drain a
	// daemon would not hold.
	KVDaemonControls = "daemon.controls"
)

// ControlsVersion is the version of the controls a daemon of this build
// knows: the drain for a reboot and its acknowledgement, and the factory's
// pause.json.
const ControlsVersion = 1

// The ends of a RebootDrain (RebootDrain.Until).
const (
	// DrainUntilBoot ends the drain at the first start of a daemon on
	// another boot of the Mac.
	DrainUntilBoot = "boot"
	// DrainUntilResume ends it at `magnum resume` alone (`magnum drain
	// --keep`).
	DrainUntilResume = "resume"
)

// DrainQuietToastAfter is how long a daemon stays quiet under a drain for a
// reboot before it says once that the drain still holds: the Mac did not
// reboot.
const DrainQuietToastAfter = time.Hour

// RebootDrain is a drain for a reboot: the command's generation (its start
// in nanoseconds, the same on both daemons' records), since when, until
// what ends it (DrainUntilBoot, DrainUntilResume), the boot of the Mac it
// began on (unknown when it could not be read: only `magnum resume` ends
// such a drain) and why.
type RebootDrain struct {
	Generation int64     `json:"generation"`
	Since      time.Time `json:"since"`
	Until      string    `json:"until"`
	Boot       BootID    `json:"boot,omitzero"`
	Reason     string    `json:"reason,omitempty"`
}

// BootID names a boot of the Mac: its boot session (kern.bootsessionuuid,
// unique per boot) and its boot time (kern.boottime), either empty when it
// could not be read.
type BootID struct {
	Session string    `json:"session,omitempty"`
	Time    time.Time `json:"time,omitzero"`
}

// bootTimeSlack is how far kern.boottime may move on the same boot without
// naming another one: macOS moves it when it steps the clock after a wake.
// It decides only when a boot session is missing.
const bootTimeSlack = 2 * time.Minute

// Known reports whether b names a boot.
func (b BootID) Known() bool { return b.Session != "" || !b.Time.IsZero() }

// Other reports whether b is another boot than o: their sessions differ when
// both have one, else their boot times are more than bootTimeSlack apart.
// An unknown boot is no other boot.
func (b BootID) Other(o BootID) bool {
	switch {
	case b.Session != "" && o.Session != "":
		return b.Session != o.Session
	case !b.Time.IsZero() && !o.Time.IsZero():
		d := b.Time.Sub(o.Time)
		return d > bootTimeSlack || d < -bootTimeSlack
	}
	return false
}

// Value is d as its records hold it.
func (d RebootDrain) Value() string {
	b, _ := json.Marshal(d)
	return string(b)
}

// ParseRebootDrain reads a record's value (Value); false for anything else,
// "" included.
func ParseRebootDrain(v string) (RebootDrain, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return RebootDrain{}, false
	}
	var d RebootDrain
	if json.Unmarshal([]byte(v), &d) != nil || d.Generation == 0 || d.Since.IsZero() {
		return RebootDrain{}, false
	}
	if d.Until != DrainUntilResume {
		d.Until = DrainUntilBoot
	}
	return d, true
}

// EndsAtStart reports whether a daemon that starts on boot (the Mac's boot
// now) ends d: a drain until "boot" begun on another boot (BootID.Other). A
// crash restart or a restart on the same boot keeps it, and so does any
// start when either boot is unknown.
func (d RebootDrain) EndsAtStart(boot BootID) bool {
	return d.Until == DrainUntilBoot && boot.Other(d.Boot)
}

// Words say what ends d, for the status lines and the events.
func (d RebootDrain) Words() string {
	if d.Until == DrainUntilResume {
		return "until `magnum resume`"
	}
	return "until the first start after a reboot, or `magnum resume`"
}

// DrainAck is a daemon's acknowledgement of a drain for a reboot: the
// generation its gates hold, its pid (a restarted daemon's acknowledgement
// is void), whether nothing it would cut runs (Quiet) and since when, the
// work that still runs (in words, "round of talkable/talkable#12"), and
// whether it parked its idle agents once quiet (Parked).
type DrainAck struct {
	Generation int64     `json:"generation"`
	PID        int       `json:"pid"`
	Quiet      bool      `json:"quiet"`
	QuietSince time.Time `json:"quiet_since,omitzero"`
	Parked     bool      `json:"parked"`
	Work       []string  `json:"work,omitempty"`
}

// Value is a as KVDaemonDrainAck holds it.
func (a DrainAck) Value() string {
	b, _ := json.Marshal(a)
	return string(b)
}

// ParseDrainAck reads an acknowledgement (Value); false for anything else.
func ParseDrainAck(v string) (DrainAck, bool) {
	var a DrainAck
	if strings.TrimSpace(v) == "" || json.Unmarshal([]byte(v), &a) != nil || a.Generation == 0 {
		return DrainAck{}, false
	}
	return a, true
}

// Done reports whether a is the acknowledgement of generation gen by the
// daemon pid, quiet and parked: that daemon is safe to reboot.
func (a DrainAck) Done(gen int64, pid int) bool {
	return a.Generation == gen && a.PID == pid && a.Quiet && a.Parked
}

// Controls is what KVDaemonControls holds: the ControlsVersion the daemon
// pid knows.
type Controls struct {
	Version int `json:"version"`
	PID     int `json:"pid"`
}

// Value is c as KVDaemonControls holds it.
func (c Controls) Value() string {
	b, _ := json.Marshal(c)
	return string(b)
}

// KnowsControls reports whether the KVDaemonControls value v says that the
// daemon pid knows the controls of version want.
func KnowsControls(v string, pid, want int) bool {
	var c Controls
	if json.Unmarshal([]byte(v), &c) != nil {
		return false
	}
	return c.PID == pid && c.Version >= want
}

// The kernel's boot session (unique per boot) and boot time.
const (
	bootSessionOID = "kern.bootsessionuuid"
	bootTimeOID    = "kern.boottime"
)

// bootSessionRe is a boot session's id.
var bootSessionRe = regexp.MustCompile(`^[0-9A-Fa-f-]{16,64}$`)

// ReadBoot reads the Mac's boot through r, read only: `sysctl -n
// kern.bootsessionuuid` and `sysctl -n kern.boottime` ("{ sec = 1762940000,
// usec = 123456 } Wed Nov 12 10:13:20 2026"). Either may fail alone; an
// error when neither names the boot (another system, sysctl missing).
func ReadBoot(ctx context.Context, r execx.Runner) (BootID, error) {
	if r == nil {
		return BootID{}, errors.New("no runner reads the boot")
	}
	read := func(oid string) (string, error) {
		res, err := r.Run(ctx, execx.Cmd{Name: sysctlPath, Args: []string{"-n", oid}, Timeout: memoryTimeout, Probe: true, Quiet: true,
			Label: "sysctl " + oid})
		if err != nil {
			return "", fmt.Errorf("sysctl %s: %w", oid, err)
		}
		return strings.TrimSpace(string(res.Stdout)), nil
	}
	var b BootID
	var errs []error
	if s, err := read(bootSessionOID); err != nil {
		errs = append(errs, err)
	} else if bootSessionRe.MatchString(s) {
		b.Session = s
	} else {
		errs = append(errs, fmt.Errorf("sysctl %s printed %q, not a boot session", bootSessionOID, s))
	}
	if out, err := read(bootTimeOID); err != nil {
		errs = append(errs, err)
	} else if t, err := ParseBootTime(out); err != nil {
		errs = append(errs, err)
	} else {
		b.Time = t
	}
	if !b.Known() {
		return b, errors.Join(errs...)
	}
	return b, nil
}

// bootRe matches kern.boottime's seconds and microseconds.
var bootRe = regexp.MustCompile(`sec\s*=\s*([0-9]+)\s*,\s*usec\s*=\s*([0-9]+)`)

// ParseBootTime reads `sysctl -n kern.boottime`'s output.
func ParseBootTime(out string) (time.Time, error) {
	m := bootRe.FindStringSubmatch(out)
	if m == nil {
		return time.Time{}, fmt.Errorf("sysctl %s printed %q, not a boot time", bootTimeOID, strings.TrimSpace(out))
	}
	sec, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	usec, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, usec*int64(time.Microsecond)).UTC(), nil
}
