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
// is named as such, against the factory daemon's PATH (redisCLIMissing), and
// the Redis is read with the redis-cli that PATH finds. No repository on
// Redis is a skip that runs nothing.
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

// redisCheck is one repository's Redis check: what it says first (name and
// needs), the Redis it reads (addr), the repository, how far its copies
// reach, and the probe that reads the Redis's databases.
type redisCheck struct {
	name, needs, addr string
	repo              config.FactoryRepo
	copies            redisCopies
	probe             execx.Cmd
}

// doctorRedisRepo is doctorRedis for repository r, whose copies reach
// copies.last.
func doctorRedisRepo(ctx context.Context, d doctorDeps, r config.FactoryRepo, copies redisCopies) doctorCheck {
	name, needs := "redis "+r.Repo, redisNeed(r, copies.last)
	cmd, addr, err := factory.RedisDatabasesProbe(r, doctorRedisTimeout)
	if err != nil {
		return doctorWarned(name, needs+", and doctor cannot tell how many its Redis has: "+err.Error(),
			"set REDIS_URL (or REDIS_HOST and REDIS_PORT) in the env of "+r.Repo+"'s [[factory.repo]]")
	}
	rc := redisCheck{name: name, needs: needs, addr: addr, repo: r, copies: copies, probe: cmd}
	out, err := doctorCmd(ctx, d, cmd)
	if errors.Is(err, exec.ErrNotFound) {
		return redisCLIMissing(ctx, d, rc)
	}
	return rc.verdict(d, out, err)
}

// verdict is the check once the probe ran: out and err are what
// `CONFIG GET databases` answered. A Redis that does not answer, or whose
// answer has no number, is unknown, a WARN; fewer databases than the copies
// need a FAIL; the copies made unread, a WARN; otherwise a PASS.
func (rc redisCheck) verdict(d doctorDeps, out string, err error) doctorCheck {
	if err != nil {
		return doctorWarned(rc.name, fmt.Sprintf("%s; how many the Redis at %s has is unknown, since it does not answer: %s", rc.needs, rc.addr, err.Error()),
			"start the Redis at "+rc.addr+", then run `magnum doctor` again")
	}
	have, ok := redisDatabasesOf(out)
	switch {
	case !ok:
		return doctorWarned(rc.name, fmt.Sprintf("%s; how many the Redis at %s has is unknown: CONFIG GET databases answered %q", rc.needs, rc.addr,
			textx.Clip(textx.FirstLine(out), 80)), "let the Redis at "+rc.addr+" answer CONFIG GET databases (a renamed CONFIG or an ACL hides it)")
	case have < rc.copies.need():
		return redisTooFew(rc.name, rc.needs, rc.addr, rc.repo, rc.copies, have)
	case rc.copies.unread != nil:
		return doctorWarned(rc.name, fmt.Sprintf("%s, and the Redis at %s has %d; which copies the factory made, past max_copies + fix_copies, is unknown: %s",
			rc.needs, rc.addr, have, rc.copies.unread.Error()), "let doctor read "+inspTilde(d.Layout.FactoryAgentsDB())+", then run `magnum doctor` again")
	}
	return doctorOK(rc.name, fmt.Sprintf("%s, and the Redis at %s has %d", rc.needs, rc.addr, have))
}

// redisCLIMissing is the check when redis-cli is not on doctor's own PATH,
// which is no Redis that does not answer. Where the factory daemon's PATH
// finds it (doctorDaemonLookPath), doctor reads the Redis's databases with
// that redis-cli, as the claims' flush runs it: the usual verdict, with a
// note on doctor's PATH, and "so its claims flush" only where the count
// passes. Where doctor cannot tell (the login shell did not answer), a WARN.
// Where the daemon's PATH lacks it too, a WARN, and a FAIL where the copies'
// env names REDIS_DB_OFFSET, since each claim of a copy flushes its Redis
// databases with redis-cli too, and those claims fail. Each says, when the
// daemon recorded no PATH, that doctor rebuilt the daemon's from today's
// configuration.
func redisCLIMissing(ctx context.Context, d doctorDeps, rc redisCheck) doctorCheck {
	fix := "install redis-cli (it comes with Redis) where the factory daemon's PATH finds it, then run `magnum doctor` again"
	look := doctorDaemonLookPath(ctx, d, "redis-cli")
	switch {
	case look.at != "":
		rc.probe.Name = look.at
		out, err := doctorCmd(ctx, d, rc.probe)
		c := rc.verdict(d, out, err)
		c.Detail += fmt.Sprintf("; redis-cli is not on doctor's PATH, so doctor read the Redis with the one the factory daemon's PATH finds at %s",
			inspTilde(look.at))
		if c.Status == doctorPass {
			c.Detail += ", so its claims flush"
		}
		c.Detail += look.note()
		return c
	case !look.known:
		return doctorWarned(rc.name, fmt.Sprintf("%s; redis-cli is not on doctor's PATH, so how many the Redis at %s has is unknown, "+
			"and whether the factory daemon's PATH finds it is unknown: the login shell, whose PATH [daemon] shell_path adds to it, "+
			"did not answer%s", rc.needs, rc.addr, look.note()), fix)
	}
	detail := fmt.Sprintf("%s; redis-cli is not on PATH, neither doctor's nor the factory daemon's, so how many the Redis at %s has is unknown",
		rc.needs, rc.addr)
	if _, flushes := rc.repo.Env[config.RedisOffsetEnv]; flushes {
		return doctorFailed(rc.name, detail+fmt.Sprintf(", and each claim of %s's copies flushes their Redis databases with it: those claims fail%s",
			rc.repo.Repo, look.note()), fix)
	}
	return doctorWarned(rc.name, detail+look.note(), fix)
}

// daemonLook is where the PATH the factory daemon runs with finds a file
// (doctorDaemonLookPath).
type daemonLook struct {
	// at is the file's path there; "" for nowhere.
	at string
	// known is false when the shell did not answer and launchd's PATH lacks
	// the file: where the daemon's PATH finds it is then unknown.
	known bool
	// recorded is true when the PATH is the running daemon's own record
	// (factoryDaemonPATH), false when doctor rebuilt it.
	recorded bool
}

// note is what a check says when the PATH it weighed was rebuilt: the
// daemon recorded none, so it is today's configuration's, which a daemon
// started under another one does not share. "" for the daemon's own record.
func (l daemonLook) note() string {
	if l.recorded {
		return ""
	}
	return "; the factory daemon recorded no PATH (an older daemon, or none running), so doctor rebuilt its PATH from today's configuration"
}

// doctorDaemonLookPath is where the PATH the factory daemon runs with finds
// executable file: the PATH the running daemon recorded at its start
// (factoryDaemonPATH of its pid), else, with no such record (an older
// daemon, none running, an earlier daemon's), the PATH its start would build
// from today's configuration: launchd's, the LaunchAgent's (launchdPATH),
// then, with [daemon] shell_path on, the directories of the login shell's
// PATH it lacks, which the daemon's start appends (daemonShellPATH); doctor
// asks the shell as the daemon does.
func doctorDaemonLookPath(ctx context.Context, d doctorDeps, file string) daemonLook {
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
	if path, ok := doctorRecordedDaemonPATH(ctx, d); ok {
		return daemonLook{at: lookIn(path), known: true, recorded: true}
	}
	own := launchdPATH(d.UserHome)
	if at := lookIn(own); at != "" || d.Config == nil || !d.Config.Daemon.ShellPath {
		return daemonLook{at: at, known: true}
	}
	shell := "zsh"
	if d.Getenv != nil {
		shell = cmp.Or(d.Getenv("SHELL"), shell)
	}
	out, err := doctorCmd(ctx, d, execx.Cmd{Name: shell, Args: []string{"-ilc", shellPATHScript}, Timeout: shellPATHTimeout, NoTTY: true,
		Label: "doctor"})
	line, ok := lastPATHLine(out)
	if err != nil || !ok {
		return daemonLook{}
	}
	_, added := mergePATH(own, line)
	return daemonLook{at: lookIn(strings.Join(added, ":")), known: true}
}

// doctorRecordedDaemonPATH is the PATH the running factory daemon recorded:
// false when no daemon runs, or the record is another process's (an earlier
// daemon's, which stays in the store until the running one writes its own)
// or missing.
func doctorRecordedDaemonPATH(ctx context.Context, d doctorDeps) (string, bool) {
	if d.FactoryPID == nil {
		return "", false
	}
	pid, err := d.FactoryPID()
	if err != nil || pid <= 0 {
		return "", false
	}
	rec, ok := readFactoryDaemonPATH(ctx, d.Layout.FactoryAgentsDB())
	return rec.Path, ok && rec.PID == pid
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
