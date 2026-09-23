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
                           │   ordered      │   (REST polling while
                           ├─► event  ──────┤    the websocket is down)
  Slack  ◄──── Web API ────┤   queue        ├──── REST API ────►   Webex
                           │                │
                           └── SQLite / PostgreSQL: message links ─┘
```

* Every mirrored message is stored as a **link** (Slack `ts` ↔ Webex message
  id). Links are how edits, deletes, thread replies and reactions find their
  counterpart.
* Events from both platforms go through **one queue** and are handled in
  order, so a reply is never processed before the message it replies to.
* **Loop prevention:** anything posted by the bridge's own identities (the
  Slack bot and the Webex service account) is ignored. A message that already
  has a link is never mirrored twice, which also makes redeliveries harmless.
* Deleting an original deletes its copy. Deleting a *copy* (for example, a
  Slack admin removing a mirrored post) only unlinks it. The author's
  original stays.

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

## Configuration

See [`config.example.yaml`](config.example.yaml) for every option. The main ones:

| Key | Default | |
|---|---|---|
| `storage.driver` | `sqlite` | `sqlite` (single file, no cgo) or `postgres` |
| `webex.websocket` | `true` | Use the real-time websocket. Set `false` to poll only. |
| `webex.poll_interval` | `10s` | How often to poll while the websocket is down |
| `sync.bot_messages` | `true` | Mirror posts from other bots and integrations |
| `sync.files` / `sync.max_file_bytes` | `true` / 50 MiB | Copy attachments up to this size. Larger files get a note instead. |
| `sync.mentions` | `true` | Turn mentions into real mentions when emails match |
| `sync.reactions` | `true` | Mirror reactions |
| `display.slack_username_suffix` | `" (Webex)"` | Appended to Webex authors' names in Slack |
| `display.webex_name_suffix` | `""` | Appended to Slack authors' names in Webex |

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
* The Webex service account's own messages are never mirrored, so don't use
  it as a personal account.
* Webex messages are limited to about 7 KB. Longer Slack messages are
  truncated with a note.

## Development

```sh
make test   # go test -race ./...
make vet
```

The store tests also run against PostgreSQL when `SWS_TEST_POSTGRES_DSN` is set:

```sh
SWS_TEST_POSTGRES_DSN="postgres://postgres@localhost:5432/postgres?sslmode=disable" go test ./internal/store/
```

Layout:

```
cmd/slack-webex-sync/   CLI: run, check, webex-login, version
internal/bridge/        sync engine (platform-neutral, tested with fakes)
internal/format/        Slack mrkdwn <-> Webex markdown, mentions, emoji
internal/slackapi/      slack-go adapter + Socket Mode listener
internal/webex/         REST client, OAuth, device websocket, poller
internal/store/         message links on SQLite / PostgreSQL
internal/config/        YAML config with ${ENV} expansion
```
