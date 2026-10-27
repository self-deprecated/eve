# Troubleshooting

A failed job explains itself. The result has `failure.class`, `summary`, `hint` and `excerpt`, and
the full log is behind `eve logs <job> --errors`. This page covers what a result can't explain:
the connection, the phone and the build Mac itself. Start with `eve doctor` (connection and
versions) and `eve doctor --host` (the build Mac's own checks).

## The client

| Symptom | Cause | Fix |
|---|---|---|
| `submit` exits 69: "the eve server … is older than this client (API level N, needs M)" | The client was updated before the server | Update eve on the build Mac. Nothing was uploaded |
| `eve doctor` warns "this client is older than the server" | The client lags behind | Update the client (`brew upgrade`, `scoop update eve`) |
| `submit` exits 64: "X is not available for app on this server (available: …)" | The verb isn't allowed for the app. `exec` needs `allowExec`, per app or at the top level | Allow it in `server.yml`, or use a listed verb. `eve apps` shows each app's verbs |
| `submit` exits 64: "app … is not configured on the server" | `eve.yml`'s `app:` isn't in `server.yml` | Compare with `eve apps`; see [new-app.md](new-app.md) |
| `submit` exits 76 before uploading | The source check found something that looks wrong (`build/`, `node_modules/`, huge files) | Ignore it (`.gitignore`, `.eveignore`, `ignore:`), or pass `--accept-source-size "<why>"` |
| An agent describes verbs or flags that don't exist | Its copy of the skill is out of date | Reinstall `skill/eve/SKILL.md` from the release you run |
| Connection refused or unreachable | The build Mac is off, asleep, or not logged in at the console | `eve doctor`. A cold boot needs a console login unless automatic login is on ([setup.md](setup.md#6-reboots)) |

## The phone (`eve submit device`)

| Result | Meaning | Fix |
|---|---|---|
| `waiting_for_device`, then `needs_human: connect_phone` | The build Mac can't reach the phone (CoreDevice 4000 or 4016, or 4 through the bridge): Wi-Fi off, another network, away, or asleep. Or the connection dropped mid-install (3002) | Turn on the phone's Wi-Fi on the Mac's network (or plug it in) and wake it; away from it, `eve bridge on` (below). eve retries every 5 s for 10 minutes |
| `waiting_for_device`, then `needs_human: unlock_phone` | The phone is reachable but locked, and Xcode's developer disk image isn't mounted yet (CoreDevice 12040; once after the phone restarts) | Unlock it once; later installs work while it's locked |
| `succeeded` with a `not_launched` note | Installed, but the phone was locked at launch | Open the app on the phone |
| `device.unknown` | No paired physical device matches `--device`, or none is paired | Pair it in Xcode on the build Mac (Window → Devices and Simulators). The failure lists the devices the Mac knows |
| `device.untrusted_developer` / `device.attestation` | The phone refused the app | Follow the hint: trust the developer, or restart the phone |

On the build Mac, `xcrun devicectl list devices` shows the phone as `available (paired)` when the
Mac can reach it and `unavailable` when it can't.

### Away from the build Mac's network

`eve bridge on --for 2h` lets the build Mac reach the phone over Tailscale; a job waiting for the
phone carries on by itself. It needs:

- the phone on **Wi-Fi** (any network; on mobile data alone it doesn't listen for Xcode) with
  **Tailscale connected**, and awake for the first call;
- the phone seen on the build Mac's Wi-Fi at least once before (eve remembers it; `eve doctor
  --host` says when it last saw it);
- the phone not restarted since it was last unlocked near the Mac, if Xcode's developer disk
  image has to be mounted again (untested away from home).

| `eve bridge on` says | Fix |
|---|---|
| eve hasn't seen the phone on the build Mac's network yet | Put the phone on the Mac's Wi-Fi once, then try again |
| no iPhone is online on the tailnet / several iPhones are online | Open Tailscale on the phone; or pass `--phone <tailnet name>` (or set `device.bridge.phone`) |
| on, but the phone hasn't answered (exit 69) | Wake the phone, check it's on Wi-Fi with Tailscale connected, and run `eve bridge on` again |

The bridge ends by itself at the end of its lease (default 2h, at most `device.bridge.maxDuration`)
or with `eve bridge off`. While it runs, the build Mac republishes the phone's Bonjour advert on
its own network and relays CoreDevice's (encrypted) connections to the phone's tailnet address,
accepting them only from itself ([design §6.4.1](design.md)).

## The build Mac

| Symptom | Cause | Fix |
|---|---|---|
| `xcode.account` / `needs_human: xcode_sign_in` | The Apple ID in Xcode needs signing in again | Xcode → Settings → Accounts on the build Mac, then resubmit |
| `doctor --host`: "not in the GUI session", or the login keychain is locked | eve isn't running as the logged-in user's LaunchAgent, or the keychain locked | Log in at the console. eve must run in the Aqua session to sign |
| `asc.agreement` / `needs_human: accept_agreement` | Apple wants a new agreement accepted | The Account Holder accepts it in App Store Connect |
| `env.rsync` ("Copy failed" during export) | A non-system `rsync` came first on PATH (Nix's breaks IPA export) | eve puts `/usr/bin` first for its own commands. Check `pre:` scripts and the LaunchAgent's environment |
| `env.tool_missing` (`command not found: corepack`, `node` or `pod`) | A `pre:` command needs a tool that isn't on jobs' PATH. Jobs get only Apple's directories plus server.yml's `path` | Install it on the build Mac and add its directory to `path` ([new-app.md](new-app.md#expo-react-native-apps)) |
| `env.metal_toolchain` | The app compiles Metal shaders and this Xcode's Metal Toolchain isn't installed | `xcodebuild -downloadComponent MetalToolchain` |
| `simulator.unavailable` mentioning bootstatus | The Simulator never finished booting, typically because the Mac is out of memory | Check memory and CoreSimulator, then resubmit |
| A build or tool fails with `opening pseudoterminal master: Device not configured` | Every pseudoterminal is taken (`kern.tty.ptmx_max`), for example by idle detached terminal-multiplexer sessions | `lsof /dev/ptmx` shows who holds them |
| Disk use grows | Finished jobs shrink after a day and go after 30; build caches stay, one per app and verb | `du -sh ~/.local/share/eve/*` ([setup.md](setup.md#7-disk-and-retention)) |

Logs: the server's own is `~/.local/share/eve/logs/eve.err.log`, and each job's are in
`~/.local/share/eve/jobs/<id>/logs/` (also `eve logs <job> --phase <phase>`).
