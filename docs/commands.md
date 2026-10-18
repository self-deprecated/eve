# Commands

| Command | What it does |
|---|---|
| `eve submit <testflight\|check\|device\|exec>` | Upload the working tree and start a job. Add `--wait` to follow it, and `--events` for one line per phase |
| `eve wait <job>` | Follow a job again; the exit code is the outcome |
| `eve pending` | Outcomes of jobs from this machine that nobody has seen |
| `eve status <job>` · `eve logs <job> --errors` · `eve jobs` | The result document, redacted logs, recent jobs |
| `eve size <job>` · `eve confirm <job> --reason …` · `eve reject <job>` | Inspect, accept or refuse a size-guard pause |
| `eve artifacts <job> --out DIR` | Download what an `exec` job produced |
| `eve builds <app>` · `eve apps` | Latest builds in App Store Connect; configured apps and their verbs |
| `eve init` · `eve doctor [--host]` · `eve login` | Starter `eve.yml`; check the connection or the build Mac; store a server and token |
| `eve serve` · `eve agent install` · `eve token create` · `eve run <verb>` | Server side: run, install as a LaunchAgent, issue tokens, build in-process without the server |

Every command takes `--json`.

## Exit codes

For `eve wait`, `submit --wait` and `run`:

| Code | Outcome |
|---|---|
| 0 | succeeded |
| 1 | failed (`failure.class`, `summary`, `hint`, `excerpt`) |
| 64 | usage, or a verb the server can't run for this app (refused before uploading) |
| 65 | rejected by Apple |
| 69 | unavailable: the server or Apple can't be reached, or the server is older than the client |
| 75 | still running when `--timeout` elapsed |
| 76 | confirmation required (size guard) |
| 78 | needs a human (`human.action`: unlock the phone, sign in to Xcode, accept an agreement, …) |
| 130 | cancelled |

## The size guard

The client refuses to upload a tree that looks like an incomplete `.gitignore`. After archiving,
the app bundle is compared with the last accepted build, and three things pause the job until an
agent or person confirms or rejects it: repository files in the app, unexpected growth, or a new
top-level directory. Secret material in the bundle always fails the job.

## Crash safety

Jobs, events and notifications are committed atomically in SQLite. A server restart kills orphaned
builds, then resumes or requeues jobs. An interrupted upload is checked with App Store Connect
instead of being uploaded twice. `eve wait` reconnects through network drops and server restarts
without losing events.
