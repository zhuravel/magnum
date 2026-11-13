package cli

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// doctorRedisTimeout bounds one read of a Redis's databases.
var doctorRedisTimeout = 5 * time.Second

// doctorRedis checks, for each [[factory.repo]] whose copies use Redis
// offsets (config.FactoryRepo.UsesRedis), that the Redis its copies' env
// names has the databases they need (the highest copy's offset plus 16),
// read through the runner with `CONFIG GET databases`
// (factory.RedisDatabasesProbe). The highest copy is the configuration's
// (max_copies + fix_copies) or one the factory made past it, a pool slot of
// its agents store (redisCopies): a lower max_copies or fix_copies retires
// no copy. Fewer fail: the claims of the copies past them cannot flush
// theirs. A Redis that does not answer, or whose answer has no number, is
// unknown, a warning; so is an env that names no Redis, and a PASS while
// the agents store cannot be read. A redis-cli that is not on doctor's PATH
// is named as such, against the factory daemon's PATH (redisCLIMissing). No
// repository on Redis is a skip that runs nothing.
func doctorRedis(ctx context.Context, d doctorDeps) []doctorCheck {
	made, unread := factoryCopiesMade(ctx, d.Layout.FactoryAgentsDB())
	var out []doctorCheck
	for _, r := range d.Config.Factory.Repos {
		if r.UsesRedis() {
			made := factory.LastCopyMade(r, made[strings.ToLower(r.Repo)])
			out = append(out, doctorRedisRepo(ctx, d, r, redisCopies{last: max(r.LastCopy(), made), made: made, unread: unread}))
		}
	}
	if len(out) == 0 {
		return []doctorCheck{doctorSkipped("redis", "no [[factory.repo]] uses Redis offsets (REDIS_DB_OFFSET or {n*16} in its env)")}
	}
	return out
}

// redisCopies is how far a repository's copies reach for its Redis check:
// last, its highest copy, the configuration's or one made, made, the
// highest copy the factory made (0 for none), and why the copies it made
// could not be read (unread; nil when they were, or the factory has no
// agents store).
type redisCopies struct {
	last, made int
	unread     error
}

// need is how many databases the copies up to last need: the highest
// copy's offset plus RedisDBsPerCopy.
func (c redisCopies) need() int { return (c.last + 1) * config.RedisDBsPerCopy }

// doctorRedisRepo is doctorRedis for repository r, whose copies reach
// copies.last.
func doctorRedisRepo(ctx context.Context, d doctorDeps, r config.FactoryRepo, copies redisCopies) doctorCheck {
	name, needs := "redis "+r.Repo, redisNeed(r, copies.last)
	cmd, addr, err := factory.RedisDatabasesProbe(r, doctorRedisTimeout)
	if err != nil {
		return doctorWarned(name, needs+", and doctor cannot tell how many its Redis has: "+err.Error(),
			"set REDIS_URL (or REDIS_HOST and REDIS_PORT) in the env of "+r.Repo+"'s [[factory.repo]]")
	}
	out, err := doctorCmd(ctx, d, cmd)
	if errors.Is(err, exec.ErrNotFound) {
		return redisCLIMissing(ctx, d, name, needs, addr, r)
	}
	if err != nil {
		return doctorWarned(name, fmt.Sprintf("%s; how many the Redis at %s has is unknown, since it does not answer: %s", needs, addr, err.Error()),
			"start the Redis at "+addr+", then run `magnum doctor` again")
	}
	have, ok := redisDatabasesOf(out)
	switch {
	case !ok:
		return doctorWarned(name, fmt.Sprintf("%s; how many the Redis at %s has is unknown: CONFIG GET databases answered %q", needs, addr,
			textx.Clip(textx.FirstLine(out), 80)), "let the Redis at "+addr+" answer CONFIG GET databases (a renamed CONFIG or an ACL hides it)")
	case have < copies.need():
		return redisTooFew(name, needs, addr, r, copies, have)
	case copies.unread != nil:
		return doctorWarned(name, fmt.Sprintf("%s, and the Redis at %s has %d; which copies the factory made, past max_copies + fix_copies, is unknown: %s",
			needs, addr, have, copies.unread.Error()), "let doctor read "+inspTilde(d.Layout.FactoryAgentsDB())+", then run `magnum doctor` again")
	}
	return doctorOK(name, fmt.Sprintf("%s, and the Redis at %s has %d", needs, addr, have))
}

// redisCLIMissing is repository r's check when redis-cli is not on
// doctor's own PATH, which is no Redis that does not answer: how many
// databases the Redis at addr has is unknown. Where the factory daemon's
// PATH finds it (doctorDaemonLookPath), its claims flush: a WARN that names
// doctor's PATH. Where doctor cannot tell (the login shell did not answer),
// a WARN. Where it does not, a WARN, and a FAIL where the copies' env names
// REDIS_DB_OFFSET, since each claim of a copy flushes its Redis databases
// with redis-cli too, and those claims fail.
func redisCLIMissing(ctx context.Context, d doctorDeps, name, needs, addr string, r config.FactoryRepo) doctorCheck {
	fix := "install redis-cli (it comes with Redis) where the factory daemon's PATH finds it, then run `magnum doctor` again"
	switch at, known := doctorDaemonLookPath(ctx, d, "redis-cli"); {
	case at != "":
		return doctorWarned(name, fmt.Sprintf("%s; redis-cli is not on doctor's PATH, so how many the Redis at %s has is unknown, "+
			"while the factory daemon's PATH finds it at %s, so its claims flush", needs, addr, inspTilde(at)),
			"run `magnum doctor` from a shell whose PATH finds redis-cli, as the factory daemon's does")
	case !known:
		return doctorWarned(name, fmt.Sprintf("%s; redis-cli is not on doctor's PATH, so how many the Redis at %s has is unknown, "+
			"and whether the factory daemon's PATH finds it is unknown: the login shell, whose PATH [daemon] shell_path adds to it, "+
			"did not answer", needs, addr), fix)
	}
	detail := fmt.Sprintf("%s; redis-cli is not on PATH, neither doctor's nor the factory daemon's, so how many the Redis at %s has is unknown",
		needs, addr)
	if _, flushes := r.Env[config.RedisOffsetEnv]; flushes {
		return doctorFailed(name, detail+fmt.Sprintf(", and each claim of %s's copies flushes their Redis databases with it: those claims fail", r.Repo), fix)
	}
	return doctorWarned(name, detail, fix)
}

// doctorDaemonLookPath is where the PATH the daemons run with finds
// executable file ("" for nowhere): launchd's, the LaunchAgent's
// (launchdPATH), then, with [daemon] shell_path on, the directories of the
// login shell's PATH it lacks, which the daemon's start appends
// (daemonShellPATH); doctor asks the shell as the daemon does. known is
// false when the shell did not answer and launchd's PATH lacks file.
func doctorDaemonLookPath(ctx context.Context, d doctorDeps, file string) (at string, known bool) {
	isExec := d.Executable
	if isExec == nil {
		isExec = isExecutableFile
	}
	lookIn := func(path string) string {
		for dir := range strings.SplitSeq(path, ":") {
			if p := filepath.Join(dir, file); filepath.IsAbs(dir) && isExec(p) {
				return p
			}
		}
		return ""
	}
	own := launchdPATH(d.UserHome)
	if at := lookIn(own); at != "" || d.Config == nil || !d.Config.Daemon.ShellPath {
		return at, true
	}
	shell := "zsh"
	if d.Getenv != nil {
		shell = cmp.Or(d.Getenv("SHELL"), shell)
	}
	out, err := doctorCmd(ctx, d, execx.Cmd{Name: shell, Args: []string{"-ilc", shellPATHScript}, Timeout: shellPATHTimeout, NoTTY: true,
		Label: "doctor"})
	line, ok := lastPATHLine(out)
	if err != nil || !ok {
		return "", false
	}
	_, added := mergePATH(own, line)
	return lookIn(strings.Join(added, ":")), true
}

// isExecutableFile reports whether p is a regular file someone may run.
func isExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// redisTooFew is the FAIL of repository r's Redis at addr, which has have
// databases, fewer than its copies need: it names the copies whose claims
// cannot flush theirs, first to copies.last, and the fix. Lowering
// max_copies or fix_copies is a fix only while none of those copies exists
// (copies.made below first): no code retires a copy the factory made.
func redisTooFew(name, needs, addr string, r config.FactoryRepo, copies redisCopies, have int) doctorCheck {
	first := max(have/config.RedisDBsPerCopy, 1)
	fix := fmt.Sprintf("set `databases %d` (or more) in the redis.conf of the Redis at %s and restart it (CONFIG SET cannot change it)",
		copies.need(), addr)
	if copies.made >= first {
		verb := " exist"
		if copies.made == first {
			verb = " exists"
		}
		fix += fmt.Sprintf(": %s%s already and stay, whatever max_copies and fix_copies say", copiesRange(first, copies.made), verb)
	} else {
		fix += fmt.Sprintf(", or lower %s's max_copies or fix_copies", r.Repo)
	}
	return doctorFailed(name, fmt.Sprintf("%s, and the Redis at %s has %d: the claims of %s cannot flush theirs", needs, addr, have,
		copiesRange(first, copies.last)), fix)
}

// copiesRange names copies first to last: "copy 5", "copies 4 to 5".
func copiesRange(first, last int) string {
	if first == last {
		return fmt.Sprintf("copy %d", first)
	}
	return fmt.Sprintf("copies %d to %d", first, last)
}

// redisNeed says how many Redis databases repository r's copies up to copy
// last need, and why: "talkable/talkable's copies need 96 Redis databases
// (5 copies, 16 each from database 16)", with "; copies 4 to 5 exist past
// max_copies + fix_copies (3)" in the parentheses when the factory made
// copies past them.
func redisNeed(r config.FactoryRepo, last int) string {
	past := ""
	if r.LastCopy() < last {
		verb := " exist"
		if r.LastCopy()+1 == last {
			verb = " exists"
		}
		past = fmt.Sprintf("; %s%s past max_copies + fix_copies (%d)", copiesRange(r.LastCopy()+1, last), verb, r.LastCopy())
	}
	return fmt.Sprintf("%s's copies need %d Redis databases (%s, %d each from database %d%s)", r.Repo, redisCopies{last: last}.need(),
		textx.Count(last, "copy", "copies"), config.RedisDBsPerCopy, config.RedisDBsPerCopy, past)
}

// factoryCopiesMade lists the names of the pool slots in the factory's
// agents store at path by repository (owner/name, lower case: the store
// matches repo_full_name without case, so the factory claims a copy made
// under another case of the key's name): the copies its claims made,
// read-only. None when the store does not exist.
func factoryCopiesMade(ctx context.Context, path string) (map[string][]string, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", registryReadOnlyDSN(path))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, completeTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT repo_full_name, name FROM slots WHERE kind = ?`, store.SlotKindPool)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var repo, name string
		if err := rows.Scan(&repo, &name); err != nil {
			return nil, err
		}
		key := strings.ToLower(repo)
		out[key] = append(out[key], name)
	}
	return out, rows.Err()
}

// redisDatabasesOf is the number of databases a Redis's answer to CONFIG
// GET databases names: "databases" and the number, one per line.
func redisDatabasesOf(out string) (int, bool) {
	f := strings.Fields(out)
	if len(f) != 2 || f[0] != "databases" {
		return 0, false
	}
	n, err := strconv.Atoi(f[1])
	return n, err == nil && n > 0
}
