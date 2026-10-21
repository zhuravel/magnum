// Command magnum-relay receives a GitHub App's webhook deliveries and keeps their routing envelopes for
// magnum's daemon to read (internal/relay; deploy/relay/README.md deploys it).
//
//	magnum-relay [serve] [flags]   serve the webhook, the listing and /healthz
//	magnum-relay hash-token        print the sha256 of the token on stdin, for MAGNUM_RELAY_TOKEN_HASHES
//	magnum-relay healthcheck       exit 0 when the relay on MAGNUM_RELAY_ADDR answers /healthz ok
//	magnum-relay version
//
// Each flag of serve defaults to its environment variable: -addr MAGNUM_RELAY_ADDR (":8080"), -db
// MAGNUM_RELAY_DB ("/data/relay.db"), -secret-file MAGNUM_RELAY_SECRET_FILE and -previous-secret-file
// MAGNUM_RELAY_PREVIOUS_SECRET_FILE (the webhook secrets, read from files), -token-hashes
// MAGNUM_RELAY_TOKEN_HASHES (comma-separated sha256 hex of the accepted bearer tokens), -installations
// MAGNUM_RELAY_INSTALLATIONS (comma-separated ids of the App's installations whose deliveries are kept;
// empty keeps every installation's, and serve logs that it does) and -retention MAGNUM_RELAY_RETENTION
// ("720h"). SIGINT or SIGTERM answers the waiting long polls and shuts down.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/relay"
)

var version = "dev"

const (
	usage = `usage: magnum-relay [serve] [-addr :8080] [-db /data/relay.db] [-secret-file F] [-previous-secret-file F]
                    [-token-hashes H1,H2] [-installations ID1,ID2] [-retention 720h]
       magnum-relay hash-token < token-file
       magnum-relay healthcheck [-addr :8080]
       magnum-relay version
`
	// minSecret is the shortest webhook secret accepted (openssl rand -hex 32 makes 64 characters); a bearer
	// token's bounds are the relay's own (relay.CheckToken).
	minSecret = 16
	// maxSecretFile bounds a secret file and what hash-token reads from stdin.
	maxSecretFile = 4096

	// shutdownTimeout lets a delivery that has begun finish (relay.WebhookDeadline) before the connections
	// still open are closed.
	shutdownTimeout = relay.WebhookDeadline + 5*time.Second
	pruneEvery      = time.Hour
	// refusalLogEvery is how often the refusals counted are logged, one line a kind (relay.RunRefusalLog).
	refusalLogEvery = time.Minute
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// usageError is a refusal of the command line or the settings: exit 2.
type usageError struct{ error }

func run(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		var cfg config
		if cfg, err = parseServe(args, getenv, stderr); err == nil {
			logger := slog.New(slog.NewJSONHandler(stderr, nil))
			err = serve(ctx, cfg, logger, version, nil)
		}
	case "hash-token":
		err = hashToken(stdin, stdout)
	case "healthcheck":
		fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
		fs.SetOutput(stderr)
		addr := fs.String("addr", envOr(getenv, "MAGNUM_RELAY_ADDR", ":8080"), "the relay's listen address")
		if err = fs.Parse(args); err != nil {
			err = usageError{err}
		} else {
			err = healthcheck(ctx, *addr)
		}
	case "version":
		fmt.Fprintf(stdout, "magnum-relay %s\n", version)
	default:
		err = usageError{fmt.Errorf("unknown command %q", cmd)}
	}
	if err == nil {
		return 0
	}
	fmt.Fprintf(stderr, "magnum-relay: %v\n", err)
	if _, ok := errors.AsType[usageError](err); ok {
		fmt.Fprint(stderr, usage)
		return 2
	}
	return 1
}

// config is serve's settings.
type config struct {
	addr, db      string
	secrets       [][]byte
	tokens        [][sha256.Size]byte
	installations []int64 // sorted, once each; none: every installation
	retention     time.Duration
	shutdown      time.Duration
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// parseServe reads serve's flags over their environment variables, the secret files and the token hashes.
// Its errors never quote a secret.
func parseServe(args []string, getenv func(string) string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", envOr(getenv, "MAGNUM_RELAY_ADDR", ":8080"), "listen address")
	db := fs.String("db", envOr(getenv, "MAGNUM_RELAY_DB", "/data/relay.db"), "the journal's SQLite file")
	secretFile := fs.String("secret-file", getenv("MAGNUM_RELAY_SECRET_FILE"), "file holding the webhook secret")
	previousFile := fs.String("previous-secret-file", getenv("MAGNUM_RELAY_PREVIOUS_SECRET_FILE"), "file holding the previous webhook secret, during a rotation")
	hashes := fs.String("token-hashes", getenv("MAGNUM_RELAY_TOKEN_HASHES"), "comma-separated sha256 hex of the accepted bearer tokens")
	installations := fs.String("installations", getenv("MAGNUM_RELAY_INSTALLATIONS"), "comma-separated ids of the App's installations whose deliveries are kept (every installation's when empty)")
	retention := fs.String("retention", envOr(getenv, "MAGNUM_RELAY_RETENTION", "720h"), "how long events are kept")
	if err := fs.Parse(args); err != nil {
		return config{}, usageError{err}
	}
	if fs.NArg() > 0 {
		return config{}, usageError{fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}
	cfg := config{addr: *addr, db: *db, shutdown: shutdownTimeout}
	if *secretFile == "" {
		return config{}, usageError{errors.New("MAGNUM_RELAY_SECRET_FILE (or -secret-file) is required")}
	}
	for _, f := range []string{*secretFile, *previousFile} {
		if f == "" {
			continue
		}
		s, err := readSecret(f)
		if err != nil {
			return config{}, usageError{err}
		}
		cfg.secrets = append(cfg.secrets, s)
	}
	tokens, err := parseHashes(*hashes)
	if err != nil {
		return config{}, usageError{err}
	}
	cfg.tokens = tokens
	if cfg.installations, err = parseInstallations(*installations); err != nil {
		return config{}, usageError{err}
	}
	d, err := time.ParseDuration(*retention)
	if err != nil || d < relay.MinRetention {
		return config{}, usageError{fmt.Errorf("retention %q: a duration of at least %v, such as 720h", *retention, relay.MinRetention)}
	}
	cfg.retention = d
	return cfg, nil
}

// readSecret reads a webhook secret file: a regular file of at most maxSecretFile bytes, its content without
// surrounding whitespace, at least minSecret characters.
func readSecret(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("webhook secret file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("webhook secret file: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxSecretFile {
		return nil, fmt.Errorf("webhook secret file %s: not a regular file of at most %d bytes", path, maxSecretFile)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSecretFile+1))
	if err != nil {
		return nil, fmt.Errorf("webhook secret file %s: %w", path, err)
	}
	s := strings.TrimSpace(string(b))
	if len(s) < minSecret {
		return nil, fmt.Errorf("webhook secret file %s: the secret must have at least %d characters (openssl rand -hex 32)", path, minSecret)
	}
	return []byte(s), nil
}

func parseHashes(s string) ([][sha256.Size]byte, error) {
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("MAGNUM_RELAY_TOKEN_HASHES (or -token-hashes) is required: the sha256 of each token, from `magnum-relay hash-token`")
	}
	var out [][sha256.Size]byte
	for i, h := range strings.Split(s, ",") {
		b, err := hex.DecodeString(strings.TrimSpace(h))
		if err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("MAGNUM_RELAY_TOKEN_HASHES: entry %d is not a sha256 in hex (64 characters)", i+1)
		}
		out = append(out, [sha256.Size]byte(b))
	}
	return out, nil
}

// parseInstallations reads the comma-separated installation ids, sorted and once each: none for "" (every
// installation's deliveries are kept).
func parseInstallations(s string) ([]int64, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []int64
	for i, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil || id < 1 || strings.HasPrefix(f, "+") {
			return nil, fmt.Errorf("MAGNUM_RELAY_INSTALLATIONS: entry %d is not an installation id (a whole number of at least 1, the last part of the installation's settings URL)", i+1)
		}
		out = append(out, id)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// hashToken prints the sha256 hex of the one token on stdin (a trailing newline dropped).
func hashToken(stdin io.Reader, stdout io.Writer) error {
	b, err := io.ReadAll(io.LimitReader(bufio.NewReader(stdin), maxSecretFile+1))
	if err != nil {
		return err
	}
	if len(b) > maxSecretFile {
		return usageError{fmt.Errorf("the token on stdin is longer than %d bytes", maxSecretFile)}
	}
	token := strings.TrimSpace(string(b))
	if err := relay.CheckToken(token); err != nil {
		return usageError{fmt.Errorf("the token on stdin: %w (openssl rand -hex 32 makes one)", err)}
	}
	sum := sha256.Sum256([]byte(token))
	_, err = fmt.Fprintln(stdout, hex.EncodeToString(sum[:]))
	return err
}

// serve runs the relay on cfg.addr until ctx ends, then answers the waiting long polls, lets the requests in
// flight finish and closes the journal. ready, when set, gets the listening address.
func serve(ctx context.Context, cfg config, logger *slog.Logger, ver string, ready func(net.Addr)) error {
	r, err := relay.Open(ctx, relay.Options{
		DB:            cfg.db,
		Secrets:       cfg.secrets,
		TokenHashes:   cfg.tokens,
		Installations: cfg.installations,
		Retention:     cfg.retention,
		Logger:        logger,
	})
	if err != nil {
		return err
	}
	defer r.Close()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler: r.Handler(),
		// The headers come first; then the webhook gives its body relay.DefaultBodyTimeout and the listing
		// sets its own write deadline for a long poll, so these are only the backstops.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       relay.WebhookDeadline,
		WriteTimeout:      relay.MaxWait + 15*time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	// The retention and the refusals' log run until the server has shut down; the log's last lines then
	// count what came up to the end.
	bgCtx, stopBg := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Go(func() { r.RunRetention(bgCtx, pruneEvery) })
	wg.Go(func() { r.RunRefusalLog(bgCtx, refusalLogEvery) })
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	logger.Info("relay listening", "addr", ln.Addr().String(), "version", ver, "epoch", r.Epoch(),
		"secrets", len(cfg.secrets), "tokens", len(cfg.tokens), "installations", len(cfg.installations),
		"retention", cfg.retention.String())
	if len(cfg.installations) == 0 {
		logger.Warn("every installation's deliveries are kept: MAGNUM_RELAY_INSTALLATIONS names none, and anyone may install a public App")
	}
	if ready != nil {
		ready(ln.Addr())
	}
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-served:
	}
	r.StopWaiting()
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.shutdown)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		// What is still open after a delivery's whole deadline is a stalled connection: close it.
		logger.Warn("shutdown timed out, closing the connections left", "class", "timeout")
		_ = srv.Close()
	}
	stopBg()
	wg.Wait()
	logger.Info("relay stopped")
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}

// healthcheck asks the relay listening on addr (on the loopback when it listens on every interface) for
// /healthz and fails unless it answers 200 with "ok": true.
func healthcheck(ctx context.Context, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return usageError{fmt.Errorf("address %q: %w", addr, err)}
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/healthz", nil)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var v struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&v); err != nil || resp.StatusCode != http.StatusOK || !v.OK {
		return fmt.Errorf("relay unhealthy: %s", resp.Status)
	}
	return nil
}
