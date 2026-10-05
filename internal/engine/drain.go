package engine

// A drain (`magnum daemon-restart --drain`, `magnum install --drain`) holds
// every new round until the restart. It used to have no owner: a drainer
// killed before it lifted the drain (closing its terminal sends SIGHUP) held
// every round until the next restart. The kv value now names the drainer's
// pid, and the daemon lifts a drain whose drainer is gone (checkDrain).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// Drain is a drain in progress, the KVDaemonDraining value: when it started
// and the pid of the command that drains (0 = an older CLI did not say).
type Drain struct {
	Since time.Time `json:"since"`
	PID   int       `json:"pid,omitempty"`
}

// Value is d as KVDaemonDraining stores it.
func (d Drain) Value() string {
	b, _ := json.Marshal(struct {
		Since string `json:"since"`
		PID   int    `json:"pid,omitempty"`
	}{store.FormatTime(d.Since), d.PID})
	return string(b)
}

// ParseDrain reads a KVDaemonDraining value: Drain.Value, or the bare start
// time an older CLI wrote. false for "" or anything else.
func ParseDrain(v string) (Drain, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return Drain{}, false
	}
	if strings.HasPrefix(v, "{") {
		var raw struct {
			Since string `json:"since"`
			PID   int    `json:"pid"`
		}
		if json.Unmarshal([]byte(v), &raw) != nil {
			return Drain{}, false
		}
		t, err := store.ParseTime(raw.Since)
		if err != nil {
			return Drain{}, false
		}
		return Drain{Since: t, PID: raw.PID}, true
	}
	t, err := store.ParseTime(v)
	if err != nil {
		return Drain{}, false
	}
	return Drain{Since: t}, true
}

// DrainerAlive reports whether the drainer pid still runs as a magnum
// process. A pid that is gone, or that now belongs to another program (pids
// are reused), is not; a process ps cannot describe counts as alive (the
// drain stays: lifting it by mistake would start rounds the restart then
// abandons).
func DrainerAlive(pid int) bool {
	if pid <= 0 {
		return true // an owner nobody named: only the restart ends it
	}
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	comm, _, err := processCommand(pid)
	if err != nil {
		return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}
	return filepath.Base(strings.TrimSpace(comm)) == "magnum"
}

// checkDrain lifts a drain whose drainer is gone (each tick, with the other
// pauses): the drain stays only while the command that holds it runs. A drain
// an older CLI started names no pid and lasts until the restart.
func (e *Engine) checkDrain(ctx context.Context) {
	v, ok := e.getKV(ctx, KVDaemonDraining)
	if !ok || v == "" {
		return
	}
	d, ok := ParseDrain(v)
	if !ok || d.PID <= 0 || DrainerAlive(d.PID) {
		return
	}
	if cur, _ := e.getKV(ctx, KVDaemonDraining); cur != v {
		return // another drain started meanwhile
	}
	e.delKV(ctx, KVDaemonDraining)
	e.event(ctx, "warn", "", "daemon.drain_lifted",
		fmt.Sprintf("drain lifted: the command that started it at %s (pid %d) is gone, so no restart follows; rounds start again",
			d.Since.Local().Format("15:04:05"), d.PID), map[string]any{"pid": d.PID})
}
