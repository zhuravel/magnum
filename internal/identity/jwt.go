package identity

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/paths"
)

// signJWT builds the GitHub App JWT: RS256 over {iat: now-60, exp: now+540, iss}.
// The backdated iat absorbs clock drift; exp stays under GitHub's 10-minute cap.
func signJWT(key *rsa.PrivateKey, issuer string, now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	claims, err := json.Marshal(struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}{now.Unix() - 60, now.Unix() + 540, issuer})
	if err != nil {
		return "", fmt.Errorf("encode JWT claims: %w", err)
	}
	signing := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// looseKeyFile returns the key file named by value (the env var's content)
// and its mode when other users can read it; "" when value is PEM text, the
// file is missing or its mode is private.
func looseKeyFile(value string) (path string, mode fs.FileMode) {
	v := strings.TrimSpace(value)
	if v == "" || strings.Contains(v, "-----BEGIN") {
		return "", 0
	}
	path = paths.Expand(v)
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o077 == 0 {
		return "", 0
	}
	return path, st.Mode().Perm()
}

// ErrPlaceholderKey matches (errors.Is) the error for a key variable that
// still holds the placeholder sentence the tracked .mise.toml sets, instead
// of the PEM text or key file path from .mise.local.toml.
var ErrPlaceholderKey = errors.New("identity: placeholder private key")

type placeholderKeyError struct{ env string }

func (e placeholderKeyError) Error() string {
	return fmt.Sprintf("the private key in $%s is the placeholder from .mise.toml; put the PEM in .mise.local.toml", e.env)
}

func (e placeholderKeyError) Is(target error) bool { return target == ErrPlaceholderKey }

// isPlaceholderKey reports whether v (trimmed, not PEM text) is placeholder
// text rather than a key file path: it carries the placeholder's
// "you-must-configure", or it names no existing file and does not look like
// a path (no separator, no leading ~, no .pem/.key ending), so a mistyped
// path still reports the missing file.
func isPlaceholderKey(v string) bool {
	lower := strings.ToLower(v)
	if strings.Contains(lower, "you-must-configure") {
		return true
	}
	if strings.ContainsAny(v, `/\`) || strings.HasPrefix(v, "~") ||
		strings.HasSuffix(lower, ".pem") || strings.HasSuffix(lower, ".key") {
		return false
	}
	_, err := os.Stat(paths.Expand(v))
	return err != nil
}

// loadPrivateKey parses the App key from the value of env var envName: PEM
// text (real or \n-escaped newlines) or a path to a PEM file. PKCS#1 and
// PKCS#8 RSA keys are accepted. The placeholder of the tracked .mise.toml is
// reported as such (ErrPlaceholderKey), never read as a path. Errors never
// echo the value.
func loadPrivateKey(envName, value string) (*rsa.PrivateKey, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil, fmt.Errorf("$%s is empty", envName)
	}
	var data []byte
	if strings.Contains(v, "-----BEGIN") {
		if !strings.Contains(v, "\n") && strings.Contains(v, `\n`) {
			v = strings.ReplaceAll(v, `\n`, "\n")
		}
		data = []byte(v)
	} else {
		if isPlaceholderKey(v) {
			return nil, placeholderKeyError{env: envName}
		}
		b, err := os.ReadFile(paths.Expand(v))
		if err != nil {
			var pe *fs.PathError
			if errors.As(err, &pe) {
				err = pe.Err // drop the path: the value might be key material, not a path
			}
			return nil, fmt.Errorf("read private key file named by $%s: %w", envName, err)
		}
		data = b
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("parse private key from $%s: no PEM block", envName)
	}
	if block.Type == "RSA PRIVATE KEY" {
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse private key from $%s (PKCS#1): %w", envName, err)
		}
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if k1, err1 := x509.ParsePKCS1PrivateKey(block.Bytes); err1 == nil {
			return k1, nil
		}
		return nil, fmt.Errorf("parse private key from $%s (%s): %w", envName, block.Type, err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key from $%s is not an RSA key (%T)", envName, k)
	}
	return rk, nil
}
