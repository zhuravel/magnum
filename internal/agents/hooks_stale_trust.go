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
// its [hooks.state] entry has now: magnum rewrites its hook there in its
// place, or puts its hook at a place whose entry a hook that once stood
// there left (Codex never removes an entry), and ReadCodexHooksTrust counts
// the place untrusted while its entry keeps that hash. A place without a
// trust entry, or with one without a hash, needs no record (it reads
// untrusted anyway). hooks.json need not exist yet. Without a records
// directory (SetStaleCodexTrustDir) it records nothing and says so.
func RecordStaleCodexTrust(home string, places []string) error {
	_, err := recordStale(home, places, false, false)
	return err
}

// RecordOldCodexTrust records, as RecordStaleCodexTrust does, the trust
// entries at places (magnum's entries in the Codex home home's hooks.json)
// that no record covers yet, and says how many it recorded (with dry, how
// many it would). It is the operator's word (`magnum hooks install codex
// --assume-untrusted`) for entries an install made before magnum recorded
// the trust an append landed on: no file tells those old entries from the
// operator's Trust all.
func RecordOldCodexTrust(home string, places []string, dry bool) (int, error) {
	return recordStale(home, places, true, dry)
}

// recordStale writes a record for each of places whose trust entry has a
// hash, and says how many (dry: writes none); with fresh, only for places
// whose hash no record holds yet.
func recordStale(home string, places []string, fresh, dry bool) (int, error) {
	dir := staleTrustDirectory()
	if dir == "" {
		return 0, errors.New("agents: no directory for magnum's stale Codex trust records")
	}
	f := codexHookPaths(home)
	state, err := readCodexHookState(f.configFile)
	if err != nil {
		return 0, err
	}
	recorded := readStaleTrust(dir, f.hooksFile)
	n := 0
	for _, place := range places {
		_, e, ok := f.entry(state, place)
		if !ok || e.TrustedHash == "" {
			continue
		}
		if fresh && slices.ContainsFunc(recorded[place], func(r staleRecord) bool { return r.hash == e.TrustedHash }) {
			continue
		}
		n++
		if dry {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return n, fmt.Errorf("agents: %w", err)
		}
		b, err := json.Marshal(staleTrust{Hooks: f.hooksFile, Place: place, TrustedHash: e.TrustedHash})
		if err != nil {
			return n, fmt.Errorf("agents: %w", err)
		}
		if err := fsx.WriteFileAtomic(staleTrustFile(dir, f.hooksFile, place, e.TrustedHash), b, 0o600); err != nil {
			return n, fmt.Errorf("agents: record the stale trust of %s: %w", place, err)
		}
	}
	return n, nil
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
// read (a newer write of the same record) or keep says, at the last moment,
// that the reason to remove it is gone (nil: none).
func removeStale(r staleRecord, keep func() bool) {
	if staleTrustBeforeRemove != nil {
		staleTrustBeforeRemove()
	}
	if keep != nil && keep() {
		return
	}
	if fi, err := os.Lstat(r.file); err != nil || !fi.ModTime().Equal(r.mod) {
		return
	}
	_ = os.Remove(r.file)
}

// OwnCodexHook is one of magnum's Codex hooks at its place in a hooks.json
// ("stop:0:0"): Definition is what Codex hashes of it as magnum reads it
// (the CLI's form of its type, command, timeout, async and extra keys), so
// two hooks with one Definition hash the same.
type OwnCodexHook struct{ Place, Definition string }

// ownTrust is a memory of magnum's own trust: the hook of Definition stood
// at Place of the hooks.json Hooks, and Codex trusted it with TrustedHash.
type ownTrust struct {
	Hooks       string `json:"hooks"`
	Place       string `json:"place"`
	Definition  string `json:"definition"`
	TrustedHash string `json:"trusted_hash"`
}

// ownTrustDir is where the memories of magnum's own trust are kept: beside
// the stale records, in a directory of their own the records' reader skips.
func ownTrustDir() string {
	if dir := staleTrustDirectory(); dir != "" {
		return filepath.Join(dir, "own")
	}
	return ""
}

// ownTrustFile is the memory of the place of the hooks.json hooksFile in
// dir: one per place, the latest uninstall's.
func ownTrustFile(dir, hooksFile, place string) string {
	sum := sha256.Sum256([]byte(hooksFile + "\n" + place))
	return filepath.Join(dir, hex.EncodeToString(sum[:12])+".json")
}

// RememberOwnCodexTrust remembers, for each of hooks (magnum's own, which
// an uninstall is about to remove from the Codex home home's hooks.json),
// the trusted_hash at its place when the place reads trusted (it has a hash,
// and no stale record holds that hash): Codex keeps the entry after the
// hook goes, so an install that later puts the identical hook back at that
// place finds magnum's own valid trust there, which TakeOwnCodexTrust tells
// apart from a hook's that once stood there. A place with no trust, or a
// stale one, is not remembered. Without a records directory it remembers
// nothing.
func RememberOwnCodexTrust(home string, hooks []OwnCodexHook) error {
	dir := ownTrustDir()
	if dir == "" || len(hooks) == 0 {
		return nil
	}
	trust, ok, err := ReadCodexHooksTrust(home)
	if err != nil || !ok {
		return err
	}
	f := codexHookPaths(home)
	state, err := readCodexHookState(f.configFile)
	if err != nil {
		return err
	}
	for _, h := range hooks {
		_, e, ok := f.entry(state, h.Place)
		if !ok || e.TrustedHash == "" || slices.ContainsFunc(trust.Untrusted, func(u CodexHookAt) bool { return u.Place == h.Place }) {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("agents: %w", err)
		}
		b, err := json.Marshal(ownTrust{Hooks: f.hooksFile, Place: h.Place, Definition: h.Definition, TrustedHash: e.TrustedHash})
		if err != nil {
			return fmt.Errorf("agents: %w", err)
		}
		if err := fsx.WriteFileAtomic(ownTrustFile(dir, f.hooksFile, h.Place), b, 0o600); err != nil {
			return fmt.Errorf("agents: remember magnum's own trust of %s: %w", h.Place, err)
		}
	}
	return nil
}

// TakeOwnCodexTrust reports whether the trust entry at hook's place in the
// Codex home home is magnum's own, remembered at an uninstall for the
// identical hook (RememberOwnCodexTrust) and still with that hash: an
// install appending that hook there then records no stale trust. The
// memory is forgotten either way: it serves one install.
func TakeOwnCodexTrust(home string, hook OwnCodexHook) bool {
	dir := ownTrustDir()
	if dir == "" {
		return false
	}
	f := codexHookPaths(home)
	file := ownTrustFile(dir, f.hooksFile, hook.Place)
	b, err := fsx.ReadRegular(file, 4<<10)
	if err != nil {
		return false
	}
	_ = os.Remove(file)
	var o ownTrust
	if json.Unmarshal(b, &o) != nil || o.Hooks != f.hooksFile || o.Place != hook.Place || o.Definition != hook.Definition {
		return false
	}
	state, err := readCodexHookState(f.configFile)
	if err != nil {
		return false
	}
	_, e, ok := f.entry(state, hook.Place)
	return ok && e.TrustedHash != "" && e.TrustedHash == o.TrustedHash
}

// OwnCodexTrustCount is how many memories of magnum's own trust the Codex
// home home's hooks.json has.
func OwnCodexTrustCount(home string) int {
	return len(readOwnTrust(ownTrustDir(), filepath.Join(home, "hooks.json")))
}

// readOwnTrust reads the memories of the hooks.json hooksFile in dir: by
// file, the memory.
func readOwnTrust(dir, hooksFile string) map[string]ownTrust {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := map[string]ownTrust{}
	for _, de := range entries {
		if !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		file := filepath.Join(dir, de.Name())
		b, err := fsx.ReadRegular(file, 4<<10)
		if err != nil {
			continue
		}
		var o ownTrust
		if json.Unmarshal(b, &o) == nil && o.Hooks == hooksFile {
			out[file] = o
		}
	}
	return out
}

// forgetChangedOwnTrust removes the memories of the hooks.json of f whose
// place's trust entry no longer has the remembered hash, or none: the
// trust they stood for is gone.
func forgetChangedOwnTrust(f codexHookFiles, state map[string]codexHookEntry) {
	for file, o := range readOwnTrust(ownTrustDir(), f.hooksFile) {
		if _, e, ok := f.entry(state, o.Place); !ok || e.TrustedHash != o.TrustedHash {
			_ = os.Remove(file)
		}
	}
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
