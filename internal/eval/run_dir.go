package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Case outcomes of a replay besides the round's (pipeline.Outcome*), which
// evalrun.RunCase records (OutcomeSkipped: `magnum eval run`) and the
// daemon's gate of a skill proposal reads.
const (
	// OutcomeFlagged: the case is not replayed, since an earlier replay of
	// it or of its PR was refused or the live registry holds a Codex flag
	// for its PR.
	OutcomeFlagged = "flagged"
	// OutcomeBudget: the Codex budget is at or past [usage] codex_soft; no
	// later case should start either.
	OutcomeBudget = "budget"
	// OutcomeSkipped: the case is not replayed, since an earlier case of the
	// run hit a usage limit, needed a login or reached the Codex budget, or
	// the run was interrupted; `magnum eval run` records it so.
	OutcomeSkipped = "skipped"
)

// RunIDFormat is a run id's time part: sortable, to the second.
const RunIDFormat = "20060102-150405"

// NewRunDir creates the directory of a run started at now under root and
// returns the run's id and directory. The id is now to the second, with
// "-2", "-3"... when a run started in the same second holds it (os.Mkdir
// fails with ErrExist), so no two runs share a directory, a record or an
// agent tag (AgentTag).
func NewRunDir(root string, now time.Time) (id, dir string, err error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", "", err
	}
	base := now.Format(RunIDFormat)
	for n := 1; n <= 100; n++ {
		id = base
		if n > 1 {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		dir = filepath.Join(root, id)
		mkErr := os.Mkdir(dir, 0o700)
		if mkErr == nil {
			return id, dir, nil
		}
		if !errors.Is(mkErr, fs.ErrExist) {
			return "", "", mkErr
		}
	}
	return "", "", fmt.Errorf("no free run id for %s under %s", base, root)
}

// AgentTag tags the agents of run id (agents.Deps.Tag): "eval-" and the
// first 6 hex digits of the id's SHA-256, the same for all the run's cases.
// The tag joins the agents' names' hash, so they are named apart from the
// PR's own agents and from every other run's: StartAgent adopts an agent
// that carries a role's name, and a run once adopted the agents the run
// before it left behind. It also leads their titles and pane labels.
func AgentTag(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "eval-" + hex.EncodeToString(sum[:3])
}
