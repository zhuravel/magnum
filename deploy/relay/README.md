# magnum-relay

magnum's daemon runs on a Mac that sleeps behind NAT, so it cannot receive GitHub's webhooks itself.
magnum-relay is a small always-on service that receives them for it. It receives the GitHub App's
webhook deliveries, checks each signature, and keeps only the routing envelope of each delivery in a
SQLite journal. The daemon then pulls the journal with a bearer token. The source is in
[cmd/magnum-relay](../../cmd/magnum-relay) and [internal/relay](../../internal/relay), and the reasons
are in [docs/DECISIONS.md](../../docs/DECISIONS.md) ("A webhook relay…", under Operations).

- **What it keeps.** For each delivery it keeps the delivery's GUID, the event and action, the
  installation and repository ids, `owner/name`, the pull request numbers, the head SHA, the base
  branch, and a check's name and conclusion. It never keeps a body, a title, a comment, a user name or
  a URL.
- **Whose deliveries.** Only those of the App's installations that `MAGNUM_RELAY_INSTALLATIONS` lists
  (see [Find the App's installation ids](#find-the-apps-installation-ids)). When it is empty, the relay
  keeps every installation's deliveries and logs a line saying so at start.
- **Retention.** Events are kept for 30 days (`MAGNUM_RELAY_RETENTION`). The journal can be thrown
  away: a new one gets a new epoch, and the daemon resyncs by polling GitHub.
- **Logs.** The logs carry delivery ids, event names, sequence numbers and error classes. They never
  carry a body, a secret or a token.
  - A request refused before it is authenticated logs no line of its own: a bad signature or header, a
    body too large or too slow, no turn, a wrong token. Neither does a delivery of another installation.
  - Once a minute the relay logs one line for each kind, with its `count`: `webhook refused`,
    `listing refused`, `health counters refused` and `webhook ignored`, each with its `status` and
    `class`. A flood of forged requests costs a few lines a minute.
  - The log's size is capped at three files of 10 MB, as described in [Log limits](#log-limits).

## The API

| Endpoint | Auth | What |
|---|---|---|
| `POST /github/webhook` | `X-Hub-Signature-256` | GitHub's deliveries, described below. |
| `GET /v1/events?after=<cursor>&limit=<1..500>&wait=<0..30s>` | `Authorization: Bearer <token>` | The events after the cursor, in journal order (described below). |
| `GET /healthz` | none, or `Authorization: Bearer <token>` | `{"ok":true}`, or 503 `{"ok":false}` when the journal does not answer. With an accepted token, the counters as well: `{"ok":true,"epoch":…,"events":<count>,"last_received_at":…}`. It never answers event data, and it answers from a snapshot at most 2 s old. |

**Deliveries.**
- **Before the body.** A request without well-formed `X-Hub-Signature-256`, `X-GitHub-Event` and
  `X-GitHub-Delivery` headers is answered (401, 400) before its body is read, and its connection is
  closed.
- **The body.** Four deliveries are read at once. A delivery has 7 s from its headers to get its turn
  and send its whole body (up to 26 MiB). A body that stalls is cut with 408, which frees its turn; no
  turn within 2 s answers 503; a larger body answers 413.
- **The signature** is the HMAC-SHA256 of the exact bytes, checked with the current or the previous
  secret. A wrong one answers 401, and the body is neither parsed nor stored.
- **The answer** comes within 9 s of the headers, under GitHub's 10. A stored delivery answers 202, and
  a delivery already stored answers 200 (a redelivery keeps its GUID). When the journal refuses a write,
  the relay answers 503: GitHub shows the failed delivery, and the daemon's poll, the backstop, finds
  the change.
- **Another installation.** When `MAGNUM_RELAY_INSTALLATIONS` is set, some signed deliveries answer 202
  `{"status":"ignored"}` and are not stored: those of an installation it does not list, and those that
  name no installation. The App's `ping` names none and is still stored.

**The listing.** A cursor is `<epoch>:<seq>`.
- **The token** is one word of 32 to 512 printable ASCII characters, the bounds `hash-token` applies.
- **Paging.** `after=` (empty) starts at the oldest kept event. `limit` defaults to 200. `wait` (up to
  `30s`, or whole seconds) makes a long poll that waits for a first event, and an insert wakes it. At
  most 16 polls wait at once; one more answers 429 with `Retry-After: 1`.
- **The answer** is `{"epoch","events","next","high_water","oldest","server_time"}`. `next` is the
  cursor to pass on. `oldest` is the cursor just before the oldest kept event, so `after=<oldest>`
  lists every kept event.
- **410 `gap`.** The relay answers 410 to a cursor of another epoch, to one below `oldest` (its events
  were pruned) and to one ahead of `high_water`. The daemon then resyncs by polling.

## Generate the webhook secret and the token

On your Mac, in a directory only you can read:

```bash
umask 077
openssl rand -hex 32 > webhook-secret   # the App's webhook secret, and the relay's secret file
openssl rand -hex 32 > relay-token      # the daemon's bearer token for the relay
```

The relay keeps only the token's SHA-256. Get it in either of two ways:

```bash
go run ./cmd/magnum-relay hash-token < relay-token                  # from a magnum checkout
docker run --rm -i magnum-relay hash-token < relay-token            # from a built image
```

`hash-token` reads the token from stdin and drops the trailing newline. It prints only the hash.

Put the token where the daemon reads it, `~/.config/magnum/keys/relay.token` (chmod 600) by
convention. Delete the working copies once the secret is on the server and in the App.

## Find the App's installation ids

The App is public, so anyone can install it. GitHub signs that installation's deliveries with the App's
secret too, so the signature alone does not tell them from yours. Set `MAGNUM_RELAY_INSTALLATIONS` to
your installations' ids, separated by commas, and the relay keeps only their deliveries. Each id is either
of these:
- The `installation_id` of a `kind = "app"` identity in `~/.config/magnum/config.toml`.
- The number at the end of the installation's settings page:
  `https://github.com/organizations/<org>/settings/installations/<id>`, or
  `https://github.com/settings/installations/<id>` for a personal account.

An entry that is not a whole number of at least 1 stops the relay at start, and the error names the
variable.

## Deploy on a container platform

These steps fit a self-hosted platform that builds an application from a repository's Dockerfile and
routes a domain to it through a reverse proxy with Let's Encrypt (Traefik, for example). The field
names below are the usual ones; yours may differ. Create an application on the platform.

1. **Source.** Use the Git provider with this repository (`https://github.com/zhuravel/magnum.git`) and
   the `master` branch.
2. **Build.** Set Build Type to **Dockerfile**:
   - Dockerfile Path: `deploy/relay/Dockerfile`
   - Docker Context Path: `.` (the repository root)
3. **Environment.** Set these variables:

   ```
   MAGNUM_RELAY_SECRET_FILE=/run/secrets/webhook-secret
   MAGNUM_RELAY_TOKEN_HASHES=<the hash hash-token printed>
   MAGNUM_RELAY_INSTALLATIONS=<your installation ids, separated by commas>
   ```

   - `MAGNUM_RELAY_TOKEN_HASHES` takes several hashes, separated by commas.
   - `MAGNUM_RELAY_INSTALLATIONS` is described in
     [Find the App's installation ids](#find-the-apps-installation-ids).
   - `MAGNUM_RELAY_RETENTION` defaults to `720h`.
   - `MAGNUM_RELAY_ADDR` (`:8080`) and `MAGNUM_RELAY_DB` (`/data/relay.db`) are set in the image.
   - The secret is read only from a file, never from a variable.
4. **Mounts** (under Advanced):
   - **Volume Mount.** Volume Name `magnum-relay-data`, Mount Path `/data`. A new named volume takes
     the image's `/data`, which is owned by the relay's user (uid 65532). A Bind Mount needs its host
     directory chowned to `65532:65532` first.
   - **File Mount.** Content: the content of `webhook-secret`. File Path: `webhook-secret`. Mount Path:
     `/run/secrets/webhook-secret`. The relay runs as uid 65532 and must be able to read this file.
     When it cannot, it exits at start with "permission denied".
5. **Domain.**
   - Host: your relay's name, for example `relay.example.com`.
   - Path: `/`
   - Container Port: `8080`
   - HTTPS: on, Certificate: `letsencrypt`

   Publish no port on the host: the reverse proxy reaches the container on the platform's network.
6. **Logs.** Cap the container's log as described in [Log limits](#log-limits).
7. **Deploy.** Then check `https://relay.example.com/healthz`, which should answer `{"ok":true}`. The
   image's `HEALTHCHECK` runs `magnum-relay healthcheck` against the same endpoint, without a token.
   With the token, the answer adds the counters. The token goes to curl on stdin, never on its command
   line:

   ```bash
   printf 'header = "Authorization: Bearer %s"\n' "$(cat ~/.config/magnum/keys/relay.token)" |
     curl -s --config - https://relay.example.com/healthz
   ```

## Deploy with Compose

[compose.yaml](compose.yaml) builds the same image from the repository root and runs it with the named
volume `relay-data` on `/data`. Use it on a platform that deploys a Compose project from a repository.

1. **Source.** Create a Compose project from this repository's `master` branch. Set the Compose Path to
   `./deploy/relay/compose.yaml`.
2. **Environment.** Set these variables. The platform writes them to a `.env` file beside the compose
   file, and Compose reads them from there.

   ```
   RELAY_WEBHOOK_SECRET=<the content of webhook-secret>
   RELAY_TOKEN_HASHES=<the hash hash-token printed>
   RELAY_INSTALLATIONS=<your installation ids, separated by commas>
   ```

   - Compose gives the secret to the relay as the file `/run/secrets/webhook_secret` (`secrets:` with
     `environment:`). The relay's environment never holds it.
   - A deploy without `RELAY_TOKEN_HASHES` stops with an error that names the variable.
   - `RELAY_INSTALLATIONS` becomes `MAGNUM_RELAY_INSTALLATIONS`, described in
     [Find the App's installation ids](#find-the-apps-installation-ids). It is optional.
   - `RELAY_RETENTION` is optional and defaults to `720h`.
   - `compose.yaml` caps the container's log already (`logging:`, as in [Log limits](#log-limits)).
3. **Domain.** Add the domain to the service `relay`: container port `8080`, path `/`, HTTPS on, and a
   Let's Encrypt certificate. The platform adds its proxy's labels and network to that service. Publish
   no port on the host.
4. **Deploy.** Then check `https://relay.example.com/healthz`, as in step 7 above.

## Log limits

Docker keeps a container's log in a file that grows without bound by default. Cap it with the
`json-file` driver at three files of 10 MB.
- **Compose.** `compose.yaml` sets it on the service `relay`:

  ```yaml
  logging:
    driver: json-file
    options:
      max-size: 10m
      max-file: "3"
  ```

- **An application built from the Dockerfile.** Set the same options where your platform sets a
  container's log options. When it has no such setting, make them Docker's defaults on the server, in
  `/etc/docker/daemon.json`:

  ```json
  {"log-driver": "json-file", "log-opts": {"max-size": "10m", "max-file": "3"}}
  ```

  Then restart Docker, which restarts its containers. The defaults reach only containers created after
  the restart, so redeploy the relay.

## Rotating the secret or the token

- **The webhook secret.**
  1. Mount the old secret as a second file, for example at `/run/secrets/webhook-secret-previous`.
  2. Set `MAGNUM_RELAY_PREVIOUS_SECRET_FILE` to that path.
  3. Put the new secret in the first file mount and redeploy. The relay now accepts both secrets.
  4. Change the App's webhook secret.
  5. Once the App's deliveries succeed, remove the previous file and its variable, then redeploy.
- **The webhook secret, with Compose.** `compose.yaml` holds one secret only. Set the new
  `RELAY_WEBHOOK_SECRET`, redeploy, and then change the App's secret at once. The deliveries between
  the two changes answer 401. Redeliver them from the App's Recent Deliveries, or let the daemon's poll
  find their changes.
- **The token.** Add the new token's hash after a comma and redeploy. Switch the daemon to the new
  token, then remove the old hash.

## GitHub App settings (set by hand)

These are outward-facing changes: the App's owner makes them in the App's settings on GitHub.

**General, Webhook.**
- Active: checked.
- Webhook URL: `https://<host>/github/webhook`.
- Webhook secret: the content of `webhook-secret`.
- SSL verification: Enable.
- Content type: `application/json`.
  - An App's settings page has no content type field.
  - The App's hook config carries one: `GET /app/hook/config`, read with the App's JWT, shows it, and
    `PATCH /app/hook/config` with `content_type: json` sets it.
  - The relay also reads a form-encoded delivery (`payload=…`), whose signature covers the encoded
    body, so either setting works.

**Permissions & events, Repository permissions.** These are the least each event needs. An App that
posts reviews already has Pull requests at Read and write.

| Permission | Access | For the events |
|---|---|---|
| Pull requests | Read-only | `pull_request`, `pull_request_review`, `pull_request_review_comment`, `pull_request_review_thread` |
| Checks | Read-only | `check_run`, `check_suite` |
| Commit statuses | Read-only | `status` |
| Issues | Read-only | `issue_comment`: GitHub's event docs name the Issues permission for it, on pull requests too |

**Subscribe to events.** Check these events:
- Pull request
- Pull request review
- Pull request review comment
- Pull request review thread
- Issue comment
- Check run
- Check suite
- Status

`installation_repositories` has no checkbox: every App receives it.

**A new permission needs approval.** A permission added to an installed App does not take effect until
the owner of each account where the App is installed approves it. For an organization, that is an
organization owner. Until then, the installation keeps its old permissions, and the events that need
the new permission do not arrive.

**Checking it.**
- Saving the webhook sends a `ping`, which the relay stores like any delivery.
- The App's **Advanced** tab lists Recent Deliveries: 202 is stored, 200 a duplicate, 401 a secret that
  does not match.
- A **Redeliver** from that list answers 200.
