#!/bin/zsh
# M0 spike 1 (docs/design.md §16): can a LaunchAgent in the GUI session archive and sign an app with
# Eve's child environment and no GUI interaction? Archives a throwaway copy of Driver and exports
# a signed IPA locally (destination=export). Nothing is uploaded to App Store Connect.
# Usage: spike/m0-signing/run.sh   → prints the job's output and exits with its status.
set -u
work=$(mktemp -d /tmp/eve-spike-signing.XXXXXX)
label=ai.selfdeprecated.eve.spike-signing
domain=gui/$(id -u)
src=$HOME/Developer/driver
xcodegen=$HOME/Developer/screen-gate/.tools/xcodegen/bin/xcodegen
echo "work dir: $work"

/usr/bin/rsync -a --exclude 'ios/build/' --exclude '.tools/' "$src/" "$work/driver/"

# The job script: what Eve's worker would run, with Eve's child environment (docs/design.md §4.1).
cat > $work/job.sh <<JOB
#!/bin/zsh
set -eu
echo "session: \$(launchctl managername 2>/dev/null || echo unknown)  uid: \$(id -u)"
echo "keychain: \$(security show-keychain-info ~/Library/Keychains/login.keychain-db 2>&1 | head -1)"
echo "identities: \$(security find-identity -v -p codesigning | tail -1)"
echo "DEVELOPER_DIR=\$DEVELOPER_DIR  rsync=\$(command -v rsync)"
cd $work/driver/ios
$xcodegen generate --quiet
echo "==> archive"
xcodebuild archive -project Driver.xcodeproj -scheme Driver -configuration Release \\
  -destination 'generic/platform=iOS' -archivePath $work/Driver.xcarchive \\
  -derivedDataPath $work/DerivedData CURRENT_PROJECT_VERSION=999 -allowProvisioningUpdates \\
  > $work/archive.log 2>&1 || { grep -E 'error:' $work/archive.log | head -20; exit 1; }
tail -n 1 $work/archive.log
cat > $work/ExportOptions.plist <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>method</key><string>app-store-connect</string>
  <key>destination</key><string>export</string>
  <key>teamID</key><string>2559B88H6C</string>
  <key>signingStyle</key><string>automatic</string>
  <key>manageAppVersionAndBuildNumber</key><false/>
  <key>testFlightInternalTestingOnly</key><true/>
</dict></plist>
PLIST
echo "==> export (local, no upload)"
xcodebuild -exportArchive -archivePath $work/Driver.xcarchive \\
  -exportOptionsPlist $work/ExportOptions.plist -exportPath $work/export -allowProvisioningUpdates \\
  > $work/export.log 2>&1 || { grep -E 'error:' $work/export.log | head -20; tail -5 $work/export.log; exit 1; }
tail -n 1 $work/export.log
ls -la $work/export
codesign -dvv $work/export/Payload/Driver.app 2>&1 | grep -E 'Authority|TeamIdentifier' || true
JOB

cat > $work/job.plist <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>$label</string>
<key>LimitLoadToSessionType</key><string>Aqua</string>
<key>ProcessType</key><string>Interactive</string>
<key>ProgramArguments</key><array><string>/bin/zsh</string><string>-f</string><string>-c</string>
<string>/bin/zsh -f $work/job.sh &gt; $work/out.log 2&gt;&amp;1; echo \$? &gt; $work/rc</string></array>
<key>EnvironmentVariables</key><dict>
  <key>PATH</key><string>/usr/bin:/bin:/usr/sbin:/sbin</string>
  <key>DEVELOPER_DIR</key><string>$(/usr/bin/xcode-select -p)</string>
  <key>LANG</key><string>en_US.UTF-8</string>
  <key>LC_ALL</key><string>en_US.UTF-8</string>
</dict>
<key>RunAtLoad</key><true/>
</dict></plist>
PLIST

launchctl bootout $domain/$label 2>/dev/null
trap 'launchctl bootout $domain/$label 2>/dev/null' EXIT
launchctl bootstrap $domain $work/job.plist || exit 1
while [[ ! -f $work/rc ]]; do sleep 5; done
cat $work/out.log
exit $(<$work/rc)
