---
name: eve
description: Build the user's iOS apps on Quartz with eve — ship to TestFlight, install on their iPhone, or check that it builds — with one long-running command that returns the result. Use when the user says "ship it", "send it to TestFlight", "put it on my phone", "upload a build", "check it builds", or when you would otherwise run xcodebuild archive/export, devicectl or App Store Connect calls yourself.
---

# Eve

`eve` sends the repo to the user's Mac Mini (Quartz), which archives, signs and uploads it with
Apple's tools, follows it through App Store Connect, and reports back. **Waiting is free**: one
long tool call costs nothing while it blocks, whereas polling turns are expensive. So submit and
wait in one command; never poll with `sleep`, never run `xcodebuild archive`/`-exportArchive`,
`devicectl` or App Store Connect API calls yourself for these tasks, and never read raw
xcodebuild logs unless the result's excerpt is not enough.

## Ship to TestFlight

From the repo root (it needs an `eve.yml`, see below):

```bash
eve submit testflight --wait --json --notes "what changed"
```

- **Claude Code:** run it with Bash `run_in_background: true` and `timeout: 3600000` (Apple's
  processing usually takes 5–30 minutes), tell the user the build is on its way in one line, and
  end the turn: you're woken when it exits, then read its output. If you want to react while it
  runs (a size-guard pause, a failure), use `Monitor` instead, with a command that exits on its
  own:

  ```
  Monitor({command: "eve submit testflight --wait --events", description: "eve TestFlight build", timeout_ms: 3600000})
  ```

  Each line is one phase change (`eve <job> archiving: …`); the last is
  `eve <job> result <OUTCOME> {json}`, and the exit code is the outcome.
- **Other harnesses:** run it in the foreground with a long command timeout (an hour).

The working tree is uploaded as it is (committed or not); git-ignored files, build output and
secrets are never sent. Before it submits, the command prints an idempotency key on stderr, then
the job id once Quartz accepts it. If the command dies before you see a job id, rerun the same
command with `--idempotency-key <key>`: you get the same job, never a duplicate.

If the waiting command died (harness restart, session ended), the build carried on:
`eve wait <job> --json` picks it up again, and `eve pending` lists jobs from this machine whose
outcome nobody has seen.

Other verbs: `eve submit device --wait` (install and launch on the user's iPhone; it builds first,
then waits for the phone to be unlocked) and `eve submit check --wait` (simulator build and tests).

## Reading the result

The exit code is the outcome; the JSON is the result document. Read only that.

| Exit | Outcome | What to do |
|---|---|---|
| 0 | `succeeded` | Report the build number in one line. Pass on any `notes` (e.g. `accept_testflight_invite`: the user must accept the TestFlight invite on their phone). |
| 1 | `failed` | Use `failure.class`, `summary`, `hint` and `excerpt` to fix it, then resubmit. Only if that is not enough: `eve logs <job> --errors`. |
| 65 | `rejected` | Apple refused the build (`failure.excerpt` has Apple's reasons). Fix and resubmit. |
| 69 | `unavailable` | Quartz or Apple unreachable. Try again later. |
| 75 | still running | Your `--timeout` elapsed; the job continues. `eve wait <job>` again. |
| 76 | `awaiting_confirmation` | The size guard paused before anything reached Apple. See below. |
| 78 | `needs_human` | Tell the user `human.action` and `human.detail` in one line and stop (unlock the phone, sign in to Xcode on Quartz, accept an agreement, answer export compliance). Once they say it's done, resubmit. |
| 130 | `cancelled` | Someone cancelled it, or a size pause was rejected. |

## The size guard — confirm only what you meant to ship

Eve stops when the uploaded source or the built app grows unexpectedly or contains repository
files. This exists because whole repos have been packaged into the app before.

- `submit` exits **76 before uploading** when the source tree looks wrong (e.g. `build/`,
  `node_modules/`, huge files). Read the report. If the large new directories are not source,
  add the suggested lines to `.gitignore` (or `.eveignore`) and resubmit. Only if the growth is
  explained by your change: resubmit with `--accept-source-size "<why>"`.
- `awaiting_confirmation` after archiving: read `size.reasons`, `size.largest` and `size.hint`;
  `eve size <job>` shows the full diff against the last accepted build.
  - Repository files in the app (`.swift`, `docs/`, `scripts/`, `project.yml`, `.xcodeproj`) are
    **never** expected: `eve reject <job>`, fix the target's sources/resources (often an XcodeGen
    `sources:` entry or a folder reference that reaches the repo root), and resubmit.
  - Growth you caused on purpose (new assets, a new framework):
    `eve confirm <job> --reason "<names what grew>"`, then `eve wait <job> --json` again.
  - Unsure: ask the user before confirming.
- If you already know a change adds weight, pass `--expect-size-change "<why>"` to `submit`.
- Secret material in the app (`.p8`, `.p12`, provisioning profiles, `.env`) always fails; remove
  it from the target.

## eve.yml

```yaml
app: driver                       # the app's name on the server (`eve apps`)
project:
  dir: ios                        # where the project lives, relative to the repo root
  xcodegen: project.yml           # or: xcodeproj: Lineage.xcodeproj
  scheme: Driver
pre:                              # optional commands run before building
  - swift test --package-path DriverCore
```

Identity (bundle id, team, signing, secrets, TestFlight group) lives on the server, not here.

## Useful commands

`eve status <job>` · `eve jobs` · `eve pending` · `eve logs <job> --errors` · `eve size <job>` ·
`eve builds <app>` · `eve cancel <job>` (`--force` while uploading) · `eve doctor`
(`--host` for Quartz's checks).
