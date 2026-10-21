package usage

// Codex's login: a running Codex process keeps the login it started with, so
// after a switch of account (clauth makes $CODEX_HOME/auth.json a symlink to
// a profile's file) every live session still spends the old account's
// budget. The daemon tells logins apart by their account alone: CodexLogin
// reads auth.json's tokens.account_id (or notes API-key mode) and keeps
// only a fingerprint of it, never a token.

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// LoginAPIKey stands for the account of a login in API-key mode (auth.json
// holds OPENAI_API_KEY and no ChatGPT account): the key itself is never
// read past telling it is there.
const LoginAPIKey = "api-key"

// maxAuthBytes bounds the read of auth.json (a few KB in practice).
const maxAuthBytes = 1 << 20

// ErrNoLogin means auth.json is absent, or holds neither an account nor an
// API key.
var ErrNoLogin = errors.New("usage: no Codex login in auth.json")

// Login is the Codex login of a CODEX_HOME, as far as magnum tells logins
// apart.
type Login struct {
	// Fingerprint is the first 12 hex digits of the sha256 of the login's
	// account: tokens.account_id, or LoginAPIKey in API-key mode.
	Fingerprint string
	// ChangedAt is when auth.json last changed: the newer of its own
	// modification time and, when it is a symlink, its target's.
	ChangedAt time.Time
}

// authFile is all of auth.json that is decoded: the account id, and
// whether an API key is set (never its value).
type authFile struct {
	APIKey presence `json:"OPENAI_API_KEY"`
	Tokens *struct {
		AccountID string `json:"account_id"`
	} `json:"tokens"`
}

// presence decodes a JSON value into whether it is set (neither null nor
// ""), keeping nothing of it.
type presence bool

// UnmarshalJSON implements json.Unmarshaler.
func (p *presence) UnmarshalJSON(b []byte) error {
	s := string(b)
	*p = presence(s != "null" && s != `""`)
	return nil
}

// CodexLogin reads the login of home's auth.json (home is a CODEX_HOME;
// empty means DefaultCodexHome), following a symlink. A ChatGPT login's
// account (tokens.account_id) wins over an API key. It returns ErrNoLogin
// for no file, a dangling link and a file with neither; a file that is not
// JSON is an error that quotes nothing of it.
func CodexLogin(home string) (Login, error) {
	path := filepath.Join(cmp.Or(home, DefaultCodexHome()), "auth.json")
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Login{}, ErrNoLogin
	}
	if err != nil {
		return Login{}, fmt.Errorf("usage: open %s: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Login{}, fmt.Errorf("usage: stat %s: %w", path, err)
	}
	changed := st.ModTime()
	if lst, err := os.Lstat(path); err == nil && lst.ModTime().After(changed) {
		changed = lst.ModTime()
	}
	b, err := io.ReadAll(io.LimitReader(f, maxAuthBytes))
	if err != nil {
		return Login{}, fmt.Errorf("usage: read %s: %w", path, err)
	}
	var auth authFile
	if json.Unmarshal(b, &auth) != nil {
		return Login{}, fmt.Errorf("usage: %s is not the JSON Codex writes", path) // the decoder's error may quote the file
	}
	account := ""
	switch {
	case auth.Tokens != nil && auth.Tokens.AccountID != "":
		account = auth.Tokens.AccountID
	case bool(auth.APIKey):
		account = LoginAPIKey
	default:
		return Login{}, ErrNoLogin
	}
	return Login{Fingerprint: fingerprint(account), ChangedAt: changed}, nil
}

// fingerprint is the first 12 hex digits of account's sha256.
func fingerprint(account string) string {
	sum := sha256.Sum256([]byte(account))
	return hex.EncodeToString(sum[:])[:12]
}
