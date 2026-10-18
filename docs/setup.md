# Setting up and running the build Mac

The build Mac runs `eve serve`. Every other machine runs only the client.

## 1. Prepare the Mac

- Install Xcode, open it once, and sign in to your Apple ID under Settings → Accounts. eve signs
  with that account (`-allowProvisioningUpdates`).
- Pair the iPhone you want `eve submit device` to use: connect it once, then trust the Mac in
  Window → Devices and Simulators. After that it works over Wi-Fi.
- Create an App Store Connect API key (Users and Access → Integrations; App Manager is enough),
  download the `.p8`, and keep it in `~/.appstoreconnect/private_keys/` with mode 0600.
- Install eve: `brew install --cask self-deprecated/tap/eve`, or the Nix module below.

## 2. Configure it

Write `~/.config/eve/server.yml` with your team, the API key and one entry per app. Every key is
in [configuration.md](configuration.md).

## 3. Run it as a LaunchAgent

eve has to run in the logged-in user's GUI (Aqua) session, because that's the only place where the
login keychain and the Xcode account work. It can't be a LaunchDaemon or run over SSH.

```sh
eve agent install --config ~/.config/eve/server.yml   # loads it into gui/<uid>
eve agent status
eve doctor --host
```

`doctor --host` checks the GUI session, that the login keychain is unlocked, the signing
identities, Xcode, `/usr/bin/rsync`, App Store Connect and free disk space.

### With Nix (home-manager)

```nix
# flake inputs
eve.url = "github:self-deprecated/eve";

# home-manager configuration of the build Mac's user
imports = [ inputs.eve.homeManagerModules.default ];
services.eve = {
  enable = true;
  settings = {
    team = "ABCDE12345";
    asc = { keyId = "KEY1234567"; issuerId = "…"; keyPath = "~/.appstoreconnect/private_keys/AuthKey_KEY1234567.p8"; };
    apps.myapp = { bundleId = "com.example.myapp"; testflight.group = "Internal"; };
  };
};
```

The module writes `server.yml` from `settings`, installs the LaunchAgent with
`LimitLoadToSessionType=Aqua`, and puts the CLI and the agent skill on the PATH. Keep secrets as
`file:` paths so they never enter the Nix store.

## 4. Publish it on your tailnet

`eve serve` listens on `127.0.0.1:4790` and nowhere else. Publish it to your other machines over a
private network, for example with Tailscale:

```sh
# a Tailscale Service, with its own name and port 443 (create svc:eve in the admin console first)
tailscale serve --service=svc:eve --https=443 http://127.0.0.1:4790
# or simply on the Mac's own name
tailscale serve --bg --https=443 http://127.0.0.1:4790
```

Clients then use `https://eve.<tailnet>.ts.net` (or the Mac's name). Don't use Tailscale Funnel or
any other public exposure: a token that can submit a build can run code as you (see the README's
trust model).

## 5. Tokens and clients

The owner token is created on first start (`~/.local/share/eve/token`) and never leaves the Mac.
Create a token for each other machine:

```sh
eve token create laptop            # prints it once
eve token list
eve token revoke laptop
```

On the client:

```sh
eve login https://eve.<tailnet>.ts.net --token eve_…
eve doctor                         # connection, versions, the token
```

`submit` refuses to upload to a server older than the client (exit 69, "update eve on the build
Mac"). An older client talking to a newer server is fine.

## 6. Reboots

After a reboot, nothing runs until someone logs in at the console, because a LaunchAgent needs a
GUI session. For unattended reboots, turn on automatic login for the build user. macOS only allows
that with FileVault off (`sysadminctl -autologin status` says so). That's a real trade-off:
the disk is no longer encrypted at rest, and after a reboot the login keychain, with your
signing identity and the Xcode account, unlocks with nobody present. The Mac's physical security
becomes the control. If FileVault stays on, the client says the server is unreachable until
someone logs in.

## 7. Disk and retention

eve applies retention at most hourly and never touches unfinished jobs:

- A day after a job ends, its directory keeps only `logs/`, `artifacts/` and the bundle manifest
  (the size guard's record of the app). The workspace, archive and build products are removed.
- After 30 days the directory goes, except a size baseline's manifest.
- Uploaded sources go after 7 days without use.
- DerivedData is one cache per app and verb (`~/.local/share/eve/derived/`) and is kept.

```
~/.config/eve/server.yml          server config
~/.local/share/eve/
  eve.db                          SQLite (WAL): jobs, events, outbox, build numbers, tokens
  sources/<sha256>.tar.zst        uploads, content-addressed
  derived/<app>/<verb>/           DerivedData
  jobs/<id>/                      workspace, logs/, artifacts/, bundle manifest
  tools/                          pinned, checksum-verified XcodeGen
  logs/eve.err.log                the server's own log
```

## Day to day

| Task | Command |
|---|---|
| What's running | `eve jobs` |
| Outcomes nobody has seen (from this machine) | `eve pending` |
| Follow a job again | `eve wait <job>` (`--events` for one line per phase) |
| A job's result and notifications | `eve status <job>` |
| Logs | `eve logs <job> --errors` (or `--phase archive`) |
| Size report; accept or refuse a pause | `eve size <job>`; `eve confirm <job> --reason "…"` / `eve reject <job>` |
| Latest builds in App Store Connect | `eve builds <app>` |
| Reset an app's size baseline | `eve baseline <app> --from <job>` |
| Apps and the verbs each allows | `eve apps` |
| Build in-process on the Mac, without the server | `eve run testflight` in the repo (`eve run exec -- <cmd>` too) |
| Server health | `eve doctor --host` |

When something goes wrong: [troubleshooting.md](troubleshooting.md).
