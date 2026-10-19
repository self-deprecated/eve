---
name: eve
description: Build the user's iOS apps on their build Mac with eve — ship to TestFlight, install and launch on their iPhone, check that it builds and its tests pass, or run a repo script (screenshots) on the Mac — with one long-running command that returns the result. Use when the user says "ship it", "send it to TestFlight", "put it on my phone", "upload a build", "check it builds", "update the screenshots", or when you would otherwise run xcodebuild archive/export/test, devicectl, simctl, App Store Connect calls, or ssh/rsync to the build Mac yourself.
---

# Eve

`eve` sends the repo to the user's build Mac (the eve server), which archives, signs and uploads it with
Apple's tools, follows it through App Store Connect, and reports back. **Waiting is free**: one
long tool call costs nothing while it blocks, whereas polling turns are expensive. So submit and
wait in one command; never poll with `sleep`, never run `xcodebuild archive`/`-exportArchive`,
`xcodebuild test`, `simctl`, `devicectl`, App Store Connect API calls or ssh/rsync to the build
Mac yourself for these tasks, and never read raw xcodebuild logs unless the result's excerpt is not
enough.

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
the job id once the server accepts it. If the command dies before you see a job id, rerun the same
command with `--idempotency-key <key>`: you get the same job, never a duplicate.

If the waiting command died (harness restart, session ended), the build carried on:
`eve wait <job> --json` picks it up again, and `eve pending` lists jobs from this machine whose
outcome nobody has seen.

`eve submit check --wait` builds for the Simulator (signed ad hoc, never uploaded). By default it
**only builds**: tests run only when eve.yml sets `check: {test: true}`, on a fresh Simulator. A
failing test exits 1 with `failure.class: test.failed`; `tests.failures` names the first failing
tests and their assertions. Eve leases each fresh device through the test, then shuts it down on
success, failure or cancellation, preserving its apps/data. A `simulator_cleanup` result note
means teardown was deferred; inspect `eve simulator list` on the build Mac. Do not use a pending
Eve-owned device through T3 Device, agent-device, Indentured or Simulator.app until
`eve simulator renew <udid> --for 30m --json` succeeds; wait for its Eve job to finish. Use the
returned `leaseUntil` deadline: renew before expiry while still using it, and stop using it if
renewal fails or the lease expires. Every request is capped by the server's `maxLeaseDuration`
(default 1h), and a renewal sets a fresh deadline from now. Run `eve simulator release <udid>`
when finished to end the borrow lease early. Deadlines persist across restarts. Existing shared
devices are not managed by Eve; see docs/operations.md for the host integration boundary.

`eve submit device --wait` puts a development build on the user's iPhone and launches it. It
builds (Debug, or eve.yml's `device.configuration`; `--release` for Release), then installs and
launches. A locked phone still gets the install: the job succeeds with a `not_launched` note
(tell the user to open the app). Only if the phone can't take the install (asleep, off Wi-Fi, or
locked before Xcode's developer disk image is mounted, which needs one unlock after the phone
restarts) does the job wait in `waiting_for_device`, retrying every 5 s for up to 10 minutes; it
continues within seconds, and otherwise ends `needs_human` (exit 78). `connect_phone` means
the build Mac can't reach the phone at all (its Wi-Fi is off, it's away, or asleep); `unlock_phone`
means it's locked. Pass `human.detail` on to the user in one line; resubmit once it's sorted. `--device <name or UDID>` picks another paired device. Success has a `launched` note
naming the phone.

`eve submit exec --wait -- <command> [args…]` runs a repo command on the build Mac in the job's copy of
the repo, from its root, in the GUI session, so it can drive the Simulator. The app needs
`allowExec` on the server (per app, or top-level for every app). The command gets
`$EVE_ARTIFACTS` (a directory: everything at its top level becomes an artifact, directories
included), `$EVE_PROJECT_DIR` (the Xcode project's directory, after XcodeGen) and `$EVE_JOB`. It may run for up to an hour. Then
`eve artifacts <job> --out <dir>` downloads them; a directory artifact replaces `<dir>/<name>`
entirely, so `--out docs/screenshots` refreshes `docs/screenshots/current/`. A failing command
exits 1 with `failure.class: exec.failed` and the log's last lines; artifacts it wrote are kept.
Simulator devices created by that command remain its script's responsibility: use explicit
shutdown on completion/failure; Eve does not infer ownership from names or job completion.

`eve apps --json` lists the `Verbs` each app allows, and `submit` refuses any other verb with
exit 64 before uploading anything.

## Reading the result

The exit code is the outcome; the JSON is the result document. Read only that. `phase` is where a
job is, or where it stopped if it did not succeed (absent once it has); `source.files` and
`source.bytes` are what was uploaded, so you can see an `ignore` took effect.

| Exit | Outcome | What to do |
|---|---|---|
| 0 | `succeeded` | Report the build number in one line. The `distributed` note says which group got it, that What to Test was set and that the tester has accepted their invite: only then say it's on their phone. Pass on any other `notes` (e.g. `accept_testflight_invite`: the user must accept the TestFlight invite first). For `device`, say it's running on the phone the `launched` note names (or, with a `not_launched` note, that it's installed and they should open it); for `exec`, fetch `artifacts` with `eve artifacts`. |
| 1 | `failed` | Use `failure.class`, `summary`, `hint` and `excerpt` to fix it, then resubmit. Only if that is not enough: `eve logs <job> --errors`. |
| 65 | `rejected` | Apple refused the build (`failure.excerpt` has Apple's reasons). Fix and resubmit. |
| 69 | `unavailable` | The build Mac or Apple unreachable, or the server is older than this client ("update eve on the build Mac": tell the user). Try again later. |
| 75 | still running | Your `--timeout` elapsed; the job continues. `eve wait <job>` again. |
| 76 | `awaiting_confirmation` | The size guard paused before anything reached Apple. See below. |
| 78 | `needs_human` | Tell the user `human.action` and `human.detail` in one line and stop (unlock the phone, sign in to Xcode on the build Mac, accept an agreement, answer export compliance). Once they say it's done, resubmit. |
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

`eve init` writes a starter one from the `project.yml` or `.xcodeproj` it finds in the current
directory or one level down; check `app` against `eve apps`. Every key:

```yaml
app: driver                       # required: the app's name on the server (`eve apps`)
project:
  dir: ios                        # where the project lives, relative to the repo root (default .)
  xcodegen: project.yml           # exactly one of xcodegen (path relative to dir)
  # xcodeproj: Lineage.xcodeproj  #   or xcodeproj
  scheme: Driver                  # required
ignore:                           # gitignore-style paths never uploaded, on top of .gitignore
  - docs/screenshots/             #   and .eveignore: large assets the build doesn't need
pre:                              # commands run in the workspace before building
  - swift test --package-path DriverCore
check:                            # for `eve submit check`
  test: true                      # also run the scheme's tests (default false: build only)
  simulator: iPhone 17 Pro        # device type (default: the newest iPhone Pro)
xcode: "26"                       # fail unless the build Mac's selected Xcode is this version (26 matches 26.x)
```

`device: {configuration: Release}` sets the configuration `eve submit device` builds (default
Debug). Unknown keys are an error. Identity (bundle id, team, signing, secrets, TestFlight
group) lives on the server, not here.

Before the first submit, make sure big directories the build doesn't need (screenshots, design
files, fixtures) are covered by `.gitignore`, `.eveignore` or `ignore`: the size guard only stops
uploads that look wrong, and every submit uploads everything else.

## Useful commands

`eve apps` · `eve init` · `eve artifacts <job> --out <dir>` · `eve status <job>` · `eve jobs` · `eve pending` · `eve logs <job> --errors` · `eve size <job>` ·
`eve builds <app>` · `eve cancel <job>` (`--force` while uploading) · `eve doctor`
(`--host` for the build Mac's own checks).
