# Troubleshooting eve

A failed job explains itself: `failure.class`, `summary`, `hint` and `excerpt` in the result, the
full log behind `eve logs <job> --errors`. This page covers what a result can't: the host, the
connection, the phone, and the problems seen so far. Start with `eve doctor` (connection and
version) and `eve doctor --host` (Quartz's own checks).

## The client

| Symptom | Cause | Fix |
|---|---|---|
| `submit` exits 69: "the eve server … is older than this client (API level N, needs M)" | Lattice's fast lane updated the client before Quartz switched | Switch Quartz to the latest mono-nix. Nothing was uploaded |
| `eve doctor` warns "this client is older than the server" | The client lags | `sd fast-lane update` (from a mono-nix checkout) |
| `submit` exits 64: "X is not available for app on this server (available: …)" | The verb isn't allowed for the app (`exec` needs `allowExec`, per app or top-level for all) | Add it in the server config, or use a listed verb. `eve apps` shows each app's verbs |
| `submit` exits 64: "app … is not configured on the server" | `eve.yml`'s `app:` isn't a server app | Compare with `eve apps`; see [new-app.md](new-app.md) |
| `submit` exits 76 before uploading | The source check found something that looks wrong (`build/`, `node_modules/`, huge files) | Ignore it (`.gitignore`, `.eveignore`, `ignore:`), or `--accept-source-size "<why>"` |
| An agent describes verbs or flags that don't exist | Its skill is stale | The skill follows the fast lane: `sd fast-lane update` |
| Connection refused / unreachable | Quartz is down, asleep, or not logged in at the console | `eve doctor`; a cold boot needs a console login unless auto-login is set up (operations.md) |

## The phone (`eve submit device`)

| Result | Meaning | Fix |
|---|---|---|
| `waiting_for_device`, then `needs_human: connect_phone` | Quartz can't reach the phone (CoreDevice 4000/4016): Wi-Fi off, on another network, away, or asleep | Turn on its Wi-Fi on Quartz's network (or plug it into Quartz) and wake it. eve retries every 5 s for 10 minutes |
| `waiting_for_device`, then `needs_human: unlock_phone` | Reachable but locked before Xcode's developer disk image is mounted (CoreDevice 12040, once after a restart) | Unlock it once; later installs work while it's locked |
| `succeeded` with a `not_launched` note | Installed, but the phone was locked at launch | Open the app on the phone |
| `device.unknown` | No paired physical device matches `--device` (or none is paired) | Pair it in Xcode on Quartz (Window → Devices and Simulators); the failure lists the devices Quartz knows |
| `device.untrusted_developer` / `device.attestation` | The phone refused the app | Follow the hint (trust the developer; restart the phone) |

`xcrun devicectl list devices` on Quartz shows the phone as `available (paired)` when Quartz can
reach it and `unavailable` when it can't.

## Quartz

| Symptom | Cause | Fix |
|---|---|---|
| A Nix build or switch on Quartz fails: `opening pseudoterminal master: Device not configured` | Every pseudoterminal is taken (kern.tty.ptmx_max is 511). On 2026-10-17, 507 idle, detached Zellij sessions held them | `lsof /dev/ptmx` shows who holds them; `zellij kill-session` the idle detached ones. Unrelated to eve, but it blocks switching to a new eve |
| `xcode.account` / `needs_human: xcode_sign_in` | The Apple ID in Xcode on Quartz needs signing in again | Xcode → Settings → Accounts on Quartz, then resubmit |
| `doctor --host`: "not in the GUI session" / login keychain locked | eve isn't running as the logged-in user's LaunchAgent, or the keychain locked | Log in at the console; eve must run in the Aqua session to sign |
| `asc.agreement` / `needs_human: accept_agreement` | Apple wants a new agreement accepted | The Account Holder accepts it in App Store Connect |
| `simulator.unavailable` mentioning bootstatus | The Simulator never finished booting (seen when Quartz was out of memory) | Check Quartz's memory and CoreSimulator, then resubmit |
| Disk use | Finished jobs shrink after a day and go after 30; build caches stay, one per app and verb | `du -sh ~/.local/share/eve/*` on Quartz |

Server log: `~/.local/share/eve/logs/eve.err.log` on Quartz. Job logs:
`~/.local/share/eve/jobs/<id>/logs/` (also `eve logs <job> --phase <phase>`).
