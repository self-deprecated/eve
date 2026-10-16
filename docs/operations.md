# Operating eve

## On Quartz (once)

### 1. Install through mono-nix

Add the flake input and the module to `hosts/quartz` (nix-darwin):

```nix
# flake.nix inputs
eve = {
  url = "path:/Users/jmo/Development/projects/eve"; # or the git remote once pushed
  inputs.nixpkgs.follows = "nixpkgs";
};

# hosts/quartz/default.nix
imports = [ inputs.eve.darwinModules.default ];
services.eve = {
  enable = true;
  user = "jmo";
  settings = import ./_eve-settings.nix;   # the contents of server.yml below, as Nix
};
```

Before the first `darwin-rebuild`, set `vendorHash` in `nix/package.nix`: build once
(`nix build .#eve`), copy the expected hash from the error.

Without Nix: `eve agent install --config ~/.config/eve/server.yml`.

### 2. server.yml

```yaml
team: 2559B88H6C
asc:
  keyId: J9584QW2TF
  issuerId: 25a2d76f-91fe-49e0-9551-09af06f209dc
  keyPath: ~/.appstoreconnect/private_keys/AuthKey_J9584QW2TF.p8
apps:
  driver:
    bundleId: ai.selfdeprecated.driver
    testflight: {group: Driver}
  lineage:
    bundleId: ai.selfdeprecated.lineage
    testflight: {group: Internal}
  screengate:
    bundleId: ai.selfdeprecated.screengate
    testflight: {group: Internal}
    allowExec: true                # screenshots.sh via `eve submit exec` (M3)
  bodydiff:
    bundleId: ai.selfdeprecated.bodydiff
    testflight: {group: Internal}
    secrets:
      BODYDIFF_COACH_CODE: file:~/.config/bodydiff/coach-code
notify:
  allowHosts: ["*.tail6e682f.ts.net"]
  # human: {url: https://…}        # optional: a GrokBot webhook for things only you can do
```

Every app is internal-only on TestFlight by default (`export.internalOnly`); set it to `false`
explicitly only for an app that should reach external testers. Size-guard thresholds can be raised
per app under `apps.<app>.sizeGuard` (PLAN.md §6.7).

### 3. Publish it on the tailnet: `svc:eve`

Quartz's tailnet ACL admits only 443 and 22, and Quartz's own :443 belongs to Indentured, so eve
gets its own Tailscale Service (like `svc:slipway`):

1. Tailscale admin console → Services → create `svc:eve`, TCP 443, and approve Quartz as host.
2. **Add the route to Indentured's Serve reconciler in mono-nix, not by hand.**
   `org.jeprecated.indentured-server-serve` (root LaunchDaemon, see
   `mono-nix/hosts/quartz/tailscale-https.md`) is the sole owner of Quartz's complete Serve state and
   reverts anything else every 60 s. Its owned state gains the eve service, applied with the same
   form slipway uses on Lattice:
   `tailscale serve --yes --service=svc:eve --https=443 http://127.0.0.1:4790`,
   and its drift classifier must accept that service entry.
3. Clients use `https://eve.tail6e682f.ts.net`.

### 4. Connect T3 (results posted into agent threads)

```bash
eve t3 login     # opens T3's approval page once; stores the token in ~/.local/share/eve
eve t3 status
```

`eve doctor --host` warns before the authorization expires.

### 5. Tokens

The owner token is created on first start (`~/.local/share/eve/token`) and never leaves Quartz.
For each other machine:

```bash
eve token create lattice     # prints the token once
# on Lattice:
eve login https://eve.tail6e682f.ts.net --token eve_…
eve doctor
```

Agents running on Quartz itself log in the same way with a `quartz-agents` token.

### 6. Auto-login after reboots

Decided in PLAN.md §18: Quartz logs in automatically so eve comes back by itself. macOS refuses
automatic login while FileVault is on, so: `sudo fdesetup disable` (wait for decryption), then
System Settings → Users & Groups → Automatically log in as `jmo`, then reboot and check
`eve doctor` from Lattice. Until then, a cold boot needs a console login.

## In each app repository

Add `eve.yml` at the repo root:

```yaml
# Driver (~/Developer/driver)
app: driver
project: {dir: ios, xcodegen: project.yml, scheme: Driver}
```

```yaml
# Lineage
app: lineage
project: {xcodeproj: Lineage.xcodeproj, scheme: Lineage}
pre:
  - swift test --package-path LineageKit
```

```yaml
# ScreenGate
app: screengate
project: {dir: ios, xcodegen: project.yml, scheme: ScreenGate}
```

```yaml
# BodyDiff (native)
app: bodydiff
project: {xcodegen: project.yml, scheme: BodyDiff}
pre:
  - swift test --package-path BodyDiffCore
```

Then from the repo: `eve submit testflight --wait`.

## Day to day

| Task | Command |
|---|---|
| What's running | `eve jobs` |
| A job's result and notifications | `eve status <job>` |
| Logs | `eve logs <job> --errors` (or `--phase archive`) |
| Size report | `eve size <job>` |
| Accept / refuse a size pause | `eve confirm <job> --reason "…"` / `eve reject <job>` |
| Latest builds in App Store Connect | `eve builds <app>` |
| Reset an app's size baseline | `eve baseline <app> --from <job>` |
| Build in-process on Quartz (no server) | `eve run testflight` in the repo |
| Server health | `eve doctor --host` |

Logs: `~/.local/share/eve/logs/eve.err.log` (server), `~/.local/share/eve/jobs/<id>/logs/` (jobs).
