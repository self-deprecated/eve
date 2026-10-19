# Configuration

eve has three config files. Identity and credentials live only on the build Mac, so an uploaded
tree can't redirect a build to another app or read a secret it wasn't given.

| File | Where | Holds |
|---|---|---|
| `server.yml` | build Mac, `~/.config/eve/server.yml` | Team, App Store Connect key, the apps it may build and everything about them |
| `eve.yml` | each app repository | Where the Xcode project is, and how to build and check it |
| `client.yml` | every client, `~/.config/eve/` | The server URL (`eve login` writes it; the token is in a 0600 file next to it) |

Two environment variables override the client config: `EVE_SERVER` (a URL) and
`EVE_CONFIG_DIR` (another config directory). On the build Mac itself, a client with no
`client.yml` talks to the local server with its owner token.

## server.yml

```yaml
team: ABCDE12345                   # required: Apple Developer team id
asc:                               # App Store Connect API key (App Manager role is enough)
  keyId: KEY1234567
  issuerId: 00000000-0000-0000-0000-000000000000
  keyPath: ~/.appstoreconnect/private_keys/AuthKey_KEY1234567.p8
# listen: 127.0.0.1:4790           # loopback only; publish it on your tailnet
# dataDir: ~/.local/share/eve
# allowExec: true                  # every app may run `eve submit exec`

apps:
  myapp:                           # lowercase letters, digits and dashes; eve.yml's `app:`
    bundleId: com.example.myapp    # required
    # ascAppId: "1234567890"       # looked up from the bundle id when absent
    # signing: xcode-account       # or api-key (cloud signing needs an Admin key)
    testflight:
      group: Internal              # the internal group that gets every build
      # tester: you@example.com    # eve reports whether they've accepted the invite
    # export:
    #   internalOnly: true         # default true; false lets the build reach external groups
    #   uploadSymbols: true        # default true
    # secrets:                     # build settings, written to a 0600 xcconfig per job
    #   API_BASE_URL: file:~/.config/myapp/api-base-url
    #   SENTRY_DSN: env:MYAPP_SENTRY_DSN
    # allowExec: true              # this app may run `eve submit exec`
    # device: "Sam’s iPhone"       # default phone for `eve submit device` (name or UDID)
    # sizeGuard: {…}               # overrides the defaults below, field by field

# device:
#   default: "Sam’s iPhone"        # default phone for every app

# simulators:                      # only exact devices created and tracked by Eve
#   idleTimeout: 15m               # pending/recovered cleanup; 0s disables automatic retries
#   shutdownTimeout: 30s           # bounded teardown; 1s–2m
#   maxLeaseDuration: 1h           # maximum per agent borrow acquisition/renewal
#   protectedDevices: []           # operator-only indefinite exclusions (UDIDs)

# sizeGuard:                       # defaults shown
#   source:                        # the uploaded tree, checked on the client before upload
#     growth: 50%                  # vs the last accepted upload…
#     growthMin: 20MB              # …and at least this much
#     files: 50%
#     ceiling: 500MB
#     largeFile: 50MB
#     allowDirs: []
#   bundle:                        # the archived .app, checked before anything reaches Apple
#     growth: 15%
#     growthMin: 5MB
#     files: 20%
#     filesMin: 200
#     newFile: 5MB
#     ceiling: 300MB
#     allowPatterns: []            # e.g. ["*.md"] for an app that ships help pages
#   confirmTimeout: 24h            # a paused job ends after this

# notify:
#   allowHosts: ["*.example.ts.net"]   # hosts `--notify-url` webhooks may target
#   human:                             # one-line messages for things only a person can do
#     url: https://…
#     secret: file:~/.config/eve/human-hook-secret   # HMAC-SHA256 signature when set
#   secrets:                           # HMAC secrets for --notify-url, by URL prefix
#     - {prefix: "https://hooks.example.ts.net/", secret: "file:~/.config/eve/hook-secret"}

# xcode:                           # the pinned XcodeGen eve downloads and verifies
#   xcodegenVersion: 2.46.0
#   xcodegenSha256: 4d9e34b6…
```

Notes:

- `eve serve` refuses to start when the config has mistakes that would only show up mid-build:
  a missing team or bundle id, a non-loopback `listen`, or secrets that aren't `file:` or `env:`.
- **Signing.** `xcode-account` (the default) signs with the Apple ID signed in to Xcode on the build
  Mac, using `-allowProvisioningUpdates`. `api-key` signs with the App Store Connect key, which
  only works for cloud-managed certificates when the key has the Admin role.
- **Secrets** are read on the build Mac when the job starts. They are passed to xcodebuild as a
  per-job 0600 xcconfig, never on the command line, and are redacted from stored and served logs,
  results and artifacts.
- **`allowExec`** decides whether `eve submit exec` is available. It prevents accidents, not
  attacks: any build can already run code (see the trust model in the README).
- **Webhooks.** `--notify-url` posts the result document from a durable outbox, with
  `X-Eve-Delivery` for deduplication and `X-Eve-Signature` when a secret applies, and retries for
  24 hours. `notify.human` gets one-line copies of `needs_human`, `waiting_for_device` and
  deliveries that exhausted their retries. Size confirmations never go there.

`eve apps` shows what the running server loaded, including the verbs each app may run.
See [Simulator operations](operations.md) for borrowing, expiry, cleanup and the shared-host boundary.

## eve.yml

`eve init` writes a starter file from the `project.yml` or `.xcodeproj` it finds in the current
directory or one level down. Every key:

```yaml
app: myapp                         # required: the app's name on the server (`eve apps`)
project:
  dir: ios                         # the project's directory, relative to the repo root (default .)
  xcodegen: project.yml            # exactly one of xcodegen (relative to dir)…
  # xcodeproj: MyApp.xcodeproj     # …or xcodeproj
  scheme: MyApp                    # required
ignore:                            # gitignore-style paths never uploaded, on top of .gitignore
  - docs/screenshots/              #   and .eveignore
pre:                               # commands run in the job's workspace before building
  - swift test --package-path Core
check:                             # `eve submit check`
  test: true                       # also run the scheme's tests (default: build only)
  simulator: iPhone 17 Pro         # device type (default: the newest iPhone Pro)
device:
  configuration: Release           # what `eve submit device` builds (default Debug)
xcode: "26"                        # refuse to build with another Xcode ("26" matches 26.x)
```

Unknown keys are an error. Every submit uploads everything that isn't ignored, so before the first
one, make sure large directories the build doesn't need (screenshots, design files, a backend)
are covered by `.gitignore`, `.eveignore` or `ignore:`. The size guard stops uploads that look
wrong, but it doesn't trim them.

`pre:` and `exec` commands run under `/bin/zsh -f`, with no shell startup files. A nix-darwin
`/etc/zshenv` would otherwise put Nix's `rsync` ahead of `/usr/bin`, and that breaks IPA export.
Commands get a clean environment: `HOME`, `USER`, `TMPDIR`, a system `PATH`, the selected Xcode's
`DEVELOPER_DIR` and a UTF-8 locale. `exec` commands also get `$EVE_ARTIFACTS`,
`$EVE_PROJECT_DIR` and `$EVE_JOB`.
