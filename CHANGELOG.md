# Changelog

## Unreleased

### Expo apps
- eve.yml's `project.xcworkspace` builds a workspace (`-workspace`) for every verb: CocoaPods
  projects and Expo apps.
- eve looks for the project after `pre:`, so `pre:` can generate it (`expo prebuild`,
  `pod install`).
- server.yml's `path` appends tool directories (Node, CocoaPods) to every job's PATH, after
  Apple's. `doctor --host` checks they exist.
- `env.tool_missing` names a command a job couldn't find (`zsh: command not found: corepack`)
  and points at `path`.
- Expo/React Native is no longer a non-goal (design §18.10). docs/new-app.md has the recipe:
  the eve.yml, `ios.appleTeamId`, `ios.buildNumber: "$(CURRENT_PROJECT_VERSION)"`, and
  `EXPO_PUBLIC_*` values as server-side secrets.

### Simulator lifecycle
- Track exact UDID ownership before boot and hold a non-expiring job lease. Shut down on
  completion, boot/test failure, cancellation and graceful stop, preserving installed apps/data.
- Retry released/crash-recovered leases after a configurable idle period (default 15m). Borrow
  devices with `eve simulator renew <udid> --for 30m` and end early with `release`; renewals
  set a deadline from now, capped by server `maxLeaseDuration` (default 1h). Never adopt
  untracked devices by name or reclaim a device again after successful cleanup.
- Remove prefix-based orphan deletion. Recovery and cleanup respect the `eve run` host lock.
  Shared T3/Indentured/human device idle policy needs separate mono-nix coordination; see operations.

### Ready for release (2026-10-18)
- CI on Linux, macOS and Windows (vet, race-enabled tests, gofumpt, golangci-lint per GOOS, the
  Nix build and a GoReleaser snapshot). A `v*` tag releases archives for darwin, linux and
  windows, a Homebrew cask (`self-deprecated/tap/eve`) and a Scoop manifest. MIT licence.
- The client builds and runs on Windows: process groups, file locks and free space sit behind
  build tags (`internal/flock`), scripts keep their exec bit from git's index, and symlink
  targets are judged with slashes.
- Tests run off the Mac (they called `/bin/cp`, `/bin/sleep`, `/usr/bin/perl`). The rsync check
  refuses only a non-system rsync first on PATH.
- Classifier: CoreDeviceError 3002 / IXRemoteErrorDomain 6 (the connection dropped mid-install)
  is `device.unavailable`, so the device verb waits and retries. `upload.invalid_bundle` ends an
  "Invalid bundle" export at once instead of waiting out reconciliation and uploading again
  (Chaotic Order lost ~12 minutes to that), with the fix for iPad multitasking orientations.
- Redaction: the phone's CoreDevice identifier, UDID, serial number and ECID are redacted by
  value; the 40-hex UDID pattern needs an id label, so git hashes and certificate SHA-1s stay.
- Nix builds report `0.1.0+<rev>` in `eve version`.
- Messages, help and the skill say "the build Mac" instead of naming one host. Docs rewritten
  for anyone running eve: README, [setup](docs/setup.md), [configuration](docs/configuration.md),
  [adding an app](docs/new-app.md), [troubleshooting](docs/troubleshooting.md); PLAN.md is now
  [docs/design.md](docs/design.md).

## M1–M5 (2026-10-16 to 2026-10-17)

- `allowExec: true` at the top of server.yml lets every app run `exec`, without setting it per
  app.

### M5 start: retention, version skew, "no phone connected"
- Retention: a day after a job ends its directory keeps only `logs/`, `artifacts/` and the bundle
  manifest; after 30 days it goes, except a baseline's manifest. Uploads unused for 7 days go
  unless an unfinished job needs them. The worker applies it at most hourly. Quartz had 5.3 GB
  after ~30 jobs (1.5 GB of job directories, one 791 MB).
- Version skew: `/v1/version` reports an API level; `submit` refuses an older server with exit 69
  before uploading ("switch Quartz"), and `eve doctor` warns either way. Level 2 = apps list verbs.
  An older client meeting a newer server is fine.
- `device` says when Quartz can't reach the phone (CoreDevice 4000/4016, e.g. its Wi-Fi is off):
  `connect_phone` with "turn on its Wi-Fi on Quartz's network", instead of "asleep, locked, or not
  on the network; unlock it". A phone that is reachable but locked before the developer disk image
  is mounted (12040) is `device.locked` and asks for an unlock.

### M2 `device` and the rest of M3: `exec` and `eve artifacts`
- `eve submit device`: a development build for `generic/platform=iOS` (Debug, eve.yml's
  `device.configuration`, or `--release`), then install and launch with `devicectl` on
  `--device` (name or UDID), the app's or server's default, or the first paired iPhone. The build
  needs no reachable phone; install and launch then run straight away, as the old scripts did.
  Only an install or launch refused because the phone is locked (`device.locked`) or unreachable
  (`device.unavailable`, now also devicectl's "A connection to this device could not be
  established" and, from the first live run with a locked phone, CoreDeviceError 4016 "not able to
  fulfill the requested usage assertion requirements" on install, and 12040 "The developer disk
  image could not be mounted", which CoreDevice explains as "the device was still locked") puts the job in `waiting_for_device` once and retries every 5 s, continuing on
  unlock, or ends `needs_human: unlock_phone` after 10 minutes. A restart resumes at install from
  the build kept in the job directory. Success carries a `launched` note naming the phone.
- `eve submit exec -- <command…>` (apps with `allowExec`): runs the command from the job's copy
  of the repo in the GUI session, after XcodeGen and `pre`, with `$EVE_ARTIFACTS`,
  `$EVE_PROJECT_DIR` and `$EVE_JOB`; at most an hour (`exec.timeout`); `exec.failed` carries the
  log's last lines. Each top-level entry of `$EVE_ARTIFACTS` is an artifact; directories are
  served as tar streams, symlinks never.
- `eve artifacts <job> [--out DIR]` downloads them; a directory artifact replaces `DIR/<name>`
  only after it has fully unpacked, and paths escaping it are refused.
- Removed the reserved eve.yml key `exec: {artifacts}`: everything in `$EVE_ARTIFACTS` is one.
- Live on Quartz: ScreenGate's mockup screenshots via `eve submit exec` from Lattice (a fresh
  Simulator, `xcodebuild test -scheme ScreenGateMockups` into `$EVE_ARTIFACTS`) in 3m23s;
  `eve artifacts` brought back all 22, 21 byte-identical to the committed ones.
- A locked phone no longer makes the job wait for the launch: when only the launch is refused
  (`device.locked`), the job succeeds with a `not_launched` note. An unlock is needed only when
  Xcode's developer disk image isn't mounted yet (once after the phone restarts); it stays mounted
  for later locked installs.
- Each install or launch attempt is classified on its own output: an early "unreachable" in the
  phase log no longer keeps a later, real failure waiting.
- No lock-state check before install: an earlier draft checked `devicectl device info lockState`
  first, but the old scripts never needed it and `passcodeRequired` may just mean a passcode is
  set, so it could have made every job wait.

### Feedback from converting ScreenGate
- `submit` refuses a verb the server cannot run for the app with exit 64 **before uploading**
  (it used to queue the job and fail with `eve.unsupported`). `eve apps` lists each app's `Verbs`
  (verbs with a pipeline; `exec` also needs `allowExec`); the server refuses the rest with
  `unsupported_verb`. Usage is `submit <testflight|check|device|exec> [-- command for exec]`.
- `eve init` writes a starter eve.yml from the `project.yml` or `.xcodeproj` it finds. The skill
  documents every eve.yml key (`ignore`, `check`, `xcode`, …), says tests run only with
  `check.test: true`, and lists the verbs that are not available yet.
- Results: `phase` is omitted once a job succeeds; `source.files` and `source.bytes` show what
  was uploaded; a `distributed` note confirms the TestFlight group, What to Test and the tester's
  accepted invite.
- PLAN: `exec` artifacts may be directories, fetched with `eve artifacts <job> [--out DIR]` (M3).

### `check` verb (first part of M3)
- `eve submit check`: a Simulator Debug build, plus the scheme's tests on a fresh `eve-<job>`
  Simulator when eve.yml has `check: {test: true}` (`check.simulator` picks the device type).
  The result's `tests` has counts and the first failures; a failing test is `test.failed` with
  the test name and assertion. Simulator lifecycle behavior is described above.
- Simulator builds are signed ad hoc, not `CODE_SIGNING_ALLOWED=NO`: unsigned apps lose their
  entitlements and the Simulator keychain refuses them (Driver's `keychainRoundTrip`).
- Tests run with `-collect-test-diagnostics never`: a failing test made xcodebuild sysdiagnose
  for ten minutes. The runner stops waiting 10 s after a command exits for output held open by
  an escaped grandchild (xcodebuild's `simctl diagnose`).
- Fixed: `eve size` diffed a build against itself once it had become the baseline; reports now
  record the baseline job. `EVE_SERVER` works without a `client.yml`.
- Live on Quartz with Driver: build-only check 20 s; 35 tests 1m24s; a failing test reported
  by name and assertion.

### v4: client-side streaming replaces T3 delivery
- Removed the T3 MCP/OAuth client, `eve t3 login` and `--notify-t3`. Agents follow jobs with
  `eve submit --wait` / `eve wait`, which reconnects forever through drops and server restarts
  (Last-Event-ID), with `--events` (one stdout line per phase, final one-line result) and
  `eve pending` (local ledger of uncollected outcomes). Webhooks stay for optional integrations.
- Fixed: a server shutdown cancelled the running job. Only `eve cancel` cancels now; shutdown
  leaves the job for restart recovery, and `serve` waits for the worker to stop its build.
- Live-tested on Quartz: server killed mid-archive → job requeued with the same build number,
  client reconnected and received the size-guard pause (exit 76).
- Review by GPT-6.1 Sol: fixed replayed pauses ending a resumed wait, shutdown persisting
  failures through helpers, lost cancellations of watched jobs, per-server ledger scoping,
  collect-after-print, `--timeout` bounds, SSE event ordering (drain from the store), printing
  the idempotency key before submitting, ledger fsync and retention. Legacy `t3` outbox rows
  (none outside test databases) exhaust as unknown targets.
- Nix module is now home-manager (`homeManagerModules.default`, `services.eve`); Go module path
  `github.com/self-deprecated/eve`.

### Done
- M0 spikes: signing from an Aqua LaunchAgent works (with the no-shell-startup-files rule); T3
  wake-up via T3's local MCP + OAuth instead of webhook tasks.
- Packages: api, config, run (process groups), store (SQLite, transactional outbox, allocator,
  tokens, baselines), queue (worker, host lock, watchers, recovery, confirm/reject/cancel), xcode,
  pipeline (testflight), server (HTTP API + SSE), client, cli, source (listing, deterministic
  packing, safe extraction, APFS clone), sizeguard, asc (port of app-store-connect.mjs), classify,
  redact, t3 (OAuth + MCP client), notify (outbox delivery, webhooks), launchd; Nix flake and
  nix-darwin module; agent skill.
- Smoke-tested against the real Driver project on Quartz: login, source check refusing a
  `node_modules/` (exit 76, nothing uploaded), upload, real signed archive, size-guard pause,
  `eve size`, `eve reject` — no delivery attempted.

- Classifier failures that need a person (Xcode sign-in, locked phone, agreements) end the job
  `needs_human` instead of `failed`.
- Bundle-root rule narrowed to new top-level directories (plain resources copied into the root
  were too noisy).
- `go vet`, golangci-lint (0 issues) and `go test -race ./...` clean; Nix package builds.

### Follow-ups
- Quartz has ~23 GB free: watch DerivedData and archive retention.

### Done since
- First real TestFlight uploads through eve (BodyDiff, ScreenGate, Driver, Chaotic Order).
- `svc:eve` Tailscale Service and the mono-nix module on Quartz.
