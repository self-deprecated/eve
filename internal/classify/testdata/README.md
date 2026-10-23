# Classifier fixtures

`logs/<class>/<name>.log` must classify as `<class>`. Two directories are not classes:

- `success/`: logs of runs that succeeded. They must stay `unknown` in every phase.
- `unknown/`: real failures that no rule explains yet. They must stay `unknown` too.

Real fixtures were redacted with `internal/redact` by `harvest_test.go` (home → `~`, UDIDs and
SHA-1s → `[udid]`, the owner's name, the iPhone's ECID, serial and CoreDevice ID → `[secret]`),
and trimmed to the relevant part (≤ 200 lines; distribution logs are the last 200 lines of
`IDEDistribution.verbose.log`). Team ID 2559B88H6C and bundle IDs are public and kept.

Sources, all on Quartz, harvested 2026-10-16:

- **build log**: `~/Developer/*/ios/build/**`, `~/.t3/worktrees/bodydiff/t3-f88410d8/build/**`
- **distribution log**: `$TMPDIR/<App>_<date>.xcdistributionlogs/IDEDistribution.verbose.log`
- **transcript**: verbatim tool output (xcodebuild, devicectl, `swift test`) in Claude Code
  session transcripts under `~/.claude/projects/` (the user's scripts print xcodebuild's
  `error:` lines and result line, so these are partial logs)
- **synthetic**: written for this package from the exact error strings in the session dossier,
  docs/design.md §9 and Apple's documented messages, because no real log of the failure survived

| Class | Fixture | Source |
|---|---|---|
| asc.agreement | asc-403-required-agreements | synthetic (ASC API 403 body) |
| asc.app_record | barbican-missing-app | distribution log (Barbican, 2026-10-15 13:06) |
| asc.app_record | barbican-name-taken | distribution log (Barbican, 2026-10-15 14:38) |
| asc.auth | asc-401-not-authorized | synthetic (ASC API 401 body) |
| asc.auth | screengate-bearer-token | transcript (ScreenGate device build) |
| build.compile | bodydiff-camera | transcript (BodyDiff simulator build) |
| build.compile | lineage-observable-macro | transcript (Lineage) |
| build.compile | lineage-session-ansi | transcript (Lineage `swift build`, ANSI colours) |
| build.compile | lineage-transition | transcript (Lineage) |
| device.attestation | screengate-unable-to-verify | synthetic ("Unable to Verify App", syslog line from the dossier) |
| device.locked | bodydiff-launch | transcript (devicectl launch) |
| device.locked | lineage-launch | transcript (devicectl launch) |
| device.unavailable | lineage-devicectl-info | transcript (`devicectl device info details`) |
| device.unavailable | xcodebuild-destination | synthetic around real "Ineligible destinations" lines |
| device.untrusted_developer | screengate-launch | transcript (devicectl launch, FBSOpenApplicationErrorDomain 3) |
| env.metal_toolchain | bodydiff-expo-run | transcript (`expo run:ios`, Expo-formatted xcodebuild output) |
| env.tool_missing | approveal-pre-corepack | pre log (Approve AL on Quartz without `path`, 2026-10-23) |
| env.rsync | driver-distribution | distribution log (Driver, 2026-10-16 19:21) |
| env.rsync | lineage-distribution | distribution log (Lineage, 2026-10-14 14:13) |
| env.rsync | lineage-export | transcript (Lineage upload) |
| env.rsync | screengate-export | transcript (ScreenGate upload) |
| signing.cloud_permission | quoth-distribution | distribution log (Quoth, 2026-10-14 17:07) |
| signing.cloud_permission | screengate-distribution | distribution log (ScreenGate, 2026-10-13 20:36) |
| signing.cloud_permission | screengate-export | transcript (ScreenGate upload) |
| signing.cloud_permission | screengate-profile-qualification | distribution log (ScreenGate, 2026-10-14 18:28) |
| signing.device_unregistered | screengate-build | transcript (ScreenGate device build) |
| signing.entitlement_mismatch | screengate-app-group | transcript (ScreenGate device build) |
| signing.no_team | screengate-build | transcript (ScreenGate device build) |
| signing.wwdr_expired | codesign-chain | synthetic (codesign chain warning + errSecInternalComponent) |
| tests.failed | lineage-island-swift-testing | transcript (Lineage Swift Testing run) |
| tests.failed | screengate-xcuitest | synthetic (XCTest/XCUITest format) |
| tests.pre | lineage-swift-test | transcript (Lineage `swift test`) |
| unknown | barbican-xcframework | transcript (Barbican simulator build) |
| unknown | bodydiff-missing-return | transcript (only Swift's diagnostic drawing survived) |
| upload.duplicate | altool-itms-90189 | synthetic |
| upload.duplicate | export-redundant-binary | synthetic |
| upload.export_compliance | asc-missing-compliance | synthetic (internalBuildState) |
| upload.export_compliance | itms-90592 | synthetic |
| upload.missing_icon | screengate-distribution | distribution log (ScreenGate, 2026-10-13 20:37) |
| upload.missing_icon | screengate-export | transcript (ScreenGate upload) |
| upload.purpose_string | asc-processing-two-keys | synthetic (two keys, from the BodyDiff Expo era) |
| upload.purpose_string | driver-camera | synthetic (NSCameraUsageDescription, Driver/WebRTC) |
| xcode.account | barbican-failed-to-use-accounts | distribution log (Barbican, 2026-10-15 14:33) |
| xcode.account | xcode-session-expired | synthetic |
| success | bodydiff-archive, bodydiff-export, bodydiff-device | build log |
| success | bodydiff-distribution | distribution log (BodyDiff, 2026-10-14 13:11) |
| success | driver-archive, driver-device, driver-sim, driver-test, driver-ux-device | build log |
| success | driver-upload-warnings-distribution | distribution log ("Upload completed with warnings": missing WebRTC dSYM, -19237) |
| success | screengate-buildupload-warning-distribution | distribution log (-19239 buildUploadFiles warning) |
| success | screengate-device, screengate-uitest | build log |

To regenerate the real fixtures, stage raw selections under a directory mirroring `logs/` and run

```
EVE_HARVEST_SRC=<dir> EVE_HARVEST_SECRETS='<name>,<surname>,…' go test -tags harvest -run Harvest ./internal/classify/
```
