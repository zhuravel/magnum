#!/usr/bin/env bash
# evidence.sh [--factory-since <since>] <since> <run-dir>
#
# Writes what magnum did since <since> (an RFC 3339 UTC time such as
# 2026-10-04T00:00:00Z, or "3d" for three days ago) into <run-dir>. The factory's tables and factory-reports.txt read
# from --factory-since instead, the loop's second cursor (runs.md's "factory since"; default: <since>):
#   evidence.md   the registry's numbers and lists, read-only, and at its end how often each rule of ../rules.txt
#                 shows in the reports ("Rules applied")
#   reports.txt   the review report directories written since then
#   gitlog.txt    magnum's own commits since then
#   claude-review-origin.txt   each rejected finding claude-review alone raised, with whether it matches a
#                 finding of the reviewer's report, a bullet of its Rejected list, or neither, and the text it
#                 matched
#   factory-reports.txt   the report directories of the factory's local review rounds written since then
#                 (<tasks>/<id>/local-review/<round>/reviews/<owner>/<repo>/<number>/<sha>)
# With a factory registry, evidence.md ends with the factory's tasks, the PRs they published, their local reviews,
# their waits and failures, its holds and alerts, the time per stage, the waits on you by cause, the take rate of the
# recommended answers by role, what they asked of the operator, and the liaison's notes (each task folder's notes.md,
# scrubbed).
# It reads the registries with sqlite3 -readonly and never writes to them.
set -euo pipefail

usage="usage: evidence.sh [--factory-since <since>] <since> <run-dir>"
fsince=
args=()
while [[ $# -gt 0 ]]; do
	case $1 in
	--factory-since)
		[[ $# -ge 2 ]] || { echo "evidence.sh: --factory-since needs a since; $usage" >&2; exit 2; }
		fsince=$2
		shift 2 ;;
	-*) echo "evidence.sh: unknown flag $1; $usage" >&2; exit 2 ;;
	*) args+=("$1"); shift ;;
	esac
done
[[ ${#args[@]} -eq 2 ]] || { echo "evidence.sh: want <since> and <run-dir>; $usage" >&2; exit 2; }
since=${args[0]}
dir=${args[1]}
db=${MAGNUM_DB:-$HOME/.local/share/magnum/magnum.db}
reviews=${MAGNUM_REVIEWS:-$HOME/.local/share/magnum/reviews}
fdb=${MAGNUM_FACTORY_DB:-$HOME/.local/share/magnum/factory/factory.db}
ftasks=${MAGNUM_FACTORY_TASKS:-$HOME/.local/share/magnum/factory/tasks}
repo=$(cd "$(dirname "$0")/../../../.." && pwd)

# when prints a since as an RFC 3339 UTC time: "<n>d" is n days ago, and anything else but such a time is refused,
# since it goes into SQL and into touch -t as it is. The time must be a real one: date reads it and prints it back
# unchanged (a month 13 does not parse, and a February 31 comes back as March 3).
when() {
	local t=$1
	if [[ $t =~ ^([0-9]+)d$ ]]; then
		t=$(date -u -v-"${BASH_REMATCH[1]}"d +%Y-%m-%dT%H:%M:%SZ 2>/dev/null) || t=
	fi
	if [[ $t =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ &&
		$(date -j -u -f %Y-%m-%dT%H:%M:%SZ "$t" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null) == "$t" ]]; then
		echo "$t"
	else
		echo "evidence.sh: '$1' is not a since (an RFC 3339 UTC time such as 2026-10-04T00:00:00Z, or 3d); $usage" >&2
		return 2
	fi
}
since=$(when "$since") || exit 2
fsince=$(when "${fsince:-$since}") || exit 2
mkdir -p "$dir"
out=$dir/evidence.md

q() { sqlite3 -readonly -header -markdown "$db" "$1"; }

# miss_from says where a miss row came from: a declined or argued finding of magnum's own that the
# retro judged missing_context at repo scope is also stored as a miss (so the notes curation takes it)
# with the declines row's source_url. Those are magnum's own threads, not what a human reviewer
# raised, and the Misses section counts them apart.
miss_from="case when exists (select 1 from declines d where d.source_url = misses.source_url)
	then 'declined finding' else 'comment' end"

# miss_origin is where a miss came from and where it went wrong (migration 0031): origin factory for a comment on a PR
# the factory published that its local review round missed, with the retro's stage (plan, code or review), and
# posted 1 for a comment only magnum's posted round caught: a miss of the factory's local review, never of magnum's
# review, which the review skill never learns from. A registry from before the migration has none of the columns.
miss_origin="'-' as origin, '-' as stage, 0 as posted"
if [[ $(sqlite3 -readonly "$db" "select count(*) from pragma_table_info('misses') where name = 'posted_caught'") == 1 ]]; then
	miss_origin="coalesce(origin, '-') as origin, coalesce(stage, '-') as stage, posted_caught as posted"
fi

# origin says where each rejected finding that claude-review alone raised came from. A claude-review.md report
# ends with a "Rejected" list: candidates the reviewer dropped itself, with its reason, and the judge re-judges them like
# any claim (skills/magnum-review/SKILL.md), so the ledger holds each one as a rejected finding with the
# reviewer as its only source. In one window about two thirds of the rejected rows of claude-review alone were
# such bullets, and the findings tables then read as a reviewer far less precise than it is. Neither the ledger
# (codex-judge.json) nor the registry marks them, so the row's title is matched against the reviewer's report:
# the cosine of their word stems, a title against a bullet's claim (its words before the first colon) or
# against a finding's heading ("Finding 2", "3.", "P2:"); the judge words its titles freely:
#   self       the best match is a bullet of the Rejected list, at 0.2 or more (a tie goes to the bullet);
#   reported   the best match is a finding of the report, at 0.3 or more;
#   unmatched  neither (a bullet the judge reworded past recognition, or something of the judge's own);
#   no report  the claude-review.md of the row's report directory is gone.
# It is a heuristic, not a fact: claude-review-origin.txt lists each row with the text it matched, to check by
# eye. A posted finding is not classified (its title matches a bullet about the same code as often as not). The
# first column is findings.id; the classes go to the SQL below as a VALUES list.
origin=$(mktemp "$dir/.origin.XXXXXX")
python3 - "$db" "$since" "$dir/claude-review-origin.txt" >"$origin" <<'PY' || {
import json, os, re, subprocess, sys

db, since, audit = sys.argv[1:4]
SELF_MIN, REPORTED_MIN = 0.2, 0.3
STOP = set("""the and for with without that this these those from into than then but not does did can could would
should may might will has have had are was were been being its their there here when while which what who whom also
only still new now more most other any all each such via per use used uses using make makes made get gets got one
two rather instead already even just""".split())
ITEM = re.compile(r"^\s*(?:[-*]|\d+[.)])\s+")
FINDING = re.compile(r"^(?:findings?\b|\d+[.):]\s|[A-Z]?\d+[.)]\s|P[0-3]\b|issue\s+\d)", re.I)


def sql(q):
    r = subprocess.run(["sqlite3", "-readonly", "-json", db, q], capture_output=True, text=True, check=True)
    return json.loads(r.stdout or "[]")


def stem(w):
    for suf in ("ing", "ed", "es", "s"):
        if w.endswith(suf) and len(w) - len(suf) >= 4:
            return w[: -len(suf)]
    return w


def toks(s):
    words = re.sub(r"[^a-z0-9]+", " ", s.lower()).split()
    return {stem(w) for w in words if len(w) > 2 and not w.isdigit() and w not in STOP}


def cos(a, b):
    return len(a & b) / ((len(a) * len(b)) ** 0.5) if a and b else 0.0


def lead(text):
    m = re.search(r":\s", text[:260])
    return text[: m.start()] if m else text[:260]


def parse(text):
    """The bullets of the report's Rejected section, and its findings: a section whose heading names one
    (Finding 2, 3., P2:), with its paragraphs, down to a heading of its own level or higher."""
    bullets, chunks = [], []
    in_rej, rej_level, root, root_level, para, cur = False, 0, "", 0, [], None

    def flush_para():
        nonlocal para
        if para and root:
            chunks.append((root + " " + " ".join(para))[:900])
        para = []

    def flush_bullet():
        nonlocal cur
        if cur is not None:
            bullets.append(cur)
        cur = None

    for line in text.splitlines():
        h = re.match(r"^(#{1,6})\s+(.*)$", line)
        if h:
            flush_para()
            flush_bullet()
            level, title = len(h.group(1)), h.group(2)
            if re.match(r"rejected\b", title, re.I):
                in_rej, rej_level, root = True, level, ""
                continue
            if in_rej and level <= rej_level:
                in_rej = False
            if in_rej:
                continue
            if FINDING.match(title):
                root, root_level = title, level
                chunks.append(title)
            elif root and level > root_level:
                pass  # a subsection of the finding
            elif level >= 2 and re.match(r"findings?\b", title, re.I):
                root, root_level = title, level
            else:
                root, root_level = "", 0
        elif in_rej:
            if ITEM.match(line):
                flush_bullet()
                cur = ITEM.sub("", line).strip()
            elif cur is not None and line.strip():
                cur += " " + line.strip()
        elif not line.strip():
            flush_para()
        else:
            para.append(line.strip())
    flush_para()
    flush_bullet()
    return bullets, chunks


def best(t, base, items, view):
    top, text = 0.0, ""
    for x in items:
        score = view(t, x) + (0.1 if base and base in x else 0.0)
        if score > top:
            top, text = score, x
    return top, text


def clip(s, n):
    return " ".join((s or "").split())[:n]


rows = sql(f"""select f.id, f.run_id, f.finding_id, f.verdict, coalesce(f.severity, '-') as pri, f.path, f.title,
                      r.report_path
               from findings f join runs r on r.id = f.run_id
               where f.sources_json = '["claude-review"]' and f.verdict = 'rejected' and f.created_at >= '{since}'
               order by f.id""")
reports, values, lines = {}, [], []
for r in rows:
    path = os.path.join(os.path.dirname(r["report_path"] or ""), "claude-review.md")
    if path not in reports:
        try:
            with open(path, encoding="utf-8", errors="replace") as f:
                reports[path] = parse(f.read())
        except OSError:
            reports[path] = None
    cls, score, text = "no report", 0.0, ""
    if reports[path] is not None:
        bullets, chunks = reports[path]
        t, base = toks(r["title"] or ""), os.path.basename(r["path"] or "")
        sb, tb = best(t, base, bullets, lambda t, x: max(cos(t, toks(lead(x))), 0.8 * cos(t, toks(x))))
        sr, tr = best(t, base, chunks, lambda t, x: cos(t, toks(lead(x))) if len(x) < 200 else 0.8 * cos(t, toks(x[:400])))
        if sb >= SELF_MIN and sb >= sr:
            cls, score, text = "self", sb, tb
        elif sr >= REPORTED_MIN:
            cls, score, text = "reported", sr, tr
        else:
            cls, score, text = "unmatched", max(sb, sr), tb if sb >= sr else tr
    values.append("(%d,'%s')" % (r["id"], cls))
    lines.append("\t".join([str(r["id"]), cls, "%.2f" % score, r["run_id"], r["finding_id"], r["verdict"], r["pri"],
                            clip(r["path"], 80), clip(r["title"], 100), clip(text, 160)]))
with open(audit, "w") as f:
    f.write("id\tclass\tscore\trun\tfinding\tverdict\tpri\tpath\ttitle\tmatched\n")
    f.write("\n".join(lines) + ("\n" if lines else ""))
print(",".join(values))
PY
	echo "evidence.sh: could not tell where claude-review's findings came from; the self columns stay empty" >&2
	: >"$origin"
}
cls=$(<"$origin")
rm -f "$origin"
cls=${cls:-"(0,'')"}
# with_cls prefixes a query with the class (cls.c) of each rejected finding claude-review alone raised (cls.id =
# findings.id).
with_cls="with cls(id, c) as (values $cls)"

# test_gap_posted is a CTE of the window's posted findings with their repository, their run's kind and tg, 1 for
# the test-gap class: a missing or ineffective test, magnum's most repeated finding. A title states the wrong
# result, so the class is matched by title words (globs on the lowercase title padded with a space at each end),
# read before any "(in the fix for <other title>)" part, which names another finding. The words were checked
# against the registry's titles: a bare "test" or "spec" is left out, since most titles with it name A/B tests,
# test data or a test that fails, which is no gap. Older findings have no title, so the counts are a lower bound.
test_gap_posted="test_gap_words(w) as (values ('*regression*'), ('*coverage*'), ('*untested*'), ('* no example*'),
        ('*negative example*'), ('*without failing*'), ('*the new test*'), ('* no test *'), ('* no tests *'),
        ('*missing test*'), ('*behavior test*'), ('* lack* test *'), ('* lack* tests *'), ('*tests miss*'),
        ('*specs miss*'), ('*tests do not*'), ('*specs do not*'), ('*does not test*'), ('*tests allow*'),
        ('*specs accept*'), ('*passes the tests*'), ('*removing * passes *'), ('*specs green*'), ('*tests green*'),
        ('*suite green*'), ('*checks green*'), ('* stay* green*')),
    test_gap_posted as (select f.id, f.pr_id, f.run_id, r.kind, rp.owner || '/' || rp.name as repo,
            f.title is null as untitled,
            exists (select 1 from test_gap_words where ' ' || lower(case when instr(f.title, ' (in the fix for') > 0
                then substr(f.title, 1, instr(f.title, ' (in the fix for') - 1) else f.title end) || ' ' glob w) as tg
        from findings f join runs r on r.id = f.run_id join prs p on p.id = f.pr_id join repos rp on rp.id = p.repo_id
        where f.verdict = 'posted' and f.created_at >= '$since')"

{
	echo "# magnum since $since"
	echo
	echo "## Rounds by kind and outcome (judge runs)"
	q "select kind, coalesce(outcome, state) as outcome, count(*) as runs
	   from runs where role like '%judge%' and created_at >= '$since' group by 1, 2 order by 3 desc"
	echo
	echo "## Posted reviews"
	echo
	echo "A nudge run whose review id another run here carries has no row of its own."
	echo
	q "select substr(r.created_at, 1, 16) as at, rp.owner || '/' || rp.name || '#' || p.number as pr, r.round, r.kind,
	          r.review_event as event, json_extract(r.result_json, '\$.verdict') as verdict,
	          json_extract(r.result_json, '\$.findings') as findings, r.review_url as url
	   from runs r join prs p on p.id = r.pr_id join repos rp on rp.id = p.repo_id
	   where r.role like '%judge%' and r.outcome = 'posted' and r.created_at >= '$since'
	     and not (r.kind = 'nudge' and exists (select 1 from runs o
	           where o.review_id = r.review_id and o.id <> r.id and o.role like '%judge%' and o.outcome = 'posted'
	             and o.created_at >= '$since'))
	   order by r.created_at"
	echo
	echo "## Findings: verdict, reason and priority"
	echo
	echo "\`self\`: of those rejected rows, the ones claude-review alone raised that its report's own Rejected list"
	echo "holds (claude-review-origin.txt; a heuristic match, see evidence.sh). The reviewer dropped those itself and"
	echo "the judge only re-checked them: leave them out when you judge a reviewer's precision."
	echo
	q "$with_cls select f.verdict, coalesce(f.reason_code, '-') as reason, coalesce(f.severity, '-') as pri,
	          count(*) as n, coalesce(sum(f.verdict = 'rejected' and cls.c = 'self'), 0) as self
	   from findings f left join cls on cls.id = f.id
	   where f.created_at >= '$since' group by 1, 2, 3 order by 1, 4 desc"
	echo
	echo "## Posted findings by source"
	q "select sources_json as sources, coalesce(severity, '-') as pri, count(*) as n
	   from findings where verdict = 'posted' and created_at >= '$since' group by 1, 2 order by 3 desc"
	echo
	echo "## Recurring posted classes per repository"
	echo
	echo "The test-gap class (test_gap_posted in evidence.sh: title words such as regression, coverage, no example,"
	echo "without failing, specs green; a lower bound). \`first reviews\` is the class's share of the findings of initial"
	echo "rounds, the measure of a rule at the source (lenses.md, lens 9). \`rounds of test gaps only\` are the rounds whose"
	echo "posted findings all have a title and all are test gaps: a test written up front would have left them clean."
	echo
	q "with $test_gap_posted,
	     rounds as (select repo, run_id, pr_id, count(*) = sum(tg) and sum(untitled) = 0 as only_tg
	         from test_gap_posted group by 1, 2, 3)
	   select p.repo as repository, count(distinct p.pr_id) as \"PRs with findings\", count(*) as posted,
	          count(distinct case when p.tg then p.pr_id end) as \"test-gap PRs\", sum(p.tg) as \"test gaps\",
	          sum(p.tg and p.kind = 'initial') || '/' || sum(p.kind = 'initial') as \"first reviews: test gaps/posted\",
	          (select count(*) from rounds r where r.repo = p.repo and r.only_tg) as \"rounds of test gaps only\",
	          (select count(distinct r.pr_id) from rounds r where r.repo = p.repo and r.only_tg) as \"their PRs\"
	   from test_gap_posted p group by 1 order by 3 desc, 1"
	echo
	echo "How the authors answered the test gaps: the outcome of each finding's latest answer (declines.finding_row_id)."
	echo
	q "with $test_gap_posted,
	     answers as (select d.finding_row_id as id, d.outcome from declines d
	         where d.id = (select max(o.id) from declines o where o.finding_row_id = d.finding_row_id))
	   select p.repo as repository, coalesce(a.outcome, 'no answer') as answer, count(*) as \"test gaps\"
	   from test_gap_posted p left join answers a on a.id = p.id where p.tg group by 1, 2 order by 1, 3 desc, 2"
	echo
	echo "## Fixer trial"
	echo
	echo "The fixer fixes the same defect in the PR's own lines too and names each extra site with \", also at"
	echo "<path>:<line>\" in its Fixed-in reply (skills/magnum-fix/SKILL.md, section 5, a trial since 2026-10-26). For each"
	echo "fix run of the window that pushed: the \"Fixed in <sha7>\" replies to the commits its report lists under"
	echo "\`## Pushed\`, read from the review-threads.json of the PR's next posted round (a reply is cut at 600 characters,"
	echo "which may hide its \"also at\"), and that round's findings tagged \"(missed earlier)\" or \"(in the fix for\", with"
	echo "those on a path a posted finding of the round the fix answered named: a repeat where the fix worked. The table"
	echo "lists the runs with an \"also at\" reply or a tagged finding after them, and those with no posted round after them."
	echo
	python3 - "$db" "$since" <<'PY' || echo "(the fixer trial could not be read: see the error evidence.sh printed)"
import json, os, re, subprocess, sys

db, since = sys.argv[1:3]


def sql(q):
    res = subprocess.run(["sqlite3", "-readonly", "-json", db, q], capture_output=True, text=True, check=True)
    return json.loads(res.stdout or "[]")


def read(path):
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            return f.read()
    except (OSError, TypeError):
        return None


SHA = re.compile(r"([0-9a-f]{7,40})\s")
FIXED_IN = re.compile(r"\W*fixed in `?([0-9a-f]{7,40})\b", re.I)
ALSO_AT = re.compile(r"\balso at\b", re.I)
MISSED, FALLOUT = "(missed earlier)", "(in the fix for"


def pushed(report):
    """The commits a fix report lists under ## Pushed, by their first 7 characters."""
    out, on = set(), False
    for line in (report or "").splitlines():
        if line.startswith("## "):
            on = line.strip() == "## Pushed"
        elif on and (m := SHA.match(line)):
            out.add(m.group(1)[:7])
    return out


def replies(path, shas):
    """The Fixed-in replies to one of shas in a threads file and those of them naming "also at", or None unread."""
    try:
        threads = json.loads(read(path) or "")
    except ValueError:
        return None
    n = also = 0
    for t in threads if isinstance(threads, list) else threads.get("threads", []):
        for r in t.get("replies") or []:
            m = FIXED_IN.match(r.get("body") or "")
            if m and m.group(1)[:7] in shas:
                n += 1
                also += bool(ALSO_AT.search(r["body"]))
    return n, also


fixes = sql(f"""select r.id, r.pr_id, r.created_at, r.report_path, rp.owner || '/' || rp.name || '#' || p.number as pr
                from runs r join prs p on p.id = r.pr_id join repos rp on rp.id = p.repo_id
                where r.kind = 'fix' and r.created_at >= '{since.replace("'", "''")}' order by r.created_at""")
prs = ",".join(sorted({str(int(f["pr_id"])) for f in fixes})) or "0"
found = {}
for f in sql(f"select run_id, title, path from findings where verdict = 'posted' and pr_id in ({prs})"):
    found.setdefault(f["run_id"], []).append(f)
# A round's nudge or model-fallback run is posted too, with its first run's review id and none of the findings,
# which the round stores on its first run only: the runs of one review are one round, named by the earliest, with
# the findings of all of them.
judges, review = [], {}
for j in sql(f"""select id, pr_id, round, created_at, report_path, review_id from runs
                 where role like '%judge%' and outcome = 'posted' and pr_id in ({prs}) order by created_at, id"""):
    key = j["id"] if j["review_id"] is None else (j["pr_id"], j["review_id"])
    if key in review:
        review[key]["findings"] += found.get(j["id"], [])
        continue
    review[key] = j | {"findings": list(found.get(j["id"], []))}
    judges.append(review[key])

rows, rounds = [], {}
n_pushed = n_replies = n_also = unread = waiting = 0
for fx in fixes:
    shas = pushed(read(fx["report_path"]))
    if not shas:
        continue
    n_pushed += 1
    mine = [j for j in judges if j["pr_id"] == fx["pr_id"]]
    nxt = next((j for j in mine if j["created_at"] > fx["created_at"]), None)
    if nxt is None:
        waiting += 1
        rows.append([fx["pr"], fx["id"], fx["created_at"][:16], len(shas), "-", "-", "none yet", "-", "-", "-", "-"])
        continue
    prev = [j for j in mine if j["created_at"] < fx["created_at"]]
    answered = {f["path"] for f in prev[-1]["findings"]} if prev else set()
    new = nxt["findings"]
    tagged = [f for f in new if MISSED in (f["title"] or "") or FALLOUT in (f["title"] or "")]
    missed = sum(MISSED in f["title"] for f in tagged)
    fallout = sum(FALLOUT in f["title"] for f in tagged)
    same = sum(f["path"] in answered for f in tagged)
    rounds[nxt["id"]] = (len(new), missed, fallout, same)
    got = replies(os.path.join(os.path.dirname(nxt["report_path"] or ""), "review-threads.json"), shas)
    if got is None:
        unread += 1
        n = also = "-"
    else:
        n, also = got
        n_replies += n
        n_also += also
    if (got and also) or tagged:
        rows.append([fx["pr"], fx["id"], fx["created_at"][:16], len(shas), n, also, f"{nxt['id']} (r{nxt['round']})",
                     len(new), missed, fallout, same])

total = [sum(r[i] for r in rounds.values()) for i in range(4)]
note = f" ({unread} next rounds have no threads file)" if unread else ""
print(f'Fixer trial: {len(fixes)} fix runs, {n_pushed} pushed. Fixed-in replies to their commits: {n_replies}, '
      f'{n_also} naming "also at"{note}. The next posted rounds ({len(rounds)}): {total[0]} findings, {total[1]} '
      f'"{MISSED}" and {total[2]} "{FALLOUT}", {total[3]} of those on a path a finding of the answered round named. '
      f'Without a posted round after them: {waiting}.')
print()
if rows:
    print("| PR | fix run | at | commits | Fixed-in replies | also at | next round | findings | missed earlier | in the fix for | on an answered path |")
    print("|---|---|---|---|---|---|---|---|---|---|---|")
    for r in rows:
        print("| " + " | ".join(str(x) for x in r) + " |")
else:
    print('No fix run of the window has an "also at" reply or a tagged finding after it.')
PY
	echo
	echo "## claude-review alone: what its rejected findings are (priority)"
	echo
	echo "The findings whose only source is claude-review. A rejected one is \`reported\` when it matches a finding"
	echo "of the reviewer's report (the judge's rejection of something the reviewer reported), \`self\` when it"
	echo "matches a bullet of the report's Rejected list, \`unmatched\` when neither (a bullet reworded past"
	echo "recognition, or the judge's own), \`no report\` when the report is gone. The reviewer's precision lies"
	echo "between posted / (posted + reported + unmatched) and posted / (posted + reported)."
	echo
	q "$with_cls select coalesce(f.severity, '-') as pri,
	          sum(f.verdict = 'posted') as posted,
	          sum(f.verdict = 'rejected' and cls.c = 'reported') as \"rejected (reported)\",
	          sum(f.verdict = 'rejected' and cls.c = 'self') as \"rejected (self)\",
	          sum(f.verdict = 'rejected' and cls.c = 'unmatched') as \"rejected (unmatched)\",
	          sum(f.verdict = 'rejected' and cls.c = 'no report') as \"rejected (no report)\"
	   from findings f left join cls on cls.id = f.id
	   where f.sources_json = json_array('claude-review') and f.created_at >= '$since' group by 1 order by 1"
	echo
	echo "## Role minutes (submitted to ended)"
	q "select role, kind, count(*) as runs,
	          round(avg((julianday(ended_at) - julianday(submitted_at)) * 1440), 1) as avg_min,
	          round(max((julianday(ended_at) - julianday(submitted_at)) * 1440), 1) as max_min
	   from runs where submitted_at is not null and ended_at is not null and created_at >= '$since'
	   group by 1, 2 order by 1, 2"
	echo
	echo "## Misses (retro)"
	q "select class, coalesce(scope, '-') as scope, coalesce(severity, '-') as pri, raised, state, $miss_from as \"from\",
	          $miss_origin, count(*) as n
	   from misses where created_at >= '$since' group by 1, 2, 3, 4, 5, 6, 7, 8, 9 order by 10 desc"
	q "select substr(created_at, 1, 10) as at, severity as pri, scope, $miss_from as \"from\", $miss_origin, title, lesson
	   from misses where class = 'miss' and created_at >= '$since' order by created_at"
	echo
	echo "## Warn and error events by kind"
	q "select level, kind, count(*) as n, max(substr(at, 1, 16)) as last
	   from events where level in ('warn', 'error') and at >= '$since' group by 1, 2 order by 3 desc limit 40"
	echo
	echo "## Operator requests (what the operator did by hand)"
	q "select kind, state, count(*) as n from requests where created_at >= '$since' group by 1, 2 order by 3 desc"
	echo
	echo "## PRs that need attention or retry now"
	q "select rp.owner || '/' || rp.name || '#' || p.number as pr, p.state, p.attempts,
	          substr(coalesce(p.last_error, ''), 1, 160) as last_error
	   from prs p join repos rp on rp.id = p.repo_id
	   where p.gh_state = 'OPEN' and (p.state = 'needs_attention' or p.attempts > 0 or p.last_error is not null)
	   order by p.state, p.attempts desc"
} >"$out"

# Report directories touched since then, newest first. touch -t reads its time in the local zone, so it runs in UTC.
stamp=$(mktemp "$dir/.since.XXXXXX")
TZ=UTC touch -t "$(date -j -u -f %Y-%m-%dT%H:%M:%SZ "$since" +%Y%m%d%H%M.%S)" "$stamp"
find "$reviews" -mindepth 4 -maxdepth 4 -type d -newer "$stamp" -print 2>/dev/null |
	xargs -I{} stat -f '%m %N' {} | sort -rn | cut -d' ' -f2- >"$dir/reports.txt" || true
# The factory's local review rounds keep the same report directories, under each task's folder, since the factory's
# own since.
TZ=UTC touch -t "$(date -j -u -f %Y-%m-%dT%H:%M:%SZ "$fsince" +%Y%m%d%H%M.%S)" "$stamp"
find "$ftasks" -mindepth 8 -maxdepth 8 -type d -path '*/local-review/*/reviews/*' -newer "$stamp" -print 2>/dev/null |
	xargs -I{} stat -f '%m %N' {} | sort -rn | cut -d' ' -f2- >"$dir/factory-reports.txt" || true
rm -f "$stamp"

# Every reply to magnum's threads: the newest review-threads.json of each PR
# reviewed since then. The class is magnum's keyword guess only, made when the
# round wrote the file (an older round, an older classifier); people answer in
# free form (a score such as "Net: -3", a deferral, an argument), so the
# declined-findings lens reads each reply and decides its meaning itself. A
# thread says when magnum posted its finding (created_at) and in which review
# (review_id, joined to the run that posted it); "current skill" marks a
# finding whose run named the skill copy (<state>/skill/<hash>/SKILL.md) that
# the running daemon's rounds name: the newest judge run since the daemon
# loaded its prompts (kv daemon.prompts_loaded_at) tells which. Right after a
# restart no judge has run yet; then, when nothing changed on disk since the
# daemon loaded (kv daemon.prompts_changed absent), the hash is the first 12
# hex characters of the SHA-256 of $repo/skills/magnum-review/SKILL.md, the
# name the daemon gave its copy, and the summary says so. "net" is the score
# the author's last reply gives the fix ("Net +3", "Net: -2", "net 0", "Low
# priority (Net 0)"; a minus may be U+2212), as "+3", "-2", "0" or "+0.5",
# empty when the reply names none. "by" says who replied, in order: "fixer"
# for magnum's fixer, which posts as the PR's author and ends every reply
# with <!-- magnum:fix --> (skills/magnum-fix/SKILL.md), "own" for magnum's
# reviewer, "person" for anyone else, and "person?" for a reply the file cut
# at 600 characters without the marker in what it kept, which may still be
# the fixer's. A marked reply counts as an answer even when the file calls
# it own (a watch that posts as the fixer's login). A thread whose last
# reply is magnum's own with kind "rebuttal" reads "rebutted, unanswered" in
# both the class and the "last reply by" columns: the author never answered
# the rebuttal, and the reply shown (the last by anyone else) is the one it
# answered, which a class such as "fixed" would otherwise present as the
# thread's end.
python3 - "$dir/reports.txt" "$db" "$repo" >>"$out" <<'PY'
import hashlib, json, os, re, subprocess, sys, collections
latest = {}
for d in open(sys.argv[1]).read().split():
    pr = os.path.dirname(d)
    if pr not in latest and os.path.exists(os.path.join(d, "review-threads.json")):
        latest[pr] = os.path.join(d, "review-threads.json")

def sql(q):
    try:
        res = subprocess.run(["sqlite3", "-readonly", "-json", sys.argv[2], q], capture_output=True, text=True, check=True)
        return json.loads(res.stdout or "[]")
    except Exception:
        return []

def skill(fragment):
    m = re.match(r"([0-9a-f]{12})/SKILL\.md", fragment or "")
    return m.group(1) if m else None

NET = re.compile(r"\bnet\b:?\s*([-+−]?)\s*(\d+(?:\.\d+)?)(?!\w)", re.I)

def net(body):
    m = NET.search(body or "")
    if not m:
        return ""
    num = m.group(2)
    if float(num) == 0:
        return "0"
    return ("-" if m.group(1) in ("-", "−") else "+") + num

frag = "case when instr(prompt_text, '/skill/') > 0 then substr(prompt_text, instr(prompt_text, '/skill/') + 7, 21) end"
posted = {}  # review id -> [run id, round, kind, skill]: the first run that names the review
for r in sql(f"select id, review_id, round, kind, {frag} as skill from runs where review_id is not null order by id"):
    p = posted.setdefault(r["review_id"], [r["id"], r["round"], r["kind"], None])
    p[3] = p[3] or skill(r["skill"])
cur = sql(f"""select {frag} as skill from runs where role like '%judge%' and instr(prompt_text, '/skill/') > 0
              and created_at >= (select value from kv where key = 'daemon.prompts_loaded_at') order by id desc limit 1""")
current, from_file = (skill(cur[0]["skill"]) if cur else None), False
changed = sql("select count(*) as n from kv where key = 'daemon.prompts_changed'")
if not cur and changed and changed[0]["n"] == 0:
    try:
        with open(os.path.join(sys.argv[3], "skills", "magnum-review", "SKILL.md"), "rb") as f:
            current, from_file = hashlib.sha256(f.read()).hexdigest()[:12], True
    except OSError:
        pass

FIX = "<!-- magnum:fix -->"
REBUTTED = "rebutted, unanswered"

def who(r):
    body = r.get("body") or ""
    if FIX in body:
        return "fixer"
    if r.get("own"):
        return "own"
    return "person?" if r.get("truncated") else "person"

guess, by, rows, unanswered, under_current = collections.Counter(), collections.Counter(), [], 0, 0
for pr, f in sorted(latest.items()):
    try:
        threads = json.load(open(f))
    except Exception:
        continue
    for t in threads if isinstance(threads, list) else threads.get("threads", []):
        replies = t.get("replies") or []
        theirs = [r for r in replies if who(r) != "own"]
        own = len(replies) - len(theirs)
        if not theirs:
            unanswered += 1
            continue
        last = theirs[-1]
        rebutted = who(replies[-1]) == "own" and replies[-1].get("kind") == "rebuttal"
        cls = REBUTTED if rebutted else last.get("class") or "-"
        repliers = []
        for r in replies:
            if not repliers or repliers[-1] != who(r):
                repliers.append(who(r))
        guess[cls] += 1
        at = (t.get("created_at") or "-").replace("T", " ")[:16]
        run = posted.get(t.get("review_id"))
        where = f"run {run[0]}, r{run[1]} {run[2]}" if run else "-"
        if run and current and run[3] == current:
            at += ", current skill"
            under_current += 1
        text = " ".join((last.get("body") or "").split())[:400].replace("|", "/")
        by[REBUTTED if rebutted else who(last)] += 1
        rows.append(f"| {cls} | {net(last.get('body'))} | {len(theirs)}/{own} | {', '.join(repliers)} | {at} | {where} | {t.get('finding','')[:90]} | {t.get('location','')} | {t.get('url','')} | {text} |")
print("\n## Replies to magnum's threads (latest round per PR)\n")
skill_note = f", {under_current} of them posted under the current skill" if current else ""
if from_file:
    skill_note += f" ({current}, from SKILL.md: no judge ran since the restart)"
print(f"{len(rows)} answered threads{skill_note}, {unanswered} without a reply. The class is a keyword guess made at round time, not a verdict:")
print("read every reply below and decide what it says. A thread whose last reply is magnum's own rebuttal reads \"rebutted, unanswered\" in the")
print("class and \"last reply by\" columns: the author has not answered it, and the reply shown is the author's earlier one, which the rebuttal")
print("answered. \"(here)\" and \"follow-up\" in a reply mark a deferral (\"fixed (here) ... template follow-up\": part of the work is done and the")
print("rest waits for a later change), not a fix.\n")
print("| class at round time | threads |\n|---|---|")
for k, v in guess.most_common():
    print(f"| {k} | {v} |")
print("\n| last reply by | threads |\n|---|---|")
for k, v in by.most_common():
    print(f"| {k} | {v} |")
print("\n### Every answered thread\n")
print("| class at round time | net | replies theirs/own | by | posted (UTC) | round | finding | where | thread | last reply (first 400 chars) |")
print("|---|---|---|---|---|---|---|---|---|---|")
print("\n".join(rows) or "| - | - | - | - | - | - | - | - | - | - |")
PY

# How often each review rule shows in the reports: the phrase counts of rules.txt (one `<rule> | <pattern>` line
# per rule, in the skill directory beside SKILL.md; the file explains its patterns). For each rule, the report
# directories of reports.txt where the pattern matches the posted review (review.json: the body and the inline
# comments), a judge's result file (*-judge.json: every string in it) or claude-review.md, and the posted reviews
# (a review.json whose judge result says status "posted") whose own text matches. A rule that stays at 0 for
# three runs is a candidate for a cut when the skill is at its byte cap. The counts are phrase hits, not proof
# that the rule was applied or that it paid.
python3 - "$dir/reports.txt" "$(dirname "$0")/../rules.txt" >>"$out" <<'PY'
import glob, json, os, re, sys

def strings(v):
    if isinstance(v, str):
        yield v
    elif isinstance(v, dict):
        for x in v.values():
            yield from strings(x)
    elif isinstance(v, list):
        for x in v:
            yield from strings(x)

def load(path):
    try:
        with open(path, encoding="utf-8") as f:
            return json.load(f)
    except (OSError, ValueError):
        return None

def text(path):
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            return f.read()
    except OSError:
        return ""

rules, problems = [], []
try:
    with open(sys.argv[2], encoding="utf-8") as f:
        for n, line in enumerate(f, 1):
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            name, sep, pat = line.partition(" | ")
            if not sep or not name.strip() or not pat.strip():
                problems.append(f"rules.txt line {n} is not `<rule> | <pattern>`")
                continue
            try:
                rules.append([name.strip(), re.compile(pat.strip()), 0, 0])
            except re.error as e:
                problems.append(f"rules.txt line {n}: pattern does not compile ({e})")
except OSError:
    problems.append("rules.txt is missing")

dirs, posted = open(sys.argv[1]).read().split(), 0
for d in dirs:
    review = load(os.path.join(d, "review.json"))
    body = ""
    if isinstance(review, dict):
        body = "\n".join([review.get("body") or ""] + [c.get("body") or "" for c in review.get("comments") or [] if isinstance(c, dict)])
    judges = [load(p) for p in sorted(glob.glob(os.path.join(d, "*-judge.json")))]
    is_posted = isinstance(review, dict) and any(isinstance(j, dict) and j.get("status") == "posted" for j in judges)
    posted += is_posted
    other = text(os.path.join(d, "claude-review.md")) + "\n" + "\n".join("\n".join(strings(j)) for j in judges)
    for r in rules:
        hit = r[1].search(body)
        r[2] += bool(hit or r[1].search(other))
        r[3] += bool(hit and is_posted)

print("\n## Rules applied (rules.txt)\n")
print(f"{len(dirs)} report directories, {posted} posted reviews. A count is the reports whose posted review, judge result or claude-review.md")
print("holds the rule's phrase, and the posted reviews whose own text does: a phrase hit, not proof that the rule was applied.")
print("A rule at 0 for three runs in a row is a cut candidate when the skill is at its byte cap.\n")
for p in problems:
    print(f"- {p}")
if problems:
    print()
print("| rule | report dirs | posted reviews |\n|---|---|---|")
for name, _, d, p in rules:
    print(f"| {name} | {d} | {p} |")
if not rules:
    print("| - | - | - |")
PY

# The factory (magnum factory): its local review is a blind round before the PR exists, so magnum's retro never
# sees it. People's comments on a PR it published, after opened_at, are misses of that local review (lenses.md,
# lens 0), and its waits, failures, holds, stage times, questions and the liaison's notes are the factory lens's
# input (lens 13). Every factory table reads from the factory's own since (fsince, --factory-since).
if [[ -f $fdb ]]; then
	fq() { sqlite3 -readonly -header -markdown "$fdb" "$1"; }
	# magnum factory stats --since takes no time of day: its local date starts at or before the factory's since.
	fdate=$(date -r "$(date -j -u -f %Y-%m-%dT%H:%M:%SZ "$fsince" +%s)" +%Y-%m-%d)
	{
		echo
		echo "## Factory tasks (updated since $fsince)"
		echo
		echo "The factory's tables read from the factory's own since (--factory-since, runs.md's \"factory since\"), the"
		echo "tables above from $since. Lens 13's stats of the tasks started since then (--since takes a date and no time,"
		echo "so they may add the tasks started earlier that day): \`magnum factory stats --since $fdate --markdown\`"
		echo
		fq "select id, coalesce(jira_key, '-') as key, repo, kind, state, stage, coalesce(pr_number, '-') as pr,
		          substr(created_at, 1, 16) as created, substr(updated_at, 1, 16) as updated
		   from tasks where updated_at >= '$fsince' order by id"
		echo
		echo "## Factory PRs (people's comments after opened_at are misses of the local review)"
		fq "select l.task_id as task, l.repo || '#' || l.number as pr, substr(l.opened_at, 1, 16) as opened,
		          substr(l.opened_head, 1, 12) as head
		   from pr_links l where l.opened_at >= '$fsince' order by l.opened_at"
		echo
		echo "## Factory local review cycles"
		fq "select task_id as task, n, exit, substr(head_after, 1, 12) as head, tokens, substr(started_at, 1, 16) as started
		   from review_cycles where started_at >= '$fsince' order by task_id, n"
		fq "select f.task_id as task, coalesce(f.priority, '-') as pri, f.class, f.verdict, f.outcome, f.blocking, count(*) as n
		   from review_findings f join review_cycles c on c.task_id = f.task_id and c.n = f.first_cycle
		   where c.started_at >= '$fsince' group by 1, 2, 3, 4, 5, 6 order by 1, 7 desc"
		echo
		echo "## Factory waits and failures (events)"
		echo
		echo "The failures (phase fail), the holds (kind hold: begin when every stage or the launches hold, info for a try, ok"
		echo "when lifted) and the alerts with their delivery (kind alert, then alert_held, alert_dropped, alert_toast)."
		echo
		fq "select kind, phase, count(*) as n, max(substr(at, 1, 16)) as last
		   from events where at >= '$fsince' and (phase = 'fail' or kind = 'hold' or kind glob 'alert*')
		   group by 1, 2 order by 3 desc limit 30"
		echo
		echo "## Factory operator inputs (what it asked of you; magnum factory stats has the per-task numbers)"
		fq "select kind, surface, count(*) as n from operator_inputs where at >= '$fsince' group by 1, 2 order by 3 desc"
	} >>"$out"
	# The time per stage, the waits on you by cause, the holds' words, the take rate of the recommended answers and the
	# liaison's notes. A span of task_spans lasts until the next span of its task; the last one runs to now, or to the
	# task's last change when it is done or stopped (magnum factory stats measures its waits so), and every span is
	# clipped to the window.
	python3 - "$fdb" "$fsince" "$ftasks" >>"$out" <<'PY' ||
import errno, json, os, re, stat, subprocess, sys
from datetime import datetime, timezone

fdb, fsince, ftasks = sys.argv[1:4]
NOTES_LINES, NOTES_MAX = 80, 1 << 20


def sql(q):
    res = subprocess.run(["sqlite3", "-readonly", "-json", fdb, q], capture_output=True, text=True, check=True)
    return json.loads(res.stdout or "[]")


# The scrub the factory runs before it keeps text (internal/factory/runtime/scrub.go, after execx.Redact): GitHub
# tokens, JWTs, Authorization values and private keys; AWS, Slack, OpenAI, Anthropic, GitLab, Stripe and Google keys;
# the values of password=, token: and the like (the key stays); emails; host names and IPv4 addresses. Each pattern is
# the Go one word for word (Go's end of text, $, is \Z here), which selftest.sh checks.
REDACT = re.compile("|".join([
    r"(?:gho_|ghs_|ghp_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]+",
    r"eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}",
    r"(?i:authorization:\s*(?:bearer|token|basic)\s+)\S+",
    r"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\Z)",
]), re.ASCII)
TOKENS = re.compile("|".join([
    r"\b(?:AKIA|ASIA|AGPA|AIDA|AROA)[0-9A-Z]{16}\b",
    r"\bxox[abposr]-[A-Za-z0-9-]{10,}",
    r"\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}",
    r"\bglpat-[A-Za-z0-9_-]{16,}",
    r"\b[rs]k_(?:live|test)_[A-Za-z0-9]{16,}",
    r"\bAIza[0-9A-Za-z_-]{35}\b",
]), re.ASCII)
PAIRS = re.compile(r'''(?i)\b((?:aws_)?(?:secret(?:_access)?_key|password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret))(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)''', re.ASCII)
EMAIL = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}", re.ASCII)
HOST = re.compile(r"(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?:com|net|org|io|dev|app|ai|cloud|internal|local|lan|corp|intra|info|biz|tech|xyz|site|us|uk|de|ua|eu|me|cc|tv|gg)\b|\b(?:\d{1,3}\.){3}\d{1,3}\b", re.ASCII)
# REDACT, TOKENS and PAIRS run on the whole text, in the Go scrub's order, since a private key, an Authorization value
# and a key's value (on the next line, or quoted over lines) cross a line break; EMAIL and HOST, which cannot, run on
# each line kept. Python's engine, unlike Go's, backtracks for minutes on some long words (a dotted run, an eyJ
# repeated), so first a word (a run of characters without whitespace) longer than WORD_MAX becomes [...], and keeps
# the key it ends with, whose value is the next word. No line is cut: what the Go scrub masks across words is masked
# here (a cut before the masks parted keys from their values). The slowest text measured at the notes' 1 MiB cap,
# one line of 253-character dotted words, takes 5 s. And HOST runs on each run of the characters a host name or an address is made of: a match never leaves such a
# run, and the characters around a run are no word characters, so the matches are the same as on the whole line; a
# run longer than a host name may be (253 characters) is masked whole.
WORD_MAX, HOST_MAX = 2000, 253
WORD = re.compile(r"\S+")
# the key (and its separator) a long word ends with: the word's value is the next word, which must stay masked
KEYEND = re.compile(r"""(?i)(?<![A-Za-z0-9_])(?:authorization|(?:aws_)?(?:secret(?:_access)?_key|password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret))[=:]["']?$""", re.ASCII)
HOST_RUN = re.compile(r"[A-Za-z0-9_.-]+")


def redacted(m):
    s = m.group(0)
    i = s.find(":")
    return s[: i + 1] + " <redacted>" if i >= 0 and s.lower().startswith("authorization") else "<redacted>"


def host(m):
    run = m.group(0)
    if "." not in run:
        return run
    return "<host>" if len(run) > HOST_MAX else HOST.sub("<host>", run)


def capped(m):
    w = m.group(0)
    if len(w) <= WORD_MAX:
        return w
    k = KEYEND.search(w[-40:])
    return "[...]" + (k.group(0) if k else "")


def scrub_lines(text, keep=0):
    """The lines of text scrubbed, the last keep of them (0: all), and how many it left out."""
    text = WORD.sub(capped, text or "")
    text = PAIRS.sub(r"\1\2<redacted>", TOKENS.sub("<redacted>", REDACT.sub(redacted, text)))
    lines = text.splitlines()
    left = max(0, len(lines) - keep) if keep else 0
    return [HOST_RUN.sub(host, EMAIL.sub("<email>", line)) for line in lines[left:]], left


def scrub(s):
    return "\n".join(scrub_lines(s)[0])


def clip(s, n):
    return " ".join((s or "").split())[:n]


def table(head, rows, none):
    if not rows:
        print(none)
        return
    print("| " + " | ".join(head) + " |")
    print("|" + "---|" * len(head))
    for r in rows:
        print("| " + " | ".join(str(x).replace("|", "/") for x in r) + " |")


def mins(x):
    return "%.1f" % x


# The window in minutes (julianday * 1440), the registry's times read by SQLite whatever their fraction.
w = sql(f"select julianday('{fsince}') * 1440 as start, julianday('now') * 1440 as now")[0]
start, now = w["start"], w["now"]
tasks = {t["id"]: t for t in sql("select id, jira_key, repo, state, julianday(updated_at) * 1440 as updated from tasks")}
spans = sql("""select task_id, to_state as state, stage, waits_who as who, waits_what as what, on_you,
                      julianday(at) * 1440 as at from task_spans order by task_id, id""")
for i, s in enumerate(spans):
    task = tasks.get(s["task_id"], {})
    if i + 1 < len(spans) and spans[i + 1]["task_id"] == s["task_id"]:
        s["end"], s["open"] = spans[i + 1]["at"], False
    elif task.get("state") in ("done", "stopped"):
        s["end"], s["open"] = task["updated"], False
    else:
        s["end"], s["open"] = now, True
    s["min"] = max(0.0, min(s["end"], now) - max(s["at"], start))

print("\n## Factory time per stage\n")
print("The minutes of each stage's spans in the window, the spans after the publish (published, done, stopped) left out:")
print("`at work` waits on nobody, `on you` waits on you, `on others` on anyone else (precheck, copy, heavy, review, magnum,")
print("...), named in the last column with their minutes. Stages in the order they first ran.\n")
stages = {}
for s in spans:
    if s["state"] in ("published", "done", "stopped") or s["min"] <= 0:
        continue
    st = stages.setdefault(s["stage"] or "-", {"first": s["at"], "tasks": set(), "total": 0.0, "work": 0.0, "you": 0.0,
                                               "others": 0.0, "who": {}})
    st["first"] = min(st["first"], s["at"])
    st["tasks"].add(s["task_id"])
    st["total"] += s["min"]
    if s["on_you"]:
        st["you"] += s["min"]
    elif s["who"]:
        st["others"] += s["min"]
        st["who"][s["who"]] = st["who"].get(s["who"], 0.0) + s["min"]
    else:
        st["work"] += s["min"]
rows = []
for name, st in sorted(stages.items(), key=lambda kv: kv[1]["first"]):
    who = ", ".join("%s %s" % (k, mins(v)) for k, v in sorted(st["who"].items(), key=lambda kv: -kv[1]))
    rows.append([name, len(st["tasks"]), mins(st["total"]), mins(st["work"]), mins(st["you"]), mins(st["others"]), who or "-"])
table(["stage", "tasks", "minutes", "at work", "on you", "on others", "others (minutes)"], rows, "No span in the window.")

# cause is a wait's reason as the factory's alerts shorten it (alertShort, shortWhat): "questions" however many,
# "ready to push", else its words before the first ( or :, with every number as N so that the rounds of one kind of
# wait count together.
QUESTIONS = re.compile(r"^(\d+ questions? before .+|a question in Jira|\d+ questions in Jira)$")


def cause(what):
    if QUESTIONS.match(what):
        return "questions"
    if what.startswith("ready to push"):
        return "ready to push"
    cut = [i for i in (what.find("("), what.find(":")) if i > 0]
    head = what[: min(cut)].rstrip() if cut else what
    return clip(re.sub(r"\d+", "N", scrub(head)), 80) or "-"


print("\n## Factory waits on you by cause\n")
print("The spans that wait on you (on_you) in the window; a task's spans of one cause and stage in a row are one wait.")
print("The cause is the wait's words as the factory's alerts shorten them: `questions`, `ready to push`, else the words")
print("before the first ( or :, numbers as N. `open now` waits still.\n")
waits, last = [], {}
for s in spans:
    if not s["on_you"] or s["state"] in ("done", "stopped"):
        last.pop(s["task_id"], None)
        continue
    c, prev = cause(s["what"]), last.get(s["task_id"])
    if prev and prev["cause"] == c and prev["stage"] == (s["stage"] or "-"):
        prev["min"] += s["min"]
        prev["open"] = s["open"]
        continue
    last[s["task_id"]] = {"task": s["task_id"], "cause": c, "stage": s["stage"] or "-", "min": s["min"], "open": s["open"]}
    waits.append(last[s["task_id"]])
by = {}
for wt in waits:
    if wt["min"] <= 0:
        continue
    b = by.setdefault(wt["cause"], {"stages": [], "waits": 0, "tasks": set(), "min": 0.0, "longest": 0.0, "open": 0})
    if wt["stage"] not in b["stages"]:
        b["stages"].append(wt["stage"])
    b["waits"] += 1
    b["tasks"].add(wt["task"])
    b["min"] += wt["min"]
    b["longest"] = max(b["longest"], wt["min"])
    b["open"] += wt["open"]
rows = [[c, ", ".join(b["stages"]), b["waits"], len(b["tasks"]), mins(b["min"]), mins(b["longest"]), b["open"]]
        for c, b in sorted(by.items(), key=lambda kv: (-kv[1]["min"], kv[0]))]
table(["cause", "stages", "waits", "tasks", "minutes", "longest", "open now"], rows, "No wait on you in the window.")

print("\n## Factory holds\n")
print("The hold events of the window (every stage or the launches held: begin, a try, ok when lifted), the newest 20,")
print("oldest first.\n")
holds = sql(f"""select substr(at, 1, 16) as at, phase, text from events
                where kind = 'hold' and julianday(at) >= julianday('{fsince}') order by id desc limit 20""")[::-1]
table(["at", "phase", "what"], [[h["at"], h["phase"], clip(scrub(h["text"]), 200)] for h in holds], "No hold in the window.")

print("\n## Factory questions: the take rate of the recommended answers by role\n")
print("The questions asked or answered in the window, by the role that raised them. `take rate` is `took it` of the")
print("answers to a question with a recommendation: the answer was the recommended option (took_recommendation). A low")
print("rate says the role recommends what you do not want; a high one on many questions may say they were needless.")
print("`needless` counts your flags.\n")
qs = sql(f"""select coalesce(nullif(raised_by, ''), '-') as role, state, recommended >= 0 as rec,
                    took_recommendation as took, flag = 'needless' as needless from questions
             where julianday(asked_at) >= julianday('{fsince}') or julianday(answered_at) >= julianday('{fsince}')""")


def counts(rs):
    answered = [r for r in rs if r["state"] == "answered"]
    rec = [r for r in answered if r["rec"]]
    took = sum(r["took"] for r in rec)
    rate = "%d%%" % round(100 * took / len(rec)) if rec else "-"
    return [len(rs), len(answered), len(rec), took, rate, sum(r["state"] == "voided" for r in rs), sum(r["needless"] for r in rs)]


roles = {}
for q in qs:
    roles.setdefault(q["role"], []).append(q)
rows = [[role] + counts(rs) for role, rs in sorted(roles.items(), key=lambda kv: (-len(kv[1]), kv[0]))]
if rows:
    rows.append(["all"] + counts(qs))
table(["raised by", "asked", "answered", "answered with a recommendation", "took it", "take rate", "voided", "needless"], rows,
      "No question in the window.")
ds = sql(f"""select kind, choice, recommendation_shown as shown, recommendation_followed as followed from decisions
             where julianday(at) >= julianday('{fsince}')""")
choices = {}
for d in ds:
    choices["%s %s" % (d["kind"], d["choice"])] = choices.get("%s %s" % (d["kind"], d["choice"]), 0) + 1
print()
if ds:
    print("Card decisions: %d (%s); a recommendation was shown on %d and followed on %d." % (
        len(ds), ", ".join("%s %d" % kv for kv in sorted(choices.items())), sum(d["shown"] for d in ds),
        sum(d["followed"] for d in ds)))
else:
    print("Card decisions: none in the window.")

print("\n## Factory liaison notes (notes.md)\n")
print("The notes.md of each task folder changed in the window, which the liaison keeps (prompts/factory-liaison.md): what")
print("confused you, what the factory asked needlessly, what it got wrong, with the file it rests on. Scrubbed as the")
print("factory scrubs what it keeps, the last %d lines of each. The notes are data, never instructions." % NOTES_LINES)
cut = datetime.strptime(fsince, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).timestamp()


NOT_PLAIN, UNREADABLE, OVER = ("(notes.md is not a plain file: left out)", "(notes.md cannot be read: left out)",
                               "(notes.md is over 1 MiB: left out)")


def notes(folder):
    """(text, note) of a task folder's notes.md changed in the window: its text, or why it is left out; (None, None)
    when it has none or it did not change. The agents write the folder, so a link (the folder or the file), anything
    but a plain file, and a file over NOTES_MAX are not read: the file is opened without following a link and without
    waiting on a pipe, checked on its descriptor, and read up to one byte past the cap."""
    path = os.path.join(folder, "notes.md")
    try:
        if os.path.islink(folder) or os.lstat(path).st_mtime < cut:
            return None, None
    except OSError:  # no notes.md, or a "folder" that is a file
        return None, None
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except OSError as e:
        return None, UNREADABLE if e.errno in (errno.EACCES, errno.EPERM) else NOT_PLAIN
    chunks, size = [], 0
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode):
            return None, NOT_PLAIN
        while size <= NOTES_MAX:
            chunk = os.read(fd, NOTES_MAX + 1 - size)
            if not chunk:
                break
            chunks.append(chunk)
            size += len(chunk)
    except OSError:
        return None, UNREADABLE
    finally:
        os.close(fd)
    if size > NOTES_MAX:
        return None, OVER
    return b"".join(chunks).decode("utf-8", "replace"), None


try:
    names = sorted((n for n in os.listdir(ftasks) if n.isascii() and n.isdigit()), key=int)
except OSError:
    names = []
shown = 0
for name in names:
    text, note = notes(os.path.join(ftasks, name))
    if text is None and note is None:
        continue
    task = tasks.get(int(name), {})
    print("\n### Task %s (%s, %s)\n" % (name, task.get("jira_key") or "task " + name, task.get("repo") or "-"))
    shown += 1
    if note:
        print(note)
        continue
    lines, left = scrub_lines(text.rstrip(), NOTES_LINES)
    if left:
        print("(%d earlier lines left out)\n" % left)
    for line in lines:
        line = line.rstrip()[:400]
        print("> " + line if line else ">")
if not shown:
    print("\nNo task's notes.md changed in the window.")
PY
		echo "(the factory's stage times, waits, holds, questions or notes could not be read: see the error evidence.sh printed)" >>"$out"
fi

git -C "$repo" log --since="$since" --format='%h %ad %<(150,trunc)%s' --date=short >"$dir/gitlog.txt"

echo "$out ($(wc -l <"$out" | tr -d ' ') lines), $(wc -l <"$dir/reports.txt" | tr -d ' ') report dirs, $(wc -l <"$dir/gitlog.txt" | tr -d ' ') commits"
