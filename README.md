# slack-webex-sync

Keeps pairs of Slack channels and Webex spaces in sync, so team members can
work in whichever client they prefer and still see every update.

Each **pairing** in the config links one Slack channel with one Webex space.
Everything posted on one side shows up on the other:

| Slack | ↔ | Webex |
|---|---|---|
| Message (shown with the Webex author's name and avatar) | ↔ | Message prefixed with the Slack author's name in bold |
| Thread reply | ↔ | Threaded reply |
| Edit / delete | ↔ | Edit / delete |
| File attachments | ↔ | File attachments |
| `@mention` of a colleague | ↔ | `@mention` of the same person (matched by email) |
| `@here` / `@channel` | ↔ | `@All` |
| Emoji reaction | → | Threaded note, e.g. _Ada reacted 🎉_ (deleted again if the reaction is removed) |
| Emoji reaction (added by the bridge bot) | ← | Webex reaction |
| Posts from other bots and integrations | ↔ | Posts from other bots (configurable) |

It runs as a single binary with **no public URL required**. Slack events
arrive over Socket Mode. Webex events arrive over the Webex device websocket,
and paired spaces are polled whenever the websocket is down.

## How it works

```
             Socket Mode                          device websocket
  Slack  ─────────────────►┐                ┌◄───────────────────  Webex
                           │    durable     │   (REST polling while
                           ├─► event  ──────┤    the websocket is down)
  Slack  ◄──── Web API ────┤    queue       ├──── REST API ────►   Webex
                           │                │
                           └── store: queue + message links ───┘
                     (SQLite · PostgreSQL · Redis · DynamoDB · MongoDB · memory)
```

* Every mirrored message is stored as a **link** (Slack `ts` ↔ Webex message
  id). Links are how edits, deletes, thread replies and reactions find their
  counterpart.
* Every incoming event is **saved to the store before it is acknowledged**,
  then processed oldest first. A crash or a failed API call doesn't lose it:
  failures are retried, and events that keep failing are parked for an
  operator (see [Running in production](#running-in-production)).
* **Loop prevention:** anything posted by the bridge's own identities (the
  Slack bot and the Webex service account) is ignored. A message that already
  has a link is never mirrored twice, which also makes redeliveries harmless.
* Deleting an original deletes its copy. Deleting a *copy* (for example, a
  Slack admin removing a mirrored post) only unlinks it. The author's
  original stays.
* Links expire after `storage.retention_days` (30 by default), so the store
  stays small. After that, activity on the old message no longer syncs.

## Setup

### 1. Slack app

1. Go to <https://api.slack.com/apps> → **Create New App** → **From a
   manifest**, and paste [`slack-app-manifest.yaml`](slack-app-manifest.yaml).
2. **Install to Workspace** and copy the **Bot User OAuth Token** (`xoxb-…`).
3. Under **Basic Information → App-Level Tokens**, create a token with the
   `connections:write` scope (`xapp-…`).
4. Invite the bot to every paired channel: `/invite @Webex Bridge`.

### 2. Webex service account and integration

A Webex *bot* only receives messages that @mention it, so the bridge uses a
regular Webex **user account** instead (for example, "Slack Bridge"). Its
posts show up as that user, with the Slack author's name in bold.

1. Create the service account and add it to every paired space.
2. At <https://developer.webex.com/my-apps>, create an **Integration**:
   * Redirect URI: `http://localhost:8765/callback`
   * Scopes: `spark:all` and `spark:kms`. `spark:all` is what the Webex SDKs
     use for device registration, which the websocket needs.
3. Note the client ID and client secret.

### 3. Configure

```sh
cp config.example.yaml config.yaml
export SLACK_BOT_TOKEN=xoxb-… SLACK_APP_TOKEN=xapp-…
export WEBEX_CLIENT_ID=… WEBEX_CLIENT_SECRET=…
```

Fill in `pairings`:

* **Slack channel ID:** open the channel's details; the ID is at the bottom
  of the *About* tab (`C…`).
* **Webex room ID:** open the space in the Webex web app and use *Copy space
  link*, or list spaces with `GET https://webexapis.com/v1/rooms` in the
  developer portal.

### 4. Authorize the Webex account (once)

Building requires Go 1.27 or newer.

```sh
make build
./bin/slack-webex-sync webex-login -config config.yaml
```

Open the printed URL **signed in as the service account**. The tokens are
saved in the configured storage and refreshed automatically after that.

### 5. Check, then run

```sh
./bin/slack-webex-sync check -config config.yaml   # verifies credentials and every pairing
./bin/slack-webex-sync run   -config config.yaml
```

To run it as a service, see [`deploy/slack-webex-sync.service`](deploy/slack-webex-sync.service)
(a systemd unit that reads secrets from `/etc/slack-webex-sync/env`).

Before relying on it, work through the [live smoke test](docs/SMOKE_TEST.md).

## Running in production

### Retries and parked events

Failed events are retried after 1, 2, 4 and 8 minutes: 5 attempts over about
15 minutes. After that they are **parked**, which means kept but no longer
retried, and logged as `event parked`. Other events keep flowing while one
is backing off.

```sh
slack-webex-sync events list          # parked events, with the last error
slack-webex-sync events retry ID      # put one back in the queue
slack-webex-sync events retry -all    # ...or all of them
slack-webex-sync events drop ID       # give up on one
```

A running bridge picks up retried events within a few seconds.

### Encrypting stored tokens

The Webex OAuth tokens live in the store. To encrypt them (AES-256-GCM),
generate a key and set `storage.encryption_key`, normally from an environment variable:

```sh
slack-webex-sync generate-key          # prints a base64 key; keep it in your secret manager
export SWS_ENCRYPTION_KEY=...          # config: encryption_key: ${SWS_ENCRYPTION_KEY}
```

Tokens already stored in plain text are encrypted on the next start. Without
a key the bridge still runs but logs a warning. If you lose the key, run
`webex-login` again.

### Health and metrics

By default the bridge serves these on `127.0.0.1:9090` (`health.listen`; set
it to `""` to turn them off):

* `GET /healthz`: `200` when Slack is connected, Webex events are arriving
  (via the websocket or polling), and the store answers. Otherwise `503`.
  The JSON body shows each check.
* `GET /metrics`: Prometheus format.

| Metric | |
|---|---|
| `sws_events_received_total{source}` | Events queued, by source platform |
| `sws_events_synced_total{source}` | Events processed successfully |
| `sws_event_failures_total{source}` | Failed attempts (each is retried or parked) |
| `sws_events_parked_total{source}` | Events that were parked |
| `sws_queue_events{status}` | Current `pending` and `parked` counts |
| `sws_connected{platform}` | 1 when events are arriving from `slack` / `webex` |
| `sws_webex_websocket_up` | 1 on the real-time websocket, 0 when polling |

Good alerts: `sws_queue_events{status="parked"} > 0`, `sws_connected == 0`
for more than 5 minutes, and `sws_webex_websocket_up == 0` for more than an
hour (polling works, but deletes and reactions from Webex won't sync).

## Configuration

See [`config.example.yaml`](config.example.yaml) for every option. The main ones:

| Key | Default | |
|---|---|---|
| `storage.driver` | `sqlite` | `sqlite`, `postgres`, `redis`, `dynamodb`, `mongodb` or `memory`. See [Storage](#storage). |
| `storage.retention_days` | `30` | How long message links are kept (30, 60, 90, …). `0` keeps them forever. |
| `storage.purge_interval` | `1h` | How often expired links are deleted |
| `storage.encryption_key` | none | Encrypts stored Webex tokens. See [Encrypting stored tokens](#encrypting-stored-tokens). |
| `health.listen` | `127.0.0.1:9090` | Address for `/healthz` and `/metrics`; `""` disables them |
| `webex.websocket` | `true` | Use the real-time websocket. Set `false` to poll only. |
| `webex.poll_interval` | `10s` | How often to poll while the websocket is down |
| `sync.bot_messages` | `true` | Mirror posts from other bots and integrations |
| `sync.files` / `sync.max_file_bytes` | `true` / 50 MiB | Copy attachments up to this size. Larger files get a note instead. |
| `sync.mentions` | `true` | Turn mentions into real mentions when emails match |
| `sync.reactions` | `true` | Mirror reactions |
| `display.slack_username_suffix` | `" (Webex)"` | Appended to Webex authors' names in Slack |
| `display.webex_name_suffix` | `""` | Appended to Slack authors' names in Webex |

## Storage

The bridge stores small records: which Slack message matches which Webex
message, reaction bookkeeping, the Webex OAuth tokens, and the event queue.
Queued events contain message text (plus ids and file links, not file
contents). An event is deleted as soon as it syncs, but **parked events keep
their text** until they are retried or dropped. Treat the store as holding
message content. Any backend below works; pick whichever
your team already runs.

| `driver` | `dsn` example | How old links expire |
|---|---|---|
| `sqlite` | `/var/lib/slack-webex-sync/links.db` | The purge job deletes them every `purge_interval` |
| `postgres` | `postgres://user:pass@host:5432/db?sslmode=require` | The purge job |
| `redis` | `redis://user:pass@host:6379/0?prefix=sws:` (`rediss://` for TLS) | Native key TTL, plus the purge job to clean up index entries |
| `dynamodb` | `dynamodb://TABLE?region=us-east-1&create_table=true` | Native DynamoDB TTL on the `expires` attribute (enabled automatically) |
| `mongodb` | `mongodb://user:pass@host:27017/slack_webex_sync` | Native TTL index on `created_at`, plus the purge job |
| `memory` | (none) | The purge job. Nothing survives a restart; for trials and tests. |

Backend notes:

* **Redis:** standalone Redis or Sentinel. Redis Cluster isn't supported,
  because the atomic scripts touch keys that can hash to different slots.
* **DynamoDB:** credentials come from the standard AWS chain (environment,
  shared config, or instance/task role). The role needs `GetItem`,
  `PutItem`, `DeleteItem`, `Query`, `TransactWriteItems`, `DescribeTable`,
  `DescribeTimeToLive` and `UpdateTimeToLive`, plus `CreateTable` if you use
  `create_table=true`. DynamoDB deletes expired items lazily (usually within
  a few days), so the bridge hides expired items itself in the meantime.
  Expiry times are stamped when a record is written, so changing
  `retention_days` only affects new records. Records written while retention
  was `0` have no expiry stamp and are never deleted by DynamoDB, though the
  bridge still hides them once they're past the retention.
* **MongoDB:** the TTL index follows `retention_days` automatically: it is
  created, updated in place, or dropped when you set 0. For
  MongoDB-compatible servers without TTL indexes (FerretDB, for example), add
  `ttl_index=false` to the DSN. The purge job then does all the expiring.

### Adding another backend

Backends register themselves by name, the way `database/sql` drivers do:

1. Create a package under `internal/store/` that implements
   [`store.Store`](internal/store/store.go). The doc comment on each method
   is the contract: atomicity, not-found behavior, and what `Purge` must
   guarantee.
2. In its `init`, call `store.Register("name", Open)`, and add a blank import
   to [`internal/store/all`](internal/store/all/all.go).
3. Run the shared suite from the package's tests. It checks the whole
   contract, including concurrency and purging:

   ```go
   func TestConformance(t *testing.T) {
       storetest.Run(t, func(t *testing.T, opts store.Options) store.Store {
           return openFreshStore(t, opts)
       })
   }
   ```

`storage.driver: name` then selects the new backend. Nothing else changes.

## Limitations and caveats

* **The Webex websocket is not a documented public API.** It is what
  Webex's own SDKs use. If Webex changes it, the bridge falls back to polling
  automatically, and messages and edits keep flowing. Polling cannot see
  **deletions or reactions**, though.
* **Reactions are asymmetric.** Webex's public API cannot add reactions, so
  Slack reactions appear in Webex as threaded notes. Webex reactions appear
  in Slack as real reactions added by the bridge bot. When several people
  react with the same emoji in Webex, the Slack reaction shows once and is
  removed when the last of them removes it.
* **Mentions** only become real mentions when the person uses the same email
  address on both platforms. Otherwise they appear as plain `@Name` text. If
  Webex rejects a mention (for example, the person isn't in the space), the
  message is re-sent with plain names.
* **Files copied from Webex** appear in Slack in the thread of the mirrored
  message, posted by the bridge bot (Slack doesn't allow a custom name on
  uploads). Webex allows one file per message, so extra Slack files are sent
  as follow-up replies.
* **History is not backfilled.** Only messages posted while the bridge is
  running are mirrored, plus anything the catch-up poll finds after a
  websocket reconnect (the last 100 messages per space).
* **Slack messages posted while the bridge is down for more than a few
  minutes are missed.** Slack retries delivery only briefly, and the bridge
  has no Slack catch-up yet. Webex messages are caught up on restart.
* **One instance at a time.** Don't run two bridges against the same
  channels or the same store.
* **A retried event can occasionally post twice.** This happens if a post
  succeeded but saving its link failed.
* The Webex service account's own messages are never mirrored, so don't use
  it as a personal account.
* Webex messages are limited to about 7 KB. Longer Slack messages are
  truncated with a note.

## Development

```sh
make test        # go test -race ./...
make vet
make vulncheck   # govulncheck against the Go vulnerability database
```

CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) runs gofmt, vet,
`go mod tidy`, govulncheck, a build, and the race-detector tests. The store
tests run against real PostgreSQL, Redis and MongoDB service containers and
the moto DynamoDB emulator, and CI fails if any backend test is skipped.
Actions are pinned to commit SHAs.

Dependencies are kept on their latest releases. To update them:

```sh
go get -u ./... && go mod tidy && make test vulncheck
```

Every store backend runs the same conformance suite (`internal/store/storetest`).
SQLite, memory and Redis run by default; Redis runs in-process via miniredis.
To run against real servers, point these at disposable instances (the
tests wipe what they use):

```sh
SWS_TEST_POSTGRES_DSN="postgres://postgres@localhost:5432/postgres?sslmode=disable" \
SWS_TEST_REDIS_URL="redis://localhost:6379/15" \
SWS_TEST_DYNAMODB_ENDPOINT="http://localhost:8000" \
SWS_TEST_MONGODB_URI="mongodb://localhost:27017" \
go test ./internal/store/...
```

The DynamoDB tests work with DynamoDB Local or `moto_server`. For a
MongoDB-compatible server without TTL indexes, also set `SWS_TEST_MONGODB_TTL=false`.

Layout:

```
cmd/slack-webex-sync/   CLI: run, check, webex-login, version
internal/bridge/        sync engine (platform-neutral, tested with fakes)
internal/format/        Slack mrkdwn <-> Webex markdown, mentions, emoji
internal/slackapi/      slack-go adapter + Socket Mode listener
internal/webex/         REST client, OAuth, device websocket, poller
internal/store/         Store contract, backend registry, purge scheduler
internal/store/*store/  backends: sqlstore, redisstore, dynamostore, mongostore, memstore
internal/store/storetest/  conformance suite every backend must pass
internal/config/        YAML config with ${ENV} expansion
```
