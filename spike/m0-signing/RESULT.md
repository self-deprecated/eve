# M0 spike 1 — signing from a LaunchAgent (2026-10-16)

**Result: works.** A one-off LaunchAgent in `gui/501` (`LimitLoadToSessionType=Aqua`,
`ProcessType=Interactive`), bootstrapped from a non-GUI shell, archived a throwaway copy of Driver
and exported a signed IPA (`destination=export`, nothing uploaded), with no GUI interaction.

Observed inside the agent:

- `launchctl managername` → `Aqua`; login keychain unlocked (`no-timeout`); 1 valid codesigning
  identity.
- `xcodebuild archive … -allowProvisioningUpdates CURRENT_PROJECT_VERSION=999` succeeded with the
  Apple ID logged in to Xcode.
- `xcodebuild -exportArchive` produced `Driver.ipa` (6.2 MB). Signed by
  `Apple Distribution: Self-Deprecated ApS (2559B88H6C)`; `codesign --verify --deep --strict`
  passes.
- `CFBundleVersion` = 999 in `Driver.app` and `PlugIns/DriverWidgets.appex` (the archive version
  check in PLAN §6.3 works as designed).
- Bundle: 12 MB, 20 files, no repository content (a first manual run of the size-guard scan).

## The failure on the first run — and the rule it produced

Run 1 archived fine but export failed with `exportArchive Copy failed`. The plist set
`PATH=/usr/bin:/bin:/usr/sbin:/sbin`, yet inside the job `command -v rsync` was
`/etc/profiles/per-user/jmo/bin/rsync`: the job ran through `/bin/zsh`, which reads nix-darwin's
`/etc/zshenv` even non-interactively, and that put Nix's rsync first again. Run 2 used
`/bin/zsh -f` (no startup files): rsync was `/usr/bin/rsync` and export succeeded.

Rule (PLAN §4.1): Eve execs tools directly; any shell it starts is `zsh -f` or `sh`; the pipeline
asserts `/usr/bin/rsync` before export.

## Not covered

- Upload (`destination=upload`) — deliberately not run; covered by M1's acceptance.
- Behaviour when the Xcode account session has expired — observed only as a classifier input later.
- Reboot / auto-login (operator step, §18).

Reproduce: `spike/m0-signing/run.sh` (copies `~/Developer/driver` to `/tmp`, leaves the real
checkout untouched).

## Side effect found afterwards

`xcodebuild -exportArchive` with `method=app-store-connect` and `destination=export` still
registered an upload with App Store Connect: Driver shows a `buildUploads` record for version 0.1,
build 999, state `AWAITING_UPLOAD` (found by the asc package's live read-only checks). No binary
was uploaded and nothing reached testers. Consequences for Eve: local "export only" runs are not
invisible to Apple, so spikes and `--no-upload`-style runs should use throwaway build numbers far
below real ones (as 999 was) or another export method; and reconciliation must ignore
`AWAITING_UPLOAD`-only records (it does: they count as "not visible").
