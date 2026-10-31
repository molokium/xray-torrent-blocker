# QA record

Version: `v0.3.1-pg.5` (fork of xray-torrent-blocker v0.3.1)

## Environment

- Ubuntu 26.04, Linux 7.0.0-38-generic
- Go 1.24.6 (module targets Go 1.22)
- PasarGuard panel v5.5.1, SQLite database, owner + API key with
  `users.read` / `users.update` scope `all`
- One disposable PasarGuard user (`id=1`, `username=alice`)

## Automated tests

```text
$ go test -race -shuffle=on -cover ./...
ok  tblocker            1.040s  coverage: 10.6% of statements
ok  tblocker/blocker    1.122s  coverage: 78.9% of statements
ok  tblocker/config     1.050s  coverage: 92.5% of statements
ok  tblocker/logwatch   7.189s  coverage: 86.2% of statements
ok  tblocker/notify     1.329s  coverage: 83.1% of statements
ok  tblocker/panel      1.543s  coverage: 78.9% of statements
ok  tblocker/storage    1.149s  coverage: 79.8% of statements
```

`go vet ./...` is clean. `gofmt -l .` is empty. `shellcheck install.sh` and
`bash -n install.sh` are clean.

## Security regression tests

The security review in `README.md` is backed by these tests:

| Test | Property |
|---|---|
| `TestParseLineIgnoresEmailInDestination` | `tcp:email:5` cannot become the user field; the real `email:` value wins |
| `TestTelegramIDComesFromIdentityNotDestination` | a destination `tgid-999` cannot select a Telegram chat |
| `TestNoRedirectHeaderLeak` | panel `X-Api-Key` is not sent to a redirect target |
| `TestWebhookDoesNotFollowRedirect` | webhook Authorization/custom headers are not sent to a redirect target |
| `TestAPIKeyRedactedFromErrorDetail` | panel error bodies cannot echo the API key into logs |
| `TestSecurityWarnings` | non-loopback HTTP and `InsecureSkipVerify` are reported |
| `TestConfigPermissionWarning` | a group/world-readable config containing a real key is reported |
| `TestEmbeddedCredentialsRejected` | `user:pass@host` panel URLs are rejected |
| `TestLegacyDefaultRegexMigrated` | the old unanchored default is replaced on startup |

## Performance and later-version ports

Ported from upstream v1.x where it did not conflict with account-level API
blocking:

- byte-scan fast path for the default `email:` lookup (custom regexes still use
  `regexp`); now anchored on whitespace so a destination cannot be spoofed;
- inotify by default, with `UsePolling` as the reliable-filesystem fallback;
- `-perf` runtime metrics and a final shutdown snapshot. The upstream v1.1
  implementation sampled `time.Since(time.Now())` immediately and always
  reported ~0 parse time; this port measures around `ParseLine`;
- optional sixth `%d` webhook argument (duration);
- maintained `nxadm/tail`, `Hostname`, atomic journal, and the existing test
  structure were already present in this fork.

Deliberately not ported: firewall/`nftables`/`iptables`/conntrack backends,
`BlockMode`, `BypassIPS`, and `IgnoreEmail`. The first four are the mechanism
this fork replaces; ignoring the email would make account resolution impossible.

Benchmarks on the development host after the security changes:

```text
BenchmarkParseLineDefault-4       8130405    149.3 ns/op    0 B/op   0 allocs/op
BenchmarkParseLineCustomRegex-4   1073173   1080 ns/op     32 B/op   1 allocs/op
BenchmarkFastEmailValue-4        21944464     54.71 ns/op    0 B/op   0 allocs/op
BenchmarkDefaultEmailRegex-4       787584   1594 ns/op     32 B/op   1 allocs/op
```

The fast path is allocation-free and about 10× faster than the regex path. A
10-second fuzz run of `FuzzParseLine` completed 727,546 executions without a
crash:

```text
fuzz: elapsed: 11s, execs: 727546 (28026/sec), new interesting: 80 (total: 195)
PASS
```

Tail tests exercise both `Poll: false` (inotify) and `Poll: true`, including a
file created after startup.

Truncation tests pass in both modes: `TestFollowHandlesTruncation` truncates a
live file to zero and verifies the next appended line is read;
`TestFollowDoesNotEmitPartialLine` writes half an `email:` value, verifies it is
not emitted, then completes it and verifies the full line is emitted.
`CompleteLines` is enabled in the follower.

`-perf` smoke test (no panel required; shutdown after 3 s):

```text
PERF final lines=0 detections=0 parse_errors=0 queue_drops=0 lines_per_sec=0
avg_parse=0s goroutines=4 heap_mb=1 gc=0 uptime=2s
```

## Black-box binary test (fake PasarGuard)

`python3 qa/e2e.py` passed on `v0.3.1-pg.5` after the parser, queue,
inotify/polling, complete-line, security, and non-root changes. It starts the
real binary as the unprivileged current user and verifies:

- `PUT /api/user/by-id/42/disabled` with `{"disabled":true}`
- `X-Api-Key` header
- Telegram admin + user messages
- webhook `block`
- journal `state=active`, `previous_status=active`
- restart with a future expiry does not enable the user
- restart with an expired journal record calls `disabled:false` and clears the
  journal
- an already-disabled user is journaled with `pre_existing_disabled=true` and is
  never enabled by the expiry worker

## Live PasarGuard test

`python3 qa/live_panel.py` against PasarGuard v5.5.1 passed on
`v0.3.1-pg.4` after the parser and hardening changes; pg.5 changes only the
service unit and installer, so the runtime path is identical. It verified:

- numeric ID (`email: 1`, `Identifier: auto`)
- username mode (`Identifier: username`)
- already-disabled user is not re-enabled
- restart before expiry leaves the user disabled
- restart after expiry (journal rewritten to the past) enables the user

A separate **live spoof regression** used the real panel and the pg.4 binary
with this access line:

```text
2026/10/31 12:00:00 from 203.0.113.9:4567 accepted tcp:email:5 [in >> TORRENT] email: 1
```

User 1 (`alice`) became `disabled` and the journal key was `id:1`. With the old
unanchored regex this line resolves user 5, which does not exist, so no block
would have been applied. This is the wrong-account bug fixed in pg.4.

A separate real-time test with `BlockDuration: 1` and
`CheckIntervalSeconds: 5` produced:

```text
blocked 03:42:36
enabled 03:43:41 after 64.2 s
journal: {}
```

The 64.2 s is the 1-minute block plus the 5-second expiry tick.

## Permission failure

A second API key with only `users.read` (no `users.update`) produced:

```text
ERROR: disable user alice: PasarGuard API PUT /api/user/by-id/1/disabled
returned HTTP 403: Permission denied: users.update
```

The journal was removed, no notification was sent, and the user stayed
`active`. This confirms definitive API errors do not leave stale blocks.

## Firewall audit

```text
grep -RIn --include='*.go' -E 'os/exec|iptables|ufw|nftables|conntrack|exec\.Command' .
-> only a test string containing BlockMode: ufw
```

The binary has no `os/exec` import and never invokes a firewall tool. The
systemd unit has no `NET_ADMIN`. `install.sh` installs no firewall packages.

## Network egress audit

Source-level review found exactly three outbound destinations:

| Destination | Request | Data |
|---|---|---|
| `PasarGuard.BaseURL` | `GET /api/admin`, `GET /api/user/by-id/{id}`, `GET /api/user/by-username/{name}`, `PUT /api/user/by-id/{id}/disabled`, `POST /api/admin/token` (JWT only) | API key, identity, disabled flag |
| `TelegramAPIURL` | `POST /bot<token>/sendMessage` | bot token, chat ID, block text |
| `WebhookURL` | `POST` | username, IP, hostname, action, timestamp, optional duration |

There is no inbound listener, telemetry, update check, remote config fetch, or
analytics call. DNS for those hosts goes through the system resolver. Go's HTTP
transport honors `HTTP_PROXY`/`HTTPS_PROXY` if the service environment sets
them; the unit does not set proxy variables.

`govulncheck` (Go vulnerability database, 2026-10-31):

```text
No vulnerabilities found in called symbols or packages.
Module result: GO-2026-5024 in golang.org/x/sys/windows,
fixed in v0.44.0; platform windows; not reached on Linux.
```

Two residual supply-chain notes: `gopkg.in/yaml.v2` is unmaintained (config is
root-controlled, no remote YAML), and release archives have SHA-256 checksums
but no signature.

## Hardening applied in pg.4

- Default `UsernameRegex` is `\semail:\s+(\S+)`; requires whitespace before the
  field and after the colon, so `tcp:email:5` is not an identity.
- The fast path scans for the first whitespace-delimited `email:` field, making
  it equivalent to the default regex.
- `TidRegex` is applied to the identity/email value, then panel username, then
  note; the raw access-log line is not searched.
- Panel, Telegram, and webhook clients do not follow redirects.
- Panel error details are redacted for the API key.
- Non-loopback `http://` endpoints and `InsecureSkipVerify` produce startup
  warnings.
- Config files with real-looking keys and group/other permission bits produce a
  startup warning.
- Embedded credentials in panel/webhook/Telegram URLs are rejected.
- `install.sh` passes secrets to `sed` through a 0600 script file instead of
  command-line arguments; both the API key and the admin bot token are read with
  `read -s`.

## Non-root service hardening (pg.5)

- The unit runs as `User=tblocker` / `Group=tblocker`, a system account created
  with `--system --no-create-home --shell /usr/sbin/nologin`.
- `CapabilityBoundingSet=` and `AmbientCapabilities=` are empty; the process has
  no capabilities after systemd drops to the service user.
- `NoNewPrivileges`, `PrivateTmp`, `PrivateDevices`, `PrivateMounts`,
  `ProtectSystem=strict`, `ProtectHome=true`, `ProtectClock`,
  `ProtectHostname`, `ProtectKernelTunables`, `ProtectKernelModules`,
  `ProtectKernelLogs`, `ProtectControlGroups`, `ProtectProc=invisible`,
  `ProcSubset=pid`, `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`,
  `RestrictNamespaces`, `RestrictRealtime`, `RestrictSUIDSGID`,
  `LockPersonality`, `MemoryDenyWriteExecute`, `SystemCallArchitectures=native`,
  a deny-list `SystemCallFilter`, `UMask=0077`, `LimitCORE=0`,
  `KeyringMode=private`, and `RemoveIPC`.
- `ReadOnlyPaths` protects the binary and config; only the journal directory is
  writable (`ReadWritePaths=/opt/tblocker`).
- `install.sh` creates the account and group, sets `/opt/tblocker` to
  `tblocker:tblocker 0750`, the config to `tblocker:tblocker 0600`, the binary
  to `root:root 0755`, and fixes a root-owned `blocks.json` left by pg.4.
- `install.sh` checks whether the log is readable by `tblocker` and tries a
  targeted `setfacl` for the file and its directory; otherwise it prints the
  exact commands. ACLs survive truncation but must be reapplied after a
  logrotate `create` that replaces the inode.

`systemd-analyze verify` reports only the expected missing `/opt/tblocker/tblocker`
on this development host. `systemd-analyze security --offline=yes` scores the
unit **1.5 OK**, down from **5.4 MEDIUM** when it ran as root with a full
capability set.

Non-root runtime evidence: `qa/e2e.py` starts the real binary as the
unprivileged `ubuntu` user with a 0600 config and a private log/journal
directory; it completes the block/unblock, restart, Telegram, and webhook
checks. The systemd account/ACL path itself cannot be exercised on this host
because creating a system user requires interactive root authentication, so
`install.sh` is covered by `shellcheck`/`bash -n` and the unit by
`systemd-analyze`.

## Pending-record reconciliation

Unit tests cover the ambiguous-failure path: a 500/timeout while disabling
leaves a `pending` journal record; the next worker tick reads the user and either
marks the block active (panel applied it) or drops the record (panel did not).
A crash after the API call but before the journal update is handled the same way
on restart.

## Known limitations verified in review

- The access log must contain `email:`. PasarGuard sets it to the numeric user
  ID; without it, a line cannot be mapped to a panel user.
- The access log is a trust boundary: a user who can append to it can write a
  synthetic `email:` line and disable an account.
- If an administrator disables a user during a tblocker block, the expiry worker
  can re-enable the account (the API does not expose who changed the status).
  `PasarGuard.ReenableOnUnblock: false` avoids this.
- Unblock uses the recorded numeric user ID, never a username fallback, so a
  deleted ID whose username was reused cannot enable the new account. Covered by
  `TestUnblockDoesNotTargetRecreatedUsername`.
- One process per `StorageDir`; the journal is not locked against a second
  instance.
- Blocking fails open if the panel is unreachable or the API key lacks
  `users.update`.
- The API key's `users.read` permission can read user proxy credentials, and
  `users.update` can disable every in-scope account; use a dedicated key and
  rotate it.
