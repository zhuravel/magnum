package cli

// `magnum init`: write the user's config (~/.config/magnum/config.toml) for a new machine from three
// questions (the gh login, one repository, who posts), validate it with the
// committed config and print what to do next. It never asks for a key:
// an App's private key is saved by hand as the file the config names.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
)

const initUsage = "[--force]"

func newInitCmd(c *Context) *cobra.Command {
	var force bool
	cmd := newCommand(groupAct, "init "+initUsage, "write ~/.config/magnum/config.toml for this machine: your gh login, one repository, who posts",
		"Ask three questions and write your config, ~/.config/magnum/config.toml ($XDG_CONFIG_HOME/magnum when set), which layers over the built-in defaults: your gh login (the default comes "+
			"from `gh api user`), one repository to watch (owner/name) and who posts the reviews, your gh login or a "+
			"GitHub App (then its app id, client id and installation id). The result is the smallest valid setup: one identity (plus the App when it posts), one "+
			"watch of that repository, no pool and no database, with daemon.default_repo set so `magnum review 123` "+
			"works. It is validated with config.toml before it is written. An App's private key is never asked for: "+
			"init names the file to save it as (~/.config/magnum/keys/<app>.pem, chmod 600). An existing config is refused unless --force "+
			"(the old file is kept as config.toml.bak). ctrl+c or an empty input stops without writing.",
		func(pos []string) int { return runInit(c, force, pos) })
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing config (kept as <file>.bak)")
	return cmd
}

// initAnswers are what init asked.
type initAnswers struct {
	login, owner, name string
	app                *initApp // nil: reviews are posted as login
}

// initApp is a GitHub App identity's settings.
type initApp struct {
	login, clientID       string
	keyFile               string // where the App's PEM is to be saved (~ form), next to the config
	appID, installationID int64
}

var (
	initLoginRe  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	initRepoRe   = regexp.MustCompile(`^([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))/([A-Za-z0-9._-]{1,100})$`)
	initBotRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*\[bot\]$`)
	initClientRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

func runInit(c *Context, force bool, pos []string) int {
	if len(pos) > 0 {
		return inspUsage(c, "init", fmt.Sprintf("unexpected argument %q", pos[0]), initUsage)
	}
	file := daemonConfigOverride(c) // "" = the built-in defaults (or a legacy config.toml in the home)
	local := initTarget(c, file)
	if _, err := os.Stat(local); err == nil && !force {
		fmt.Fprintf(c.Stderr, "magnum init: %s exists\nfix: edit it (config.full.example.toml has every key in a worked setup), or re-run with --force to replace it (the old file is kept as %s)\n",
			inspTilde(local), filepath.Base(local)+".bak")
		return 1
	}
	if _, err := config.LoadWithOptions(c.Layout, file, config.LoadOptions{NoOverlay: true}); err != nil {
		fmt.Fprintf(c.Stderr, "magnum init: the defaults do not load: %v\nfix: restore config.defaults.toml and rebuild (make build)\n", err)
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	ans, err := initAsk(ctx, c.Stdout, inspPrompt(), initGHLogin(ctx))
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum init: cancelled (%v); nothing was written\n", err)
		return 1
	}
	if ans.app != nil {
		ans.app.keyFile = inspTilde(filepath.Join(filepath.Dir(local), "keys", initAppName(ans.app.login)+".pem"))
	}
	content := initRender(ans, time.Now())
	if err := initValidate(c, file, content); err != nil {
		fmt.Fprintf(c.Stderr, "magnum init: the answers do not make a valid config, nothing was written: %v\n", err)
		return 1
	}
	if err := initWrite(local, content); err != nil {
		fmt.Fprintf(c.Stderr, "magnum init: %v\n", err)
		return 1
	}
	cfg, err := config.Load(c.Layout, file)
	if err != nil { // validated above; only a race with another writer gets here
		fmt.Fprintf(c.Stderr, "magnum init: wrote %s, but it does not validate: %v\n", inspTilde(local), err)
		return 1
	}
	initReport(c.Stdout, local, ans, cfg)
	return 0
}

// initTarget is the file init writes: next to an explicit --config file its
// config.local.toml; else the user config (~/.config/magnum/config.toml);
// else (a layout without one) config.local.toml in the home.
func initTarget(c *Context, file string) string {
	switch {
	case file != "":
		return filepath.Join(filepath.Dir(file), "config.local.toml")
	case c.Layout.UserConfig != "":
		return c.Layout.UserConfig
	}
	if c.Layout.Home == "" {
		return ""
	}
	return filepath.Join(c.Layout.Home, "config.local.toml")
}

// initGHLogin is the login gh is logged in as; "" when gh cannot tell.
func initGHLogin(ctx context.Context) string {
	run := daemonSys.Runner
	if run == nil {
		run = &execx.Real{}
	}
	res, err := run.Run(ctx, execx.Cmd{Name: "gh", Args: []string{"api", "user", "--jq", ".login"}, Timeout: 15 * time.Second, Label: "init gh login"})
	if err != nil {
		return ""
	}
	if l := res.Out(); initLoginRe.MatchString(l) {
		return l
	}
	return ""
}

// initAsk asks the questions; defLogin is the default gh login.
func initAsk(ctx context.Context, w io.Writer, in *promptIn, defLogin string) (initAnswers, error) {
	var a initAnswers
	fmt.Fprintln(w, "magnum init writes your config (~/.config/magnum/config.toml): one identity, one watched repository, no pool. Empty input stops.")
	var err error
	if a.login, err = in.ask(ctx, w, "Your GitHub login (gh polls GitHub as it)", defLogin, initCheckLogin); err != nil {
		return a, err
	}
	repo, err := in.ask(ctx, w, "Repository to review (owner/name)", "", initCheckRepo)
	if err != nil {
		return a, err
	}
	a.owner, a.name, _ = strings.Cut(repo, "/")
	who, err := in.ask(ctx, w, "Post reviews as 1) your login "+a.login+" or 2) a GitHub App", "1", func(s string) (string, error) {
		switch strings.ToLower(s) {
		case "1", "gh", "me", a.login:
			return "gh", nil
		case "2", "app":
			return "app", nil
		}
		return "", errors.New("answer 1 or 2")
	})
	if err != nil || who == "gh" {
		return a, err
	}
	app := &initApp{}
	if app.login, err = in.ask(ctx, w, "The App's bot login (<slug>[bot], as its reviews show)", "", initCheckBot); err != nil {
		return a, err
	}
	if app.appID, err = initAskID(ctx, w, in, "App id (the App's settings page)"); err != nil {
		return a, err
	}
	if app.clientID, err = in.ask(ctx, w, "Client id (Iv23…)", "", func(s string) (string, error) {
		if !initClientRe.MatchString(s) {
			return "", errors.New("letters, digits, '.', '_' and '-' only")
		}
		return s, nil
	}); err != nil {
		return a, err
	}
	if app.installationID, err = initAskID(ctx, w, in, "Installation id (the number in the installation's URL)"); err != nil {
		return a, err
	}
	a.app = app
	return a, nil
}

func initAskID(ctx context.Context, w io.Writer, in *promptIn, q string) (int64, error) {
	s, err := in.ask(ctx, w, q, "", func(s string) (string, error) {
		if n, err := strconv.ParseInt(s, 10, 64); err != nil || n <= 0 {
			return "", errors.New("a positive number")
		}
		return s, nil
	})
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(s, 10, 64)
}

func initCheckLogin(s string) (string, error) {
	s = strings.TrimPrefix(s, "@")
	if !initLoginRe.MatchString(s) {
		return "", errors.New("a GitHub login: letters, digits and '-'")
	}
	return s, nil
}

// initCheckRepo takes owner/name, also as a github.com URL.
func initCheckRepo(s string) (string, error) {
	for _, p := range []string{"https://", "http://", "github.com/", "git@github.com:"} {
		s = strings.TrimPrefix(s, p)
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	if !initRepoRe.MatchString(s) {
		return "", errors.New("owner/name, for example talkable/talkable")
	}
	return s, nil
}

// initCheckBot takes the App's bot login, adding a missing "[bot]".
func initCheckBot(s string) (string, error) {
	if !strings.HasSuffix(s, "[bot]") {
		s += "[bot]"
	}
	if !initBotRe.MatchString(s) {
		return "", errors.New("<slug>[bot], for example example-reviewer[bot]")
	}
	return s, nil
}

// initAppName names the App identity: its slug and "-app".
func initAppName(bot string) string { return strings.TrimSuffix(bot, "[bot]") + "-app" }

// initRender is the config.local.toml for a. Every value was validated to
// a plain character set, so strconv.Quote yields valid TOML strings.
func initRender(a initAnswers, now time.Time) string {
	q := strconv.Quote
	var b strings.Builder
	fmt.Fprintf(&b, "# This machine's identities and watches, written by `magnum init` on %s (gitignored).\n", now.Format("2006-01-02"))
	b.WriteString("# config.toml.example has every key in a worked setup; `magnum config` validates the result.\n\n")
	fmt.Fprintf(&b, "[daemon]\ndefault_repo = %s   # `magnum review 123` means this repository\n\n", q(a.owner+"/"+a.name))
	fmt.Fprintf(&b, "[[identity]]\nname = %s\nkind = \"gh\"                 # your gh login: polls GitHub", q(a.login))
	if a.app == nil {
		b.WriteString(" and posts the reviews")
	}
	fmt.Fprintf(&b, "\nlogin = %s\n\n", q(a.login))
	post := a.login
	if a.app != nil {
		post = initAppName(a.app.login)
		fmt.Fprintf(&b, "[[identity]]\nname = %s\nkind = \"app\"                # a GitHub App: posts the reviews\nlogin = %s\n", q(post), q(a.app.login))
		fmt.Fprintf(&b, "app_id = %d\nclient_id = %s\ninstallation_id = %d\n", a.app.appID, q(a.app.clientID), a.app.installationID)
		fmt.Fprintf(&b, "private_key_file = %s   # the App's PEM (chmod 600); magnum never asks for it\n\n", q(a.app.keyFile))
	}
	fmt.Fprintf(&b, "[[watch]]\nowner = %s\ninclude = [%s]\nidentity = %s\npoll_identity = %s\n", q(a.owner), q(a.name), q(post), q(a.login))
	b.WriteString("# clone_root = \"~/Projects\"   # where the clone is found, or created; reviews run in a worktree next to it\n")
	return b.String()
}

// initValidate loads the base (file, or the built-in defaults) with content
// as the user's config, from a scratch file, so nothing is written unless
// the result is valid.
func initValidate(c *Context, file, content string) error {
	dir, err := os.MkdirTemp("", "magnum-init-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	user := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(user, []byte(content), 0o600); err != nil {
		return err
	}
	if file != "" { // a full config with its legacy overlay next to it
		base, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if err := os.WriteFile(user, base, 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "config.local.toml"), []byte(content), 0o600); err != nil {
			return err
		}
		_, err = config.Load(paths.Layout{Home: c.Layout.Home}, user)
		return err
	}
	_, err = config.Load(paths.Layout{Home: c.Layout.Home, UserConfig: user}, "")
	return err
}

// initWrite writes content to path (0600) through a temporary file; an
// existing file is kept as path.bak first.
func initWrite(path, content string) error {
	if old, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", old, 0o600); err != nil {
			return fmt.Errorf("keep the old %s: %w", filepath.Base(path), err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// initReport says what was written and what to do next.
func initReport(w io.Writer, path string, a initAnswers, cfg *config.Config) {
	repo := a.owner + "/" + a.name
	fmt.Fprintf(w, "\nwrote %s: %d identities, %d watch (%s), no pool; config is valid\n",
		inspTilde(path), len(cfg.Identities), len(cfg.Watches), repo)
	if a.app != nil {
		fmt.Fprintf(w, "\nSave the App's private key (the .pem GitHub gave you) as %s; magnum never asks for it:\n\n", a.app.keyFile)
		fmt.Fprintf(w, "  mkdir -p %s && mv <downloaded>.pem %s && chmod 600 %s\n\n", filepath.Dir(a.app.keyFile), a.app.keyFile, a.app.keyFile)
		fmt.Fprintln(w, "`magnum identities check` verifies the App.")
	}
	bin := "bin/magnum"
	steps := [][2]string{
		{bin + " doctor", "what is missing, with the fix"},
		{bin + " daemon", "in a second terminal: the daemon in the foreground"},
		{bin + " review " + repo + "#<N> --wait", "one review, start to finish"},
		{"make install", "then the daemon under launchd and the herdr plugin"},
	}
	width := 0
	for _, st := range steps {
		width = max(width, len(st[0]))
	}
	fmt.Fprintln(w, "\nNext:")
	for _, st := range steps {
		fmt.Fprintf(w, "  %-*s   # %s\n", width, st[0], st[1])
	}
}
