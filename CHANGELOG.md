# Changelog

## Unreleased — M1 in progress (2026-10-16)

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
- `eve t3 login` approval and delivery into a thread across a T3 restart.
- `svc:eve` Tailscale Service and the mono-nix module on Quartz (operator steps).
