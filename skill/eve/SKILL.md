---
name: eve
description: Build the user's iOS apps on Quartz with eve — ship to TestFlight, install on their iPhone, or check that it builds — fire-and-forget, then get woken with the result. Use when the user says "ship it", "send it to TestFlight", "put it on my phone", "upload a build", "check it builds", or when you would otherwise run xcodebuild archive/export, devicectl or App Store Connect calls yourself.
---

# Eve

`eve` sends a build to the user's Mac Mini (Quartz), which archives, signs and uploads it with
Apple's tools, follows it through App Store Connect, and tells you when it is done. You submit
and **end your turn**; you are woken with a short result. Never poll with `sleep`, never run
`xcodebuild archive`/`-exportArchive`, `devicectl` or App Store Connect API calls yourself for
these tasks, and never read raw xcodebuild logs unless the result's excerpt is not enough.

## Ship to TestFlight

1. Make sure the repo root has an `eve.yml` (see below). Commit or not — eve uploads the working
   tree as it is (git-ignored files and secrets are never sent).
2. Find your T3 thread id: `orchestrator_capabilities` → `parentThreadId`.
3. Submit from the repo root:

   ```bash
   eve submit testflight --notify-t3 <thread-id> --notes "what changed" --json
   ```

   It prints `{id, idempotencyKey, …}`. Tell the user the job id in one line and **end the turn**.
   Outside T3, run `eve wait <id> --json` as a background command with a long timeout instead.
4. If `submit` itself failed with a lost/timeout response, retry with `--idempotency-key <key>`;
   you get the same job, never a duplicate.

Other verbs: `eve submit device` (install and launch on the user's iPhone; build first, then it
waits for the phone to be unlocked) and `eve submit check` (simulator build and tests).

## When you are woken

You receive a line like `[eve] testflight driver j_… → <outcome>` and the result JSON. Read only
that.

- `succeeded`: report the build number in one line. Pass on any `notes` (e.g.
  `accept_testflight_invite`: the user must accept the TestFlight invite on their phone).
- `failed`: use `failure.class`, `summary`, `hint` and `excerpt` to fix the cause, then resubmit.
  Only if that is not enough: `eve logs <id> --errors`.
- `rejected`: Apple refused the build (see `failure.excerpt` for Apple's reasons); fix and resubmit.
- `needs_human`: tell the user `human.action` and `human.detail` in one line and stop
  (unlock the phone, sign in to Xcode on Quartz, accept an agreement, answer export compliance).
- `awaiting_confirmation`: the **size guard** paused before anything reached Apple. See below.
- `unavailable`: Quartz or Apple was unreachable; resubmit later.

## The size guard — confirm only what you meant to ship

Eve stops when the uploaded source or the built app grows unexpectedly or contains repository
files. This exists because whole repos have been packaged into the app before.

- `submit` exits **76** before uploading when the source tree looks wrong (e.g. `build/`,
  `node_modules/` or huge files). Read the report. If the large new directories are not source,
  add them to `.gitignore`/`.eveignore` and resubmit. Only if the growth is explained by your
  change: resubmit with `--accept-source-size "<why>"`.
- Woken with `awaiting_confirmation`: read `size.reasons`, `size.largest` and `size.hint`; run
  `eve size <id>` for the full diff if needed.
  - Repository files in the app (`.swift`, `docs/`, `scripts/`, `project.yml`, `.xcodeproj`) are
    **never** expected: `eve reject <id>`, fix the target's sources/resources (often an XcodeGen
    `sources:` entry or a folder reference that reaches the repo root), and resubmit.
  - Growth you caused on purpose (new assets, a new framework): `eve confirm <id> --reason
    "<names what grew>"`. Then end the turn again; you will be woken when it finishes.
  - Unsure: ask the user before confirming.
- If you already know a change adds weight, pass `--expect-size-change "<why>"` at submit.
- Secret material in the app (`.p8`, `.p12`, provisioning profiles, `.env`) always fails; remove
  it from the target.

## eve.yml

```yaml
app: driver                       # the app's name on the server (eve apps)
project:
  dir: ios                        # where the project lives, relative to the repo root
  xcodegen: project.yml           # or: xcodeproj: Lineage.xcodeproj
  scheme: Driver
pre:                              # optional commands run before building
  - swift test --package-path DriverCore
```

Identity (bundle id, team, signing, secrets, TestFlight group) is configured on the server, not
in the repo.

## Useful commands

`eve status <id>` · `eve jobs` · `eve logs <id> --errors` · `eve size <id>` · `eve builds <app>` ·
`eve cancel <id>` (`--force` while uploading) · `eve doctor` (`--host` for Quartz's checks).

Exit codes of `eve wait`/`submit --wait`: 0 succeeded, 1 failed, 65 rejected, 69 unavailable,
75 still running at the timeout, 76 confirmation required, 78 needs a human, 130 cancelled.
