# Eve — Design and Delivery Plan

**Status:** Draft v4, 2026-10-16; M0 done, M1 implemented. v4 replaces T3/webhook delivery with client-side streaming (§8, §19). v2 was revised after independent reviews by Claude Fable 5.1
and GPT-6 Astra (both: "revise"; findings and resolutions in §19). v3 adds the size guard (§6.7),
the name and the operator decisions (§18). Nothing is built yet; milestone 0 is two spikes that can
still change the design.

**Name:** Eve, the one who takes the apple.

**Repository:** `~/Development/projects/eve` · github.com/self-deprecated/eve (private)

**Go module:** `github.com/self-deprecated/eve`

**One binary:** `eve` is the CLI on every machine and, as `eve serve`, the build server on Quartz.

## 1. Purpose

Eve gives the user and their agents the part of Expo EAS they miss: send a build off from
anywhere, walk away, and be told when it is on the phone or in TestFlight. It runs on the user's
own Mac Mini (Quartz), with Apple's tools, for their native iOS apps.

The design target is **agent cost**, not build speed. Session mining (Lineage, BodyDiff,
ScreenGate on Quartz, 2026-10-16) found that a main agent session re-reads 340–564K cached tokens
per turn, so every `sleep 60; poll`, every rebuild-after-signing-fix and every rediscovery of the
toolchain costs a full turn. (Cached reads are discounted, so measure the result in turns removed
and the actual bill, not raw token counts.) The same sessions show:

- ~25 TestFlight uploads, each followed by hand-written polling loops (63 `sleep` calls and 17 poll
  loops in BodyDiff alone).
- App Store Connect JWT clients rewritten from scratch in most sessions (`/tmp/asc.mjs`,
  `/tmp/asc.py`, 34+ calls).
- Device installs that fail because the phone is locked or "unavailable", handled with 60–90 × 10 s
  unlock loops.
- ~70 commands wrapped in `env -u DEVELOPER_DIR -u SDKROOT` and repeated Nix-rsync export failures.
- A whole day of three-agent "TestFlight blockers" debugging that was an unaccepted invite.

**Success criteria for v1**

1. An agent ships a TestFlight build with **one long tool call**: `eve submit testflight --wait`
   (run as a background or long-running command). Waiting costs no tokens; there are no `sleep`
   or polling turns.
2. The result it gets back is under 4 KB: outcome, build number, and for failures a classified
   cause with a fix hint and a ≤40-line excerpt. Never a raw xcodebuild log.
3. The wait survives network drops and Eve server restarts (it reconnects and replays without
   loss), the job survives the waiting process dying, and `eve pending` finds outcomes nobody
   collected. Eve depends on no particular agent harness.
4. A crash at any point never produces a second upload of the same build, and never turns an
   upload that succeeded into a reported failure.
5. States only a human can resolve (phone locked, Xcode sign-in, agreement, export compliance,
   invite pending) are reported as such, so the agent relays them instead of retrying.
6. Nothing unexpected reaches Apple: a source upload or app bundle that grows suspiciously, or that
   contains repo files, stops and asks the requesting agent to confirm before it is accepted.

## 2. Product boundaries

### Eve owns

- A durable job queue on Quartz, running in the logged-in GUI (Aqua) session.
- A deterministic pipeline for `testflight`, `device` and `check`, plus `exec` for repo commands.
- Getting source onto Quartz from any machine (uploaded working trees).
- Build-number allocation per app, shared by the server and `eve run`.
- A size guard on source uploads and app bundles, with agent confirmation for unexpected growth.
- An App Store Connect API client for upload reconciliation, processing, internal distribution and
  tester invite state, ported from `bodydiff-testflight/scripts/app-store-connect.mjs`.
- Failure classification (rules first, optional small-model fallback).
- A resumable event stream for each job (`eve wait`), and an optional human webhook.
- Host diagnostics (`doctor --host`).

### Repositories own

- Their Xcode project or XcodeGen spec, schemes, targets, tests, Info.plist keys and icons.
- A small `eve.yml` describing how to build them (§7).

### The user owns

- Apple ID sign-in in Xcode, 2FA, agreements, export compliance answers, Family Controls approval,
  app names, creating app records.
- Unlocking and restarting the phone, accepting TestFlight invites.
- Logging in at Quartz's console after a reboot (§4.1).

### Non-goals for v1

Android; multiple build Macs; App Store submission and review; external TestFlight groups;
parallel jobs; Expo/React Native (all four apps are native SwiftUI now, BodyDiff since
2026-10-14); git-ref builds; registering bundle IDs or capabilities; any computer use; keeping an
Apple ID web session alive; isolating one app's builds from another's credentials (§14).

## 3. Ground truth this plan builds on

| Fact | Source |
|---|---|
| Driver, BodyDiff and Lineage sign with the Apple ID logged in to Xcode (`-allowProvisioningUpdates`). ScreenGate's script defaults to API-key auth, which fails cloud signing because key `J9584QW2TF` is not Admin; its working uploads used `ASC_AUTH=xcode` | `driver/scripts/testflight.sh`, `bodydiff …/scripts/ios.sh`, `lineage/scripts/upload-testflight.sh`, `screen-gate/scripts/testflight.sh`, mined failures |
| Export options differ per app: Lineage sets `uploadSymbols=true` and no internal-only flag; BodyDiff sets `testFlightInternalTestingOnly=true` | `lineage/Config/ExportOptions.plist`, `bodydiff …/scripts/ios.sh` |
| Xcode-account signing has worked from `gui-run.sh`'s one-off launchd job in `gui/<uid>`; Indentured lanes (root security session) can't see the login keychain | `screen-gate/scripts/gui-run.sh`, `bodydiff-testflight/docs/testflight.md` §3 |
| Issuer ID `25a2d76f-91fe-49e0-9551-09af06f209dc`, team `2559B88H6C` | `bodydiff …/scripts/ios.sh` |
| A manual-signing route (dedicated keychain, App Store profile, API-key upload) and the exit-code contract 0/1/65/69/75/78. The manual route is **designed but unverified** | `bodydiff-testflight/docs/testflight.md`, `scripts/ios-testflight.sh` |
| ASC state handling already written and tested offline: `processingState` `PROCESSING/FAILED/INVALID/VALID`, `internalBuildState` incl. `MISSING_EXPORT_COMPLIANCE`, `PROCESSING_EXCEPTION`, `IN_EXPORT_COMPLIANCE_REVIEW`, `EXPIRED`; `buildUploads` lookup; "FAILED only when every upload record failed" | `app-store-connect.mjs:150-230` |
| A duplicate (ITMS-90189 / "already been uploaded") on a retry means an earlier attempt landed | `ios-testflight.sh:496-498` |
| Toolchain: unset Nix's `DEVELOPER_DIR`/`SDKROOT`/compilers, then set `DEVELOPER_DIR=$(xcode-select -p)`; `/usr/bin` first (Nix rsync breaks IPA export); `LANG`/`LC_ALL=en_US.UTF-8`; fresh simulator per test run | `lineage/scripts/check-ios.sh:9-16,67` |
| UTC-minute build numbers (`%Y%m%d%H%M`) are in use and accepted | ScreenGate/Driver/BodyDiff |
| Quartz: Xcode 27.0 (27A266a), macOS 26.6, nix-darwin, FileVault on, no auto-login, Go not on the global PATH. Tailnet ACL allows only TCP 443 and 22 to Quartz; 443 is owned exactly by Indentured's Tailscale Serve route; T3 is on `:10000` | checked on Quartz; `mono-nix/hosts/quartz/README.md`; `bodydiff-testflight/docs/testflight.md:277` |
| Slipway's pattern: one binary, `serve` on a host behind a Tailscale Service (`svc:slipway`, own VIP on :443), bearer tokens, `login`/`doctor`, ignore-file uploads, an agent skill, a flake consumed by mono-nix, and an explicit trust model | `~/Development/projects/slipway/README.md` |
| Agent tokens are spent per turn, not per second of waiting: one long blocking tool call is free compared with repeated polling turns | dossier |
| Each machine (Quartz, Lattice, Prism) runs its own T3 server; Quartz cannot SSH to Lattice | Driver brain commit `a94b285`; M1 checks |

## 4. Architecture

```
 Lattice / laptop / Quartz                     Quartz (logged-in GUI session)
 ┌──────────────────────┐   HTTPS (tailnet)   ┌─────────────────────────────────────────────┐
 │ eve submit …         │ ──────────────────▶ │ svc:eve :443 ─▶ 127.0.0.1:4790               │
 │ eve wait <id>        │ ◀── SSE events ──── │ eve serve (LaunchAgent, gui/<uid>)           │
 │ agent skill          │                     │  ├─ API (net/http), bearer tokens            │
 └──────────────────────┘                     │  ├─ store (SQLite WAL: jobs, events, outbox) │
          ▲                                   │  ├─ worker (one job, own process group)      │
          │ (optional human webhook)          │  │   ├─ source: verified tar.zst, APFS clone  │
          └───────────────────────────────────│  │   ├─ pipeline: testflight | device | check │
                                              │  │   └─ asc: reconcile, process, distribute   │
                                              │  ├─ watcher (ASC processing, off the worker) │
                                              │  ├─ classify: rules → optional Luna          │
                                              │  └─ notify: outbox → human webhook           │
                                              └─────────────────────────────────────────────┘
```

### 4.1 Process and session

- `eve serve` runs as a **LaunchAgent** in `gui/<uid>` with `LimitLoadToSessionType=Aqua`,
  `KeepAlive=true`, `ProcessType=Interactive`. Installed through mono-nix
  (`launchd.user.agents.eve` in `hosts/quartz`); `eve agent install` is the non-Nix
  fallback.
- **Aqua is an execution context, not a signing guarantee.** Signing additionally needs an
  unlocked login keychain, private-key access for `codesign`, and a valid Xcode account session.
  None of these is assumed: M0's spike proves an archive+export from the installed agent, and
  `doctor --host` checks the keychain state and the age of the last successful signing.
- **Child environment** (allowlist, nothing inherited from Nix): `HOME`, `USER`, `TMPDIR`,
  `PATH=/usr/bin:/bin:/usr/sbin:/sbin:<eve tools>`, `DEVELOPER_DIR=$(xcode-select -p)` (or the
  pinned Xcode), `LANG=LC_ALL=en_US.UTF-8`; `SDKROOT`, `CC`, `LD` and the other compiler variables
  are never set.
- **No shell startup files, ever.** Eve execs tools directly. `pre:` and `exec` commands run
  under `/bin/zsh -f` (or `/bin/sh`), never a plain `zsh`: nix-darwin's `/etc/zshenv` is read by
  every non-interactive zsh and puts Nix's rsync back in front of `/usr/bin`, which breaks IPA
  export (M0 spike 1 reproduced exactly this from a LaunchAgent whose plist PATH was clean).
  The pipeline asserts `command -v rsync` resolves to `/usr/bin/rsync` before exporting.
- **Reboots.** Decision (§18): Quartz logs in automatically so Eve returns after unattended
  reboots. macOS refuses automatic login while FileVault is on (`sysadminctl -autologin status`
  on Quartz: "Automatic login is disabled because FileVault is enabled"), so this means turning
  FileVault off on Quartz first, an operator step in mono-nix and System Settings, not something Eve
  changes. If FileVault stays on, nothing runs after a cold boot until someone types the password
  at the pre-boot screen. In both cases the CLI reports
  `server unreachable: Quartz may need a console login` when the tailnet node answers but the
  service does not.
- **Process ownership.** Each job's commands run in their own process group; the pgid is stored on
  the job. On start, `serve` kills any surviving group from a previous run (after checking the
  pgid still belongs to a Eve child) **before** recovering jobs, so a requeued job never races
  an orphaned xcodebuild for DerivedData or provisioning state.

### 4.2 Network and auth

- `eve serve` listens on `127.0.0.1:4790` only.
- It is published as a **Tailscale Service `svc:eve`** on :443 (its own VIP and
  `eve.<tailnet>.ts.net` hostname), as slipway does, because the tailnet ACL only admits 443
  and 22 to Quartz and Quartz's own :443 route belongs to Indentured (decided, §18). Creating the
  Service and approving Quartz as its host is an operator step (mono-nix + Tailscale admin),
  listed in M1. Quartz's Serve state is owned exclusively by Indentured's root reconciler
  (`indentured-server-serve`, reverts drift every 60 s), so the `svc:eve` route must be added to
  that reconciler's owned state in mono-nix; a hand-made `tailscale serve` would be removed.
- **Authorization is the bearer token, nothing else.** `Tailscale-User-Login` is recorded on each
  job for the audit log but never trusted: any local process can reach loopback and forge it.
  Owner token created on first `serve` (`~/.local/share/eve/token`, 0600, never leaves
  Quartz); named tokens per machine (`eve token create lattice`), stored hashed, revocable.
  Clients keep their token in a 0600 file (`~/.config/eve/token`), as slipway does.
- Nothing is exposed to the internet. Eve does not receive webhooks; it only sends them.

### 4.3 Storage layout

```
~/.config/eve/server.yml          server config (apps allowlist, ASC key refs, notify allowlist)
~/.config/eve/client.yml          client config (server URL) — any machine; token in ./token (0600)
~/.local/share/eve/
  eve.db                          SQLite (WAL): jobs, events, outbox, build_numbers, tokens
  eve.lock                        flock shared by `serve` and `eve run` for the allocator and worker
  sources/<sha256>.tar.zst        content-addressed uploads, pruned after 7 days
  derived/<app>/<verb>/           persistent DerivedData per app and verb
  jobs/<id>/                      workspace (APFS clone), logs/, artifacts/, secrets.xcconfig (0600), result.json,
                                  bundle-manifest.json.zst (every file in the .app with its size)
  baselines/<app>.json.zst        manifest of the last accepted TestFlight bundle (size guard, §6.7)
  tools/xcodegen-<ver>/           pinned, sha256-verified tools
```

Retention: logs and result.json for the last 50 jobs per app; archives for 7 days, except the last
successful TestFlight archive per app.

## 5. Jobs

### 5.1 State machine

```
queued → preparing → building
   testflight: → archiving → measuring ─┬──────────────────────────→ delivering → processing → distributing → succeeded
                                        └→ awaiting_confirmation ─┘  (or cancelled on reject / 24 h timeout)
   device:     → installing ⇄ waiting_for_device → launching → succeeded
   check:      → testing → succeeded
terminal: succeeded | failed | rejected | needs_human | unavailable | cancelled | interrupted
```

- `delivering` is **one phase** for xcode-account signing: `xcodebuild -exportArchive` with
  `destination=upload` signs and uploads in a single call, so there is no observable boundary
  between export and upload. (Manual signing, M7, splits it into `exporting` and `uploading`.)
- Every transition is committed with its `events` row **and**, for terminal states, its `outbox`
  rows in one SQLite transaction, then fanned out over SSE.
- `needs_human` carries `{action, detail, url?}`: `unlock_phone`, `xcode_sign_in`,
  `accept_agreement`, `export_compliance`, `create_app_record`, `console_login`.
  `accept_testflight_invite` is a note on a `succeeded` result, not a failure.
- `waiting_for_device` is entered only **after** the build, so unlocking resumes at install.
- `awaiting_confirmation` is the size guard's pause (§6.7). It is non-terminal, parks the job off
  the worker with its archive kept, and ends the agent's `eve wait` (exit 76) with the size report. Only
  `eve confirm` moves it on; `eve reject` or 24 h without an answer ends it `cancelled`.

### 5.2 Outcomes and exit codes

The job's state and the client's wait are separate. `eve wait`/`submit --wait` exit with:

| Exit | Meaning |
|---|---|
| 0 | `succeeded` (ready for testers / installed and launched / checks passed) |
| 1 | `failed` (see `failure.class`) |
| 64 | Usage error |
| 65 | `rejected` (processing `INVALID`/`FAILED`, or Apple refused the upload) |
| 69 | `unavailable` (Apple, network, device service or server unreachable) |
| 75 | The wait's own `--timeout` elapsed; **the job is still running** (e.g. processing). Not a job state |
| 76 | Confirmation required: the job is `awaiting_confirmation`, or `submit` refused the source upload (§6.7). Inspect `size`, then `eve confirm`/`eve reject`, or resubmit with `--accept-source-size` |
| 78 | `needs_human` (`human.action`) |
| 130 | `cancelled` |

Processing has a durable deadline (default 4 h, persisted on the job); exceeding it ends the job
`failed` with `asc.processing_deadline`.

### 5.3 Result document (what an agent reads)

```json
{
  "id": "j_01JB…", "app": "bodydiff", "verb": "testflight",
  "outcome": "failed", "phase": "delivering", "durationS": 412,
  "source": {"sha256": "…", "commit": "3f2c1ab", "dirty": true},
  "build": {"version": "1.0", "number": "202610161512", "ascBuildId": null},
  "failure": {
    "class": "signing.cloud_permission",
    "summary": "Export needs cloud-managed distribution signing, which this API key can't use.",
    "hint": "This app is configured for api-key signing; set signing: xcode-account (see doctor).",
    "excerpt": ["error: exportArchive Cloud signing permission error …"],
    "by": "rules",
    "log": "eve logs j_01JB… --phase delivering --errors"
  },
  "human": null,
  "size": {"app": "48.2 MB", "files": 312, "delta": "+0.3 MB", "baseline": "202610151108", "gate": "passed"},
  "notes": [],
  "artifacts": ["archive.log", "delivery.log"]
}
```

Hard cap 4 KB, excerpt ≤ 40 lines, known secret values redacted. When the size guard trips,
`size` carries the full report instead (§6.7.3).

### 5.4 Idempotency

- The **CLI generates** the idempotency key (a UUID) once per submission intent and prints it with
  the job id; an agent retrying a lost response passes the same `--idempotency-key` and gets the
  same job. Without an explicit key, the server only deduplicates against **non-terminal** jobs
  with the same app, verb and source digest, so resubmitting after unlocking the phone or
  accepting an agreement starts a new job.

### 5.5 Restart recovery

On start, after killing orphaned process groups (§4.1), each non-terminal job resumes by phase:

| Phase at crash | Recovery |
|---|---|
| `queued`, `preparing`, `building`, `archiving`, `measuring`, `testing` | Requeue at the front, same build number. A second interruption in the same phase ends `interrupted` |
| `awaiting_confirmation` | Stay parked with the persisted 24 h deadline; nothing re-runs |
| `delivering` (upload intent was persisted before the call) | **Reconcile**, never blindly re-run: poll ASC `builds` and `buildUploads` for this app, platform, version and build number for up to 10 minutes. Found (or an upload record not `FAILED`) → `processing`. All upload records `FAILED` → re-run delivery from the kept archive. Nothing visible after 10 minutes → re-run delivery once; a duplicate error then means the earlier attempt landed → `processing` |
| `processing`, `distributing` | Resume the watcher with the persisted deadline |
| `installing`, `waiting_for_device`, `launching` | Resume at install (idempotent) |

The same reconciliation runs when a live `delivering` call fails with an ambiguous error (network
drop, altool-style false result).

### 5.6 Concurrency

One worker, one job at a time. Jobs in `processing`/`distributing` move to a watcher goroutine and
release the worker. `eve run` (on Quartz) takes the same `eve.lock`, so a local run and the
server never build at once.

## 6. Verbs and the pipeline

All steps go through one `Runner` interface (command, args, env, cwd, process group, log sink,
timeout, cancellation). Every xcodebuild call writes its full log to `jobs/<id>/logs/<phase>.log`.

### 6.1 Source

- The client packs `git ls-files -co --exclude-standard` (a directory walk outside git), minus
  `.eveignore` and an always-excluded list (`build/`, `DerivedData/`, `.tools/`,
  `node_modules/`, `.env*`, `*.p8`, `*.p12`, `*.mobileprovision`, `*.pem`, `*.key`), as
  `tar.zst`, sends `HEAD /v1/sources/<sha256>`, and `PUT`s only if the server lacks it.
  Provenance (commit, dirty flag, branch) travels with the job. This replaces Driver's `sync.sh`.
- Before uploading anything, the client runs the **source check** (§6.7.1) and refuses with exit
  76 if the tree looks like an incomplete `.gitignore`.
- **Extraction is hostile-input safe:** reject absolute paths, `..`, device files and symlinks that
  resolve outside the root; caps of 2 GiB decompressed, 200k entries, 1 GiB per file; the server
  re-checks the excluded secret patterns.
- The workspace is an APFS clone of the unpacked source.
- `--path DIR` builds an existing checkout in place, accepted only from the owner token on Quartz.

### 6.2 Common preparation

1. Load `eve.yml` from the workspace; merge the server-side app config, which is authoritative
   for identity, signing, export options and secrets.
2. Check the selected Xcode against `xcode:` if pinned and that the license is accepted.
3. Generate the project with the pinned XcodeGen if `xcodegen:` is set.
4. Write secrets the app needs as build settings into `jobs/<id>/secrets.xcconfig` (0600) and pass
   `-xcconfig`, so they appear neither in `ps` nor in xcodebuild's "Build settings from command
   line" header. Redaction of known secret values applies to stored logs, served logs and the
   result document.
5. Run `pre:` commands (e.g. `swift test --package-path BodyDiffCore`).

### 6.3 `testflight`

1. **Build number.** Allocated in one transaction under `eve.lock`:
   `max(UTC %Y%m%d%H%M, last allocated + 1, latest ASC build number for this version + 1)` —
   the ASC term is fetched at allocation and cached for 10 minutes, so uploads made outside Eve
   (old scripts, Xcode Organizer) cannot collide. `--build-number` is validated against the same
   floor and reserved. Passed as `CURRENT_PROJECT_VERSION`.
2. **Archive** (`generic/platform=iOS`, `-allowProvisioningUpdates` for xcode-account). Afterwards,
   read `CFBundleVersion` from the app and every embedded extension in the archive and fail with
   `build.number_mismatch` unless all equal the allocated number (extensions with hardcoded
   versions were a real 90189 cause).
3. **Measuring.** Run the bundle check (§6.7.2) on the archived `.app`. Passing goes straight to
   delivering; a trip parks the job in `awaiting_confirmation` until the agent answers. Nothing
   reaches Apple before this step passes or is confirmed.
4. **Delivering.** Persist upload intent (app, version, number, archive path) and then run
   `-exportArchive` with ExportOptions from config: `method=app-store-connect`,
   `destination=upload`, `signingStyle=automatic`, `manageAppVersionAndBuildNumber=false`,
   `uploadSymbols` (default true) and `testFlightInternalTestingOnly` (per app; default true
   for every app, §18). Success is never taken from the exit code alone: it is
   confirmed through ASC (§5.5). A duplicate error is treated as "already uploaded" and goes to
   reconciliation, not to failure.
5. **Processing** (watcher). Poll every 30 s, backing off to 2 min after 10 min, using the ported
   mapping: `VALID` → distributing; `INVALID` or `FAILED` → `rejected` with Apple's reasons;
   `PROCESSING` → keep waiting.
6. **Distributing.** `internalBuildState`:
   `READY_FOR_BETA_TESTING`/`IN_BETA_TESTING` → add to the configured internal group unless it
   distributes automatically, set "What to Test" from `--notes` or the commit subject (a role
   failure here becomes a note, not a failure) → `succeeded`;
   `MISSING_EXPORT_COMPLIANCE` → `needs_human: export_compliance`;
   `IN_EXPORT_COMPLIANCE_REVIEW` → keep waiting; `PROCESSING_EXCEPTION` → `rejected`;
   `EXPIRED` → `failed`. Refuse to distribute a build ASC reports as store-eligible when the app is
   configured internal-only. If the configured tester's invite is unaccepted, add the note
   `accept_testflight_invite`.

### 6.4 `device`

1. Pick the device: `--device`, the app's default, the server default, or the first paired
   physical iPhone (`devicectl list devices --json-output`).
2. **Build first** (`-destination id=<udid> -allowProvisioningUpdates
   -allowProvisioningDeviceRegistration`), so the wait for the human happens after the slow part.
3. Check availability and lock state (Xcode 27's `devicectl device info lockState` JSON; the exact
   fields are confirmed in M2). If not ready, enter `waiting_for_device`, notify the agent (and
   the human webhook, if set) once, and poll every 5 s for up to 10 min, then
   `needs_human: unlock_phone`.
4. Re-check immediately before install and before launch (the phone can relock during a build);
   install, then launch with `--terminate-existing`.
5. Failure classes: `device.locked` (FBSOpenApplicationErrorDomain 7), `device.unavailable`,
   `device.untrusted_developer`, `device.attestation` ("Unable to Verify App": hint restart the
   phone).

### 6.5 `check`

Simulator build signed ad hoc (`CODE_SIGN_IDENTITY=-`, manual style, no team;
`generic/platform=iOS Simulator`), plus the
scheme's tests if `check.test: true`, on a **fresh** simulator per run (created and deleted, as
`check-ios.sh` does). The result carries test counts and the first failing test's name and
assertion, never the accessibility dump. Not `CODE_SIGNING_ALLOWED=NO`: unsigned apps lose
their entitlements, so the Simulator keychain refuses them (Driver's `keychainRoundTrip` failed).
Ad-hoc signing needs no account or keychain. Tests run with `-collect-test-diagnostics never`:
otherwise any failing test makes xcodebuild sysdiagnose the Simulator for up to ten minutes
(seen on Driver: 0.3 s of tests, 11 minutes of job). Simulators are named `eve-<job>`; any left by a
crash are deleted before the next run. `check.simulator` in eve.yml picks the device type.

### 6.6 `exec`

`eve exec [<app>] -- <command…>` runs a repo command in the job workspace with the child
environment, inside the Aqua session, with `$EVE_ARTIFACTS` exported. It retires `gui-run.sh`
and covers ScreenGate's `screenshots.sh` and Lineage's `check-ios.sh`. Artifacts are listed in the
result, never inlined. It adds no new trust (§14), so it needs only `allowExec: true` on the app to
avoid accidents.

### 6.7 Size guard

The failure this prevents has happened to the user several times: an incomplete `.gitignore`,
a folder reference, or an XcodeGen `sources:` that reaches too far, and a whole repository gets
uploaded to Quartz and then packaged into the app sent to Apple. Eve catches it at two points,
and in both cases **the requesting agent must confirm the increase before Eve accepts it.**

#### 6.7.1 Source check (client, before upload)

`eve submit` measures the tree it is about to pack (bytes, file count, size per top-level
directory) and compares it with the last accepted source for the app, which the server returns
from `GET /v1/apps/{app}/source-stats`. It refuses, exits 76 and prints a short report when:

- the tree is larger than `max(1.5 × last, last + 20 MB)` or has 50 % more files;
- it is over the absolute ceiling (default 500 MB);
- it contains directories that are almost never source: `node_modules/`, `DerivedData/`, `.build/`,
  `build/`, `Pods/` (allowed per app), `.git/` (always excluded anyway), or files over 50 MB.

The report lists the five largest top-level directories with their growth and suggests the
`.gitignore`/`.eveignore` line that would exclude each new one. The agent then fixes the ignore
rules and resubmits, or resubmits with `--accept-source-size "<reason>"`. The check runs before
anything is uploaded, so a bad tree costs neither upload time nor a build. The first submit for an
app has no baseline and is checked only against the ceiling and the directory list.

#### 6.7.2 Bundle check (server, after archive, before delivery)

After archiving, Eve walks `Products/Applications/<App>.app` in the archive and records every
file with its size (`bundle-manifest.json.zst`). It compares this with the app's **baseline**, the
manifest of the last TestFlight build that was accepted (delivered without a trip, or confirmed).
Rules, in order:

1. **Secret material in the bundle** (`*.p8`, `*.p12`, `*.mobileprovision` other than
   `embedded.mobileprovision`, `.env*`, `*.pem`, `*.key`, `AuthKey_*`): the job **fails** with
   `bundle.secret_material`. This cannot be confirmed away; the repo must change.
2. **Repository content in the bundle**, even without a baseline: `.git/`, `node_modules/`,
   `*.xcodeproj/`, `*.xcworkspace/`, `project.yml`, `Package.swift`, `Package.resolved`,
   `*.swift`/`*.m`/`*.mm` source files, `docs/`, `scripts/`, `build/`, `DerivedData/`, `.DS_Store`,
   `README*`/`*.md` at the bundle root. Trips the gate.
3. **Growth against the baseline**, any of:
   - total size grows by more than `max(15 %, 5 MB)`;
   - file count grows by more than `max(20 %, 200 files)`;
   - a new top-level **directory** appears in the bundle root (other than `PlugIns/` extensions and
     `Frameworks/` entries the project declares);
   - a single new file is larger than 5 MB.
4. **Absolute ceiling**: the `.app` is larger than 300 MB (default).

All thresholds and allowed patterns are per-app settings in `server.yml` (§7.2), so an app that
legitimately ships `.md` help files or large media can say so once. Device builds run the same
measurement and report it in the result, but never pause: nothing leaves Quartz.

#### 6.7.3 Confirmation

When rules 2–4 trip, the job moves to `awaiting_confirmation` and the requesting agent is woken
(`job.awaiting_confirmation` on its notify URL; `eve wait` exits 76) with a compact report in the
result's `size` field:

```json
"size": {
  "gate": "awaiting_confirmation",
  "app": "214.6 MB", "baseline": "48.2 MB (202610151108)", "delta": "+166.4 MB", "files": "+9,412",
  "reasons": ["growth +345%", "repository content: 412 .swift files, docs/, project.yml"],
  "largest": [
    {"path": "BodyDiff.app/docs/", "size": "120.1 MB", "change": "new"},
    {"path": "BodyDiff.app/Assets.car", "size": "31.0 MB", "change": "+0.4 MB"}
  ],
  "hint": "The app bundle contains repository files. In project.yml, check `sources:` for a path
           that includes the repo root, or a folder reference; exclude docs/ and scripts/.",
  "details": "eve size j_01JB…"
}
```

`eve size <id>` prints the full diff against the baseline (added, removed and grown paths,
grouped by directory). The agent then does one of:

- `eve confirm <id> --reason "added onboarding videos (+18 MB)"`: the job continues to delivery
  from the kept archive, and on success its manifest becomes the new baseline. The reason is
  stored with the job and shown in `eve jobs`.
- `eve reject <id>`: the job ends `cancelled`; the agent fixes the project and resubmits.
- Neither within 24 h: the job ends `cancelled` with `size.confirmation_timeout`.

`eve submit testflight --expect-size-change "<reason>"` pre-approves growth (rules 3 and 4) for
one job when the agent already knows the change adds weight. Repository content (rule 2) always
asks, and secret material (rule 1) always fails.

The first TestFlight build of an app has no baseline, so it is checked only against rules 1, 2
and 4, and its manifest becomes the baseline. `eve baseline <app> --from <job>` resets it
explicitly.

### 6.8 Diagnostics

- `eve doctor`: reachability (distinguishing "tailnet up, service down → console login?"),
  token, version skew.
- `eve doctor --host`: GUI session present, login keychain unlocked, codesigning identities and
  expiry (warn at 30 days), WWDR G3 present, Xcode version and license, **time since the last
  successful signing** (there is no cheap "Xcode account signed in" probe; warn after 14 days),
  ASC key valid and agreements accepted (403 `REQUIRED_AGREEMENTS_MISSING_OR_EXPIRED`), devices
  paired, disk space, Metal toolchain, `/usr/bin/rsync` first.
- New-app bootstrap stays a human checklist in `docs/new-app.md` for v1 (all four apps exist).

## 7. Configuration

### 7.1 Repo: `eve.yml`

```yaml
app: bodydiff                 # must match an allowlisted app on the server
project:
  xcodegen: project.yml       # or: xcodeproj: Lineage.xcodeproj
  scheme: BodyDiff
xcode: "27.0"                 # optional pin
pre:
  - swift test --package-path BodyDiffCore
check: {test: false}
device: {configuration: Debug}
```

### 7.2 Server: `~/.config/eve/server.yml`

```yaml
team: 2559B88H6C
asc:
  keyId: J9584QW2TF
  issuerId: 25a2d76f-91fe-49e0-9551-09af06f209dc
  keyPath: ~/.appstoreconnect/private_keys/AuthKey_J9584QW2TF.p8
apps:
  bodydiff:
    ascAppId: "…"
    bundleId: ai.selfdeprecated.bodydiff
    signing: xcode-account
    export: {internalOnly: true, uploadSymbols: true}
    testflight: {group: Internal, tester: me@example.com}
    secrets:                  # written to the job's secrets.xcconfig
      BODYDIFF_COACH_CODE: file:~/.config/bodydiff/coach-code
  driver:     {bundleId: …, signing: xcode-account, export: {internalOnly: true}}
  lineage:    {bundleId: …, signing: xcode-account, export: {internalOnly: true, uploadSymbols: true}}
  screengate: {bundleId: …, signing: xcode-account, allowExec: true}
sizeGuard:                    # defaults; any app may override under apps.<app>.sizeGuard
  source: {growth: 50%, growthMin: 20MB, files: 50%, ceiling: 500MB, allowDirs: []}
  bundle: {growth: 15%, growthMin: 5MB, files: 20%, filesMin: 200, newFile: 5MB, ceiling: 300MB,
           allowPatterns: []}  # e.g. ["*.md"] for an app that ships help pages
  confirmTimeout: 24h
device: {default: 00008150-…}
notify:
  allowHosts: ["*.tail6e682f.ts.net"]   # --notify-url webhooks must match
  human: {url: …}             # optional (e.g. a GrokBot webhook)
luna: {enabled: false}
```

## 8. Following jobs

v4 (§19): the client is the stream. Agents are not called back; they wait with one long,
cheap tool call. No harness-specific login, thread ids or callbacks.

### 8.1 `eve wait` and `eve submit --wait`

- Holds one SSE connection from wherever the CLI runs (Lattice, Quartz, a laptop) to the server
  on Quartz, with a heartbeat every 15 s.
- **Reconnects forever** while the server is unreachable or restarting, with `Last-Event-ID`, so
  events are neither lost nor repeated (backoff 1 s → 30 s, reset after a good connection). It
  stops only on answers that will not change (bad token, unknown job) or at `--timeout`.
- **Default mode:** progress lines on stderr, the result document on stdout (`--json` for
  agents), exit code = outcome (§5.2). Ends on a terminal state or `awaiting_confirmation` (76).
- **`--events` mode:** every line on stdout stands alone, one per phase change
  (`eve <job> archiving: archiving build 202610161907`), then
  `eve <job> result <OUTCOME> <compact JSON>`. Made for harnesses that wake an agent per output
  line (Claude Code's `Monitor`), so an agent can react to a pause or a failure as it happens.
- A server shutdown (e.g. a switch restarting the LaunchAgent) leaves the running job for
  restart recovery; the waiting client reconnects and follows the resumed job.

### 8.2 How agents use it

- **Claude Code:** `eve submit testflight --wait --json` as a background command with a long
  timeout (it is woken when the command exits), or `Monitor` on `eve wait <job> --events` for
  milestone wake-ups.
- **Codex and others:** foreground, with a long command timeout. Blocking costs no tokens.
- If the waiting process dies (harness restart, session ended): `eve wait <job>` again replays
  and continues. **`eve pending`** lists jobs submitted from this machine whose outcome nobody
  collected (a local ledger in `~/.local/state/eve/jobs.json`, written by `submit`, cleared by
  `wait`/`status`/`pending` when they show a terminal outcome).

### 8.3 Webhooks (optional integrations only)

- `--notify-url <url>` (must match `notify.allowHosts`) POSTs the result document from the
  durable outbox, with `X-Eve-Delivery` for dedup and `X-Eve-Signature` (HMAC-SHA256) when a
  secret applies; retried with backoff for 24 h.
- `notify.human.url` (e.g. a GrokBot webhook) gets one-line messages for `needs_human`,
  `waiting_for_device` and exhausted deliveries. Size confirmations never go there.

## 9. Failure classification

Rules first, model second. `internal/classify` holds an ordered table of
`{class, match, summary, hint, human?}`, seeded from the mined failures:

| Class | Signal | Hint / human action |
|---|---|---|
| `signing.cloud_permission` | `Cloud signing permission error` | app configured for api-key signing; use xcode-account |
| `signing.no_team` | `requires a development team` | set team |
| `signing.device_unregistered` | device not in profile | retried automatically with device registration |
| `signing.entitlement_mismatch` | entitlements / app group mismatch | fix the App ID's capabilities (human, portal) |
| `signing.wwdr_expired` | expired intermediate / chain | install WWDR G3 (doctor) |
| `xcode.account` | Xcode account session expired / auth failed | `needs_human: xcode_sign_in` |
| `build.number_mismatch` | archive check (§6.3) | an extension hardcodes `CFBundleVersion`; use `$(CURRENT_PROJECT_VERSION)` |
| `env.rsync` | `exportArchive Copy failed` | environment regression (should be impossible) |
| `env.metal_toolchain` | Metal toolchain missing | `xcodebuild -downloadComponent MetalToolchain` |
| `upload.duplicate` | `ITMS-90189` / `already been uploaded` | not a failure: reconcile (§5.5) |
| `upload.missing_icon` | `152x152` / `CFBundleIcons` | add icon sizes |
| `upload.purpose_string` | `ITMS-90683` | add the named `NS…UsageDescription` |
| `upload.export_compliance` | `ITSAppUsesNonExemptEncryption` / 90592 | set the key, or `needs_human: export_compliance` |
| `asc.agreement` | 403 `REQUIRED_AGREEMENTS…` | `needs_human: accept_agreement` |
| `asc.auth` | 401 | key/issuer/clock (doctor) |
| `asc.processing_deadline` | durable deadline exceeded | check App Store Connect |
| `device.*` | §6.4 | §6.4 |
| `bundle.secret_material` | size guard rule 1 (§6.7.2) | remove the file from the target; cannot be confirmed |
| `size.confirmation_timeout` | no answer within 24 h (§6.7.3) | resubmit, then confirm or fix |
| `tests.pre` / `tests.failed` | pre-command or test failure | first failing test |
| `unknown` | nothing matched | Luna summary if enabled |

Fixtures: real logs harvested from `~/Developer/*/build/**` and `bodydiff/build/logs` into
`testdata/logs/`, redacted, each with its expected class.

**Luna (optional, off by default).** For `unknown` only: error lines plus the last 200 lines,
redacted (home paths, emails, tokens, UDIDs, secret values), sent to `gpt-6-luna` through
CLIProxyAPI (`https://cliproxyapi.tail6e682f.ts.net/v1`, key from `CLIPROXY_API_KEY`). Returns
`{summary, hint, confidence}` with `"by": "model"`. Never used for control decisions or retries.

## 10. CLI

```
eve login <url>                         store server URL; token into ~/.config/eve/token (0600)
eve doctor [--host]
eve submit <verb> [<app>] [--wait [--events] [--timeout D]] [--notify-url URL]... [--idempotency-key K]
           [--build-number N] [--notes TEXT] [--device UDID] [--release] [--path DIR]
           [--accept-source-size REASON] [--expect-size-change REASON]
                                        prints job id + key; <app> from eve.yml
eve wait <id> [--timeout D] [--events]  exit code per §5.2
eve size <id>                           size report and full diff against the baseline
eve confirm <id> --reason TEXT          accept a size-guard pause; delivery continues
eve reject <id>                         refuse it; the job ends cancelled
eve baseline <app> [--from <id>]        show or reset the app's size baseline
eve status [<id>] [--json]
eve logs <id> [--phase P] [--errors] [--tail N]
eve jobs [--app A] [--limit N]
eve cancel <id>
eve exec [<app>] -- CMD...
eve builds <app>                        latest ASC builds and states
eve token create|list|revoke            (server)
eve serve                               (server)
eve agent install|uninstall|status      (server, non-Nix)
eve run <verb> [<app>]                  in-process on Quartz, same lock and allocator (dev + break-glass)
eve version
```

`--json` everywhere. `submit` returns within a second of the upload finishing and prints:
`eve pending` lists jobs submitted from this machine whose outcome nobody has collected.

## 11. HTTP API (v1)

| Method | Path | Notes |
|---|---|---|
| `HEAD` / `PUT` | `/v1/sources/{sha256}` | 200/404; `PUT` body `application/zstd`, digest verified |
| `POST` | `/v1/jobs` | `{app, verb, source:{sha256\|path}, options, notifyUrls[], idempotencyKey}` → `201 {id}` (or `200` for an existing job) |
| `GET` | `/v1/jobs` | filters `app`, `state`, `limit` |
| `GET` | `/v1/jobs/{id}` | result document |
| `GET` | `/v1/jobs/{id}/events` | SSE, `Last-Event-ID`, 15 s heartbeat |
| `GET` | `/v1/jobs/{id}/logs/{phase}` | `?errors=1`, `?tail=N`, redacted |
| `GET` | `/v1/jobs/{id}/artifacts/{name}` | |
| `POST` | `/v1/jobs/{id}/cancel` | SIGINT to the process group, SIGKILL after 20 s; refused in `delivering` unless `?force=1` |
| `GET` | `/v1/jobs/{id}/size` | full size report and diff |
| `POST` | `/v1/jobs/{id}/confirm` | `{reason}`; only in `awaiting_confirmation` |
| `POST` | `/v1/jobs/{id}/reject` | only in `awaiting_confirmation` |
| `GET` | `/v1/apps`, `/v1/apps/{app}/builds` | |
| `GET` | `/v1/apps/{app}/source-stats` | last accepted source: bytes, files, size per top-level dir |
| `GET` / `PUT` | `/v1/apps/{app}/baseline` | show, or reset from a job |
| `GET` | `/v1/doctor`, `/v1/version` | |

## 12. Code layout

```
cmd/eve/                     main: cobra root, client and server subcommands
internal/cli/                commands, output, exit codes
internal/client/             HTTP client, source packing, SSE
internal/api/                shared request/response/result types
internal/server/             handlers, token auth, SSE fan-out
internal/store/              SQLite (modernc.org/sqlite), migrations, transactional outbox
internal/queue/              worker, lock, watcher, restart recovery, process groups
internal/source/             upload store, safe extraction, ignore rules, APFS clone
internal/pipeline/           testflight, device, check, exec; phases
internal/xcode/              xcodebuild, child env, xcodegen, xcconfig secrets, archive inspection
internal/devicectl/          discovery, lock state, install, launch
internal/asc/                JWT (ES256, stdlib), builds, buildUploads, betaDetail, groups, testers
internal/alloc/              build-number allocator
internal/sizeguard/          source stats, bundle manifests, baselines, rules, reports
internal/classify/           rule table, excerpts, optional Luna client
internal/notify/             outbox delivery to webhooks, HMAC, optional human webhook
internal/ledger/             local record of submitted jobs (eve pending)
internal/redact/             secret-value redaction for logs, results, artifacts
internal/launchd/            LaunchAgent plist and bootstrap
internal/config/             server.yml, client.yml, eve.yml
internal/run/                Runner interface, real and fake
skill/eve/SKILL.md           agent skill
docs/                        commands.md, decisions/, operations.md, new-app.md, troubleshooting.md
testdata/                    logs/, asc/ (ported recorded JSON), devicectl/
flake.nix, nix/              package + nix-darwin module
devenv.nix                   go, gopls, gofumpt, golangci-lint (as goput)
```

Dependencies: `spf13/cobra`, `modernc.org/sqlite`, `klauspost/compress/zstd`, `gopkg.in/yaml.v3`.
JWT and HMAC from the standard library. Go 1.26.

## 13. Agent skill (summary)

`skill/eve/SKILL.md`, installed through mono-nix like slipway's:

- "Ship to TestFlight / put it on my phone / check it builds": `eve submit <verb> --wait --json`
  from the repo root as one long-running (background) command, or `Monitor` on
  `eve wait <id> --events`. Never poll with `sleep`; never run xcodebuild for these yourself.
- If `submit` fails with a lost response, retry with the printed `--idempotency-key`.
- `submit` exits 76 on the source check: read the report. If the large new directories are not
  source, add them to `.gitignore`/`.eveignore` and resubmit; only if the growth is explained by
  the change you made, resubmit with `--accept-source-size "<why>"`. If you already know a change
  adds weight to the app, pass `--expect-size-change "<why>"`.
- Woken with `job.awaiting_confirmation`: read `size.reasons`, `largest` and `hint` (run
  `eve size <id>` if needed). Confirm **only** if every large or new path is something your change
  meant to ship, with a reason that names it: `eve confirm <id> --reason "…"`. Repository files
  in the bundle (`.swift`, `docs/`, `project.yml`, …) are never expected: `eve reject <id>`, fix
  the project's sources or resources, and resubmit. If unsure, ask the user before confirming.
- When it returns, read only the result. `needs_human`: tell the user `human.action` in one line and stop.
  `failed`: fix using `failure.class`/`hint`; fetch `eve logs --errors` only if the excerpt is
  not enough.
- New app: follow `docs/new-app.md` with the user.

## 14. Trust model

Stated plainly, as slipway does:

- **Every build runs repository code as the user, in the Aqua session, with the login keychain
  unlocked.** Xcode build phases, Swift package plugins and `pre:` commands can read every file the
  user can: other apps' secrets, the ASC key, the owner token. So **any token that can submit a
  build is equivalent to running code as the user on Quartz**, and can ship a binary signed as
  them to TestFlight. App scopes on tokens are API conveniences, not isolation; `allowExec` only
  prevents accidents.
- Therefore: few tokens, one per machine the user controls; revocation is instant; every job
  records the token name, the `Tailscale-User-Login` (unverified, for audit), the source digest and
  the commit.
- The listener is loopback-only behind `svc:eve`; nothing on the internet; Eve never
  accepts inbound webhooks.
- Secrets are read on the server, written only to the job's 0600 `secrets.xcconfig`, redacted from
  stored and served logs, results and artifacts, and never returned by the API.
- Uploaded archives are treated as hostile input during extraction (§6.1).
- Real isolation between apps (a separate macOS user per app, or manual signing with per-app
  keychains outside the user's home) is possible later and out of scope for v1.

## 15. Testing

- Unit: state machine and every row of §5.5's recovery table; transactional outbox (a crash
  between state change and delivery never loses an event); allocator floors and concurrent
  allocation from `serve` and `run`; idempotency against live vs. terminal jobs; safe extraction
  (traversal, symlinks, bombs); redaction; config validation.
- Classifier: every fixture maps to its class; unknown logs stay `unknown`.
- ASC client: recorded JSON and the offline cases from `app-store-connect.mjs`, ported; JWT golden
  test; visibility lag in reconciliation; 401/403/429.
- Pipeline: fake `Runner` asserting exact command lines and environments per verb (DEVELOPER_DIR
  from xcode-select, `/usr/bin` first, `-xcconfig` for secrets, ExportOptions per app).
- Process groups: a test child that ignores SIGTERM is killed on cancel and on restart.
- Size guard: synthetic `.app` trees and source trees for every rule (secret material fails;
  repository content trips without a baseline; each growth threshold trips at its boundary and
  not below; per-app allow patterns suppress; `--expect-size-change` skips growth but not repo
  content); confirm/reject/timeout transitions; baseline updated only after a confirmed or clean
  delivery succeeds; report stays under its size cap with 10k changed files. Fixtures include
  real manifests of the four apps' current archives.
- Integration on Quartz, per milestone, with the real apps.

## 16. Milestones

Each milestone ends with its acceptance run on Quartz and an entry in `CHANGELOG.md`.

**M0 — Spikes. Done 2026-10-16.**
- *Signing from the agent:* **passed** (`spike/m0-signing/RESULT.md`). A LaunchAgent in Aqua
  archived and exported a correctly signed Driver IPA with no GUI interaction. Produced the
  no-startup-files rule (§4.1).
- *T3 wake-up:* webhook tasks unavailable (no T3 Connect); local MCP + OAuth looked viable
  (`spike/m0-wakeup/RESULT.md`), but was **dropped in v4** for client-side streaming (§8).

**M1 — One durable TestFlight path (4–5 days).** Scaffold (devenv, flake, cobra, config);
`serve`, SQLite store, worker, process groups and orphan cleanup; source upload with safe
extraction; `testflight` with xcode-account: allocator, archive and version check, `delivering`
with upload intent and reconciliation; ASC client (port): processing, distribution, invite note;
transactional outbox and optional webhooks; resumable `eve wait` (`--events`) and `eve pending`; `submit`/`wait`/`status`/`logs`/`cancel`/`jobs`;
size guard (source check, bundle check, `awaiting_confirmation`, `size`/`confirm`/`reject`,
baselines seeded from each app's current archive); tokens and `login`; LaunchAgent via mono-nix;
`svc:eve` (operator step); classifier with the seeded rules; `eve run testflight`.
*Accept:*
1. From Lattice, `eve submit testflight --wait` in the Driver repo puts a build in TestFlight
   with no `sync.sh`, and returns the result.
2. Killing `eve serve` during `archiving` requeues and finishes with the same build number;
   killing it during `delivering` produces exactly one build in ASC and a `succeeded` job.
3. Restarting `eve serve` mid-build: the waiting client reconnects and gets the result (done in a live test, 2026-10-16).
4. A forced cloud-signing failure (ScreenGate with `signing: api-key`) yields
   `signing.cloud_permission` in the result, not a log dump.
5. Size guard, on a scratch branch of Driver: deleting a `.gitignore` line so `build/` is packed
   makes `submit` exit 76 before uploading; adding the repo root to a target's resources parks the
   job in `awaiting_confirmation`, the agent is woken with a report naming the new paths, and
   nothing reaches App Store Connect until `eve confirm`; `eve reject` ends it cleanly.

**M2 — Device (1–2 days).** `device` verb: build first, lock-state check, `waiting_for_device`,
re-checks, human notification, device classes.
*Accept:* submitting `device` with the phone locked builds, notifies once, and **resumes install
within 10 s of unlocking**; relocking during the build is handled.

**M3 — Check and exec (1–2 days).** `check` with fresh simulators and test summaries; `exec`
(retire `gui-run.sh`).
*Accept:* `check` on Lineage reports the first failing test without the accessibility dump;
ScreenGate's `screenshots.sh` runs via `exec` from Lattice and returns its artifacts.

**M4 — Skill, doctor, docs (1–2 days).** `skill/eve`, `doctor`/`doctor --host`,
`docs/new-app.md`, operations and troubleshooting docs; migrate BodyDiff, Lineage and ScreenGate
(`eve.yml`, retire their upload scripts or reduce them to `eve run` wrappers).
*Accept:* an agent ships ScreenGate in two turns with no polling calls; `doctor --host` flags a
locked login keychain and a missing GUI session.

**M5 — Hardening (1 day).** Retention and pruning, exhausted-delivery reporting, version skew,
`--path`, load test of the allocator and outbox.

**M7 — Later, each independent and on demand.** Manual signing (dedicated keychain, API-key upload,
`exporting`/`uploading` split; verify the unverified bodydiff-testflight design first); Luna
fallback; `screenshots` verb; `app init` (bundle IDs, capabilities, record checks); git-ref builds;
parallel simulator checks with resource locks; ASC webhooks via Funnel; per-app isolation.

Estimated v1 (M0–M5): ~2 weeks of agent-assisted work.

## 17. Risks

| Risk | Mitigation |
|---|---|
| Xcode account session expires; xcode-account signing stops | `xcode.account` → `needs_human`; doctor warns on signing age; manual signing (M7) |
| No GUI session after reboot | Auto-login (requires FileVault off, §18); clear client error otherwise |
| Size guard too noisy, agents learn to confirm everything | Thresholds per app; reasons recorded and listed; repo content never pre-approvable; skill says reject repo files; tune from real data in M5 |
| Size guard misses a regression below thresholds | Every result reports size and delta; the baseline only moves on accepted builds |
| The waiting process dies with its harness | The job is durable; `eve wait` replays; `eve pending` surfaces uncollected outcomes |
| Crash during delivery duplicates or misreports an upload | Upload intent, reconciliation window, duplicate = landed (§5.5); acceptance test in M1 |
| Builds compete with the user's own Xcode use | Eve-owned DerivedData and simulators; one job at a time |
| Token leak = code execution as the user | §14: few tokens, revocation, audit trail |
| Apple changes ASC states or devicectl JSON | Ported fixtures, tolerant parsing, unknown states surfaced, not guessed |
| Over-building | M0 spikes and a single vertical slice first; M7 only on demand; `eve run` useful from M1 |

## 18. Decisions

### Decided (2026-10-16)

1. **Name: Eve**, the one who takes the apple. Binary `eve`, module `github.com/self-deprecated/eve`,
   Tailscale Service `svc:eve`, config under `~/.config/eve`, data under `~/.local/share/eve`.
2. **Network publication: Tailscale Service `svc:eve`** on :443, as slipway does.
3. **Auto-login on Quartz: yes**, so Eve comes back after unattended reboots. macOS disables
   automatic login while FileVault is on (verified on Quartz with `sysadminctl -autologin
   status`), so this requires **turning FileVault off on Quartz** first. Trade-off for the user to
   accept at that step: Quartz's disk is no longer encrypted at rest, and after a reboot the login
   keychain (with the Xcode account and signing identities) unlocks without anyone present.
   Physical security of the Mac Mini becomes the control. Operator steps: disable FileVault
   (`fdesetup disable`, wait for decryption), then enable auto-login for `jmo` (System Settings or
   `sysadminctl -autologin set`), then verify a cold reboot brings `eve serve` up.
4. **Human channel: agents first.** Eve is a tool for agents; `needs_human` goes to the requesting
   agent, which relays it. An optional human webhook (e.g. a GrokBot URL) gets one-line copies of
   `needs_human`, `waiting_for_device` and exhausted deliveries (§8.1a).
5. **Size guard** on source uploads and app bundles, with agent confirmation (§6.7).

6. **Every app is internal-only** (`testFlightInternalTestingOnly=true`), Lineage included.
   Checked in App Store Connect on 2026-10-16: Lineage, Driver, ScreenGate, BodyDiff, Barbican and
   Quoth have only an internal group and a never-submitted 1.0. Agent Tick is live on the App Store
   with an external group, which is why internal-only is the default and must be set explicitly per
   app to change.
7. **The per-repo upload scripts are retired once Eve works** (end of M4); until then they stay as
   the fallback.
8. **The human webhook is configuration** (`notify.human` in `server.yml`), supplied by the user.
9. **Eve is installed through `mono-nix`** (`hosts/quartz`, nix-darwin): the flake exports the
   package and a darwin module, `services.eve`, that writes `server.yml` from Nix settings
   (secrets as file paths), installs the LaunchAgent (`launchd.user.agents.eve`, Aqua), and puts
   the `eve` CLI and the agent skill on Quartz and Lattice. `eve agent install` remains only for
   non-Nix machines.

Also in the account and candidates for later onboarding: Barbican, Quoth, Agent Tick.

## 19. Review log

### v3 → v4 (user decision, 2026-10-16)

- Dropped T3 delivery (the MCP/OAuth client, `eve t3 login`, `--notify-t3`, the relay idea). Each
  machine runs its own T3 and Quartz cannot reach Lattice's; more fundamentally, waiting is free,
  so agents follow jobs with one long `eve wait` that reconnects through restarts, with
  `--events` for per-milestone wake-ups and `eve pending` as the safety net. Webhooks remain only
  for optional integrations (the human hook).
- Found in the live restart test and fixed: a server shutdown cancelled the running job; now
  only `eve cancel` cancels, and shutdown leaves the job for restart recovery.
- Reviewed by GPT-6.1 Sol ("request changes"); all findings fixed:

| Finding | Severity | Resolution |
|---|---|---|
| A new `eve wait` after `eve confirm` replayed the old pause and stopped (exit 75 without a timeout) | blocker | A pause/terminal event is checked against the job's current state; if the job moved on, the wait continues from that event |
| Shutdown could still persist a failure through helpers (DeveloperDir, version, BundleVersions) | blocker | One guard in the job context: no terminal transition while shutting down unless a user cancelled |
| Cancelling a watched (processing) job was resumed by the next watcher; cancel/settle race | major | Per-run lock with `settled`; watchers record a user cancel; parked jobs without a runner are cancelled directly |
| `eve pending` used the current server for every entry; a 404 silently dropped jobs, even with `--peek` | major | Entries keyed by server + job; other servers reported, 404s reported (and dropped unless `--peek`) |
| Outcomes were marked collected before they were printed; one lookup error hid the rest | major | Print first, then collect; per-entry errors |
| `--timeout` did not bound the command (detached 60 s fetches) | major | Final fetch within the caller's deadline; on timeout the last seen state is returned |
| Hub publication order could differ from commit order, losing an event on the stream | major | The SSE handler treats hub messages as wake-ups and drains events in order from SQLite |
| The idempotency key was only printed after a successful submit | major | Key printed and recorded (ledger intent) before the POST |
| Ledger durability and eviction of uncollected entries | minor | fsync of file and directory; only collected entries are evicted |
| Unbounded events-mode result line; skill gaps; leftover T3 docs | minor | Result line capped at 6 KB; concrete Monitor invocation and exit-78 guidance in the skill; docs cleaned |

### v2 → v3 (user decisions)

- Renamed Drydock → **Eve**.
- Added the size guard (§6.7): client-side source check before upload, bundle check between
  archive and delivery, `awaiting_confirmation` with agent confirmation, baselines, exit 76, new
  CLI/API, skill rules, tests and an M1 acceptance case.
- Decided `svc:eve`, auto-login (with the FileVault consequence, verified on Quartz), and an
  agents-first notification model with an optional human webhook; dropped ntfy and the human-channel spike.

### v1 → v2 (reviews by Fable 5.1 and Astra)

Both reviewers: "revise; the architecture is sound". Resolutions:

| Finding | By | Resolution |
|---|---|---|
| T3 webhook model wrong: one URL per task, thread-bound, needs T3 Connect; static `notify.t3.url` can't route | Fable, Astra (blocker) | Per-job `--notify-url`; skill creates and deletes the task; M0 spike incl. local-endpoint alternative (§8) |
| Background `wait` can't meet the restart-durability criterion | Fable, Astra (blocker) | Webhook is the default in T3; `wait` documented as the weaker fallback (§8.2) |
| Recovery re-runs export, but `destination=upload` uploads during "export"; ASC lag; 90189 misclassified as allocator bug | Fable, Astra (blocker) | Single `delivering` phase, persisted upload intent, 10-min reconciliation via `builds` + `buildUploads`, duplicate = landed (§5.5, §6.3) |
| Token/app scopes and `allowExec` are not a security boundary; build phases run as the user | Astra (blocker), Fable | Honest trust model (§14); scopes demoted to conveniences |
| `Tailscale-User-Login` forgeable via loopback | Fable, Astra | Audit only; tokens are the only authorization (§4.2) |
| Tailnet ACL admits only 443/22; :8443 wouldn't work | Astra (verified) | `svc:eve` Tailscale Service, or an ACL grant (§4.2, §18) |
| Orphaned xcodebuild after a server crash | Fable | Process groups, stored pgid, kill before recovery (§4.1) |
| 10-min derived idempotency key returns dead `needs_human` jobs | Fable, Astra | Client-generated key; implicit dedup only against live jobs (§5.4) |
| ASC states incomplete (`FAILED`, `MISSING_EXPORT_COMPLIANCE`, `PROCESSING_EXCEPTION`, `IN_EXPORT_COMPLIANCE_REVIEW`, `EXPIRED`); `timed_out` both terminal and watched | Fable, Astra | Ported mapping (§6.3); wait timeout separated from job state; durable deadline (§5.2) |
| Allocator coordinates only itself | Astra | Shared lock with `run`, ASC floor, validated overrides, archive version check (§6.3) |
| Aqua ≠ signing guarantee; `authrestart` doesn't restore a session | Astra, Fable | Signing spike in M0; doctor checks; reboot behaviour stated (§4.1) |
| Device acceptance impossible (build after unlock); relock during build | Astra | Build first, re-check before install/launch, "resumes within 10 s" (§6.4, M2) |
| Scope before proof | Astra, Fable | M0 spikes, M1 single durable TestFlight slice; `--ref`, `app init`, resource locks, `logs --follow`, per-verb scopes cut to M7/removed |
| `exec`: defer (Astra) vs. keep, it retires `gui-run.sh` (Fable) | split | Kept, moved to M3 after the core path; no extra trust claimed |
| Lineage's `check-ios.sh` sets `DEVELOPER_DIR` from xcode-select and uses fresh simulators; `LANG` needed | Astra, Fable (verified) | Child env and `check` changed (§4.1, §6.5) |
| ScreenGate defaults to api-key auth; Lineage exports with `uploadSymbols` and no internal-only flag | Fable (verified) | Per-app export options; ground truth corrected (§3, §7.2) |
| Secrets in build settings leak via `ps` and the xcodebuild header | Fable | `-xcconfig` file per job; redaction of served logs and artifacts (§6.2) |
| Manual signing route is unverified | Astra | Marked unverified; M7 starts by verifying it |
| Minor: SSE heartbeat; API row typo; client token can't use a login keychain on Lattice; no cheap Xcode-account probe | Fable | Fixed (§8.2, §11, §4.2, §6.8) |
