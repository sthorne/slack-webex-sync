# Live smoke test

Run this before relying on the bridge, and again after upgrading it. It
exercises every sync path against real Slack and Webex. The automated
tests can't cover these paths, because they use stand-ins for both
platforms.

Allow about 45 minutes. You need:

* A **test Slack workspace** (or a private test channel), with the app from
  [`slack-app-manifest.yaml`](../slack-app-manifest.yaml) installed.
* A **Webex service account** plus a Webex integration (see the README).
  Also two Webex test spaces with the service account in both.
* Two people, or one person with a Slack account and a *separate* Webex
  account (not the service account). The bridge ignores its own posts, so
  testing as the service account shows nothing.
* Both people using the **same email address** on Slack and Webex, for the
  mention checks.

Keep the bridge's log open in a terminal (`run -debug` is useful the first
time) and the metrics in another: `watch -n2 'curl -s localhost:9090/metrics | grep ^sws_'`.

Record results in the table at the end. Any ✗ comes with the log lines
around it; those tell us what to fix.

---

## 0. Setup

| # | Step | Expected |
|---|------|----------|
| 0.1 | Two pairings in `config.yaml`: `smoke-a` (Slack `#smoke-a` ↔ Webex "Smoke A") and `smoke-b`. Set `storage.encryption_key` from `slack-webex-sync generate-key`. | |
| 0.2 | `slack-webex-sync webex-login`, signed in as the service account | "Authorized as … Tokens saved (encrypted)" |
| 0.3 | `slack-webex-sync check` | Both pairings `ok`; no bot-account warning |
| 0.4 | `slack-webex-sync run` | Log shows `bridge ready`, `slack socket mode connected`, `webex websocket connected` |
| 0.5 | `curl -s localhost:9090/healthz` | `"status":"ok"`; webex detail says `receiving via websocket` |

**If 0.4 never shows `webex websocket connected`:** the device websocket is
the least certain part of the bridge. Note the error, continue the test
(the bridge falls back to polling), and expect 3.x and 4.x to fail.

## 1. Slack → Webex

| # | In Slack `#smoke-a` | Expected in Webex "Smoke A" |
|---|---|---|
| 1.1 | Post `hello from slack` | "**Your Name**: hello from slack" within ~2 s |
| 1.2 | Post `*bold* _italic_ ~strike~ `code` <https://example.com|link>` | Bold, italic, strikethrough, code, and a working link |
| 1.3 | Post a multi-line message containing a ``` code block ``` | Name on its own line, code block intact |
| 1.4 | Reply in a thread to 1.1 | Appears as a threaded reply under 1.1 |
| 1.5 | Edit 1.1 to `hello (edited)` | Webex copy updates |
| 1.6 | Delete the thread reply from 1.4 | Webex copy disappears |
| 1.7 | Upload an image with a comment | Message with the comment and the image attached |
| 1.8 | Upload 3 files in one message | First file on the message, the other two as replies |
| 1.9 | Mention the other person: `@Their Name ping` | A real Webex mention that notifies them |
| 1.10 | Post `@here standup` | `@All` in Webex |
| 1.11 | React 🎉 to 1.1 | Threaded note "_Your Name reacted 🎉_" |
| 1.12 | Remove that reaction | The note disappears |
| 1.13 | Post in `#smoke-b` | Appears only in "Smoke B" |
| 1.14 | Have another bot/integration post in `#smoke-a` (e.g. `/remind me in 1 minute to test`) | Mirrored (`sync.bot_messages` defaults to true) |

## 2. Webex → Slack

| # | In Webex "Smoke A" (as a person, not the service account) | Expected in Slack `#smoke-a` |
|---|---|---|
| 2.1 | Post `hello from webex` | Shows as "**Their Name (Webex)**" with their avatar |
| 2.2 | Post `**bold** *italic* ~~strike~~ [link](https://example.com)` | Rendered as Slack formatting |
| 2.3 | Reply in a thread to 2.1 | Threaded reply in Slack |
| 2.4 | Reply in Webex to a message that *came from* Slack (1.1) | Joins the original Slack thread |
| 2.5 | Edit 2.1 | Slack copy updates |
| 2.6 | Attach a file | Message, then the file uploaded in its thread |
| 2.7 | @mention the other person | A real Slack mention |
| 2.8 | Mention `@All` | `@channel` in Slack |

## 3. Webex events that need the websocket

| # | In Webex | Expected in Slack |
|---|---|---|
| 3.1 | Delete 2.1 | Slack copy disappears |
| 3.2 | React 👍 to a mirrored message | Bridge bot adds :+1: |
| 3.3 | A second person also reacts 👍, then the first removes theirs | :+1: stays |
| 3.4 | The second person removes theirs | :+1: removed |

## 4. Resilience

| # | Step | Expected |
|---|------|----------|
| 4.1 | Stop the bridge (Ctrl-C), post in both apps, start it again **within 2 minutes** | Messages posted while it was down sync after restart. Webex ones come from the catch-up poll; Slack ones only if Slack's delivery retries (a few minutes) reach the bridge. Note which arrive; this measures the real Slack gap. |
| 4.2 | Block Webex traffic for ~2 min (for example, disconnect the network), then restore it | Log shows `webex websocket down; polling…`, then it reconnects. `/healthz` shows `receiving via polling` in between. |
| 4.3 | Make a Webex call fail: temporarily remove the service account from "Smoke A", post in `#smoke-a`, and wait ~15 min | Log shows `will retry` ×4, then `event parked`. `slack-webex-sync events list` shows it. |
| 4.4 | Add the service account back; run `slack-webex-sync events retry -all` | The parked message appears in Webex; `events list` shows 0 parked |
| 4.5 | Check `sws_events_synced_total`, `sws_event_failures_total` and `sws_events_parked_total` in `/metrics` | Counts match what you did |
| 4.6 | Look at the stored tokens (for example `sqlite3 slack-webex-sync.db "select v from kv"`) | Value starts with `enc:v1:`; no readable token |

## 5. Loop and noise checks

| # | Check | Expected |
|---|---|---|
| 5.1 | Scroll both sides after the whole test | No message mirrored twice, and nothing bounced back to its origin |
| 5.2 | Log has no repeated `ERROR` lines other than those caused on purpose in 4.x | |

---

## Results

| Section | Result (✓ / ✗ + notes) |
|---|---|
| 0 Setup | |
| 1 Slack → Webex | |
| 2 Webex → Slack | |
| 3 Websocket-only events | |
| 4 Resilience | |
| 5 Loops | |

Tested with version: `slack-webex-sync version` = ________ on ____-__-__
