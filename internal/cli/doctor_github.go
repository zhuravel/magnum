package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

func doctorGH(ctx context.Context, d doctorDeps) []doctorCheck {
	login, err := doctorExec(ctx, d, 20*time.Second, "", map[string]string{"GH_PROMPT_DISABLED": "1", "GH_NO_UPDATE_NOTIFIER": "1"},
		"gh", "api", "user", "--jq", ".login")
	if err != nil {
		return []doctorCheck{doctorFailed("gh", "gh cannot read the GitHub API: "+err.Error(), "run `gh auth login --hostname github.com` as your user")}
	}
	login = strings.TrimSpace(login)
	for _, id := range d.Config.Identities {
		if id.Kind == "gh" && !strings.EqualFold(id.Login, login) {
			return []doctorCheck{doctorFailed("gh", fmt.Sprintf("gh is logged in as %s but identity %s expects %s", login, id.Name, id.Login),
				"run `gh auth switch --user "+id.Login+"` (or `gh auth login`)")}
		}
	}
	return []doctorCheck{doctorOK("gh", "gh is logged in as "+login)}
}

func doctorIdentities(ctx context.Context, d doctorDeps) []doctorCheck {
	var out []doctorCheck
	kv := func(key string) string {
		if d.Store == nil {
			return ""
		}
		v, _, _ := d.Store.GetKV(ctx, key)
		return v
	}
	for _, id := range d.Config.Identities {
		name := "identity " + id.Name
		head := fmt.Sprintf("identity %s (%s, %s)", id.Name, id.Kind, id.Login)
		if id.Kind == "app" && id.PrivateKeyEnv != "" && d.Getenv != nil && d.Getenv(id.PrivateKeyEnv) == "" {
			key := "~/.config/magnum/keys/" + id.Name + ".pem"
			if dir := d.Layout.ConfigDir(); dir != "" {
				key = inspTilde(filepath.Join(dir, "keys", id.Name+".pem"))
			}
			out = append(out, doctorWarned(name, head+": "+id.PrivateKeyEnv+" is not set in this environment",
				"save the App PEM as "+key+" (chmod 600) and set private_key_file = \""+key+"\" on the identity; then `magnum identities check --name "+id.Name+"`"))
			continue
		}
		switch kv(store.KVIdentityCheck(id.Name)) {
		case "pass":
			detail := head + ": last check passed"
			if exp := kv(store.KVIdentityTokenExpiry(id.Name)); exp != "" {
				if t, err := store.ParseTime(exp); err == nil {
					detail += ", token valid until " + t.Local().Format("15:04")
				}
			}
			out = append(out, doctorOK(name, detail))
		case "fail":
			out = append(out, doctorFailed(name, head+": last check failed: "+kv(store.KVIdentityError(id.Name)),
				"fix it, then `magnum identities check --name "+id.Name+"`"))
		default:
			out = append(out, doctorWarned(name, head+": never checked", "magnum identities check --name "+id.Name))
		}
	}
	return out
}
