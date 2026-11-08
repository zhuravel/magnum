package agents

// When `magnum hooks install` rewrites one of magnum's own Codex hooks in
// its place (its command, timeout or async drifted), the hook keeps its
// place, so Codex's [hooks.state] entry there keeps its trusted_hash: the
// hash of the hook before the rewrite, which Codex no longer accepts and
// for which it shows its hooks review. A trust entry alone would read as
// trusted, so the install records that hash (RecordStaleCodexTrust) in
// magnum's state directory, and ReadCodexHooksTrust counts the place
// untrusted while its entry keeps it. Once the entry's hash differs (the
// operator trusted the new hook) or the entry or the hook is gone, the
// record is removed. When the rewrite restored the very hook Codex already
// trusts, Codex asks nothing and writes no new hash: a Codex session this
// Manager launched whose composer settles with no hooks review
// (codexPromptReady), and in which magnum answered no review since its
// launch, proves that Codex read the home's hooks at that launch and trusts
// them, so the home's records written before the launch go
// (forgetStaleTrustOf); `magnum hooks install codex
// --forget-stale` removes them all on the operator's word
// (ForgetStaleCodexTrust). Codex's own files are never written.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
)

// staleTrustDir is the directory of the records (SetStaleCodexTrustDir);
// nil or "" keeps none.
var staleTrustDir atomic.Pointer[string]

// SetStaleCodexTrustDir sets the directory of magnum's records of Codex
// trust entries that a rewrite of its own hooks left stale (under magnum's
// state directory). The CLI sets it once at its start, for every command
// and the daemons it runs; "" (the default, and a test binary's) keeps none,
// and ReadCodexHooksTrust then reads Codex's files alone.
func SetStaleCodexTrustDir(dir string) { staleTrustDir.Store(&dir) }

func staleTrustDirectory() string {
	if p := staleTrustDir.Load(); p != nil {
		return *p
	}
	return ""
}

// staleTrust is one record: the trusted_hash the [hooks.state] entry of the
// hook at Place of the hooks.json Hooks had when magnum rewrote that hook.
type staleTrust struct {
	Hooks       string `json:"hooks"`
	Place       string `json:"place"`
	TrustedHash string `json:"trusted_hash"`
}

// CodexForgetStaleStep is the way out of a record Codex will never replace
// (it trusts the rewritten hook as it is, so it asks nothing): status,
// doctor and the hold's texts name it.
const CodexForgetStaleStep = "if `codex` asks nothing, run `magnum hooks install codex --forget-stale`"

// staleTrustBeforeRemove runs between a reader's decision to remove a
// record and the removal (nil: nothing); a test runs a second rewrite
// there.
var staleTrustBeforeRemove func()

// staleTrustFile is the record of the hook at place of the hooks.json
// hooksFile with the trusted_hash hash, in dir: one file per hook and hash,
// written and removed whole, so a reader that removes a record it read
// never removes a newer one of the same place.
func staleTrustFile(dir, hooksFile, place, hash string) string {
	sum := sha256.Sum256([]byte(hooksFile + "\n" + place + "\n" + hash))
	return filepath.Join(dir, hex.EncodeToString(sum[:12])+".json")
}

// RecordStaleCodexTrust records, for each of places (as Codex keys its
// trust: "stop:1:0") of the Codex home home's hooks.json, the trusted_hash
// its [hooks.state] entry has now: magnum is about to rewrite the hook
// there in its place, and ReadCodexHooksTrust counts the place untrusted
// while its entry keeps that hash. A place without a trust entry needs no
// record (it reads untrusted anyway). Without a records directory
// (SetStaleCodexTrustDir) it records nothing and says so.
func RecordStaleCodexTrust(home string, places []string) error {
	dir := staleTrustDirectory()
	if dir == "" {
		return errors.New("agents: no directory for magnum's stale Codex trust records")
	}
	f, ok, err := readCodexHooksJSON(home)
	if err != nil || !ok {
		return err
	}
	state, err := readCodexHookState(f.configFile)
	if err != nil {
		return err
	}
	for _, place := range places {
		_, e, ok := f.entry(state, place)
		if !ok || e.TrustedHash == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("agents: %w", err)
		}
		b, err := json.Marshal(staleTrust{Hooks: f.hooksFile, Place: place, TrustedHash: e.TrustedHash})
		if err != nil {
			return fmt.Errorf("agents: %w", err)
		}
		if err := fsx.WriteFileAtomic(staleTrustFile(dir, f.hooksFile, place, e.TrustedHash), b, 0o600); err != nil {
			return fmt.Errorf("agents: record the stale trust of %s: %w", place, err)
		}
	}
	return nil
}

// staleRecord is a record as read: its file, the hash it holds and the
// file's modification time when it was read.
type staleRecord struct {
	file, hash string
	mod        time.Time
}

// readStaleTrust reads the records of the hooks.json hooksFile in dir, by
// place (a place rewritten again may have more than one); none without a
// directory. A record that cannot be read or decoded is skipped.
func readStaleTrust(dir, hooksFile string) map[string][]staleRecord {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := map[string][]staleRecord{}
	for _, de := range entries {
		if !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		file := filepath.Join(dir, de.Name())
		fi, err := os.Lstat(file)
		if err != nil {
			continue
		}
		b, err := fsx.ReadRegular(file, 4<<10)
		if err != nil {
			continue
		}
		var r staleTrust
		if json.Unmarshal(b, &r) != nil || r.Hooks != hooksFile || r.TrustedHash == "" {
			continue
		}
		out[r.Place] = append(out[r.Place], staleRecord{file: file, hash: r.TrustedHash, mod: fi.ModTime()})
	}
	return out
}

// removeStale removes the record r, unless its file changed since it was
// read (a newer write of the same record).
func removeStale(r staleRecord) {
	if staleTrustBeforeRemove != nil {
		staleTrustBeforeRemove()
	}
	if fi, err := os.Lstat(r.file); err != nil || !fi.ModTime().Equal(r.mod) {
		return
	}
	_ = os.Remove(r.file)
}

// ForgetStaleCodexTrust removes every record of the Codex home home's
// hooks.json and says how many it removed: a place magnum recorded that
// Codex will never trust anew (it trusts the hook as it is) then reads
// trusted again. Without a records directory there is none.
func ForgetStaleCodexTrust(home string) (int, error) { return forgetStaleBefore(home, time.Time{}) }

// forgetStaleBefore is ForgetStaleCodexTrust for the records written before
// before (all with a zero before).
func forgetStaleBefore(home string, before time.Time) (int, error) {
	n := 0
	for _, rs := range readStaleTrust(staleTrustDirectory(), filepath.Join(home, "hooks.json")) {
		for _, r := range rs {
			if !before.IsZero() && !r.mod.Before(before) {
				continue // written after the launch: that Codex read the hooks before it
			}
			if err := os.Remove(r.file); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return n, fmt.Errorf("agents: forget a stale Codex trust record: %w", err)
			}
			n++
		}
	}
	return n, nil
}

// StaleCodexTrustCount is how many records the Codex home home's hooks.json
// has (ForgetStaleCodexTrust's count, removing nothing).
func StaleCodexTrustCount(home string) int {
	n := 0
	for _, rs := range readStaleTrust(staleTrustDirectory(), filepath.Join(home, "hooks.json")) {
		n += len(rs)
	}
	return n
}

// forgetStaleTrustOf removes the stale trust records of the Codex home
// session sess (the agent at ref) reads that were written before its
// launch: its first prompt's screen settled with no hooks review and magnum
// answered none since the launch, so the Codex it launched read the home's
// hooks then and trusts them as they are. Only this Manager knows when it
// launched a session and which reviews it answered since (noteLaunch,
// markHooksAnswered), so a session it did not launch (one a daemon before a
// restart launched, an agent it adopted, another process's) clears
// nothing; `magnum hooks install codex --forget-stale` is the way out then.
// A record written after the launch stays (that Codex read the hooks before
// the rewrite). The home is the one the row's env names; a row whose env
// names none (StartAgent's fallback row) takes the one Codex home the
// configured roles read, and none when they read more than one, so the
// default home never stands in for another.
func (m *Manager) forgetStaleTrustOf(sess store.Session, ref paneRef) {
	launched, ok := m.launchOf(sess.ID)
	if !ok || m.hooksAnsweredSince(ref, launched) {
		return
	}
	home := m.staleTrustHome(sess)
	if home == "" {
		return
	}
	if n, err := forgetStaleBefore(home, launched); err != nil {
		m.logf("agents: %v", err)
	} else if n > 0 {
		m.logf("agents: %s started with no hooks review: forgot %d stale Codex trust record(s) of its home", sess.Role, n)
	}
}

// staleTrustHome is the Codex home of sess for the stale trust records:
// its env's CODEX_HOME, else the one home the configured Codex roles read
// ("" for none or more than one).
func (m *Manager) staleTrustHome(sess store.Session) string {
	if sess.Env["CODEX_HOME"] != "" {
		home, _ := m.codexHomeFor(sess.Env)
		return home
	}
	if m.d.Config == nil {
		return ""
	}
	var homes []string
	for _, r := range slices.Concat(m.d.Config.AllRoles(), m.d.Config.Factory.Roles) {
		if !r.IsAgent() || r.Kind != KindCodex {
			continue
		}
		if h := m.CodexHome(r); !slices.Contains(homes, h) {
			homes = append(homes, h)
		}
	}
	if len(homes) != 1 {
		return ""
	}
	return homes[0]
}

// noteLaunch records that this Manager launched session id's agent at at
// (StartAgent, through markStarted): the session's Codex read its home's
// hooks.json then.
func (m *Manager) noteLaunch(id int64, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.launches[id] = at
}

// launchOf is when this Manager launched session id's agent; false for a
// session it did not launch.
func (m *Manager) launchOf(id int64) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.launches[id]
	return at, ok
}

// markHooksAnswered records that magnum answered a Codex hooks review of
// the agent at ref now.
func (m *Manager) markHooksAnswered(ref paneRef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hooksAnswered[ref.String()] = m.now()
}

// hooksAnsweredSince reports whether magnum answered a Codex hooks review of
// the agent at ref at or after since.
func (m *Manager) hooksAnsweredSince(ref paneRef, since time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.hooksAnswered[ref.String()]
	return ok && !at.Before(since)
}
