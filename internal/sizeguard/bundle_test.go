package sizeguard

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
)

// with returns layout plus add (overriding sizes), minus remove.
func with(layout map[string]int64, add map[string]int64, remove ...string) map[string]int64 {
	out := maps.Clone(layout)
	maps.Copy(out, add)
	for _, r := range remove {
		delete(out, r)
	}
	return out
}

// many returns n files of size bytes named by format with the index.
func many(format string, n int, size int64) map[string]int64 {
	out := map[string]int64{}
	for i := range n {
		out[fmt.Sprintf(format, i)] = size
	}
	return out
}

const mb = 1_000_000

func TestCheckBundle(t *testing.T) {
	driver := driverLayout
	webrtc := driver["Frameworks/WebRTC.framework/WebRTC"]
	// big has a 100 MB Assets.car, so the percentage limits exceed the absolute minimums.
	big := with(driver, map[string]int64{"Assets.car": 100 * mb})
	bigAssets := big["Assets.car"]
	// wide has 2,000 files, so 20 % (400) exceeds the 200-file minimum.
	wide := with(driver, many("Metadata.appintents/nlu/f%d", 1980, 10))
	// huge sits just under the 300 MB ceiling.
	huge := with(driver, map[string]int64{"Assets.car": 300*mb - (manifest(driver).Total - driver["Assets.car"])})

	tests := []struct {
		name    string
		base    map[string]int64 // nil: no baseline
		cur     map[string]int64
		th      config.BundleThresholds
		expect  bool
		gate    string
		reasons []string // substrings each expected in some reason
	}{
		{name: "unchanged", base: driver, cur: driver, gate: api.GatePassed},
		{name: "first build", cur: driver, gate: api.GateBaselineCreated},
		{name: "embedded profiles are not secrets", cur: driver, gate: api.GateBaselineCreated},

		// Rule 1.
		{name: "p8 key", base: driver, cur: with(driver, map[string]int64{"AuthKey_ABC123.p8": 257}), gate: api.GateFailed, reasons: []string{"secret material: AuthKey_ABC123.p8"}},
		{name: "AuthKey without extension", cur: with(driver, map[string]int64{"Keys/AuthKey_ABC": 257}), gate: api.GateFailed},
		{name: "extra profile", base: driver, cur: with(driver, map[string]int64{"Dist.mobileprovision": 9000}), gate: api.GateFailed},
		{name: "p12", base: driver, cur: with(driver, map[string]int64{"certs/dist.p12": 3000}), gate: api.GateFailed},
		{name: "env", base: driver, cur: with(driver, map[string]int64{".env.production": 30}), gate: api.GateFailed},
		{name: "pem", base: driver, cur: with(driver, map[string]int64{"tls/ca.pem": 30}), gate: api.GateFailed},
		{name: "key", base: driver, cur: with(driver, map[string]int64{"PlugIns/DriverWidgets.appex/signing.key": 30}), gate: api.GateFailed},
		{name: "secret despite expect and allow", base: driver, cur: with(driver, map[string]int64{"AuthKey_X.p8": 1}), th: config.BundleThresholds{AllowPatterns: []string{"*.p8", "AuthKey_*"}}, expect: true, gate: api.GateFailed},

		// Rule 2, with a baseline and without one.
		{name: "docs dir", base: driver, cur: with(driver, map[string]int64{"docs/plan.md": 9000}), gate: api.GateAwaitingConfirmation, reasons: []string{"repository content: docs/"}},
		{name: "scripts dir no baseline", cur: with(driver, map[string]int64{"scripts/sync.sh": 900}), gate: api.GateAwaitingConfirmation, reasons: []string{"scripts/"}},
		{name: "build dir", base: driver, cur: with(driver, map[string]int64{"build/x.o": 1}), gate: api.GateAwaitingConfirmation, reasons: []string{"build/"}},
		{name: "DerivedData nested", base: driver, cur: with(driver, map[string]int64{"Res/DerivedData/info.plist": 1}), gate: api.GateAwaitingConfirmation, reasons: []string{"Res/DerivedData/"}},
		{name: "git dir", base: driver, cur: with(driver, map[string]int64{"ios/.git/HEAD": 23}), gate: api.GateAwaitingConfirmation, reasons: []string{"ios/.git/"}},
		{name: "node_modules", base: driver, cur: with(driver, map[string]int64{"web/node_modules/a/i.js": 2}), gate: api.GateAwaitingConfirmation, reasons: []string{"web/node_modules/"}},
		{name: "xcodeproj", base: driver, cur: with(driver, map[string]int64{"Driver.xcodeproj/project.pbxproj": 40000}), gate: api.GateAwaitingConfirmation, reasons: []string{"Driver.xcodeproj/"}},
		{name: "xcworkspace", cur: with(driver, map[string]int64{"Driver.xcworkspace/contents.xcworkspacedata": 200}), gate: api.GateAwaitingConfirmation},
		{name: "project.yml", base: driver, cur: with(driver, map[string]int64{"project.yml": 4000}), gate: api.GateAwaitingConfirmation, reasons: []string{"project.yml"}},
		{name: "Package.swift", cur: with(driver, map[string]int64{"Package.swift": 400}), gate: api.GateAwaitingConfirmation},
		{name: "Package.resolved nested", base: driver, cur: with(driver, map[string]int64{"Frameworks/WebRTC.framework/Package.resolved": 400}), gate: api.GateAwaitingConfirmation},
		{name: "swift sources grouped", base: driver, cur: with(driver, many("Sources/View%d.swift", 412, 2000)), gate: api.GateAwaitingConfirmation, reasons: []string{"412 .swift files"}},
		{name: "objc", cur: with(driver, map[string]int64{"Legacy/Bridge.m": 10, "Legacy/Bridge.mm": 10}), gate: api.GateAwaitingConfirmation, reasons: []string{"1 .m file", "1 .mm file"}},
		{name: "DS_Store", base: driver, cur: with(driver, map[string]int64{"Metadata.appintents/.DS_Store": 6148}), gate: api.GateAwaitingConfirmation, reasons: []string{".DS_Store"}},
		{name: "README at root", base: driver, cur: with(driver, map[string]int64{"README": 10}), gate: api.GateAwaitingConfirmation},
		{name: "md at root", base: driver, cur: with(driver, map[string]int64{"CHANGELOG.md": 10}), gate: api.GateAwaitingConfirmation},
		{name: "md below root is fine", base: driver, cur: with(driver, map[string]int64{"PlugIns/DriverWidgets.appex/Help.md": 10}), gate: api.GatePassed},
		{name: "repo content despite expect", base: driver, cur: with(driver, map[string]int64{"docs/a.md": 1}), expect: true, gate: api.GateAwaitingConfirmation},
		{name: "allow pattern suppresses rule 2 and 3", base: driver, cur: with(driver, map[string]int64{"README.md": 10, "Help/guide.md": 10}), th: config.BundleThresholds{AllowPatterns: []string{"*.md"}}, gate: api.GatePassed},
		{name: "allow dir pattern", base: driver, cur: with(driver, map[string]int64{"docs/a.md": 10}), th: config.BundleThresholds{AllowPatterns: []string{"/docs/"}}, gate: api.GatePassed},
		{name: "allow pattern is specific", base: driver, cur: with(driver, map[string]int64{"README.md": 10, "scripts/x": 1}), th: config.BundleThresholds{AllowPatterns: []string{"*.md"}}, gate: api.GateAwaitingConfirmation, reasons: []string{"scripts/"}},

		// Rule 3: total growth, max(15 %, 5 MB).
		{name: "growth at 5 MB minimum", base: driver, cur: with(driver, map[string]int64{"Frameworks/WebRTC.framework/WebRTC": webrtc + 5*mb}), gate: api.GatePassed},
		{name: "growth over 5 MB minimum", base: driver, cur: with(driver, map[string]int64{"Frameworks/WebRTC.framework/WebRTC": webrtc + 5*mb + 1}), gate: api.GateAwaitingConfirmation, reasons: []string{"growth +39%"}},
		{name: "growth at 15 %", base: big, cur: with(big, map[string]int64{"Assets.car": bigAssets + (manifest(big).Total*15)/100}), gate: api.GatePassed},
		{name: "growth over 15 %", base: big, cur: with(big, map[string]int64{"Assets.car": bigAssets + (manifest(big).Total*15)/100 + 1}), gate: api.GateAwaitingConfirmation, reasons: []string{"growth +15%"}},
		{name: "growth expected", base: driver, cur: with(driver, map[string]int64{"Frameworks/WebRTC.framework/WebRTC": webrtc + 6*mb}), expect: true, gate: api.GatePassed, reasons: []string{"expected: growth"}},
		{name: "custom growth threshold", base: driver, cur: with(driver, map[string]int64{"Frameworks/WebRTC.framework/WebRTC": webrtc + 2*mb}), th: config.BundleThresholds{GrowthMin: 1 * config.MB}, gate: api.GateAwaitingConfirmation},

		// Rule 3: file count, max(20 %, 200 files).
		{name: "files at 200 minimum", base: driver, cur: with(driver, many("Metadata.appintents/x%d", 200, 1)), gate: api.GatePassed},
		{name: "files over 200 minimum", base: driver, cur: with(driver, many("Metadata.appintents/x%d", 201, 1)), gate: api.GateAwaitingConfirmation, reasons: []string{"files +201"}},
		{name: "files at 20 %", base: wide, cur: with(wide, many("Metadata.appintents/y%d", 400, 1)), gate: api.GatePassed},
		{name: "files over 20 %", base: wide, cur: with(wide, many("Metadata.appintents/y%d", 401, 1)), gate: api.GateAwaitingConfirmation, reasons: []string{"files +401"}},

		// Rule 3: new bundle-root entries.
		{name: "new root dir", base: driver, cur: with(driver, map[string]int64{"Sounds/ding.caf": 900}), gate: api.GateAwaitingConfirmation, reasons: []string{"new in bundle root: Sounds/"}},
		// A plain resource copied into the root is normal for iOS; only new directories trip.
		{name: "new small root file passes", base: driver, cur: with(driver, map[string]int64{"ding.caf": 900}), gate: api.GatePassed},
		{name: "new large root file trips via rule 3d", base: driver, cur: with(driver, map[string]int64{"intro.mov": 6 * mb}), gate: api.GateAwaitingConfirmation, reasons: []string{"new large file: intro.mov (6.0 MB)"}},
		{name: "new extension is declared", base: driver, cur: with(driver, map[string]int64{"PlugIns/Share.appex/Share": 90000}), gate: api.GatePassed},
		{name: "new framework is declared", base: driver, cur: with(driver, map[string]int64{"Frameworks/Lottie.framework/Lottie": 900000}), gate: api.GatePassed},
		{name: "first PlugIns dir", base: with(driver, nil, "PlugIns/DriverWidgets.appex/DriverWidgets", "PlugIns/DriverWidgets.appex/Info.plist", "PlugIns/DriverWidgets.appex/_CodeSignature/CodeResources", "PlugIns/DriverWidgets.appex/embedded.mobileprovision"), cur: driver, gate: api.GatePassed},
		{name: "removed entry is fine", base: driver, cur: with(driver, nil, "AppIcon76x76@2x~ipad.png"), gate: api.GatePassed},

		// Rule 3: a single new file over 5 MB.
		{name: "new file at 5 MB", base: big, cur: with(big, map[string]int64{"Frameworks/WebRTC.framework/Model.bin": 5 * mb}), gate: api.GatePassed},
		{name: "new file over 5 MB", base: big, cur: with(big, map[string]int64{"Frameworks/WebRTC.framework/Model.bin": 5*mb + 1}), gate: api.GateAwaitingConfirmation, reasons: []string{"new large file: Frameworks/WebRTC.framework/Model.bin (5.0 MB)"}},
		{name: "grown file is not new", base: big, cur: with(big, map[string]int64{"Assets.car": bigAssets + 6*mb}), gate: api.GatePassed},
		{name: "allowed new large file", base: big, cur: with(big, map[string]int64{"Videos/intro.mp4": 9 * mb}), th: config.BundleThresholds{AllowPatterns: []string{"*.mp4"}}, gate: api.GatePassed},
		{name: "new file expected", base: big, cur: with(big, map[string]int64{"Videos/intro.mp4": 9 * mb}), expect: true, gate: api.GatePassed},

		// Rule 4: ceiling.
		{name: "at ceiling", cur: huge, gate: api.GateBaselineCreated},
		{name: "over ceiling", cur: with(huge, map[string]int64{"PkgInfo": 9}), gate: api.GateAwaitingConfirmation, reasons: []string{"over the 300.0 MB ceiling"}},
		{name: "over ceiling with baseline", base: with(huge, map[string]int64{"PkgInfo": 9}), cur: with(huge, map[string]int64{"PkgInfo": 9}), gate: api.GateAwaitingConfirmation},
		{name: "over ceiling expected", cur: with(huge, map[string]int64{"PkgInfo": 9}), expect: true, gate: api.GateBaselineCreated, reasons: []string{"expected: size"}},
		{name: "custom ceiling", cur: driver, th: config.BundleThresholds{Ceiling: 10 * config.MB}, gate: api.GateAwaitingConfirmation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var base *Manifest
			if tt.base != nil {
				b := manifest(tt.base)
				b.Build = "202610161458"
				base = &b
			}
			v := CheckBundle(manifest(tt.cur), base, tt.th, tt.expect)
			r := v.Report
			if r.Gate != tt.gate {
				t.Fatalf("gate %s, want %s; reasons %q", r.Gate, tt.gate, r.Reasons)
			}
			if v.Failed != (tt.gate == api.GateFailed) {
				t.Errorf("Failed = %v", v.Failed)
			}
			if v.Tripped && tt.gate == api.GatePassed {
				t.Error("tripped but passed")
			}
			for _, want := range tt.reasons {
				if !containsAny(r.Reasons, want) {
					t.Errorf("no reason contains %q: %q", want, r.Reasons)
				}
			}
			if (tt.gate == api.GatePassed || tt.gate == api.GateBaselineCreated) && !tt.expect && len(r.Reasons) > 0 {
				t.Errorf("unexpected reasons %q", r.Reasons)
			}
			if r.Summary == "" {
				t.Error("no summary")
			}
			if (v.Failed || v.Tripped) && r.Hint == "" {
				t.Error("no hint")
			}
		})
	}
}

func containsAny(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestCheckBundleReport(t *testing.T) {
	base := manifest(driverLayout)
	base.Build = "202610151108"
	cur := manifest(with(driverLayout, map[string]int64{
		"docs/manual.pdf":                    120 * mb,
		"project.yml":                        4000,
		"Assets.car":                         driverLayout["Assets.car"] + 400_000,
		"Frameworks/WebRTC.framework/WebRTC": driverLayout["Frameworks/WebRTC.framework/WebRTC"] + 1,
	}))
	v := CheckBundle(cur, &base, config.BundleThresholds{}, false)
	r := v.Report
	if r.Gate != api.GateAwaitingConfirmation || r.BaselineBuild != "202610151108" ||
		r.DeltaBytes != 120*mb+404_001 || r.DeltaFiles != 2 {
		t.Fatalf("report %+v", r)
	}
	if want := (api.SizeEntry{Path: "Driver.app/docs/", Bytes: 120 * mb, Change: "new"}); r.Largest[0] != want {
		t.Errorf("largest[0] = %+v", r.Largest[0])
	}
	if want := (api.SizeEntry{Path: "Driver.app/Assets.car", Bytes: 440024, Change: "+400.0 KB"}); r.Largest[1] != want {
		t.Errorf("largest[1] = %+v", r.Largest[1])
	}
	if !strings.Contains(r.Hint, "project.yml") || !strings.Contains(r.Hint, "docs/") {
		t.Errorf("hint %q", r.Hint)
	}
	if want := "Driver.app 133.3 MB, 22 files: +120.4 MB, +2 files vs baseline 12.9 MB (202610151108); confirm or reject"; r.Summary != want {
		t.Errorf("summary\n got %q\nwant %q", r.Summary, want)
	}
	if !containsAny(r.Reasons, "repository content: docs/, project.yml") {
		t.Errorf("reasons %q", r.Reasons)
	}
}

func TestReportSizeCap(t *testing.T) {
	base := manifest(driverLayout)
	add := map[string]int64{}
	for i := range 10_000 {
		dir := fmt.Sprintf("Very Long Folder Reference Name %03d/Nested/Deeper/Still Deeper", i%300)
		add[fmt.Sprintf("%s/File With A Rather Long Name %05d.swift", dir, i)] = int64(1000 + i)
		if i%7 == 0 {
			add[fmt.Sprintf("Root Level Resource Number %05d.png", i)] = 6 * mb
		}
	}
	add["AuthKey_ABCDEFGHIJ.p8"] = 257
	add["docs/x.md"] = 1
	cur := manifest(with(driverLayout, add))
	cur.Build = strings.Repeat("9", 40)
	for _, expect := range []bool{false, true} {
		r := CheckBundle(cur, &base, config.BundleThresholds{}, expect).Report
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) >= MaxReportBytes {
			t.Errorf("report is %d bytes:\n%s", len(b), b)
		}
		if len(r.Largest) < 3 || len(r.Reasons) < 2 || r.Hint == "" {
			t.Errorf("report trimmed too far: %s", b)
		}
	}
}
