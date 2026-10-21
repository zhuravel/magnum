package engine

// The idle restart on a saved config. The daemon reads the configuration
// once, at startup, so a config the settings screen saves reaches rounds
// only after a restart. The save marks it (KVConfigRestart, written by
// settings.RequestRestart), and the daemon exits at the first tick with
// nothing in flight, for launchd to start it on the new file, as it does for
// a new build or a landed skill version (restartForBuild). The file on disk
// decides, not the mark: the daemon records the SHA-256 of the user config
// it loaded (KVDaemonConfigSum), a mark whose file is the one the daemon
// runs (consumed by this start, or reverted by hand) is cleared, and one
// whose file differs (a save made after the daemon command loaded the
// config, before the engine started) waits for the idle restart. Before any
// idle restart, whatever its reason, the daemon checks what launchd would
// start (restartSafe): a binary rebuilt since the start with its own
// check (`<bin> config`), else a user config that changed since the start
// with this build, prompt check included. A failure holds the restart, says
// so once (daemon.config_restart_refused, KVConfigRestartRefused for
// `magnum status`) and drops the config's mark: exiting would leave a
// daemon launchd cannot start.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// KVConfigRestart is a saved config the daemon is to restart on
	// (ConfigRestartMark as JSON): written by the settings screen's save
	// (settings.RequestRestart), read by restartForBuild and `magnum
	// status`, and cleared once the daemon runs the file on disk.
	KVConfigRestart = "config.restart"
	// KVDaemonConfigSum is the hex SHA-256 of the user config the running
	// daemon loaded (config.Config.UserSum), written at startup; "" when it
	// read no user config.
	KVDaemonConfigSum = "daemon.config_sha256"
	// KVConfigRestartRefused is the idle restart this daemon refused because
	// what it would start on fails its check (ConfigRestartRefusal as JSON):
	// written by restartForBuild, read by `magnum status`, deleted by the
	// next start.
	KVConfigRestartRefused = "config.restart_refused"
)

// ErrRestartForConfig is what Tick and Run return when the daemon exits to
// restart on a saved config (KVConfigRestart): the exit is non-zero, so
// launchd starts it again, and the new daemon reads the saved file.
var ErrRestartForConfig = errors.New("a config was saved and nothing is in flight; exiting for launchd to start the daemon on it")

// restartRefuseRetry is how long a refused idle restart waits before its
// check runs again on the same file and binary: the binary's check renders
// the prompts too, which can be fixed while both stay as they were.
const restartRefuseRetry = 10 * time.Minute

// errRefusedBefore is restartSafe's answer for a file and binary it refused
// already: no new check, no new event.
var errRefusedBefore = errors.New("refused before")

// ConfigRestartMark is a saved config the daemon waits to restart on
// (KVConfigRestart): the hex SHA-256 of the file the save wrote and when it
// was saved.
type ConfigRestartMark struct {
	SHA256  string    `json:"sha256"`
	SavedAt time.Time `json:"saved_at"`
}

// ConfigRestartRefusal is an idle restart the daemon refused
// (KVConfigRestartRefused): the hex SHA-256 of the user config on disk it
// checked ("" for none), when, and the modification time of the binary on
// disk when that binary was the one that refused it.
type ConfigRestartRefusal struct {
	SHA256      string    `json:"sha256"`
	At          time.Time `json:"at"`
	BinaryMTime time.Time `json:"binary_mtime,omitzero"`
}

// ReadConfigRestart reads KVConfigRestart (false when none is set or it
// cannot be read).
func ReadConfigRestart(ctx context.Context, st *store.Store) (ConfigRestartMark, bool) {
	v, ok, err := st.GetKV(ctx, KVConfigRestart)
	if err != nil || !ok {
		return ConfigRestartMark{}, false
	}
	m, ok := parseConfigRestart(v)
	return m, ok
}

// ReadDaemonConfigSum reads KVDaemonConfigSum: the hex SHA-256 of the user
// config the running daemon loaded, "" when it read none; false when no
// daemon recorded one (one older than the record, or none ever ran).
func ReadDaemonConfigSum(ctx context.Context, st *store.Store) (string, bool) {
	v, ok, err := st.GetKV(ctx, KVDaemonConfigSum)
	if err != nil || !ok {
		return "", false
	}
	return v, true
}

// ReadConfigRestartRefused reads KVConfigRestartRefused (false when none is
// set or it cannot be read).
func ReadConfigRestartRefused(ctx context.Context, st *store.Store) (ConfigRestartRefusal, bool) {
	v, ok, err := st.GetKV(ctx, KVConfigRestartRefused)
	if err != nil || !ok || v == "" {
		return ConfigRestartRefusal{}, false
	}
	var r ConfigRestartRefusal
	if json.Unmarshal([]byte(v), &r) != nil || r.At.IsZero() {
		return ConfigRestartRefusal{}, false
	}
	return r, true
}

func parseConfigRestart(v string) (ConfigRestartMark, bool) {
	var m ConfigRestartMark
	if v == "" || json.Unmarshal([]byte(v), &m) != nil || m.SHA256 == "" {
		return ConfigRestartMark{}, false
	}
	return m, true
}

// configSum is the hex SHA-256 of the user config this daemon loaded, ""
// when it read none.
func (e *Engine) configSum() string {
	sum, ok := e.cfg.UserSum()
	if !ok {
		return ""
	}
	return hex.EncodeToString(sum[:])
}

// diskConfig is the user config on disk now and its hex SHA-256: no data
// and "" when there is no file (or no user config path), as a daemon that
// read none recorded; false when it cannot be read.
func (e *Engine) diskConfig() (data []byte, sum string, ok bool) {
	path := e.cfg.Layout.UserConfig
	if path == "" {
		return nil, "", true
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, "", true
	case err != nil:
		return nil, "", false
	}
	s := sha256.Sum256(data)
	return data, hex.EncodeToString(s[:]), true
}

// deleteMark deletes the config mark whose raw value is v, unless a newer
// save replaced it.
func (e *Engine) deleteMark(ctx context.Context, v string) {
	if _, err := e.st.DeleteKVIf(ctx, KVConfigRestart, v); err != nil {
		e.warnUnlessStopped(ctx, err, "kv delete", "key", KVConfigRestart)
	}
}

// recordConfig writes the user config's SHA-256 at startup, deletes the
// previous daemon's refusal (this start checks afresh) and settles a saved
// config's mark by the file on disk: one whose file is the one this start
// loaded is done (the event says whether that is the saved bytes or a file
// changed again after the save) and is deleted; one whose file differs is
// a save made after the daemon command loaded the config and stays for the
// idle restart. A dry run settles nothing.
func (e *Engine) recordConfig(ctx context.Context) {
	sum := e.configSum()
	e.setKV(ctx, KVDaemonConfigSum, sum)
	if e.d.DryRun {
		return
	}
	e.delKV(ctx, KVConfigRestartRefused)
	v, ok := e.getKV(ctx, KVConfigRestart)
	if !ok {
		return
	}
	m, valid := parseConfigRestart(v)
	if _, disk, read := e.diskConfig(); valid && (!read || disk != sum) {
		return // not what this start runs: the idle restart takes it
	}
	e.deleteMark(ctx, v)
	if !valid {
		return
	}
	msg := fmt.Sprintf("the daemon runs the config saved at %s (%s) since this start", m.SavedAt.Local().Format("15:04:05"), shortSum(m.SHA256))
	if m.SHA256 != sum {
		msg = fmt.Sprintf("the config file changed again after the save at %s (%s): the daemon runs the file as it was at this start (%s)",
			m.SavedAt.Local().Format("15:04:05"), shortSum(m.SHA256), shortSum(sum))
	}
	e.event(ctx, "info", "", "daemon.restarted_for_config", msg, map[string]any{"saved": shortSum(m.SHA256), "loaded": shortSum(sum)})
}

// configRestartDue is the saved config the daemon exits to restart on and
// the mark's raw value: a mark, neither a dry run nor --once, a user config
// on disk that is not the one this daemon loaded, and launchd to start the
// daemon again. A mark whose file is the one the daemon runs (a save
// reverted by hand) is cleared: nothing to restart on. false when none is
// due.
func (e *Engine) configRestartDue(ctx context.Context) (ConfigRestartMark, string, bool) {
	if e.d.DryRun || e.once {
		return ConfigRestartMark{}, "", false
	}
	v, ok := e.getKV(ctx, KVConfigRestart)
	if !ok {
		return ConfigRestartMark{}, "", false
	}
	m, ok := parseConfigRestart(v)
	if !ok {
		return ConfigRestartMark{}, "", false
	}
	_, disk, read := e.diskConfig()
	if !read {
		return ConfigRestartMark{}, "", false
	}
	if disk == e.configSum() {
		e.deleteMark(ctx, v)
		return ConfigRestartMark{}, "", false
	}
	if !e.supervised {
		return ConfigRestartMark{}, "", false // the operator restarts it (`magnum status` says how)
	}
	return m, v, true
}

// restartCheck is what restartSafe looked at: the user config on disk (its
// hash), the binary on disk's modification time when it changed since the
// start, and the key a refusal is remembered by.
type restartCheck struct {
	key    string
	disk   string
	binary time.Time
}

// restartRefusal is the idle restart last refused: restartSafe's key (the
// file on disk and the binary) and when its check ran.
type restartRefusal struct {
	key     string
	checked time.Time
}

// refusedRecently reports whether key was refused less than
// restartRefuseRetry ago: its check waits.
func (e *Engine) refusedRecently(key string) bool {
	r := e.restartRefused
	return r.key == key && e.now().Before(r.checked.Add(restartRefuseRetry))
}

// restartSafe checks what an idle restart would start on before the daemon
// exits: a launch binary that is missing or cannot be read is refused, one
// rebuilt since the start runs its own check (checkBuild: its version and
// `<bin> config` on the file on disk), else a user config that is not the
// one loaded (or a saved config's mark) is loaded with this build
// (configStillLoads). Nothing changed is safe. A file and binary refused
// less than restartRefuseRetry ago are refused again without a check
// (errRefusedBefore); after it the check runs again, since the binary's
// check renders the prompts too, which can be fixed while both stay.
func (e *Engine) restartSafe(ctx context.Context, cfgDue bool) (restartCheck, error) {
	data, disk, read := e.diskConfig()
	if !read {
		chk := restartCheck{key: "unreadable"}
		if e.refusedRecently(chk.key) {
			return chk, errRefusedBefore
		}
		return chk, fmt.Errorf("the user config %s cannot be read", e.cfg.Layout.UserConfig)
	}
	chk := restartCheck{disk: disk}
	missing := false
	if b := e.build; b.Path != "" && !b.ModTime.IsZero() {
		switch mt, ok := FileModTime(b.Path); {
		case !ok:
			missing = true
		case !mt.Equal(b.ModTime):
			chk.binary = mt
		}
	}
	changed := cfgDue || disk != e.configSum()
	if !missing && chk.binary.IsZero() && !changed {
		return chk, nil
	}
	chk.key = disk + "|" + chk.binary.UTC().Format(time.RFC3339Nano)
	if missing {
		chk.key = disk + "|missing"
	}
	if e.refusedRecently(chk.key) {
		return chk, errRefusedBefore
	}
	if missing {
		return chk, fmt.Errorf("the binary launchd starts (%s) is missing or cannot be read", e.build.Path)
	}
	if !chk.binary.IsZero() {
		if e.checkBuild == nil {
			return chk, fmt.Errorf("a new build is on disk (%s, built %s) and this daemon cannot check it", e.build.Path,
				chk.binary.Local().Format("15:04:05"))
		}
		if _, err := e.checkBuild(ctx, e.build.Path); err != nil {
			return chk, fmt.Errorf("the build on disk (%s, built %s) refuses the config: %w", e.build.Path,
				chk.binary.Local().Format("15:04:05"), err)
		}
		return chk, nil
	}
	if err := e.configStillLoads(data); err != nil {
		return chk, fmt.Errorf("the config on disk no longer loads with this build: %w", err)
	}
	return chk, nil
}

// configStillLoads loads data, the user config on disk, as the next daemon
// will with this build: its base, data as the user layer (nil: no file, an
// empty layer), then the prompt check (CheckPrompts), what `magnum config`
// runs.
func (e *Engine) configStillLoads(data []byte) error {
	opts := config.LoadOptions{}
	if e.cfg.Layout.UserConfig != "" {
		opts.UserData = data
		if opts.UserData == nil {
			opts.UserData = []byte{}
		}
	}
	cfg, err := config.LoadWithOptions(e.cfg.Layout, e.cfg.BaseFile(), opts)
	if err != nil {
		return err
	}
	if _, err := CheckPrompts(cfg); err != nil {
		return fmt.Errorf("prompts do not render with this build:\n%w", err)
	}
	return nil
}

// restartReasons names the idle restarts that wait, for the refusal.
func restartReasons(build, learn, cfg bool) string {
	var out []string
	if build {
		out = append(out, "a new build")
	}
	if learn {
		out = append(out, "a landed skill version")
	}
	if cfg {
		out = append(out, "a saved config")
	}
	return strings.Join(out, ", ")
}

// refuseRestart holds an idle restart whose check failed: the config's mark
// (its raw value v, "" for none) deleted unless a newer save replaced it,
// and, when announce (the first refusal of this file and binary, not a
// retry after restartRefuseRetry), a warning in the log, an event naming
// why and what waits, and the refusal recorded for `magnum status`
// (KVConfigRestartRefused). The event says how the restart comes: goesOn
// (a new build or a landed skill version waits too, and stays due) once
// the file loads; else, the saved config's mark being dropped, nothing
// restarts on a file fixed by hand, so it names `magnum daemon-restart
// --when-idle` and a new save.
func (e *Engine) refuseRestart(ctx context.Context, chk restartCheck, waiting string, goesOn bool, v string, err error, announce bool) {
	if v != "" {
		e.deleteMark(ctx, v)
	}
	if !announce {
		e.log.Debug("the idle restart is still held", "waiting", waiting, "sha256", shortSum(chk.disk), "err", err)
		return
	}
	e.log.Warn("the idle restart is held: what it would start on fails its check; the daemon keeps running on the config and build it started with",
		"waiting", waiting, "sha256", shortSum(chk.disk), "err", err)
	then := "and the restart goes on once it loads"
	if !goesOn {
		then = "then `magnum daemon-restart --when-idle` restarts the daemon on it, or save it again from the settings screen"
	}
	e.event(ctx, "warn", "", "daemon.config_restart_refused",
		fmt.Sprintf("the daemon does not restart for %s: %v. It keeps running on the config and build it started with; fix the file "+
			"(`magnum config` shows why), %s", waiting, err, then),
		map[string]any{"waiting": waiting, "config": shortSum(chk.disk)})
	if raw, jerr := json.Marshal(ConfigRestartRefusal{SHA256: chk.disk, At: e.now(), BinaryMTime: chk.binary}); jerr == nil {
		e.setKV(ctx, KVConfigRestartRefused, string(raw))
	}
}

// shortSum is a SHA-256's first 7 hex digits for messages (textx.ShortSHA);
// "none" for "".
func shortSum(sum string) string {
	if sum == "" {
		return "none"
	}
	return textx.ShortSHA(sum)
}
