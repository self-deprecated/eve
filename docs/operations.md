# Operating Eve: Simulator lifecycle

For installation and normal server operations, see [setup](setup.md). This page describes
Simulator ownership, timed borrowing, cleanup and activation.

## Simulator lifecycle

Eve owns only devices it creates for `check` with `check.test: true`, in both server jobs and
`eve run check`. Generic Simulator builds do not boot a device. TestFlight archive/export and
its parked App Store Connect watchers do not own Simulators. The physical `device` pipeline
selects a physical iPhone via CoreDevice, never a Simulator. `exec` runs arbitrary repository
commands (including screenshot scripts), and `pre` commands can run for any verb: devices
those scripts create are outside this registry. Those scripts must explicitly shut down their
owned devices on success/failure/cancellation; a successful Eve job alone does not establish
device ownership. Shared devices need coordination with their actual owner, as described below.

A test still starts with a fresh `eve-<job>` device. Eve records its exact UDID, original name
and owning job in the `simulators` table of `<dataDir>/eve.db` **before booting it**. The lease
lasts throughout boot and test execution; it never expires because of low CPU, age, an old
CoreSimulator `lastUsedAt`, lack of input, or lack of command output. A create/registration
failure may leave a stopped, unregistered device for manual review; Eve never boots it.

Completion, compile/test failure, partial boot failure, cancellation and graceful server stop
all attempt **shutdown**, preserving installed apps, data and the device definition. Shutdown
uses a cancellation-independent deadline (default 30 seconds; the command runner can then take
up to 20 seconds to kill its process group). Cleanup runs before the check publishes its
result. Both LaunchAgent definitions allow 240 seconds for graceful exit, including the
maximum configured cleanup deadline and build/HTTP drain. A cleanup failure adds a
`simulator_cleanup` result note and leaves the released lease
pending; it does not turn passing tests into failing tests. `eve simulator list` retains the
last error and JSON includes the release and last-attempt times.

The worker checks pending cleanup at most once a minute while it can acquire the same host
lock used by `eve run`. After the idle timeout, it retries only explicitly released leases.
Startup recovery takes that lock too, stops Eve's recorded orphaned process groups and
requeues/finishes interrupted jobs before cleanup can release their abandoned leases. Those
leases get a **new full idle period**, starting at recovery, rather than an age calculated from
before the crash. Unrecovered/running jobs retain their leases. `eve run` attempts pending
cleanup once before its job but has no background timer after exit; ongoing retries require
`eve serve`. A busy worker can delay cleanup beyond the timeout.

Each attempt rechecks the recorded UDID and exact name against a fresh inventory. Renamed,
missing, malformed or unknown-state inventory is left for review, with an error rather than
an inferred shutdown. Already stopped devices settle without a shutdown command. Once
shutdown succeeds, the record settles permanently: a later boot by a human or another
workflow does **not** cause Eve to shut it down again. Eve never adopts devices by name,
never runs `shutdown all`, and never erases or deletes Simulators. Stopped definitions and
their on-disk data remain; disk retention is a separate operator decision.

### Configuration and borrowing

Server-side `server.yml` (not an uploaded eve.yml):

```yaml
simulators:
  idleTimeout: 15m          # default; 0s disables automatic retry/recovery, not job teardown
  shutdownTimeout: 30s     # default; 1s–2m (omitted or 0s uses the default)
  maxLeaseDuration: 1h     # maximum requested time per acquisition/renewal; 0s uses 1h
  protectedDevices: []     # operator-only indefinite exclusions, independent of agent leases
```

Through home-manager these are `services.eve.settings.simulators` with the same fields; for
example `idleTimeout = "15m"; maxLeaseDuration = "1h";`. The maximum must be at least one second.
Each new request is checked against this server setting. Reducing it applies to new grants and
renewals; an already granted deadline remains valid until its advertised expiry.

New Eve test devices are exclusive to the test job. Eve cannot observe every T3 Device panel,
agent-device session, Indentured session or human interaction atomically. **Before borrowing
an Eve-tracked device**, acquire a timed borrow lease on Quartz and wait for the Eve job to
finish before driving it. Borrowing does not cancel or pause that job:

```bash
eve simulator list                         # host-local; add --json for expiry/status fields
eve simulator renew <exact-udid> --for 30m --json
# Use it through T3 Device / agent-device / Indentured / Simulator.app.
eve simulator renew <exact-udid> --for 30m --json   # renew before the previous expiry
# When finished, end the borrow lease early (optional; expiry also ends it):
eve simulator release <exact-udid>
```

`renew` both acquires and renews a lease. `--for` is required, must be at least one second and
must not exceed `maxLeaseDuration` (default one hour). Excessive requests fail and report the
maximum; Eve never silently grants less time than requested. Success returns the exact UTC
`leaseUntil` and `maxLeaseDuration`; `list --json` includes `leaseUntil` and `leaseActive`.
Each renewal replaces the deadline with **now + requested duration**, rather than adding time
to the old expiry. Repeating a renewal cannot stack hours past the per-request maximum; a
shorter requested duration can shorten the deadline. Leases persist across restart with the
same absolute expiry. There is one borrow lease per device: one agent/wrapper must coordinate
its renewal and release rather than treating it as a reference-counted lock for independent
borrowers.

For example: renew for 30 minutes at 12:00; the deadline is 12:30. Renew for 30 minutes at
12:25 and the deadline becomes 12:55. If the agent disappears, the lease expires on its own.
Stop using the device before the current deadline unless another renewal has succeeded.
Renewal/release is serialized against shutdown; if cleanup is in progress the command fails
with `simulator lifecycle busy`. Rejected requests leave the old deadline unchanged. If the
command's output is lost, check `list --json` before relying on a grant. The commands
do not boot a device, and unknown UDIDs are never adopted. Renewal rejects settled cleanup
records, since Eve no longer manages those devices' power state.

Expiry or explicit `release` removes the borrow hold; the normal job/idle conditions still
apply. During idle sweeping, a released job's device is eligible at the later of
**job release + idleTimeout** and **leaseUntil**, whenever the worker is free. Expiry does not
add another full 15-minute grace period if the job was already idle. Explicit release clears
the deadline without resetting the job's idle timestamp, so an already-idle device can be
shut down on the next sweep. A borrow lease expiring during an active Eve job never interrupts
that job; its separate job lease ends only when execution stops. Disabling idle cleanup also
disables automatic cleanup after borrow expiry; explicit job teardown still runs.

`protectedDevices` remains an explicit operator opt-out: agent renewal/release cannot remove
that exclusion. If upgrading a database from the earlier indefinite-pin implementation, old
pins are retained as `legacyProtected`/`legacy-protected` so an upgrade cannot silently shut
down a device somebody still uses. The operator must explicitly convert each such pin with
`renew --for ...` or end it with `release`. New commands cannot create indefinite pins;
`protect` and `unprotect` have been replaced by `renew` and `release`.

### Shared host boundary and activation

The five previously reported Quartz devices (iPhone 17 Pro, iPhone 17, two ScreenGate
Screenshots devices, and BodyDiff Screenshots) are **not enrolled** by this change. Neither are
legacy `eve-` devices without a registry row. Their boot duration, process count and memory
usage do not prove that no person or service is using them. Review their actual owner before
any separate manual shutdown; no diagnostic shutdown is required to validate this change.

General idle shutdown for those workflows belongs in a separate **mono-nix host integration**,
coordinated with their owners. It needs exact device ownership, renewable borrow leases and
non-expiring active-job leases from T3 Device/agent-device, Indentured and screenshot wrappers,
explicit release at completion/failure/disconnect, crash reconciliation, and a human opt-out
before shutdown.
Idle expiry of a session must use that owner's real activity signal. Do not infer inactivity
from low CPU, process command lines, old `lastUsedAt` or absent Eve jobs. Indentured's managed
session documentation already describes its own session idle/lifetime/teardown contract;
Eve does not query or override that authority. Uncoordinated borrowing of a pending Eve device
without a valid borrow lease is unsupported; leave shared devices outside Eve's registry.

On Quartz, the installed CLI is in the Nix profile and `~/.config/eve/server.yml` is a
home-manager symlink. After reviewing/merging this change, the operator must update the Eve
input/package used by mono-nix, build it, and perform the normal home-manager activation and
Eve LaunchAgent restart **when builds are idle**. Back up `eve.db` using the normal SQLite
backup procedure before first start; startup adds the ownership table automatically. Do not
edit the generated config symlink. This source change does not activate the package, restart
the service, or operate any existing device. No reboot or signing/secret change is needed.

For source verification, use `nix develop --command go test -shuffle=on -count=1 ./...` and
`nix develop --command go test -race ./internal/simulator ./internal/store ./internal/pipeline ./internal/queue ./internal/cli`.
These tests use fake Apple tools and temporary databases, including cancellation, boot and
shutdown failures, crash recovery, busy locks, timed renewal/expiry, server duration caps,
legacy-pin migration and later external boots.
After activation, observe the registry and a normal authorized check job's cleanup result;
live CoreSimulator behavior is not attested by the fake-tool suite.
