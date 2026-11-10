"""The scrub the factory runs before it keeps text, for the improvement loop's evidence.sh.

It is internal/factory/runtime/scrub.go after execx.Redact: GitHub tokens, JWTs, Authorization values and private keys;
AWS, Slack, OpenAI, Anthropic, GitLab, Stripe and Google keys; the values of password=, token: and the like, also
under a quoted key as JSON, YAML and TOML write one, "password": "x" (the key stays); emails; host names and IPv4
addresses. Each pattern is the Go one word for word, in the lists below, and go()
compiles it with Go's meaning. It masks every byte the Go scrub masks; it masks more only where a run of host
characters is longer than a host name may be, and where a run of non-space characters is still over 2,000 after the
masks. In internal/factory/runtime, TestEvidenceScrubHoldsTheGoScrubsPatterns checks the lists against every pattern
the Go source compiles, and Scrub's passes against its replay there; FuzzEvidenceScrubMasksEveryByteScrubMasks feeds
both scrubs the same texts and wants this one's output to be the Go scrub's with these two extra masks.

Python's engine, unlike Go's, backtracks: the JWT pattern on an eyJ repeated, the email pattern on a long run of the
characters an address is made of and the host pattern on a dotted run take minutes on a long word. So the JWT and the
email patterns skip the starts that must fail as an earlier one in the same run failed, which is no change to what
they match, and the host pattern runs on each run of host characters, a run longer than a host name (253 characters)
masked whole. Redact's patterns run as merged searches that reuse what they found, so that no text is searched again
to its end (redact). No text is dropped or cut before the masks run: a word dropped for speed took with it a quote, a
key, a -----BEGIN or an Authorization: that opens a secret going on past the word, and the rest of that secret
showed.

The secret, token and key-value patterns run on the whole text, in the Go scrub's order, since a private key, an
Authorization value and a key's value (on the next line, or quoted over lines) cross a line break; emails and hosts,
which cannot, run on each line.

python3 scrub.py --serve is the entry of the fuzz and of evidence.sh's notes: records of a 4-byte big-endian length
and a text's UTF-8 bytes in, the same for the scrubbed text out, until its input ends. python3 scrub.py --patterns
prints the pattern lists as JSON.
"""
import json
import re
import sys

# Go's \s; Python's has the vertical tab too, and Go's \S takes it.
GO_SPACE = "\\t\\n\\f\\r "
# The letters Go's (?i) folds to a rune Python's ASCII mode does not: s to the long s, k to the Kelvin sign.
GO_FOLDS = {"s": "\u017f", "k": "\u212a"}


def go(pattern):
    """A Go pattern compiled with re.ASCII, in go_syntax's Python form."""
    return re.compile(go_syntax(pattern), re.ASCII)


def go_syntax(pattern):
    """A Go pattern (RE2 syntax) in Python's, for re.ASCII, to match what Go's regexp matches: \\s and \\S
    are Go's (no vertical tab), $ is the end of the text, and (?i) folds s and k as Go does. It knows the constructs
    the scrub's patterns use, and fails on another flag, a named group, a letter's escape in a class but \\s (and
    [\\s\\S]), or a negated class under (?i) that holds s or k."""
    out, i, fold, groups = [], 0, False, []
    if pattern.startswith("(?i)"):
        out.append("(?i)")
        i, fold = 4, True
    while i < len(pattern):
        c = pattern[i]
        if c == "\\":
            e = pattern[i : i + 2]
            out.append({"\\s": "[" + GO_SPACE + "]", "\\S": "[^" + GO_SPACE + "]", "\\z": "\\Z"}.get(e, e))
            i += 2
        elif c == "[":
            j = i + 1 + pattern.startswith("[^", i)
            j += pattern[j] == "]"
            while pattern[j] != "]":
                j += 2 if pattern[j] == "\\" else 1
            out.append(go_class(pattern[i : j + 1], fold))
            i = j + 1
        elif c == "(":
            groups.append(fold)
            for head in ("(?i:", "(?:", "("):
                if pattern.startswith(head, i):
                    break
            if head == "(" and pattern.startswith("(?", i):
                raise ValueError("go: a group go() does not know in %r" % pattern)
            fold = fold or head == "(?i:"
            out.append(head)
            i += len(head)
        elif c == ")":
            fold = groups.pop()
            out.append(c)
            i += 1
        elif c == "$":
            out.append("\\Z")
            i += 1
        elif fold and c.lower() in GO_FOLDS:
            out.append("[" + c.lower() + GO_FOLDS[c.lower()] + "]")
            i += 1
        else:
            out.append(c)
            i += 1
    return "".join(out)


def go_class(cls, fold):
    """A Go character class in Python's: \\s as Go's, and under (?i) the long s and the Kelvin sign beside s and k."""
    if cls == "[\\s\\S]":
        return cls
    neg = cls.startswith("[^")
    body, members, k = cls[2 if neg else 1 : -1], [], 0
    while k < len(body):
        if body[k] == "\\":
            e = body[k : k + 2]
            if e == "\\S" or e[1:].isalpha() and e != "\\s":
                raise ValueError("go: %s in a class" % e)
            members.append(GO_SPACE if e == "\\s" else e)
            k += 2
        elif k + 2 < len(body) and body[k + 1] == "-":
            members.append(body[k : k + 3])
            k += 3
        else:
            members.append(body[k])
            k += 1

    def holds(letter):
        return any(m == letter or len(m) == 3 and m[0] <= letter <= m[2] for m in members)

    extra = "".join(f for c, f in GO_FOLDS.items() if fold and (holds(c) or holds(c.upper())))
    if extra and neg:
        raise ValueError("go: a negated class under (?i) that holds s or k: %s" % cls)
    return ("[^" if neg else "[") + extra + "".join(members) + "]"


# The Go scrub's patterns word for word, in its order: execx.Redact's four, then runtime.Scrub's token shapes, key-value
# pairs, emails and hosts. TestEvidenceScrubHoldsTheGoScrubsPatterns (internal/factory/runtime) checks these lists
# against the Go source.
REDACT_GO = [
    r"(?:gho_|ghs_|ghp_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]+",
    r"eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}",
    r"(?i:authorization:\s*(?:bearer|token|basic)\s+)\S+",
    r"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)",
]
TOKENS_GO = [
    r"\b(?:AKIA|ASIA|AGPA|AIDA|AROA)[0-9A-Z]{16}\b",
    r"\bxox[abposr]-[A-Za-z0-9-]{10,}",
    r"\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}",
    r"\bglpat-[A-Za-z0-9_-]{16,}",
    r"\b[rs]k_(?:live|test)_[A-Za-z0-9]{16,}",
    r"\bAIza[0-9A-Za-z_-]{35}\b",
]
# The key names of both key-value patterns (runtime.scrubKeyWords), and the pairs under a quoted key (R58): the key
# word is the only part of the pattern under (?i:), so a prefix that ends in the long s or the Kelvin sign is one of
# non-word characters, as \b counts it. Scrub runs the quoted pairs before the unquoted ones: a quoted value that
# holds an unquoted pair ("token":"x password: y") is then masked whole, where the other order cut its quote off.
KEYS_GO = r"(?:aws_)?(?:secret(?:_access)?_key|password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret)"
PAIRS_GO = r"""(?i)\b(""" + KEYS_GO + r""")(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)"""
QUOTED_GO = (r"""(["'](?:[^"'\s]*[^0-9A-Za-z_"'\s])?(?i:""" + KEYS_GO + r""")["'])(\s*[=:]\s*)"""
             r"""("[^"]*"|'[^']*'|[^\s,}\]]+)""")
EMAIL_GO = r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}"
HOST_GO = r"(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?:com|net|org|io|dev|app|ai|cloud|internal|local|lan|corp|intra|info|biz|tech|xyz|site|us|uk|de|ua|eu|me|cc|tv|gg)\b|\b(?:\d{1,3}\.){3}\d{1,3}\b"

# Redact runs as merged searches: the JWT pattern apart (jwt_from), so that it skips the starts that fail; a private
# key's header, whose body (body_end) is searched only for a header that wins; and every other pattern of REDACT_GO
# with that header in one search, each in a group named by its place in the list, so that a pattern added to the
# list runs, in Go's order. A search whose match another one's took in is searched again from that match's end, and
# a JWT or a body found before is reused, so that no part of the text is searched again to its end. jwt_from's skips
# hold for JWT_GO only, and body_end for a pattern of a header, a lazy body and an END or $: the import fails unless
# the list holds each once.
JWT_GO = r"eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}"
KEY_SHAPE = re.compile(r"(.+)\[\\s\\S\]\*\?\(\?:(.+)\|\$\)", re.DOTALL)
KEY_AT = [i for i, p in enumerate(REDACT_GO) if KEY_SHAPE.fullmatch(p)]
if REDACT_GO.count(JWT_GO) != 1 or len(KEY_AT) != 1:
    raise ValueError("scrub: redact() is built for Redact's JWT pattern %s and one private key pattern of a header, "
                     "a lazy body and an END or $; execx's patterns no longer hold each once" % JWT_GO)
JWT_AT, KEY_AT = REDACT_GO.index(JWT_GO), KEY_AT[0]
KEY_HEAD, KEY_END = KEY_SHAPE.fullmatch(REDACT_GO[KEY_AT]).groups()


def in_group(i, pattern):
    """Pattern i of Redact's, a group named p<i> of the one search: a leading (?i) becomes the group's own."""
    syntax = go_syntax(pattern)
    if syntax.startswith("(?i)"):
        syntax = "(?i:" + syntax[4:] + ")"
    return "(?P<p%d>%s)" % (i, syntax)


REDACT_IN = [i for i in range(len(REDACT_GO)) if i != JWT_AT]
REDACT = re.compile("|".join(in_group(i, KEY_HEAD if i == KEY_AT else REDACT_GO[i]) for i in REDACT_IN), re.ASCII)
KEY_BODY_END = go(KEY_END)
JWT = go(JWT_GO)
JWT_RUN = re.compile(r"[A-Za-z0-9_-]*", re.ASCII)
TOKENS = go("|".join(TOKENS_GO))
QUOTED = go(QUOTED_GO)
PAIRS = go(PAIRS_GO)
EMAIL = go(EMAIL_GO)
# an email that starts a run of the characters of its first part, after the place the search starts
EMAIL_RUN = re.compile(r"(?<![A-Za-z0-9._%+-])" + EMAIL.pattern, re.ASCII)
HOST = go(HOST_GO)
# The characters of a host name, the word characters and the long s and the Kelvin sign Go's (?i) folds to: a host
# never leaves such a run, and the characters around a run are no word characters, so the hosts of a run are the
# same as on the whole line.
HOST_RUN = re.compile("[A-Za-z0-9_.\u017f\u212a-]+", re.ASCII)
HOST_MAX = 253
# After every mask, a run of non-space characters still over WORD_MAX shows as [...]: it masks only more, and a long
# minified JSON shows no quoted secret the Go scrub's key-value patterns do not see (also one whose key is no key word).
WORD = re.compile("[^" + GO_SPACE + "]+")
WORD_MAX = 2000


def jwt_from(text, pos, last):
    """The JWT pattern's first match at pos or after, as Go finds it, as (start, end). From any eyJ of a run of token
    characters the first part runs to the run's end, where a dot must follow: a later eyJ of the run fails as an
    earlier one did, and matches with the earlier one's end when its first part still has 10 characters. last keeps
    the runs of the last eyJ tried (start, first part's end, end or None), so that no run is read twice."""
    while True:
        at = text.find("eyJ", pos)
        if at < 0:
            return None
        if not last[0] <= at < last[1]:
            m = JWT.match(text, at)
            last[:] = at, JWT_RUN.match(text, at).end(), m.end() if m else None
        first_end, end = last[1], last[2]
        if end is not None and first_end - (at + 3) >= 10:
            return at, end
        pos = first_end


def body_end(text, at, last):
    """Where a private key's body that starts at at ends: after the first END line at at or after, else at the text's
    end. last keeps the last search (from, match or None): one that found none from before at finds none from at,
    and one that found an END at or after at finds that one."""
    frm, m = last
    if frm is None or frm > at or m is not None and m.start() < at:
        m = KEY_BODY_END.search(text, at)
        last[:] = at, m
    return m.end() if m else len(text)


def redact_at(m):
    """The place in REDACT_GO of the pattern a match of REDACT is of."""
    return next(i for i in REDACT_IN if m.group("p%d" % i) is not None)


def redact(text):
    """execx.Redact: the leftmost match of its patterns, the first of them in Go's order where two start at one place,
    and on from its end. An Authorization value keeps its name."""
    out, pos, other, jwt, jwt_runs, ends = [], 0, None, None, [0, 0, None], [None, None]
    while True:
        # each search's next match at pos or after: None to search, False when there is none
        if other is not False and (other is None or other.start() < pos):
            other = REDACT.search(text, pos) or False
        if jwt is not False and (jwt is None or jwt[0] < pos):
            jwt = jwt_from(text, pos, jwt_runs) or False
        if not other and not jwt:
            out.append(text[pos:])
            return "".join(out)
        at = redact_at(other) if other else None
        if jwt and (not other or (jwt[0], JWT_AT) < (other.start(), at)):
            start, end = jwt
        else:
            start = other.start()
            end = body_end(text, other.end(), ends) if at == KEY_AT else other.end()
        out.append(text[pos:start])
        colon = text.find(":", start, end)
        if colon >= 0 and text[start : start + 13].lower() == "authorization":
            out.append(text[start : colon + 1] + " <redacted>")
        else:
            out.append("<redacted>")
        pos = end


def emails(line):
    """The email pattern's matches masked, as Go finds them. When it fails at a place, it fails at every later place of
    the same run of the characters of an address's first part (the run ends where it ended, at the same @ or none),
    so after the place the search starts only a run's start is tried."""
    out, pos = [], 0
    while True:
        m = EMAIL.match(line, pos) or EMAIL_RUN.search(line, pos + 1)
        if not m:
            out.append(line[pos:])
            return "".join(out)
        out.append(line[pos : m.start()])
        out.append("<email>")
        pos = m.end()


def host(m):
    run = m.group(0)
    if "." not in run:
        return run
    return "<host>" if len(run) > HOST_MAX else HOST.sub("<host>", run)


def capped(m):
    return "[...]" if len(m.group(0)) > WORD_MAX else m.group(0)


def scrub(text):
    """The text scrubbed, every line and line break kept: the secret, token and key-value patterns on the whole text,
    in the Go scrub's order, then the emails and hosts of each line, then each run of non-space characters still
    over WORD_MAX as [...]."""
    text = TOKENS.sub("<redacted>", redact(text or ""))
    text = PAIRS.sub(r"\1\2<redacted>", QUOTED.sub(r"\1\2<redacted>", text))
    text = "".join(HOST_RUN.sub(host, emails(line)) for line in text.splitlines(keepends=True))
    return WORD.sub(capped, text)


def serve(stdin, stdout):
    while True:
        head = stdin.read(4)
        if len(head) < 4:
            return
        text = stdin.read(int.from_bytes(head, "big")).decode("utf-8", "surrogateescape")
        out = scrub(text).encode("utf-8", "surrogateescape")
        stdout.write(len(out).to_bytes(4, "big") + out)
        stdout.flush()


if __name__ == "__main__":
    if sys.argv[1:] == ["--serve"]:
        serve(sys.stdin.buffer, sys.stdout.buffer)
    elif sys.argv[1:] == ["--patterns"]:
        json.dump({"redact": REDACT_GO, "tokens": TOKENS_GO, "quoted": [QUOTED_GO], "pairs": [PAIRS_GO],
                   "email": [EMAIL_GO], "host": [HOST_GO]}, sys.stdout)
    else:
        sys.exit("usage: scrub.py --serve | --patterns")
