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

When something fails, see [troubleshooting.md](troubleshooting.md).
