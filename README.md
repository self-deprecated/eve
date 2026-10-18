# eve

Fire-and-forget iOS builds on your own Mac. From any machine, one command sends your app's working
tree to the Mac that signs your builds, and comes back when the build is in TestFlight, on your
iPhone, or has passed its tests. If it fails, you get the cause and the fix, not a raw xcodebuild log.

eve is built for coding agents. An agent's turns cost tokens, and waiting inside one long command
costs none. Polling, rerunning builds after signing fixes and rediscovering the toolchain all cost
turns. So eve gives an agent one long-running command, a result under 4 KB with a classified
failure, and a clear signal when only a person can help (unlock the phone, sign in to Xcode).

```console
$ eve submit testflight --wait --events
eve j_mvbcdz6285e96fe933 preparing: preparing workspace
eve j_mvbcdz6285e96fe933 archiving: archiving build 202610161909
eve j_mvbcdz6285e96fe933 measuring: measuring the app bundle
eve j_mvbcdz6285e96fe933 delivering: signing and uploading build 202610161909
eve j_mvbcdz6285e96fe933 processing: uploaded; waiting for App Store Connect to process it
eve j_mvbcdz6285e96fe933 result SUCCEEDED {"id":"j_mvbcdz6285e96fe933",…}
```

eve is the name of the one who takes the apple.

## What it does

- **`testflight`**: archives, checks that every bundle carries the build number, then signs and
  uploads. It follows App Store Connect processing, adds the build to your internal group, sets
  What to Test, and tells you when a tester hasn't accepted their invite yet.
- **`device`**: makes a development build, installs it on your iPhone over Wi-Fi or USB, and
  launches it. A locked phone still gets the install. If the phone is unreachable, eve waits up
  to ten minutes and continues as soon as it's back.
- **`check`**: makes a Simulator build and, if you ask for it, runs the tests on a fresh
  Simulator. A failing test is reported by name and assertion.
- **`exec`**: runs a script from your repository on the Mac, in the logged-in session (so it can
  drive the Simulator), and hands back what it wrote. Screenshot generation is the usual use.

All four verbs share these guarantees:

- **Crash-safe.** Jobs, events and notifications are committed atomically in SQLite. A server
  restart kills orphaned builds, then resumes or requeues jobs. An interrupted upload is checked
  with App Store Connect instead of being uploaded twice.
- **Size guard.** The client refuses to upload a tree that looks like an incomplete `.gitignore`.
  After archiving, the app bundle is compared with the last accepted build, and three things pause
  the job until an agent or person confirms or rejects it: repository files in the app,
  unexpected growth, or a new top-level directory. Secret material in the bundle always fails the
  job.
- **Results for agents.** `eve wait` holds one connection, reconnects through network drops and
  server restarts without losing events, and exits with the outcome as its code. `eve pending`
  finds outcomes nobody collected. eve needs no integration with any particular agent harness.

## Requirements

- **A build Mac:** an always-on Mac with Xcode, signed in to your Apple ID in Xcode and logged
  in at the console. eve signs with that Xcode account, so it runs as a LaunchAgent in the GUI
  session.
- **Apple accounts:** an Apple Developer team, and an App Store Connect API key (App Manager
  role is enough) for following processing and distribution.
- **A private network** between your machines and the build Mac. eve listens on loopback and is
  meant to be published on a tailnet (Tailscale Serve or a Tailscale Service). Never expose it to
  the internet.
- **Clients:** any macOS, Linux or Windows machine.

## Install

macOS, with Homebrew:

```sh
brew install --cask self-deprecated/tap/eve
```

Windows, with Scoop:

```powershell
scoop bucket add self-deprecated https://github.com/self-deprecated/scoop-bucket
scoop install self-deprecated/eve
```

Linux, macOS and Windows archives are on the [releases page](https://github.com/self-deprecated/eve/releases).
With Nix, use the flake. Its `homeManagerModules.default` (`services.eve`) runs the server on the
build Mac; see [docs/setup.md](docs/setup.md).

## Quick start

On the build Mac, write `~/.config/eve/server.yml` (see [docs/configuration.md](docs/configuration.md)):

```yaml
team: ABCDE12345                   # your Apple Developer team id
asc:
  keyId: KEY1234567
  issuerId: 00000000-0000-0000-0000-000000000000
  keyPath: ~/.appstoreconnect/private_keys/AuthKey_KEY1234567.p8
apps:
  myapp:
    bundleId: com.example.myapp
    testflight: {group: Internal}
```

Then install the LaunchAgent, publish it on your tailnet, and create a token for each client:

```sh
eve agent install --config ~/.config/eve/server.yml
eve doctor --host                  # GUI session, keychain, signing identity, Xcode, App Store Connect
eve token create laptop            # prints the token once
```

On a client:

```sh
eve login https://eve.<tailnet>.ts.net --token eve_…
cd ~/src/myapp && eve init         # writes eve.yml from your project.yml or .xcodeproj
eve submit check --wait            # proves the setup with a Simulator build
eve submit testflight --wait --notes "first build through eve"
```

[docs/setup.md](docs/setup.md) covers publishing, reboots and disk use, and
[docs/new-app.md](docs/new-app.md) covers adding more apps.

## Using it from an agent

[skill/eve/SKILL.md](skill/eve/SKILL.md) is a skill for Claude Code and other agents that read
skills. It tells the agent to submit and wait in one long command, read only the result, and
relay `needs_human` instead of retrying. Releases include it, and the Nix module installs it.

## Commands

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

Every command takes `--json`. Exit codes for `eve wait`, `submit --wait` and `run`:

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

## Trust model and limitations

- **A token that can submit a build can run code as you on the build Mac.** Xcode build phases,
  package plugins and `pre:` commands run as the logged-in user with the login keychain unlocked.
  They can read anything you can, and they can ship a binary signed as you. Issue few tokens, one
  per machine you control, and revoke them with `eve token revoke`. `allowExec` prevents accidents;
  it doesn't isolate apps from each other.
- eve listens only on loopback, never accepts inbound webhooks, and redacts secrets, your home
  directory, emails and device identifiers from stored and served logs.
- One build Mac, one job at a time. Signing uses the Xcode account (an `api-key` mode exists for
  admin keys). TestFlight distribution is internal-only unless you opt out per app.
- There is no App Store submission, no external TestFlight groups, no Android, and no registering
  bundle ids or capabilities. Apple-side states that need a person are reported as `needs_human`.

The full design, with the decisions and review history behind it, is in
[docs/design.md](docs/design.md). When a job fails in a way its result doesn't explain, see
[docs/troubleshooting.md](docs/troubleshooting.md).

## Development

```sh
nix develop                        # go, gopls, gofumpt, golangci-lint
go test -race -shuffle=on ./...    # runs on macOS, Linux and Windows
golangci-lint run ./...
go build -o eve ./cmd/eve
```

Releases are cut by pushing a `v*` tag that matches `version` in `nix/package.nix`. GoReleaser
publishes the archives, the Homebrew cask and the Scoop manifest.

## License

MIT. See [LICENSE](LICENSE).
