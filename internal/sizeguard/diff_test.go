package sizeguard

import (
	"reflect"
	"testing"

	"github.com/self-deprecated/eve/internal/api"
)

func TestDiff(t *testing.T) {
	base := manifest(driverLayout)
	cur := manifest(with(driverLayout, map[string]int64{
		"docs/plan.md":                     9000,
		"docs/img/arch.png":                200_000,
		"Assets.car":                       440_024,
		"Metadata.appintents/version.json": 50,
	}, "AppIcon76x76@2x~ipad.png", "PlugIns/DriverWidgets.appex/Info.plist"))

	want := []api.SizeEntry{
		{Path: "Driver.app/", Bytes: 816_862, Change: "+397.8 KB"},
		{Path: "Driver.app/AppIcon76x76@2x~ipad.png", Bytes: 2156, Change: "removed"},
		{Path: "Driver.app/Assets.car", Bytes: 440_024, Change: "+400.0 KB"},
		{Path: "Driver.app/Metadata.appintents/", Bytes: 3128, Change: "-3 B"},
		{Path: "Driver.app/Metadata.appintents/version.json", Bytes: 50, Change: "-3 B"},
		{Path: "Driver.app/PlugIns/DriverWidgets.appex/", Bytes: 106_455, Change: "-940 B"},
		{Path: "Driver.app/PlugIns/DriverWidgets.appex/Info.plist", Bytes: 940, Change: "removed"},
		{Path: "Driver.app/docs/", Bytes: 9000, Change: "new"},
		{Path: "Driver.app/docs/plan.md", Bytes: 9000, Change: "new"},
		{Path: "Driver.app/docs/img/", Bytes: 200_000, Change: "new"},
		{Path: "Driver.app/docs/img/arch.png", Bytes: 200_000, Change: "new"},
	}
	if got := Diff(cur, &base); !reflect.DeepEqual(got, want) {
		t.Errorf("Diff:\n got %+v\nwant %+v", got, want)
	}
	if got := Diff(base, &base); len(got) != 0 {
		t.Errorf("identical manifests differ: %+v", got)
	}
	if got := Diff(base, nil); len(got) != 20+8 { // 20 files in 8 directories, all new
		t.Errorf("no-baseline diff has %d entries", len(got))
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0:             "0 B",
		999:           "999 B",
		1000:          "1.0 KB",
		40_024:        "40.0 KB",
		48_200_000:    "48.2 MB",
		12_921_111:    "12.9 MB",
		1_234_000_000: "1.2 GB",
		-400_000:      "-400.0 KB",
	} {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
	if got := formatDelta(166_400_000); got != "+166.4 MB" {
		t.Errorf("formatDelta = %q", got)
	}
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 9412: "9,412", 1234567: "1,234,567", -9412: "-9,412"} {
		if got := formatCount(n); got != want {
			t.Errorf("formatCount(%d) = %q, want %q", n, got, want)
		}
	}
	if got := shorten("Driver.app/Very/Long/Path/To/Some/Resource.bundle/file.png", 30); len([]rune(got)) != 30 {
		t.Errorf("shorten = %q", got)
	}
}

func TestPatterns(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    string // matched path, "" for no match
	}{
		{"*.md", "README.md", "README.md"},
		{"*.md", "Help/guide.md", "Help/guide.md"},
		{"/*.md", "Help/guide.md", ""},
		{"/docs/", "docs/a/b.png", "docs"},
		{"/docs/", "x/docs/a", ""},
		{"docs/", "x/docs/a", "x/docs"},
		{"docs/", "docs", ""}, // a file named docs is not a directory
		{"*.xcodeproj/", "App.xcodeproj/project.pbxproj", "App.xcodeproj"},
		{"Videos/**/*.mp4", "Videos/a/b/intro.mp4", "Videos/a/b/intro.mp4"},
		{"Videos/**/*.mp4", "Videos/intro.mp4", "Videos/intro.mp4"},
		{"**/*.caf", "Sounds/x.caf", "Sounds/x.caf"},
		{"AuthKey_*", "keys/AuthKey_ABC.p8", "keys/AuthKey_ABC.p8"},
		{"icon?.png", "icon1.png", "icon1.png"},
		{"icon[0-9].png", "icon7.png", "icon7.png"},
		{"icon[!0-9].png", "icon7.png", ""},
		{"a.b", "aXb", ""},
	}
	for _, tt := range tests {
		got, ok := compilePatterns([]string{tt.pattern}).match(tt.path)
		if got != tt.want || ok != (tt.want != "") {
			t.Errorf("%q on %q = %q, %v; want %q", tt.pattern, tt.path, got, ok, tt.want)
		}
	}
}
