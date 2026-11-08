package staging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/fsx"
)

// journalVersion is the version of an effect's journal file; a file of
// another version is not read (Effects and Step pass it over).
const journalVersion = 1

// maxJournal bounds the journal file a read takes.
const maxJournal = 1 << 20

// idRe is an effect's id: <env>-<pr>-<time>-<random>, a file name and a ref
// component.
var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,95}$`)

// validID reports whether id is an effect id (newID's form).
func validID(id EffectID) bool { return idRe.MatchString(string(id)) }

// newID is a new effect's id: the env, the PR, the time and 4 random bytes.
func newID(env string, number int, now time.Time) EffectID {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return EffectID(env + "-" + strconv.Itoa(number) + "-" + now.UTC().Format("20060102t150405") + "-" + hex.EncodeToString(b))
}

// journal is an effect's state, the source of truth: Step resumes at its
// recorded step and phase. It holds shas, paths, times and the effect's own
// words, never PR text (the head branch it may hold is a validBranch name).
type journal struct {
	V     int      `json:"v"`
	ID    EffectID `json:"id"`
	Repo  string   `json:"repo"`
	Env   Env      `json:"env"`
	PR    PR       `json:"pr"`
	By    string   `json:"by"`
	Step  string   `json:"step"`
	Phase string   `json:"phase,omitempty"` // the step's: "" (not begun), begin or fail
	State string   `json:"state"`
	Why   string   `json:"why,omitempty"`
	// The operator's plan: the tip, the migrations and the workflow change
	// they saw and confirmed.
	PlanTip        string    `json:"plan_tip"`
	PlanMigrations []string  `json:"plan_migrations,omitempty"`
	PlanWorkflows  bool      `json:"plan_workflows,omitempty"`
	PlanAt         time.Time `json:"plan_at"`
	// Tip is the tip the merge is made on; Remerges counts the merges again
	// after it moved (verify's and push's); Try is 1 + Remerges.
	Tip      string `json:"tip,omitempty"`
	Remerges int    `json:"remerges,omitempty"`
	// MergeTree is merge-tree's result on Tip, Conflicts its conflicted
	// paths; Merge the merge commit, clean or resolved.
	MergeTree string   `json:"merge_tree,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
	Merge     string   `json:"merge,omitempty"`
	// Job is the resolver's running job; Resolves counts its tries.
	// Resolved: the resolver's merge is in the mirror at resolvedRef.
	Job      string `json:"job,omitempty"`
	Resolves int    `json:"resolves,omitempty"`
	Resolved bool   `json:"resolved,omitempty"`
	// PushTries counts the pushes that failed for another reason than a
	// moved tip; PushedAt is when the branch first held the merge.
	PushTries int       `json:"push_tries,omitempty"`
	PushedAt  time.Time `json:"pushed_at"`
	// RunURL is ci.yml's run on the merge; CIAt when watch let it go (it
	// passed, was cancelled, or none came).
	RunURL string    `json:"run_url,omitempty"`
	CIAt   time.Time `json:"ci_at"`
	// Deploys is the last read of the env's deployments, with its ETag.
	Deploys *deployEntry `json:"deploys,omitempty"`
	Started time.Time    `json:"started"`
	Updated time.Time    `json:"updated"`
}

// live reports whether the effect still runs or waits: queued or running.
func (j *journal) live() bool { return j.State == EffectQueued || j.State == EffectRunning }

// effect is the journal as the screens see it.
func (j *journal) effect() Effect {
	return Effect{ID: j.ID, Repo: j.Repo, Env: j.Env, PR: j.PR, By: j.By, Step: j.Step, State: j.State, Why: j.Why,
		Merge: j.Merge, RunURL: j.RunURL, Try: 1 + j.Remerges, Started: j.Started, Updated: j.Updated}
}

// deploysDir is the journal directory beside the mirror at dir
// (<mirror_dir>/<owner>/<name>.git): <mirror_dir>/<owner>/<name>.deploys.
func deploysDir(dir string) string { return strings.TrimSuffix(dir, ".git") + ".deploys" }

// journalName is effect id's journal file in the deploys dir.
func journalName(id EffectID) string { return string(id) + ".json" }

// effectLockName is effect id's flock in the deploys dir, which each step,
// Cancel and a resumed Deploy hold while they read and write its journal.
func effectLockName(id EffectID) string { return string(id) + ".json.lock" }

// envLockName is the env's flock in the deploys dir: whoever holds it runs
// the env's one live effect.
func envLockName(env string) string { return env + ".lock" }

// openDeploys opens the deploys dir beside the mirror at dir, creating it
// when create (through an os.Root on the owner's dir, never above it).
func openDeploys(dir string, create bool) (*os.Root, error) {
	dd := deploysDir(dir)
	if create {
		owner, err := os.OpenRoot(filepath.Dir(dd))
		if err != nil {
			return nil, fmt.Errorf("the mirror's directory: %w", err)
		}
		err = owner.Mkdir(filepath.Base(dd), 0o700)
		owner.Close()
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	return os.OpenRoot(dd)
}

// readJournal reads effect id's journal in root.
func readJournal(root *os.Root, id EffectID) (*journal, error) {
	data, err := fsx.ReadRegularIn(root, journalName(id), maxJournal)
	if err != nil {
		return nil, err
	}
	var j journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("effect %s: %w", id, err)
	}
	if j.V != journalVersion || j.ID != id {
		return nil, fmt.Errorf("effect %s: a journal of another build", id)
	}
	return &j, nil
}

// writeJournal writes j atomically.
func writeJournal(root *os.Root, j *journal) error {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomicIn(root, journalName(j.ID), data, 0o600)
}

// readJournals reads every journal in root, oldest first (Started, then
// id); a file it cannot read is passed over.
func readJournals(root *os.Root) []*journal {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil
	}
	var out []*journal
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !e.Type().IsRegular() || !validID(EffectID(name)) {
			continue
		}
		if j, err := readJournal(root, EffectID(name)); err == nil {
			out = append(out, j)
		}
	}
	slices.SortFunc(out, func(a, b *journal) int {
		if c := a.Started.Compare(b.Started); c != 0 {
			return c
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	return out
}

// flockIn opens name in root and takes its flock; wait false tries once
// (ok false when another holds it), else it polls at lockPoll as long as
// ctx.
func flockIn(ctx context.Context, root *os.Root, name string, wait bool) (f *os.File, ok bool, err error) {
	f, err = root.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, true, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, false, err
		}
		if !wait {
			_ = f.Close()
			return nil, false, nil
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, false, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// unflock releases f's flock and closes it.
func unflock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// envLock is an env flock this client holds, for the effect it runs.
type envLock struct {
	f  *os.File
	id EffectID
}

// envKey is the key of the env flock of a deploys dir in Client.envLocks.
func envKey(dir, env string) string { return deploysDir(dir) + "\x00" + env }

// heldFor is the effect this client holds env's flock for, if any.
func (c *Client) heldFor(dir, env string) (EffectID, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.envLocks[envKey(dir, env)]
	if !ok {
		return "", false
	}
	return l.id, true
}

// release lets env's flock go, when this client holds it.
func (c *Client) release(dir, env string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := envKey(dir, env)
	if l, ok := c.envLocks[k]; ok {
		unflock(l.f)
		delete(c.envLocks, k)
	}
}

// releaseAll lets every env flock of this client go (Close): the effects
// stay as their journals say, and another client resumes them.
func (c *Client) releaseAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, l := range c.envLocks {
		unflock(l.f)
		delete(c.envLocks, k)
	}
}
