# Operating eve

## On Quartz (once)

### 1. Install through mono-nix

Done in mono/nix (`hosts/quartz/eve/_home.nix`, home-manager): the flake input and the module:

```nix
# flake.nix inputs
eve = {
  url = "path:/Users/jmo/Development/projects/eve"; # or the git remote once pushed
  inputs.nixpkgs.follows = "nixpkgs";
};

# hosts/quartz/eve/_home.nix (imported by home-manager.users.jmo)
imports = [ inputs.eve.homeManagerModules.default ];
services.eve = {
  enable = true;
  tailscaleService.enable = false;   # true once svc:eve exists (step 3)
  settings = { … };                  # the contents of server.yml below, as Nix
};
```

After changing go.mod, update `vendorHash` in `nix/package.nix` (set `lib.fakeHash`, build,
copy the hash from the error).

Without Nix: `eve agent install --config ~/.config/eve/server.yml`.

### 2. server.yml

```yaml
team: 2559B88H6C
allowExec: true                    # every app may run `eve submit exec`; or set it per app
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

### 4. Tokens

The owner token is created on first start (`~/.local/share/eve/token`) and never leaves Quartz.
For each other machine:

```bash
eve token create lattice     # prints the token once
# on Lattice:
eve login https://eve.tail6e682f.ts.net --token eve_…
eve doctor
```

Agents on Quartz itself need no login: with no `client.yml`, the CLI uses the local server and
its owner token.

### 5. Auto-login after reboots

Decided in PLAN.md §18: Quartz logs in automatically so eve comes back by itself. macOS refuses
automatic login while FileVault is on, so: `sudo fdesetup disable` (wait for decryption), then
System Settings → Users & Groups → Automatically log in as `jmo`, then reboot and check
`eve doctor` from Lattice. Until then, a cold boot needs a console login.

## In each app repository

See [new-app.md](new-app.md): the server entry, `eve init` for `eve.yml`, and the first runs.

## Day to day

| Task | Command |
|---|---|
| What's running | `eve jobs` |
| Outcomes nobody has seen (this machine) | `eve pending` |
| Follow a job again | `eve wait <job>` (`--events` for one line per phase) |
| A job's result and notifications | `eve status <job>` |
| Logs | `eve logs <job> --errors` (or `--phase archive`) |
| Size report | `eve size <job>` |
| Accept / refuse a size pause | `eve confirm <job> --reason "…"` / `eve reject <job>` |
| Latest builds in App Store Connect | `eve builds <app>` |
| Reset an app's size baseline | `eve baseline <app> --from <job>` |
| Apps and the verbs each allows | `eve apps` |
| Dev build on the phone | `eve submit device --wait` |
| Run a repo script on Quartz, fetch its output | `eve submit exec --wait -- <script>`, then `eve artifacts <job> --out <dir>` |
| Build in-process on Quartz (no server) | `eve run testflight` in the repo (`eve run exec -- <cmd>` too) |
| Server health | `eve doctor --host` |

Logs: `~/.local/share/eve/logs/eve.err.log` (server), `~/.local/share/eve/jobs/<id>/logs/` (jobs).

When something goes wrong: [troubleshooting.md](troubleshooting.md).
