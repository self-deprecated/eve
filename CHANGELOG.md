# Changelog

## Unreleased — M1 (2026-10-16)

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
  the test name and assertion. Orphaned Simulators are deleted before each run.
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
- redact: patterns for devicectl's ECID, serial number and CoreDevice UUID; the 40-hex UDID
  pattern also hides certificate SHA-1s and git hashes.
- Quartz has ~23 GB free: watch DerivedData and archive retention.

### Still open for M1
- First real TestFlight upload through eve (needs the user's go-ahead).
- `svc:eve` Tailscale Service and the mono-nix module on Quartz (operator steps).
