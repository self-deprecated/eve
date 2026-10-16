# eve

The one who takes the apple. Fire-and-forget iOS builds on your own Mac: an agent (or you) runs
`eve submit testflight`, ends its turn, and is woken with a short result when the build is ready
for testers — or with exactly what needs fixing.

```console
$ eve submit testflight --notify-t3 518788a8-… --notes "new onboarding"
queued j_mvb9t7wh26ba25ff27 TestFlight for driver (idempotency key ca8dcd41…)
End your turn; the result will be posted when the job finishes.
```

- **One binary.** `eve` is the CLI on every machine and, as `eve serve`, the build server on the
  Mac that signs (Quartz). It runs as a LaunchAgent in the GUI session, where the login keychain and
  the Xcode account work, behind a Tailscale Service.
- **Native pipeline.** XcodeGen (pinned), `xcodebuild archive`, a build-number check across every
  extension, `-exportArchive` signing and uploading in one step, then App Store Connect: processing,
  internal distribution, What to Test, and a note when a tester hasn't accepted their invite.
- **Crash-safe.** Jobs, events and notifications commit atomically in SQLite. A server restart
  kills orphaned builds, resumes or requeues jobs, and reconciles an interrupted upload with App
  Store Connect instead of uploading twice.
- **Size guard.** The client refuses to upload a tree that looks like an incomplete `.gitignore`
  (exit 76, with the ignore lines to add). After archiving, the app bundle is compared with the
  last accepted build: repository files in the app, unexpected growth or a new top-level directory
  pause the job until the agent runs `eve confirm <job> --reason "…"` or `eve reject <job>`.
  Secret material in the bundle always fails.
- **Results for agents.** A < 4 KB result with a classified failure, a fix hint and a short
  excerpt — never a raw xcodebuild log. Delivered into the requesting T3 thread through T3's local
  MCP endpoint, to webhooks, or via `eve wait` (exit code = outcome).

Design and decisions: [PLAN.md](PLAN.md). Operating it: [docs/operations.md](docs/operations.md).
Agent instructions: [skill/eve/SKILL.md](skill/eve/SKILL.md).

## Develop

```bash
nix develop               # go, gopls, gofumpt, golangci-lint
go test -shuffle=on -count=1 ./...
go build -o eve ./cmd/eve
```

Exit codes (`eve wait`, `submit --wait`, `run`): 0 succeeded · 1 failed · 64 usage · 65 rejected ·
69 unavailable · 75 still running at `--timeout` · 76 confirmation required · 78 needs a human ·
130 cancelled.
