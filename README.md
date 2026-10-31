# PasarGuard Torrent Blocker

Current build: **v0.3.1-pg.7**

A fork of [kutovoys/xray-torrent-blocker](https://github.com/kutovoys/xray-torrent-blocker)
based on **v0.3.1**, the last release with Telegram support, reworked to block
**PasarGuard accounts through the panel API** instead of blocking IP addresses
with `ufw`/`iptables`/`nftables`.

The service reads the Xray access log, finds lines tagged as torrent traffic,
and disables the matching PasarGuard user for a configurable number of minutes.
When the block expires it enables the account again. Telegram notifications to
the administrator and (optionally) the user are preserved from v0.3.1.

## What is different from upstream

| | upstream v0.3.1 | this build |
|---|---|---|
| Blocking | `ufw` or `iptables` DROP on the source IP | `PUT /api/user/by-id/{id}/disabled` |
| Scope | one IP address | the whole PasarGuard account |
| Firewall access | root + `NET_ADMIN` | none |
| Telegram | yes | yes, unchanged |
| Block journal | `blocked_ips.json` | `blocks.json`, with pending/active states |
| Restart behaviour | re-adds firewall rules | re-schedules panel unblocks |
| Already-disabled users | n/a | recorded, never re-enabled by tblocker |

There is **no code path that executes `ufw`, `iptables`, `nft`, `ipset`, or
`conntrack`**. `BlockMode` and `BypassIPS` from old configs are ignored with a
startup warning.

## How it works

For every access-log line containing `TorrentTag`:

1. Extract the `email:` value. PasarGuard writes the numeric panel user ID there
   (`email: 42`), so the default regex captures that ID.
2. Resolve the user with `GET /api/user/by-id/42` (or by username, see
   `Identifier`).
3. If the account is already disabled, journal a block record and notify the
   administrator. It is never enabled by tblocker later.
4. Otherwise journal a **pending** record, call
   `PUT /api/user/by-id/42/disabled` with `{"disabled":true}`, verify the
   response says `disabled`, and mark the record **active**.
5. Notify Telegram/webhook.
6. Every new torrent line for an active block moves `BlockedUntil` to
   `now + BlockDuration`. Duplicate lines do not call the panel again; the
   journal update is coalesced and flushed about once per second.
7. An expiry worker runs every `CheckIntervalSeconds` (default 15). When
   `BlockedUntil` passes it reads the user again and, if the account is still
   disabled, sends `{"disabled":false}`. The panel then restores the correct
   status (`active`, `limited`, `expired`, or `on_hold`).

`BlockDuration` is therefore an **idle timeout**, not a fixed duration from the
first detection. A user who keeps torrenting stays blocked; the timer starts
counting down only after the last torrent line. The same sliding timer applies
to a pre-existing-disabled record, but that record still never enables the
account.

`BlockDuration` is expressed in whole minutes (minimum 1). For a short expiry
test use `BlockDuration: 1` and `CheckIntervalSeconds: 5`; the sliding logic
works at whatever duration is configured.

The journal is written before the API call. If the process dies or the panel
times out mid-request, the pending record is reconciled on the next tick: if the
panel says disabled, the block becomes active; if the account is still active
and the block window has not elapsed, tblocker retries the disable; if the
window already elapsed, it drops the record instead of disabling the user late.
This avoids the two dangerous failure modes: an account left disabled forever,
and an account re-enabled too early.

## Restart and failure recovery

At startup tblocker immediately runs an expiry/reconciliation pass.

- An **active block past `BlockedUntil`** is enabled right away, even if the
  service was down for hours (unless `ReenableOnUnblock` is false). Example:
  block expires at 04:00, service was crashed from 03:50 to 04:30 — on restart
  at 04:30 it calls `disabled:false`.
- An **active block before `BlockedUntil`** stays disabled; the timer is not
  reset by the restart.
- A **pending block** is reconciled: if the panel is disabled it becomes active;
  if the user is active and the window remains, the disable is retried; if the
  window has passed, the pending record is dropped.
- A **pre-existing-disabled** record is removed at expiry without enabling the
  account.

Panel requests that fail with a network error, timeout, 429, or 5xx are
retried with exponential backoff: **15s, 30s, 1m, 2m, 4m, 8m, then 10m**. The
`next_attempt` and attempt count are stored in `blocks.json`, so retries
survive a restart.

- **Block path:** if the first resolve or disable fails with a non-definitive
  error, tblocker writes a durable pending record (for a resolve failure, keyed
  by the numeric ID or username). It retries from the expiry worker; a successful retry disables the
  account and sends the normal block notification. If the block window expires
  before the panel recovers, it does not disable the user late.
- **Unblock path:** the active record is kept, the failure is recorded, and the
  enable is retried until it succeeds or the user disappears.
- **Definitive 4xx errors during resolve/disable** (401/403/404/409/422) are
  not retried. Any pending record is removed and the administrator is alerted.

Failure notifications go to the **admin Telegram chat** when
`SendAdminMessage: true`. They include the action (`block`/`unblock`), user,
error, attempt number, and next retry. A later success sends the usual block or
unblock message. The end user is not notified about transport failures. If admin
Telegram is disabled, failures are only logged.

## Requirements

- A PasarGuard panel with API access (tested against the v5.5 API).
- A PasarGuard node whose Xray access log is written to a file, with the
  `email` field present.
- A panel API key with `users:read` and `users:update`. Scope must cover every
  user you want the blocker to police.
- Linux (the binary is a single static executable).

## PasarGuard setup

### 1. Create an API key

Panel → **API Keys** → create a key:

- name: `tblocker`
- permissions: `users` → `read`, `users` → `update`
- scope: `all` (or `own` when the key belongs to the admin that owns the
  users you want to police)
- `inherit_permissions` can also be used with an owner/admin account that has
  those permissions.

Copy the key once; it starts with `pg_key_`. The client sends it as
`X-Api-Key: ${PANEL_KEY}`.

Verify it before starting the service:

```bash
curl -sS -H "X-Api-Key: $PANEL_KEY" https://panel.example.com/api/admin
# {"username":"owner",...}

curl -sS -H "X-Api-Key: ${PANEL_KEY}" https://panel.example.com/api/user/by-id/1
# user JSON, or 404 for an unknown ID
```

- `401` → wrong/revoked key.
- `403` → key lacks `users:read`/`users:update` or scope.
- HTML/404 on `/api/admin` → `BaseURL` is wrong (do not include `/api`).

### 2. Enable the access log on the node

In the panel's Xray core settings, set a file for the access log, for example:

```json
"log": {
  "access": "/var/lib/pg-node/access.log",
  "error": "/var/lib/pg-node/error.log",
  "loglevel": "warning"
}
```

PasarGuard's node captures Xray's stdout and writes access lines to this file;
the path is resolved inside the node process. If the node runs in Docker, make
sure `/var/lib/pg-node` (or your chosen directory) is mounted on the host, and
point `LogFile` at the host path.

Check that user traffic produces lines with an email:

```bash
tail -f /var/lib/pg-node/access.log | grep 'email:'
```

### 3. Tag torrent traffic

Add a routing rule and a blackhole outbound to the Xray configuration
(`routing.rules` and `outbounds`):

```json
{
  "type": "field",
  "protocol": ["bittorrent"],
  "outboundTag": "TORRENT"
}
```

```json
{
  "protocol": "blackhole",
  "tag": "TORRENT"
}
```

`TORRENT` is the default `TorrentTag`. Keep the tag unique: tblocker matches it
as a substring of the log line. The blackhole alone only catches part of
BitTorrent traffic; disabling the account is the actual enforcement.

## Build

Go 1.22+ is required.

```bash
make build          # build/tblocker for the host architecture
make release        # linux/amd64 and linux/arm64 tarballs in dist/
make check          # gofmt check, go vet, go test -race
```

## Install

```bash
sudo groupadd --system tblocker 2>/dev/null || true
sudo useradd --system --gid tblocker --home-dir /opt/tblocker \
  --no-create-home --shell /usr/sbin/nologin tblocker 2>/dev/null || true
sudo install -d -m 0750 -o tblocker -g tblocker /opt/tblocker
sudo install -m 0755 -o root -g root build/tblocker /opt/tblocker/tblocker
sudo install -m 0600 -o tblocker -g tblocker config.yaml.example /opt/tblocker/config.yaml
sudo install -m 0644 -o root -g root tblocker.service /etc/systemd/system/tblocker.service
sudoedit /opt/tblocker/config.yaml
sudo chown tblocker:tblocker /opt/tblocker/config.yaml
sudo chmod 0600 /opt/tblocker/config.yaml
sudo setfacl -m u:tblocker:x /var/lib/pg-node
sudo setfacl -m u:tblocker:r /var/lib/pg-node/access.log
sudo systemctl daemon-reload
sudo systemctl enable --now tblocker
journalctl -u tblocker -f
```

The `setfacl` lines are only needed when the access log is not already
world-readable; they grant the service account read access without making the
log world-readable. `acl` must be installed for `setfacl`.

`./install.sh` does the same from a local `build/tblocker`, creates the
`tblocker` system account, fixes ownership (including a journal left by an older
root-run install), and tries the ACL automatically. It prompts for the panel URL,
API key, and Telegram admin settings. It does **not** install or modify a
firewall.

The unit runs as `tblocker:tblocker`, a nologin system account with an empty
capability set. It needs no `NET_ADMIN` and does not use root. If you move
`StorageDir` or the log file outside the paths allowed by the unit, adjust
`ReadWritePaths`, `ReadOnlyPaths`, and `ProtectHome` accordingly; `ProtectHome=true`
hides `/home` and `/root`, so a log under a home directory needs
`ProtectHome=read-only` or a move to `/var`.

If you already have a different service account, change `User=`/`Group=` in the
unit and `chown` the config, `StorageDir`, and `blocks.json` to that account.
The account still needs read access to the access log and no capabilities.
`ProtectProc=`/`ProcSubset=` require systemd 247 or newer; remove those two lines
on older releases.

## Configuration

The full annotated file is [`config.yaml.example`](config.yaml.example).

| Key | Default | Meaning |
|---|---|---|
| `LogFile` | required | Xray access log to follow |
| `BlockDuration` | required | idle ban length in minutes; resets on each new detection (> 0) |
| `TorrentTag` | `TORRENT` | substring that marks a torrent line |
| `StorageDir` | `/opt/tblocker` | directory for `blocks.json` |
| `Hostname` | system hostname | name in admin notifications |
| `CheckIntervalSeconds` | `15` | expiry worker interval (5–3600) |
| `UsePolling` | `false` | use stat polling instead of inotify |
| `PasarGuard.BaseURL` | required | panel root, without `/api` |
| `PasarGuard.APIKey` | required | `pg_key_...` (or an admin JWT) |
| `PasarGuard.Identifier` | `auto` | `auto`, `id`, or `username` |
| `PasarGuard.TimeoutSeconds` | `10` | per-request timeout |
| `PasarGuard.InsecureSkipVerify` | `false` | disable TLS verification |
| `PasarGuard.ReenableOnUnblock` | `true` | enable the account at expiry |
| `PasarGuard.StartupCheck` | `true` | probe URL/key/permissions at startup |
| `UsernameRegex` | `\semail:\s+(\S+)` | capture group 1 = identity |
| `TidRegex` | unset | Telegram chat ID in identity/username/note |
| `SendAdminMessage` | `false` | admin Telegram messages |
| `AdminBotToken`, `AdminChatID` | — | required when admin messages are on |
| `SendUserMessage` | `false` | message the user |
| `BotToken` | — | required when user messages are on |
| `UserMessageTemplate` | built-in Russian text | two `%d` arguments |
| `TelegramAPIURL` | `https://api.telegram.org` | local Bot API server support |
| `SendWebhook` | `false` | POST block/unblock events |
| `WebhookURL`, `WebhookTemplate`, `WebhookHeaders` | — | webhook settings |

The default webhook template takes five arguments: username, IP, server,
action, and RFC3339 timestamp. A sixth `%d` argument (block duration in
minutes) is also accepted if the template uses it; the correct form is detected
and validated at startup.

### Identifier modes

- `auto` (default): numeric values are tried as a user ID first, then as a
  username. PasarGuard usernames may be all digits, so the fallback matters.
- `id`: the captured value must be a numeric user ID.
- `username`: the captured value is a username.

Resolution and status changes both use the ID endpoint in `auto`/`id` mode.
`username` mode uses `/by-username/...`, which is useful with older panels that
do not expose `/by-id/...`.

### Telegram user messages

`TidRegex` is applied to the identity/email value first, then to the panel
username, then to the user's `note` field. The raw log line is deliberately not
used: a destination hostname could otherwise contain a `tgid-` value and cause
a message to an unrelated chat. PasarGuard does not put the Telegram ID in the
email field (it is the numeric user ID), so storing it in the username or note
is the practical option:

- username: `alice_tgid-123456789`
- note: `contact: tgid-123456789`

Chat IDs are validated as numeric (`-` allowed for groups) before use.

### Webhook payload

The default template receives five `%s` arguments:

```json
{"username":"alice","ip":"203.0.113.9","server":"node-1","action":"block","timestamp":"2026-01-01T00:00:00Z"}
```

`action` is `block` or `unblock`. A custom template must produce valid JSON and
must use exactly the verbs it declares; this is checked at startup.

## Performance

- **Fast log path.** The default `email:` lookup is a byte scan, not a regular
  expression. On the development host a full `ParseLine` costs ~235 ns and zero
  allocations; a custom regex path costs ~1.2 µs and one allocation. Custom
  `UsernameRegex` patterns still use the regexp engine.
- **Inotify by default.** `UsePolling: false` uses the kernel watch API; set it
  to `true` only for filesystems where inotify is unreliable.
- **Decoupled tail.** Log reading feeds a bounded 1,024-entry detection queue, so
  a slow panel API cannot stop the tail from draining the log. The queue logs
  drops at most once every five seconds.
- **Duplicate suppression.** An in-memory identity cache stops repeated torrent
  connections from turning into repeated panel requests; it is rebuilt from the
  journal at startup.
- **`-perf`.** The binary logs line/detection/error/drop counts, parse time,
  goroutines, heap, and GC once per minute, plus a final snapshot on shutdown.
- **HTTP reuse.** The panel client keeps connections alive and retries
  idempotent requests with backoff; Telegram and webhook calls are asynchronous
  with timeouts.

## Log truncation and rotation

`truncate -s 0`, `: > access.log`, and `logrotate` with `copytruncate` are
handled. Both watchers detect the size drop, reopen the file, and continue from
offset 0. This is covered by `TestFollowHandlesTruncation` for inotify and
polling.

- Lines that were already removed by the truncation cannot be recovered.
- `CompleteLines` is enabled, so a half-written line (for example a cut
  `email:` value) is buffered or discarded on reopen instead of being passed to
  the parser as a complete line.
- If the file is truncated and refilled past its previous size before the
  watcher observes it, a size-based follower can miss the reset. That requires
  the node to write more than the old file size inside the detection window
  (~250 ms), which is far above normal access-log volume. Inotify (the default)
  is the fastest detector; `UsePolling` also passes the truncation test.
- PasarGuard's node logger opens the access file with `O_APPEND`, so truncating
  a live file does not leave a sparse hole or move writes past the old offset.
- The tail library logs `Re-opening truncated file ...` on each reset. That is
  expected with a one-minute cron truncation.

For a strict guarantee against losing lines, rotate by renaming the file and
restart/reload the node, or consume a pipe instead of a file. For torrent
detection, the minute-level truncation is fine: a lost line only means one
missed detection, and the next torrent connection will trigger a block.

## Migrating from upstream v0.3.1

1. Stop the old service:
   ```bash
   sudo systemctl stop tblocker
   ```
2. **Remove the old firewall rules.** v0.3.1 added `ufw`/`iptables` rules per
   source IP. This build will not remove them, so they would keep blocking
   clients forever. Inspect first:
   ```bash
   sudo ufw status numbered
   sudo iptables -S INPUT | grep DROP
   ```
   For each IP listed in `/opt/tblocker/blocked_ips.json` (or in the rules),
   delete only that rule:
   ```bash
   sudo ufw delete deny from <IP> to any
   sudo iptables -D INPUT -s <IP> -j DROP
   ```
   Do not flush the whole firewall or reset `ufw`; delete only the tblocker
   rules. The new service ignores `blocked_ips.json` and logs a warning.
3. Keep your Telegram settings, remove `BlockMode`/`BypassIPS`, update
   `UsernameRegex` to `\semail:\s+(\S+)` (the exact old default is migrated
   automatically), and add the `PasarGuard` section.
4. Point `StorageDir` at `/opt/tblocker` and start the service. `blocks.json`
   starts empty; there is no safe way to map old IP blocks to panel users.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| no detections | `LogFile` wrong, log does not contain `TorrentTag`, Xray is not logging `email`, or the `tblocker` user cannot read the log |
| `permission denied` reading the log | `tblocker` lacks read permission or directory traversal; grant `setfacl -m u:tblocker:x <dir>` and `-m u:tblocker:r <log>` |
| `resolve user ... 404` | `Identifier` mode or `UsernameRegex` does not match the log; user does not exist |
| `403` on block | API key lacks `users:update`, or scope does not cover the user |
| `401` | wrong/revoked API key, or a JWT expired |
| `panel returned status "active" after disable request` | reverse proxy or panel version ignored the body; check the API response |
| no detections | `LogFile` wrong, log does not contain `TorrentTag`, or Xray is not logging `email` |
| `line has no PasarGuard identity` | the node is not writing `email:` (TPROXY setups); user-level blocking cannot work without it |
| user still online after block | node sync/disconnect policy; the account is disabled in the panel, but Xray may keep an established session briefly |
| user re-enabled unexpectedly | an admin disabled them during the block and PasarGuard does not expose who changed the status; set `ReenableOnUnblock: false` |

Useful commands:

```bash
journalctl -u tblocker -n 100 --no-pager
systemctl show tblocker -p User -p Group -p CapabilityBoundingSet
namei -l /var/lib/pg-node/access.log
runuser -u tblocker -- test -r /var/lib/pg-node/access.log && echo readable
cat /opt/tblocker/blocks.json
grep -c 'TORRENT' /var/lib/pg-node/access.log
curl -sS -H "X-Api-Key: ${PANEL_KEY}" https://panel.example.com/api/user/by-id/42
```

## Multiple nodes

Each tblocker instance watches one access log. Because the block is
account-wide, instances coordinate only through the panel's user status:

- If node B sees a user that node A already disabled, node B records it as
  pre-existing and never re-enables it.
- If two nodes block the same user at nearly the same time, both journals are
  normal blocks and the earliest expiry enables the account; the other instance
  sees the account is no longer disabled and skips its own enable.
- Keep `BlockDuration` identical on every node.

For strict single-owner semantics, run one instance against aggregated logs
instead of one instance per node.

## Security

The program opens **no listening socket** and has no telemetry, update check, or
analytics. It makes outbound requests only to the endpoints in the
configuration:

| Destination | Data sent | Default |
|---|---|---|
| `PasarGuard.BaseURL` | API key, user ID/username, status toggle | configured panel |
| `TelegramAPIURL` | bot token in the URL path, chat ID, block text | `https://api.telegram.org` |
| `WebhookURL` | username, IP, hostname, action, timestamp, optional duration | disabled |

DNS resolution for those hostnames goes through the system resolver. The panel
client verifies TLS certificates unless `InsecureSkipVerify` is explicitly
enabled; Telegram and webhook HTTPS use the normal Go certificate store.
Redirects are not followed on panel, Telegram, or webhook requests, so credentials
and custom headers cannot be forwarded to a second host. HTTP endpoints other
than loopback produce a startup warning because API keys and user data would be
cleartext.

Credentials live in `config.yaml` (mode 0600 when installed by `install.sh`);
`blocks.json` (mode 0600) stores user IDs, usernames, source IPs, and Telegram
chat IDs. The panel client redacts its API key from error bodies, and bot tokens
are redacted from Telegram errors, but usernames and IPs do appear in the
service log, Telegram messages, and webhook payloads by design. Protect the
journal and its backups.

The supplied unit runs as `tblocker:tblocker`, a dedicated nologin system
account, not root. It sets an empty `CapabilityBoundingSet`/`AmbientCapabilities`,
`NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome=true`, `PrivateTmp`,
`PrivateDevices`, `PrivateMounts`, `ProtectProc=invisible`, `ProcSubset=pid`,
`RestrictNamespaces`, a deny-list `SystemCallFilter`, `MemoryDenyWriteExecute`,
and `LimitCORE=0`; the binary and config are additionally read-only mount points.
It does **not** use `NET_ADMIN` or execute firewall commands. `systemd-analyze
security` scores the unit **1.5 OK** on the development host (down from 5.4
MEDIUM when it ran as root). If the log is under a home directory, change
`ProtectHome=true` to `read-only` or move the log to `/var`.

The panel API key is powerful: `users.read` can read user proxy settings, and
`users.update` can disable or enable every in-scope account. Create a dedicated
key with only those two permissions, use `scope` `own` if possible, keep the
config at 0600, and rotate the key if the host is compromised. A compromised
tblocker process can mass-disable users; that is inherent to account-level
blocking, not a hidden escalation path.

The access log is a trust boundary. Anyone who can append to it can write a
synthetic `email:` line and cause that account to be disabled. Keep the log and
its directory writable only by the node/root (0600 or 0640 with a dedicated
group); do not expose it to panel users. If the panel is unreachable or the API
key lacks `users.update`, blocking **fails open** — the user stays connected.
Monitor the service log for repeated panel errors, HTTP 401/403 responses, and
`queue_drops`.

For production, use HTTPS for the panel and webhook (loopback HTTP is accepted
for a local panel or local Bot API), restrict egress to the three destinations,
run one instance per `StorageDir`, and sign or checksum the binary you deploy.

## Limitations

- `TorrentTag` is matched as a substring of the access line, so a destination
  hostname containing the tag can trigger parsing. Only that connection's own
  account can be affected; choose a distinctive outbound tag to avoid false
  positives.
- A custom `UsernameRegex` can reintroduce the destination-spoofing bug if it
  matches a bare `email:` anywhere in the line. Keep the whitespace anchoring
  from the default, or anchor on the end-of-line field.
- **The email field is mandatory.** Without it a log line cannot be mapped to a
  PasarGuard user. Do not use the `IgnoreEmail` option from upstream v1.x.
- Blocking is per account, not per IP. A user with several devices loses all of
  them, which is normally the intent.
- The panel API does not report who changed a user's status. If an
  administrator disables a user during a tblocker block, the expiry worker may
  enable them again. Set `ReenableOnUnblock: false` if that race matters.
- One process per `StorageDir`. The journal is not locked against a second
  instance.
- If a node that applied a block is permanently lost before its journal
  expires, the account can remain disabled; another node treats it as
  pre-existing, so an administrator must enable it.
- The panel must be reachable when a block is applied. Failures are retried
  with exponential backoff (15s → 10m) and the record stays in the journal.
- Status changes are idempotent but not transactional: a crash between the API
  call and the journal update is handled by the pending-record reconciliation,
  not by a distributed transaction.

## Development

```bash
go test ./...            # unit tests
go test -race ./...      # race detector
go vet ./...
go test -run '^$' -bench . -benchmem ./logwatch
go test -run '^$' -fuzz FuzzParseLine -fuzztime 10s ./logwatch

python3 qa/e2e.py        # black-box binary test against a fake panel
PANEL_URL=http://127.0.0.1:8000 \
PANEL_API_KEY=pg_key_... \
PANEL_USER_ID=1 \
python3 qa/live_panel.py # optional: real panel + disposable user
```

The unit suite covers config defaults/validation, log parsing (IPv4/IPv6,
username and numeric IDs, missing email), API paths/headers/retries/errors,
storage atomicity and corruption, block/unblock lifecycle, restart recovery,
pending reconciliation, already-disabled users, backoff, and notification
formatting/redaction. `qa/e2e.py` exercises the real binary end to end;
`qa/live_panel.py` was used against PasarGuard v5.5.1 during development.

## License

MIT, same as upstream. Original copyright (c) 2025 Sergey Kutovoy.
