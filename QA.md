# QA record

Version: `v0.3.1-pg.9` (fork of xray-torrent-blocker v0.3.1)

## Environment

- Ubuntu 26.04, Linux 7.0.0-38-generic
- Go 1.24.6 (module targets Go 1.22)
- PasarGuard panel v5.5.1, SQLite database, owner + API key with
  `users.read` / `users.update` scope `all`; the failsafe additionally needs
  `nodes.read` / `nodes.reconnect`
- One disposable PasarGuard user (`id=1`, `username=alice`)

## Automated tests

```text
$ go test -race -shuffle=on -cover ./...
ok  tblocker            1.033s  coverage: 10.1% of statements
ok  tblocker/blocker    1.257s  coverage: 73.1% of statements
ok  tblocker/config     1.059s  coverage: 93.2% of statements
ok  tblocker/logwatch   7.192s  coverage: 86.2% of statements
ok  tblocker/notify     1.332s  coverage: 80.0% of statements
ok  tblocker/panel      1.556s  coverage: 80.0% of statements
ok  tblocker/storage    1.120s  coverage: 82.1% of statements
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
| `TestHandleDuplicateDetectionExtendsBlock` | a second torrent line moves an active block's expiry forward without a panel call |
| `TestHandleAlreadyDisabledExtendsBlock` | a pre-existing-disabled record also slides, but is never enabled |
| `TestExtendAndFlushPersists` | coalesced extension is durable after Flush/reopen |
| `TestExtendIgnoresPendingAndMissing` | pending/missing records are not extended |
| `TestRestartAfterExpiryUnblocks` | restart after the expiry still enables the account |
| `TestUnresolvedPendingSurvivesRestart` | a failed panel lookup is durable and retried after restart |
| `TestResolveFailurePersistsPendingAndRetries` | a resolve network failure is retried and eventually disables |
| `TestResolveFailureThenDisabledUserIsPreExisting` | an unresolved user found disabled is never enabled |
| `TestPendingReconcileRetriesDisable` | an unapplied disable is retried, not dropped |
| `TestPendingReconcileExpiredActiveDropsWithoutLateDisable` | an expired pending block is not applied late |
| `TestBlockFailureNotifiesAdmin` | block transport failure sends an admin alert |
| `TestUnblockFailureNotifiesAdmin` | unblock transport failure sends an admin alert |
| `TestTelegramFailureSendsAdmin` | failure alert includes action, user, error, attempt, retry |
| `TestDetectionThresholdRequiresBurst` | 9 lines do nothing; the 10th blocks |
| `TestDetectionWindowExpiresOldLines` | old lines outside the window are discarded |
| `TestActiveBlockBypassesDetectionThreshold` | an active block still slides on a single line |
| `TestDetectionToleranceDefaults` | absent keys preserve immediate blocking |
| `TestDetectionToleranceConfigured` | explicit threshold/window are loaded |
| `TestDetectionToleranceValidation` | out-of-range tolerance is rejected |
| `TestListEnabledNodes` | node list uses `GET /api/nodes?enabled=true` |
| `TestReconnectNode` | reconnect uses `POST /api/nodes/{id}/reconnect` |
| `TestReconnectNodeRejectsInvalidID` | invalid node IDs are rejected before a request |
| `TestFailsafeReconnectsAllEnabledNodesAfterDelay` | after the grace period, every enabled node is reconnected; disabled/limited nodes are skipped |
| `TestFailsafeSkipsPreExistingDisabledByDefault` | pre-existing-disabled blocks do not restart nodes by default |
| `TestFailsafeIncludesPreExistingWhenConfigured` | opt-in pre-existing handling works |
| `TestFailsafeFailureIsReportedAndRetryable` | failed reconnect is alerted and retried after cooldown |
| `TestFailsafeDefaults` | the failsafe is disabled by default |
| `TestFailsafeConfigured` | failsafe and whitelist values load and normalize |
| `TestFailsafeValidation` | out-of-range failsafe values are rejected |
| `TestWhitelistSkipsIdentityWithoutPanelCall` | whitelisted identity causes no panel call or record |
| `TestWhitelistMatchesUsernameAfterResolve` | username whitelist works after one resolve |
| `TestTelegramFailsafeSendsAdminOnly` | failsafe result goes to the admin chat |

## Detection tolerance (pg.8)

`DetectionThreshold` and `DetectionWindowMinutes` implement a per-identity
sliding count. With `10`/`5`, the first nine torrent-tagged lines in five
minutes do nothing; the tenth enters the normal resolve/block path. The counter
is cleared when it fires.

- An already active block is checked before the threshold and is extended by
  every new line, so a threshold does not stop the sliding ban timer.
- Defaults are `1` and `5`; an existing config without the keys keeps immediate
  blocking. `config.yaml.example` ships `10`/`5`.
- Counters are in memory and pruned by the expiry worker; they are not in
  `blocks.json`. A restart starts a fresh burst, while active blocks remain in
  the journal.
- `TestDetectionThresholdRequiresBurst`, `TestDetectionWindowExpiresOldLines`,
  and `TestActiveBlockBypassesDetectionThreshold` cover the behavior;
  `TestDetectionToleranceDefaults`, `TestDetectionToleranceConfigured`, and
  `TestDetectionToleranceValidation` cover config loading.

## Failsafe and whitelist (pg.9)

### Node reconnect semantics

Verified in the PasarGuard panel and node source, not just the UI label:

- `POST /api/nodes/{node_id}/reconnect` calls `NodeOperator.restart_node`.
- `restart_node` calls `connect_single_node(..., force_start=True)`.
- `NodeOperation._start_or_attach_node` uses the default `stop_first=True`,
  calls `pg_node.stop()`, then `pg_node.start(...)` with a fresh Xray config and
  user snapshot.
- In the node agent, `Stop` calls `Controller.Disconnect`, which shuts down the
  Xray backend; `Core.Stop` kills and reaps the Xray process, removes its
  sockets, and `Core.Start` launches a new process.
- For a WireGuard-backed node the same endpoint restarts WireGuard, not Xray.

So the failsafe is a real core restart, not merely a control-plane reconnect.
The all-nodes endpoint exists, but tblocker enumerates nodes itself so it can
skip disabled and limited nodes before issuing per-node reconnects.

### Implementation

- `Failsafe.Enabled` is optional and off by default.
- The trigger is a tblocker-created active block that continues producing
  torrent detections after `Failsafe.AfterMinutes`; `IncludePreExisting` opts
  into already-disabled accounts as well.
- `GET /api/nodes?enabled=true` is filtered again for `disabled`/`limited`;
  only the remaining nodes receive `POST /api/nodes/{id}/reconnect`.
- Node reconnects use a separate 3-minute timeout and no automatic retries, so
  a timed-out request that still restarted the core is not restarted again.
- Reconnects are sequential, a global `CooldownMinutes` prevents storms, and a
  successful block is marked `failsafe_triggered` in `blocks.json`; a failed
  attempt is reported to the admin and reset for a later retry.
- `Whitelist` is an exact, case-sensitive list matched against the captured
  identity, numeric user ID, and username. A whitelisted identity is ignored
  before any panel request; a whitelist match after resolution still avoids a
  disable.

Tests cover all of the above; `go test -race` and `go vet` are clean.

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

`python3 qa/e2e.py` passed on `v0.3.1-pg.9` after the parser, queue,
inotify/polling, complete-line, security, non-root, sliding-expiry,
detection-tolerance, and failsafe/whitelist changes. The E2E does not
drive the failsafe or whitelist paths itself; those are covered by the unit and
HTTP contract tests above. It starts the real binary as the unprivileged current
user and verifies:

- `PUT /api/user/by-id/42/disabled` with `{"disabled":true}`
- `X-Api-Key` header
- Telegram admin + user messages
- webhook `block`
- journal `state=active`, `previous_status=active`
- a second torrent line moves `blocked_until` forward without another panel
  call or notification
- restart with a future expiry does not enable the user
- restart with an expired journal record calls `disabled:false` and clears the
  journal
- an already-disabled user is journaled with `pre_existing_disabled=true` and is
  never enabled by the expiry worker

## Live PasarGuard test

`python3 qa/live_panel.py` against PasarGuard v5.5.1 passed on
`v0.3.1-pg.4` after the parser and hardening changes. pg.5 changed the service
unit/installer, pg.6 added sliding expiry, pg.7 added retries/failure alerts,
pg.8 added detection tolerance, and pg.9 added the failsafe and whitelist; the
user/block panel request contract is unchanged. The pg.9 E2E uses
`DetectionThreshold: 10` and covers the threshold, active-block bypass, normal
blocking, expiry, and the sliding timer. Node reconnect was verified from the
PasarGuard panel/node source and by HTTP contract tests; no real node core was
restarted in this test environment. It verified:

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

## Sliding expiry (pg.6)

`BlockDuration` is an idle timeout. Every torrent line for an active block calls
`Store.Extend` with `now + BlockDuration`; no panel call is made. The journal
update is coalesced and flushed by a 1-second ticker in `Manager.Run`, by the
expiry worker, and on shutdown. A crash can lose at most about one second of
the most recent extension; the in-memory timer always uses the latest
detection, so a running service does not unblock between flushes.

The same path extends a `pre_existing_disabled` record. That record still only
removes itself at expiry; it never sends `disabled:false`.

Unit coverage: `TestHandleDuplicateDetectionExtendsBlock`,
`TestHandleAlreadyDisabledExtendsBlock`, `TestExtendAndFlushPersists`,
`TestExtendIgnoresPendingAndMissing`.

## Restart and failure recovery (pg.7)

- `Manager.Run` calls `ProcessExpired` immediately at startup. An active record
  past `BlockedUntil` is enabled at that point, even if the service was down
  across the expiry.
- `TestRestartAfterExpiryUnblocks` covers the “crashed at 03:50, restarted at
  04:30” case: the second `ProcessExpired` calls `disabled:false`, removes the
  record, and emits an unblock notification.
- Pending blocks are reconciled at startup: disabled becomes active; active
  with the window remaining is retried; active with the window elapsed is
  dropped rather than disabled late.
- A failed initial resolve writes a durable pending record keyed by the numeric
  ID or username (`unresolved=true`) and retries. `TestUnresolvedPendingSurvivesRestart`
  covers the restart path.
- Retry backoff is 15s, 30s, 1m, 2m, 4m, 8m, then 10m; `attempts` and
  `next_attempt` are stored in `blocks.json`.
- Unblock failures keep the active record and retry the same way.
- Definitive resolve/disable 4xx errors are not retried; the pending record is
  removed and an admin alert is sent.

Failure notifications are sent through the optional `FailureNotifier`
interface implemented by `notify.Dispatcher`: they are admin-only, require
`SendAdminMessage: true`, and include action, user, error, attempt, and retry
delay. The end user is not notified. `TestTelegramFailureSendsAdmin` covers the
message shape.

## Pending-record reconciliation

Unit tests cover the ambiguous-failure path: a 500/timeout while disabling
leaves a `pending` journal record; the next worker tick reads the user and
either marks the block active (panel applied it), retries the disable (panel did
not apply it and the window remains), or drops it (window elapsed). A crash
after the API call but before the journal update is handled the same way on
restart.

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
