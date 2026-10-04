package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
)

func doctorClones(ctx context.Context, d doctorDeps) []doctorCheck {
	var out []doctorCheck
	for _, p := range d.Config.Pools {
		name := "clone " + p.Repo
		if _, err := os.Stat(filepath.Join(p.MainClone, ".git")); err != nil {
			dest := inspTilde(p.MainClone)
			out = append(out, doctorFailed(name, "main clone of "+p.Repo+" missing at "+dest,
				"git clone https://github.com/"+p.Repo+".git "+dest+" (or `gh repo clone "+p.Repo+" "+dest+"`)"))
			continue
		}
		out = append(out, doctorOK(name, "main clone of "+p.Repo+" at "+inspTilde(p.MainClone)))
		base := p.Base
		if base == "" {
			base = "master"
		}
		check := "origin " + p.Repo
		env := map[string]string{"GIT_TERMINAL_PROMPT": "0"}
		url, _ := doctorExecGit(ctx, d, 10*time.Second, env, "-C", p.MainClone, "config", "--get", "remote.origin.url")
		// Probe the way the daemon fetches (gitx HTTPSFetch): a github.com SSH
		// origin over HTTPS through gh, so the SSH agent does not matter.
		opts, target := gitx.NetworkRemote(url, true)
		how := "origin"
		if target != "origin" {
			how = "HTTPS through gh"
		}
		args := append(append([]string{"-C", p.MainClone}, opts...), "ls-remote", "--heads", target, base)
		got, err := doctorExecGit(ctx, d, 10*time.Second, env, args...)
		switch {
		case err != nil:
			fix := "check the network and `gh auth status` (magnum fetches github.com over HTTPS with gh's credentials)"
			if how == "origin" && gitx.IsSSHURL(url) {
				fix = "check `ssh -T git@github.com` and that ssh-agent holds your key (`ssh-add -l`)"
			}
			out = append(out, doctorFailed(check, "`git ls-remote --heads` ("+how+") for "+base+" failed: "+err.Error(), fix))
		case strings.TrimSpace(got) == "":
			out = append(out, doctorWarned(check, "origin has no branch "+base, "fix [[pool]] base in config.toml"))
		default:
			out = append(out, doctorOK(check, fmt.Sprintf("origin reachable (%s): %s at %s", how, base, sha7(strings.Fields(got)[0]))))
		}
	}
	seen := map[string]bool{}
	for _, w := range d.Config.Watches {
		root := w.CloneRoot
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true
		if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
			out = append(out, doctorWarned("clone root "+w.Owner, "clone_root "+inspTilde(root)+" of watch "+w.Owner+" does not exist",
				"mkdir -p "+inspTilde(root)+" (per-PR repos are cloned there on demand)"))
		}
	}
	return out
}

func doctorMise(ctx context.Context, d doctorDeps) []doctorCheck {
	const name = "mise exec"
	if d.Store == nil {
		return nil
	}
	if len(d.Config.Pools) == 0 {
		return []doctorCheck{doctorSkipped(name, "no [[pool]]: nothing runs through `mise exec`")}
	}
	sls, err := d.Store.ListSlots(ctx, store.SlotFilter{Kind: store.SlotKindPool})
	if err != nil {
		return []doctorCheck{doctorFailed(name, "registry: "+err.Error(), "")}
	}
	for _, sl := range sls {
		switch sl.State {
		case store.SlotRemoved, store.SlotLost, store.SlotProvisioning, store.SlotRemoving, store.SlotBroken:
			continue
		}
		if _, err := os.Stat(sl.Path); err != nil {
			continue
		}
		v, err := doctorExec(ctx, d, 90*time.Second, "", nil, "mise", "-C", sl.Path, "exec", "--", "ruby", "-v")
		if err != nil {
			return []doctorCheck{doctorFailed(name, "`mise -C "+inspTilde(sl.Path)+" exec -- ruby -v` failed: "+err.Error(),
				"run `mise trust` and `mise install` in "+inspTilde(sl.Path)+", then `magnum slots repair "+sl.Name+"` if it persists")}
		}
		return []doctorCheck{doctorOK(name, fmt.Sprintf("mise exec works in %s (%s)", sl.Name, inspFirstLine(v)))}
	}
	return []doctorCheck{doctorWarned(name, "no provisioned slot to test `mise exec` in", "magnum slots provision")}
}
