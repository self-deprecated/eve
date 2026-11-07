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
| `eve bridge on [--for 2h]` · `eve bridge off` · `eve bridge status` | Reach the phone over Tailscale while it's on another Wi-Fi network ([travel](troubleshooting.md#away-from-the-build-macs-network)). `on` exits 0 once a device call went through it, 69 if the phone didn't answer |
| `eve init` · `eve doctor [--host]` · `eve login` | Starter `eve.yml`; check the connection (and that `eve apple`'s tools are here) or the build Mac; store a server and token |
| `eve serve` · `eve agent install` · `eve token create` · `eve run <verb>` | Server side: run, install as a LaunchAgent, issue tokens, build in-process without the server |
| `eve apple login` · `eve apple apps` · `eve apple keys` | Client side: sign in to Apple (password and 2FA, cached on this machine); list App Store Connect apps and keys |
| `eve apple app create <bundle-id> [--name … --sku …]` | Client side: create the app group, bundle id and capabilities, App Store Connect record, extensions and internal TestFlight group; only what's missing ([below](#apple-provisioning)) |
| `eve apple key create --name … --env … --doppler …` | Client side: create an APNs key and store it in Doppler |
| `eve simulator list` · `eve simulator renew <udid> --for 30m` · `eve simulator release <udid>` | Build host only: inspect tracked devices, acquire/renew timed borrowing, or release it early ([lifecycle](operations.md)) |

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

## Apple provisioning

`eve apple` runs on the machine you type it on, with `fastlane` on its PATH (and the Doppler CLI,
signed in, for keys). Sign in once with `eve apple login`; the session lasts until Apple expires
it, then commands exit 78 with "run `eve apple login`". Pick the team with `--team` or
`EVE_APPLE_TEAM` when the Apple account has more than one. To skip typing the password, keep it
in Doppler and set `EVE_APPLE_PASSWORD_DOPPLER=<project>/<config>/<SECRET>` (or pass
`--password-doppler`): login reads it with the Doppler CLI. Apple's two-factor code, when Apple
wants one, is still asked for in the terminal. With the password from Doppler, login also runs
without a terminal (an agent's shell): it signs in while this machine is still trusted, and ends
needs_human (exit 78) when Apple asks for a two-factor code.

```sh
eve apple app create com.example.myapp --name "My App" --sku myapp --locale en-GB \
  --like com.example.otherapp --extension widget --group Internal --tester you@example.com \
  --dry-run                                                    # drop --dry-run to apply
eve apple key create --name "My App Push" --env all --doppler myapp   # myapp/dev and myapp/prd
```

- `app create` creates `group.<bundle-id>` (when the app or `--like` uses an app group), the
  bundle id and its capabilities, the App Store Connect record (`--version`, default 1.0), then
  `<bundle-id>.<extension>` for each `--extension`, then the internal TestFlight `--group` with
  access to every build, and `--tester` in it. A tester must be a user on the App Store Connect
  team (Users and Access); an external group by that name is refused. `--like` copies an existing app's
  capabilities with their settings, and each same-suffix extension's; `--capability`,
  `--extension-capability` and `--app-group` add to that. Anything that exists is left alone:
  a rerun reports `no changes`. `--name` and `--sku` are only needed for a new record, so for an
  existing app `eve apple app create <bundle-id> --group Internal --tester you@example.com` is
  enough.
- `key create` makes a team key (every topic) or, with `--scope topic --topic <bundle-id>`, a
  topic key in one environment. `--doppler <project>` stores it in the configs `--env` implies
  (sandbox: `dev`, production: `prd`, all: both); `--doppler <project>/<config>` names one
  config, and both repeat. Missing projects are created. `APNS_KEY_P8` is restricted;
  `APNS_KEY_ID` and `APPLE_TEAM_ID` aren't. Key names take letters, digits and spaces. If
  Doppler fails after Apple created the key, the error names a 0600 file holding the `.p8`,
  which Apple won't serve again.

Exit codes: 0 done, 1 failed, 64 usage, 65 Apple refused (a taken app name, a tester who isn't a
team user), 69 Apple
unreachable, 78 sign in again.

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
