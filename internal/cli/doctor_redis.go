package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/textx"
)

// doctorRedisTimeout bounds one read of a Redis's databases.
var doctorRedisTimeout = 5 * time.Second

// doctorRedis checks, for each [[factory.repo]] whose copies use Redis
// offsets (config.FactoryRepo.UsesRedis), that the Redis its copies' env
// names has the databases they need (RedisDatabases: the highest copy's
// offset plus 16), read through the runner with `CONFIG GET databases`
// (factory.RedisDatabasesProbe). Fewer fail: the claims of the copies past
// them cannot flush theirs. A Redis that does not answer, or whose answer
// has no number, is unknown, a warning; so is an env that names no Redis.
// No repository on Redis is a skip that runs nothing.
func doctorRedis(ctx context.Context, d doctorDeps) []doctorCheck {
	var out []doctorCheck
	for _, r := range d.Config.Factory.Repos {
		if need := r.RedisDatabases(); need > 0 {
			out = append(out, doctorRedisRepo(ctx, d, r, need))
		}
	}
	if len(out) == 0 {
		return []doctorCheck{doctorSkipped("redis", "no [[factory.repo]] uses Redis offsets (REDIS_DB_OFFSET or {n*16} in its env)")}
	}
	return out
}

// doctorRedisRepo is doctorRedis for repository r, whose copies need need
// databases.
func doctorRedisRepo(ctx context.Context, d doctorDeps, r config.FactoryRepo, need int) doctorCheck {
	name, needs := "redis "+r.Repo, redisNeed(r)
	cmd, addr, err := factory.RedisDatabasesProbe(r, doctorRedisTimeout)
	if err != nil {
		return doctorWarned(name, needs+", and doctor cannot tell how many its Redis has: "+err.Error(),
			"set REDIS_URL (or REDIS_HOST and REDIS_PORT) in the env of "+r.Repo+"'s [[factory.repo]]")
	}
	out, err := doctorCmd(ctx, d, cmd)
	if err != nil {
		return doctorWarned(name, fmt.Sprintf("%s; how many the Redis at %s has is unknown, since it does not answer: %s", needs, addr, err.Error()),
			"start the Redis at "+addr+", then run `magnum doctor` again")
	}
	have, ok := redisDatabasesOf(out)
	switch {
	case !ok:
		return doctorWarned(name, fmt.Sprintf("%s; how many the Redis at %s has is unknown: CONFIG GET databases answered %q", needs, addr,
			textx.Clip(textx.FirstLine(out), 80)), "let the Redis at "+addr+" answer CONFIG GET databases (a renamed CONFIG or an ACL hides it)")
	case have < need:
		first := max(have/config.RedisDBsPerCopy, 1)
		past := fmt.Sprintf("copies %d to %d", first, r.LastCopy())
		if first == r.LastCopy() {
			past = fmt.Sprintf("copy %d", first)
		}
		return doctorFailed(name, fmt.Sprintf("%s, and the Redis at %s has %d: the claims of %s cannot flush theirs", needs, addr, have, past),
			fmt.Sprintf("set `databases %d` (or more) in the redis.conf of the Redis at %s and restart it (CONFIG SET cannot change it), "+
				"or lower %s's max_copies or fix_copies", need, addr, r.Repo))
	}
	return doctorOK(name, fmt.Sprintf("%s, and the Redis at %s has %d", needs, addr, have))
}

// redisNeed says how many Redis databases repository r's copies need, and
// why: "talkable/talkable's copies need 96 Redis databases (5 copies, 16
// each from database 16)".
func redisNeed(r config.FactoryRepo) string {
	return fmt.Sprintf("%s's copies need %d Redis databases (%s, %d each from database %d)", r.Repo, r.RedisDatabases(),
		textx.Count(r.LastCopy(), "copy", "copies"), config.RedisDBsPerCopy, config.RedisDBsPerCopy)
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
