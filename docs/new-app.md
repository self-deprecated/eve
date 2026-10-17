# Adding an app to eve

An app needs two things: an entry in eve's server config on Quartz (identity: bundle id, TestFlight
group, secrets, what it may run) and an `eve.yml` in its repository (where the Xcode project is).
Neither knows about the other's details.

## 1. Apple's side

- The bundle id is registered for team `2559B88H6C`, and the app record exists in App Store
  Connect (eve doesn't create either; `create_app_record` in a result means it's missing).
- The TestFlight group the builds go to exists and is internal.
- The project signs automatically with the team, so the Apple ID signed in to Xcode on Quartz can
  build it.

## 2. Server config (mono-nix)

Add the app to `services.eve.settings.apps` in `mono-nix/hosts/quartz/eve/_home.nix`, then
switch Quartz:

```nix
myapp = {
  bundleId = "ai.selfdeprecated.myapp";
  testflight.group = "Internal";          # the internal group that gets every build
  # testflight.tester = "me@example.com"; # eve reports whether they've accepted the invite
  # allowExec = true;                     # for `eve submit exec` (scripts on Quartz)
  # secrets.MY_SETTING = "file:~/.config/myapp/secret";  # build setting, via a 0600 xcconfig
  # device = "Jordan’s iPhone";           # default phone for `eve submit device`
};
```

Every app is internal-only on TestFlight unless `export.internalOnly = false`. Size-guard
thresholds can be raised per app under `sizeGuard` (PLAN.md §6.7). Check it took: `eve apps`
lists the app and the verbs it allows.

## 3. The repository

From the repository root:

```sh
eve init        # writes eve.yml from the project.yml or .xcodeproj here or one directory down
```

Check `app:` matches the name in `eve apps`, then cover anything large the build doesn't need
(screenshots, design files, a backend) with `ignore:`, `.gitignore` or `.eveignore`: every submit
uploads the rest. The eve skill documents every key; the usual extras are `pre:` (commands before
building, e.g. a package's tests) and `check: {test: true}`.

## 4. First runs

```sh
eve submit check --wait        # Simulator build: proves eve.yml, XcodeGen and the scheme
eve submit device --wait       # development build on the phone
eve submit testflight --wait --notes "first build through eve"
```

The first TestFlight build becomes the app's size baseline; later builds that grow unexpectedly
pause for confirmation (`eve size`, `eve confirm`). Once these pass, delete the repository's own
upload, device and Quartz scripts: eve replaces `xcodebuild archive`/`-exportArchive`,
`devicectl`, and rsync-and-ssh to Quartz (scripts that must run on Quartz become
`eve submit exec -- <script>` with their output in `$EVE_ARTIFACTS`).

When something fails, see [troubleshooting.md](troubleshooting.md).
