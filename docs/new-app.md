# Adding an app

An app needs two things:

- **An entry in the build Mac's `server.yml`:** its identity, meaning bundle id, TestFlight group,
  secrets, and what it may run.
- **An `eve.yml` in its repository:** where the Xcode project is.

Neither file knows the other's details.

## 1. Apple's side

eve creates neither the bundle id nor the app record. A `create_app_record` action in a result
means the record is missing. Before the first build:

- The bundle id is registered for your team, and the app record exists in App Store Connect.
- The TestFlight group the builds go to exists and is internal.
- The project signs automatically with your team, so the Apple ID signed in to Xcode on the build
  Mac can sign it.

## 2. The server entry

Add the app under `apps` in `server.yml` (or `services.eve.settings.apps` with Nix), then restart
the server: `launchctl kickstart -k gui/$(id -u)/ai.selfdeprecated.eve`, or switch your Nix
configuration. A running job is picked up again after the restart.

```yaml
apps:
  myapp:
    bundleId: com.example.myapp
    testflight:
      group: Internal              # the internal group that gets every build
      # tester: you@example.com    # eve reports whether they've accepted the invite
    # secrets:
    #   API_BASE_URL: file:~/.config/myapp/api-base-url   # a build setting, via a 0600 xcconfig
    # device: "Sam’s iPhone"       # default phone for `eve submit device`
    # allowExec: true              # allow `eve submit exec` (or set allowExec at the top level)
```

Every app is internal-only on TestFlight unless it sets `export: {internalOnly: false}`.
Size-guard thresholds can be raised per app under `sizeGuard`. [configuration.md](configuration.md)
lists every key. Check that the change took: `eve apps` lists the app and the verbs it allows.

## 3. The repository

From the repository root:

```sh
eve init        # writes eve.yml from the project.yml or .xcodeproj here or one directory down
```

Check that `app:` matches the name in `eve apps`. Then cover anything large the build doesn't need
(screenshots, design files, a backend) with `ignore:`, `.gitignore` or `.eveignore`, because every
submit uploads the rest. The usual extras are `pre:` (commands before building, such as a
package's tests) and `check: {test: true}`.

## 4. First runs

```sh
eve submit check --wait        # Simulator build: proves eve.yml, XcodeGen and the scheme
eve submit device --wait       # development build on the phone
eve submit testflight --wait --notes "first build through eve"
```

The first TestFlight build becomes the app's size baseline. Later builds that grow unexpectedly
pause for confirmation (`eve size`, `eve confirm`).

Once these pass, the repository's own upload and device scripts can go. eve replaces
`xcodebuild archive` and `-exportArchive`, `devicectl`, and copying the tree to the build Mac over
SSH. A script that has to run on the Mac becomes `eve submit exec -- <script>`, with its output in
`$EVE_ARTIFACTS`.

## Expo (React Native) apps

eve builds an Expo app from the Xcode workspace `expo prebuild` generates, with the same pipeline
as a native app: build numbers, signing, the size guard, TestFlight and the phone. EAS isn't
involved. Each build generates `ios/` afresh, so it stays git-ignored and is never uploaded.

**The build Mac** needs Node and CocoaPods. Add their directories to `path` in `server.yml`. With
Nix, for example, use `path = [ "${pkgs.nodejs_24}/bin" "${pkgs.cocoapods}/bin" ];` in
`services.eve.settings`. Every job's commands and Xcode's script phases (Metro, Hermes) find them
after Apple's tools.

**eve.yml** goes at the root of the JavaScript workspace, next to the lockfile, because `pre:`
installs from there. For a pnpm monorepo with the app in `apps/mobile`:

```yaml
app: myapp
project:
  dir: apps/mobile/ios
  xcworkspace: MyApp.xcworkspace   # prebuild names it after the app's name, without spaces
  scheme: MyApp
pre:
  - corepack pnpm install --frozen-lockfile
  # --no-install: prebuild exits 0 even when its own pod install fails.
  - cd apps/mobile && corepack pnpm exec expo prebuild --platform ios --clean --no-install
  - cd apps/mobile/ios && pod install
```

**app.json** needs two things eve relies on:

- `"ios": {"appleTeamId": "ABCDE12345"}`, because automatic signing needs the team in the project.
- `"ios": {"buildNumber": "$(CURRENT_PROJECT_VERSION)"}`, so the archive carries the build number
  eve allocates. A literal number fails with `build.number_mismatch`.

Also:

- **`EXPO_PUBLIC_*` values.** A Release build bundles the JavaScript inside Xcode, and eve never
  uploads `.env` files. Set the values as the app's `secrets` in `server.yml`. They become build
  settings, which Xcode's script phases see as environment variables. A Debug dev-client build
  loads its JavaScript from Metro instead, so it doesn't need them.
- **Device builds.** Debug, the default, is a dev client that needs a running
  `expo start --dev-client`. Set `device: {configuration: Release}` (or pass `--release`) for an
  app that runs on its own.
- **Podfile fixes** that dependencies need belong in a config plugin, so prebuild applies them
  every time. For example, Xcode 27 refuses pods that target iOS older than 15, and Google Sign-In
  needs static frameworks.
- The first build is slow: it installs pods and compiles React Native. Later ones reuse eve's
  DerivedData, and the pnpm store and CocoaPods caches in the home directory.

When something fails, see [troubleshooting.md](troubleshooting.md).
