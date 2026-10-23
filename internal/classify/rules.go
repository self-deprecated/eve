package classify

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/self-deprecated/eve/internal/api"
)

// Failure classes (docs/design.md §9). The pipeline produces some of them itself rather than from a
// log; ForClass gives their summary and hint.
const (
	ClassSigningCloudPermission     = "signing.cloud_permission"
	ClassSigningNoTeam              = "signing.no_team"
	ClassSigningDeviceUnregistered  = "signing.device_unregistered"
	ClassSigningEntitlementMismatch = "signing.entitlement_mismatch"
	ClassSigningWWDRExpired         = "signing.wwdr_expired"
	ClassXcodeAccount               = "xcode.account"
	ClassBuildNumberMismatch        = "build.number_mismatch" // eve's archive check (§6.3)
	ClassBuildCompile               = "build.compile"
	ClassEnvRsync                   = "env.rsync"
	ClassEnvMetalToolchain          = "env.metal_toolchain"
	ClassEnvToolMissing             = "env.tool_missing"
	ClassUploadDuplicate            = "upload.duplicate" // not a failure: reconcile (§5.5)
	ClassUploadMissingIcon          = "upload.missing_icon"
	ClassUploadPurposeString        = "upload.purpose_string"
	ClassUploadExportCompliance     = "upload.export_compliance"
	ClassUploadInvalidBundle        = "upload.invalid_bundle"
	ClassASCAgreement               = "asc.agreement"
	ClassASCAuth                    = "asc.auth"
	ClassASCAppRecord               = "asc.app_record"
	ClassASCProcessingDeadline      = "asc.processing_deadline" // the watcher's deadline (§5.2)
	ClassDeviceLocked               = "device.locked"
	ClassDeviceUnavailable          = "device.unavailable"
	ClassDeviceUntrustedDeveloper   = "device.untrusted_developer"
	ClassDeviceAttestation          = "device.attestation"
	ClassBundleSecretMaterial       = "bundle.secret_material"    // size guard rule 1 (§6.7.2)
	ClassSizeConfirmationTimeout    = "size.confirmation_timeout" // §6.7.3
	ClassTestsPre                   = "tests.pre"
	ClassTestsFailed                = "tests.failed"
	ClassUnknown                    = "unknown"
)

// rule is one row of the classification table. A rule matches the first line, in log order,
// that contains one of its keys and, if it has patterns, matches one of them.
type rule struct {
	class string
	// phases scopes the rule; empty means any phase. A scoped rule never matches an Input whose
	// phase is unset.
	phases []api.State
	// keys are literal substrings that preselect candidate lines, so a multi-megabyte log is only
	// scanned with bytes.Index; no keys means the rule never matches a log.
	keys []string
	// match are optional patterns a candidate line must satisfy; named groups feed detail.
	match []*regexp.Regexp
	// exit are exit codes that match on their own, tried only after every log rule missed.
	exit    []int
	summary string
	hint    string
	human   string // an api.Human* action, or ""
	// detail refines summary and hint from the matched line's named groups and the whole input.
	// An empty return keeps the static text.
	detail func(groups map[string]string, in Input) (summary, hint string)
}

var re = regexp.MustCompile

// devicePhases are the device verb's phases after the build (§6.4).
var devicePhases = []api.State{api.StateBuilding, api.StateInstalling, api.StateWaitingForDevice, api.StateLaunching}

// rules is the ordered table. Order matters where one log shows several errors: an account or
// permission problem explains the "No profiles" lines that follow it, Apple's upload verdicts
// are more specific than a compile error, and a failing test is more specific than "error:".
var rules = []rule{
	{
		class:   ClassSigningCloudPermission,
		keys:    []string{"Cloud signing permission error", "cloud-managed distribution certificates"},
		summary: "Export needs cloud-managed distribution signing, which this account or API key can't use.",
		hint:    "Only Admin API keys can use cloud-managed certificates; set signing: xcode-account for this app (see eve doctor).",
		detail: func(_ map[string]string, in Input) (string, string) {
			if in.Context["signing"] == "api-key" {
				return "", "This app is configured for api-key signing and the key isn't Admin; set signing: xcode-account (see eve doctor)."
			}
			return "", ""
		},
	},
	{
		class:   ClassASCAgreement,
		keys:    []string{"REQUIRED_AGREEMENTS", "PLA Update available", "Program License Agreement", "required agreement"},
		summary: "App Store Connect refuses requests until the latest developer agreement is accepted.",
		hint:    "The Account Holder must accept the agreement in App Store Connect (Business) or the developer portal, then resubmit.",
		human:   api.HumanAcceptAgreement,
	},
	{
		class: ClassASCAuth,
		keys:  []string{"bearer token", "NOT_AUTHORIZED", "401", "Authentication credentials are missing or invalid"},
		match: []*regexp.Regexp{
			re(`Authentication failed: Make sure a bearer token was provided`),
			re(`NOT_AUTHORIZED`),
			re(`"status"\s*:\s*"?401\b`),
			re(`\b401 Unauthorized\b`),
			re(`Authentication credentials are missing or invalid`),
		},
		summary: "App Store Connect rejected the API key's token (401).",
		hint:    "Check the key ID, issuer ID and .p8 file, and the Mac's clock (eve doctor).",
	},
	{
		class: ClassXcodeAccount,
		keys:  []string{"Failed to Use Accounts", "App Store Connect access for", "No Accounts", "session has expired", "Unable to log in with account", "re-enter your password"},
		match: []*regexp.Regexp{
			re(`Failed to Use Accounts`),
			re(`App Store Connect access for .* is required`),
			re(`No Accounts`),
			re(`(?i)session has expired`),
			re(`Unable to log in with account`),
			re(`re-enter your password`),
		},
		summary: "Xcode's Apple Account session for this team is missing or expired.",
		hint:    "Sign in again in Xcode > Settings > Accounts on the build Mac, in the logged-in GUI session.",
		human:   api.HumanXcodeSignIn,
	},
	{
		class: ClassASCAppRecord,
		keys:  []string{"DistributionAppRecordProviderError.missingApp", "No suitable application records were found"},
		match: []*regexp.Regexp{
			re(`missingApp\(bundleId: "(?P<bundle>[^"]+)"\)`),
			re(`No suitable application records were found`),
		},
		summary: "App Store Connect has no app record for this bundle ID.",
		hint:    "Create the app in App Store Connect (the API can't); if the name is taken, choose another.",
		human:   api.HumanCreateAppRecord,
		detail: func(g map[string]string, _ Input) (string, string) {
			if g["bundle"] == "" {
				return "", ""
			}
			return fmt.Sprintf("App Store Connect has no app record for %s.", g["bundle"]), ""
		},
	},
	{
		class:   ClassSigningNoTeam,
		keys:    []string{"requires a development team"},
		match:   []*regexp.Regexp{re(`Signing for "(?P<target>[^"]+)" requires a development team`), re(`requires a development team`)},
		summary: "Signing needs a development team and none is set.",
		hint:    "Set DEVELOPMENT_TEAM (team: in the app's server config, or the project's build settings).",
		detail: func(g map[string]string, _ Input) (string, string) {
			if g["target"] == "" {
				return "", ""
			}
			return fmt.Sprintf("Signing for %q needs a development team and none is set.", g["target"]), ""
		},
	},
	{
		class: ClassSigningDeviceUnregistered,
		keys:  []string{"isn't registered in your developer account", "doesn't include the currently selected device", "is not registered"},
		match: []*regexp.Regexp{
			re(`Device "(?P<device>[^"]+)" isn't registered in your developer account`),
			re(`doesn't include the currently selected device "(?P<device>[^"]+)"`),
			re(`[Dd]evice .* is not registered`),
		},
		summary: "The phone isn't registered in the developer account, so no profile can include it.",
		hint:    "Eve retries device builds with -allowProvisioningDeviceRegistration; if this persists, register the device in the developer portal.",
		detail: func(g map[string]string, _ Input) (string, string) {
			if g["device"] == "" {
				return "", ""
			}
			return fmt.Sprintf("Device %q isn't registered in the developer account, so no profile can include it.", g["device"]), ""
		},
	},
	{
		class: ClassSigningEntitlementMismatch,
		keys:  []string{"entitlement", "capability"},
		match: []*regexp.Regexp{
			re(`doesn't match the entitlements file's value for the (?P<ent>\S+) entitlement`),
			re(`doesn't include the (?P<ent>\S+) entitlement`),
			re(`doesn't support the (?P<ent>.+?) capability`),
		},
		summary: "The provisioning profile doesn't match the app's entitlements.",
		hint:    "Enable the capability (app groups, Family Controls, …) on the App ID in the developer portal, then rebuild so the profile regenerates.",
		detail: func(g map[string]string, _ Input) (string, string) {
			if g["ent"] == "" {
				return "", ""
			}
			return fmt.Sprintf("The provisioning profile doesn't match the app's %s entitlement.", g["ent"]),
				fmt.Sprintf("Configure %s on the App ID (developer portal or ASC API), then rebuild so the profile regenerates.", g["ent"])
		},
	},
	{
		class: ClassSigningWWDRExpired,
		keys:  []string{"unable to build chain", "CSSMERR_TP", "Invalid trust settings", "errSecInternalComponent", "WWDR", "certificate has expired", "is not trusted"},
		match: []*regexp.Regexp{
			re(`unable to build chain to self-signed root`),
			re(`CSSMERR_TP_(NOT_TRUSTED|CERT_EXPIRED)`),
			re(`Invalid trust settings`),
			re(`errSecInternalComponent`),
			re(`(?i)(error|warning):.*(WWDR|intermediate certificate).*(expired|missing|not trusted)`),
			re(`(?i)error:.*certificate has expired`),
			re(`(?i)error:.*certificate .* is not trusted`),
		},
		summary: "Code signing couldn't build a trust chain for the signing certificate.",
		hint:    "Install Apple's current WWDR G3 intermediate (eve doctor). errSecInternalComponent can also mean the login keychain is locked in this session.",
	},
	{
		class:   ClassBuildNumberMismatch,
		summary: "The archive's bundles don't all carry the allocated build number.",
		hint:    "An extension hardcodes CFBundleVersion; use $(CURRENT_PROJECT_VERSION) in every Info.plist.",
	},
	{
		class:   ClassEnvRsync,
		keys:    []string{"Copy failed"},
		match:   []*regexp.Regexp{re(`exportArchive:? Copy failed`), re(`"Copy failed"`)},
		summary: "IPA packaging failed with \"Copy failed\", the symptom of a non-system rsync on PATH.",
		hint:    "Environment regression: eve should put /usr/bin first on PATH for xcodebuild; check `which rsync` in the child environment.",
	},
	{
		class:   ClassEnvMetalToolchain,
		keys:    []string{"Metal Toolchain", "MetalToolchain"},
		match:   []*regexp.Regexp{re(`missing Metal Toolchain`), re(`cannot execute tool 'metal'`), re(`error:.*-downloadComponent MetalToolchain`)},
		summary: "The build compiles Metal shaders and this Xcode's Metal Toolchain isn't installed.",
		hint:    "Run `xcodebuild -downloadComponent MetalToolchain` on the build Mac (eve doctor checks it).",
	},
	{
		class: ClassEnvToolMissing,
		// Only pre commands: build script phases print harmless "command not found" lines too.
		phases:  []api.State{api.StatePreparing},
		keys:    []string{"command not found"},
		match:   []*regexp.Regexp{re(`command not found: (?P<tool>\S+)`), re(`(?P<tool>[^\s:]+): command not found`)},
		summary: "A tool the build needs isn't on the build Mac's PATH for eve's jobs.",
		hint:    "Install it on the build Mac and add its directory to `path` in server.yml (an operator change; tell the user).",
		detail: func(g map[string]string, _ Input) (string, string) {
			return "`" + g["tool"] + "` isn't on the build Mac's PATH for eve's jobs.", ""
		},
	},
	{
		class: ClassUploadDuplicate,
		keys:  []string{"ITMS-90189", "Redundant Binary Upload", "already been uploaded", "already uploaded a build", "must be higher than the previously uploaded"},
		match: []*regexp.Regexp{
			re(`ITMS-90189`),
			re(`Redundant Binary Upload`),
			re(`already been uploaded`),
			re(`already uploaded a build with build number '(?P<number>[^']+)'`),
			re(`bundle version must be higher than the previously uploaded version`),
		},
		summary: "App Store Connect already has a build with this number.",
		hint:    "Not a failure: an earlier attempt landed; eve reconciles with App Store Connect.",
	},
	{
		class: ClassUploadMissingIcon,
		keys:  []string{"Missing required icon", "CFBundleIconName", "CFBundleIcons", "152x152", "ITMS-90022", "ITMS-90023", "ITMS-90713"},
		match: []*regexp.Regexp{
			re(`Missing required icon file`),
			re(`'CFBundleIconName' is missing`),
			re(`(?i)(error|ITMS-\d+).*CFBundleIcons`),
			re(`(?i)(error|ITMS-\d+).*152x152`),
			re(`ITMS-900(22|23)|ITMS-90713`),
		},
		summary: "Apple rejected the upload: required app icon sizes are missing.",
		hint:    "Add a full AppIcon set to the asset catalog and set ASSETCATALOG_COMPILER_APPICON_NAME.",
		detail: func(_ map[string]string, in Input) (string, string) {
			sizes := uniqueMatches(in.Log, iconSizeRE)
			if len(sizes) == 0 {
				return "", ""
			}
			return fmt.Sprintf("Apple rejected the upload: no app icon of %s.", strings.Join(sizes, ", ")),
				fmt.Sprintf("Add %s icons (a full AppIcon set, including iPad when the app supports it) and set CFBundleIconName via ASSETCATALOG_COMPILER_APPICON_NAME.", strings.Join(sizes, ", "))
		},
	},
	{
		class:   ClassUploadPurposeString,
		keys:    []string{"ITMS-90683", "Missing purpose string"},
		summary: "Apple rejected the upload: an Info.plist purpose string is missing.",
		hint:    "Add the NS…UsageDescription key Apple names to Info.plist.",
		detail: func(_ map[string]string, in Input) (string, string) {
			keys := uniqueMatches(in.Log, usageKeyRE)
			if len(keys) == 0 {
				return "", ""
			}
			return fmt.Sprintf("Apple rejected the upload: Info.plist lacks %s.", strings.Join(keys, ", ")),
				fmt.Sprintf("Add %s with a user-facing purpose string to Info.plist (a linked framework references the API even if the app doesn't).", strings.Join(keys, ", "))
		},
	},
	{
		class:   ClassUploadExportCompliance,
		keys:    []string{"MISSING_EXPORT_COMPLIANCE"},
		summary: "The build is waiting for an export compliance answer.",
		hint:    "Answer export compliance in App Store Connect, or set ITSAppUsesNonExemptEncryption in Info.plist.",
		human:   api.HumanExportCompliance,
	},
	{
		class: ClassUploadExportCompliance,
		keys:  []string{"ITMS-90592", "ITSAppUsesNonExemptEncryption", "ITSEncryptionExportComplianceCode"},
		match: []*regexp.Regexp{
			re(`ITMS-90592`),
			re(`(?i)(error|ITMS-\d+|missing|invalid).*ITSAppUsesNonExemptEncryption`),
			re(`(?i)(error|ITMS-\d+|missing|invalid).*ITSEncryptionExportComplianceCode`),
		},
		summary: "Apple rejected the export compliance declaration in Info.plist.",
		hint:    "Set ITSAppUsesNonExemptEncryption to NO, or YES with a valid ITSEncryptionExportComplianceCode.",
	},
	{
		class:   ClassUploadInvalidBundle,
		keys:    []string{"Invalid bundle."},
		match:   []*regexp.Regexp{re(`(error: exportArchive |^)Invalid bundle\. `)},
		summary: "Apple rejected the bundle as invalid.",
		hint:    "Fix the Info.plist or bundle problem Apple names in the excerpt, then resubmit.",
		detail: func(_ map[string]string, in Input) (string, string) {
			if !strings.Contains(string(in.Log), "to support iPad multitasking") {
				return "", ""
			}
			return "Apple rejected the bundle: an app that runs on iPad must list all four UISupportedInterfaceOrientations for multitasking.",
				"Add UIInterfaceOrientationPortraitUpsideDown so UISupportedInterfaceOrientations (or its ~ipad variant) has all four orientations, or make the app iPhone-only (TARGETED_DEVICE_FAMILY = 1)."
		},
	},
	{
		class: ClassDeviceLocked,
		keys: []string{
			"FBSOpenApplicationErrorDomain error 7", "BSErrorCodeDescription = Locked", "could not be, unlocked", "for reason: Locked",
			"developer disk image could not be mounted",
		},
		match: []*regexp.Regexp{
			re(`Unable to launch (?P<bundle>\S+) because the device was not, or could not be, unlocked`),
			re(`FBSOpenApplicationErrorDomain error 7\b|BSErrorCodeDescription = Locked|for reason: Locked`),
			// CoreDeviceError 12040: reachable, but locked before Xcode's developer disk image is mounted.
			re(`developer disk image could not be mounted`),
		},
		summary: "The app is installed but couldn't launch because the phone is locked.",
		hint:    "Unlock the phone, or open the app yourself.",
		human:   api.HumanUnlockPhone,
		detail: func(_ map[string]string, in Input) (string, string) {
			if in.Phase == api.StateInstalling {
				return "The phone is locked and Xcode's developer disk image isn't mounted on it, which needs one unlock.",
					"Unlock the phone once; later installs work while it's locked."
			}
			return "", ""
		},
	},
	{
		class:   ClassDeviceUntrustedDeveloper,
		keys:    []string{"explicitly trusted by the user", "FBSOpenApplicationErrorDomain error 3", "Untrusted Developer"},
		summary: "The phone refused to launch the app: invalid signature, entitlements, or an untrusted developer profile.",
		hint:    "On the phone, trust the developer in Settings > General > VPN & Device Management; if it's already trusted, check the app's signing and entitlements.",
	},
	{
		class:   ClassDeviceAttestation,
		keys:    []string{"Unable to Verify App", "Failed to create BAA info", "Couldn't get device identity"},
		summary: "The phone couldn't verify the app (device attestation failed).",
		hint:    "Restart the phone, then resubmit; this fixed it last time.",
	},
	{
		// devicectl can't reach the phone at all (CoreDeviceError 4000 and 4016; seen 2026-10-17 with
		// the phone's Wi-Fi off). Locking alone doesn't cause these.
		class: ClassDeviceUnavailable,
		keys:  []string{"A connection to this device could not be established", "not able to fulfill the requested usage assertion requirements"},
		match: []*regexp.Regexp{
			re(`A connection to this device could not be established`),
			re(`not able to fulfill the requested usage assertion requirements`),
		},
		summary: "The build Mac can't reach the phone: its Wi-Fi is off, it's on another network or away, or it's asleep.",
		hint:    "Turn on the phone's Wi-Fi on the build Mac's network (or plug it into the Mac) and wake it.",
		human:   api.HumanConnectPhone,
	},
	{
		// The connection dropped mid-install (CoreDeviceError 3002, IXRemoteErrorDomain 6; seen
		// 2026-10-16). Transient: the device verb waits and tries again.
		class:   ClassDeviceUnavailable,
		phases:  devicePhases,
		keys:    []string{"Connection with the remote side was unexpectedly closed", "IXRemoteErrorDomain error 6"},
		summary: "The connection to the phone dropped during the install.",
		hint:    "Keep the phone awake and on the build Mac's Wi-Fi, or plug it into the Mac.",
		human:   api.HumanConnectPhone,
	},
	{
		class: ClassDeviceUnavailable,
		// Swift's "'UIDevice' is unavailable" diagnostics are why there is no looser pattern.
		keys: []string{"Unable to find a destination matching", "not a valid destination", "Device State: unavailable", "platform:iOS, id:"},
		match: []*regexp.Regexp{
			re(`Unable to find a destination matching`),
			re(`not a valid destination`),
			re(`Device State: unavailable`),
			re(`\{ platform:iOS, id:[^,]+, name:(?P<device>[^,]+), error:(?P<why>[^}]+)\}`),
		},
		summary: "The phone isn't available as a destination (asleep, locked, or not on the network).",
		hint:    "Wake and unlock the phone on the build Mac's Wi-Fi (`devicectl list devices` should show it available).",
		human:   api.HumanUnlockPhone,
	},
	{
		class:   ClassDeviceUnavailable,
		phases:  devicePhases,
		exit:    []int{70},
		summary: "xcodebuild exited 70: the destination isn't available.",
		hint:    "Wake and unlock the phone on the build Mac's Wi-Fi (`devicectl list devices` should show it available).",
		human:   api.HumanUnlockPhone,
	},
	{
		class:   ClassTestsPre,
		phases:  []api.State{api.StatePreparing},
		keys:    testKeys,
		match:   testPatterns,
		summary: "A pre-build command's tests failed.",
		hint:    "Run the app's pre: commands locally; see the excerpt for the first failure.",
		detail:  testDetail,
	},
	{
		class:   ClassTestsFailed,
		keys:    testKeys,
		match:   testPatterns,
		summary: "Tests failed.",
		hint:    "See the excerpt for the first failing test; eve logs has the full run.",
		detail:  testDetail,
	},
	{
		class: ClassBuildCompile,
		keys:  []string{"error:"},
		match: []*regexp.Regexp{
			re(`^(?P<file>[^\s|][^:]*?\.(?:swift|m|mm|c|cc|cpp|h|hpp|metal|storyboard|xib|plist|xcconfig|yml|yaml)):(?P<line>\d+)(?::\d+)?: (?:fatal )?error: (?P<msg>.+)$`),
			re(`^(?P<file>macro expansion [^:]+):(?P<line>\d+):\d+: error: (?P<msg>.+)$`),
		},
		summary: "Compilation failed.",
		hint:    "Fix the first error; later ones often follow from it.",
		detail: func(g map[string]string, _ Input) (string, string) {
			file := filepath.Base(g["file"])
			if strings.HasPrefix(g["file"], "macro expansion") {
				file = g["file"]
			}
			return fmt.Sprintf("Compile error at %s:%s: %s", file, g["line"], g["msg"]),
				fmt.Sprintf("Fix %s:%s first; later errors often follow from it.", g["file"], g["line"])
		},
	},
	{
		class:   ClassASCProcessingDeadline,
		summary: "App Store Connect didn't finish processing the build before the deadline.",
		hint:    "Check the build in App Store Connect; resubmit if it is stuck.",
	},
	{
		class:   ClassBundleSecretMaterial,
		summary: "The archived app contains secret material (keys, certificates or profiles).",
		hint:    "Remove the file from the target; this can't be confirmed past.",
	},
	{
		class:   ClassSizeConfirmationTimeout,
		summary: "Nobody confirmed the size change within 24 hours.",
		hint:    "Resubmit, then eve confirm with a reason, or fix the bundle.",
	},
}

var (
	iconSizeRE = re(`exactly '(\d+x\d+)' pixels`)
	usageKeyRE = re(`\b(NS[A-Za-z]+UsageDescription)\b`)

	// testKeys preselect lines from XCTest (xcodebuild) and Swift Testing output.
	testKeys = []string{"' failed", "error: -[", "✘ Test", " failures (", " failure (", "Failing tests:"}
	// testPatterns are tried on each candidate line; the first failing assertion wins.
	testPatterns = []*regexp.Regexp{
		re(`(?:^(?P<loc>[^\s:]+:\d+): )?error: -\[(?P<suite>[\w.]+) (?P<test>\w+)\] : (?P<msg>.+)$`),
		re(`✘ Test (?P<test>\S+\(.*?\)|"[^"]+") recorded an issue at (?P<loc>\S+?): (?P<msg>.+)$`),
		re(`Test Case '-\[(?P<suite>[\w.]+) (?P<test>\w+)\]' failed`),
		re(`✘ Test (?P<test>\S+\(.*?\)|"[^"]+") failed after`),
		re(`✘ Test run with \d+ tests? .*failed`),
		re(`Executed \d+ tests?, with [1-9]\d* failures? \(`),
	}
)

func testDetail(g map[string]string, _ Input) (string, string) {
	name := g["test"]
	if name == "" {
		return "", ""
	}
	if s := g["suite"]; s != "" {
		name = s[strings.LastIndexByte(s, '.')+1:] + "." + name
	}
	summary := "Test " + name + " failed"
	if msg := strings.TrimSpace(g["msg"]); msg != "" {
		summary += ": " + msg
	}
	hint := ""
	if g["loc"] != "" {
		hint = "First failure at " + g["loc"] + "; see the excerpt."
	}
	return summary, hint
}

// uniqueMatches returns the first submatch of every match of re in log, deduplicated, in order.
func uniqueMatches(log []byte, re *regexp.Regexp) []string {
	var out []string
	for _, m := range re.FindAllSubmatch(log, 64) {
		if s := string(m[1]); !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}
