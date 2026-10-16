package sizeguard

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// driverLayout is the file list of a real Driver.app TestFlight archive (Driver 1.0, October
// 2026): 20 files, 12.9 MB, with a widget extension and an embedded framework.
var driverLayout = map[string]int64{
	"AppIcon60x60@2x.png":                    1772,
	"AppIcon76x76@2x~ipad.png":               2156,
	"Assets.car":                             40024,
	"Driver":                                 361024,
	"Frameworks/WebRTC.framework/Info.plist": 750,
	"Frameworks/WebRTC.framework/PrivacyInfo.xcprivacy":        845,
	"Frameworks/WebRTC.framework/WebRTC":                       12377648,
	"Frameworks/WebRTC.framework/_CodeSignature/CodeResources": 2034,
	"Info.plist": 1611,
	"Metadata.appintents/extract.actionsdata":                          3078,
	"Metadata.appintents/nlu/e0ebecf2d86e722b27cb31bcdbb6f67c.version": 1,
	"Metadata.appintents/nlu/nlu.lzfse":                                2178,
	"Metadata.appintents/version.json":                                 53,
	"PkgInfo":                                                          8,
	"PlugIns/DriverWidgets.appex/DriverWidgets":                        94032,
	"PlugIns/DriverWidgets.appex/Info.plist":                           940,
	"PlugIns/DriverWidgets.appex/_CodeSignature/CodeResources":         2040,
	"PlugIns/DriverWidgets.appex/embedded.mobileprovision":             12423,
	"_CodeSignature/CodeResources":                                     6071,
	"embedded.mobileprovision":                                         12423,
}

// writeApp materializes layout as sparse files under dir/name.
func writeApp(t *testing.T, dir, name string, layout map[string]int64) string {
	t.Helper()
	app := filepath.Join(dir, name)
	for rel, size := range layout {
		p := filepath.Join(app, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return app
}

// manifest builds a Manifest directly from a layout.
func manifest(layout map[string]int64) Manifest {
	m := Manifest{Name: "Driver.app"}
	for p, s := range layout {
		m.Entries = append(m.Entries, Entry{Path: p, Size: s})
	}
	m.normalize()
	return m
}

func TestBuildManifest(t *testing.T) {
	app := writeApp(t, t.TempDir(), "Driver.app", driverLayout)
	if err := os.Symlink("Driver", filepath.Join(app, "Alias")); err != nil {
		t.Fatal(err)
	}
	m, err := BuildManifest(app)
	if err != nil {
		t.Fatal(err)
	}
	want := manifest(driverLayout)
	want.Entries = append(want.Entries, Entry{Path: "Alias", Link: "Driver"})
	want.normalize()
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("manifest:\n got %+v\nwant %+v", m, want)
	}
	if m.Files != 21 || m.Total != 12921111 {
		t.Errorf("Files %d Total %d", m.Files, m.Total)
	}
	if FormatBytes(m.Total) != "12.9 MB" {
		t.Errorf("total renders as %s", FormatBytes(m.Total))
	}
}

func TestManifestRoundTrip(t *testing.T) {
	m := manifest(driverLayout)
	m.Build = "202610161458"
	var buf bytes.Buffer
	if err := WriteManifest(&buf, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(&buf)
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("ReadManifest = %+v, %v", got, err)
	}
	p := filepath.Join(t.TempDir(), "baselines", "driver.json.zst")
	if err := SaveManifest(p, m); err != nil {
		t.Fatal(err)
	}
	got, err = LoadManifest(p)
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("LoadManifest = %+v, %v", got, err)
	}
	if _, err := ReadManifest(bytes.NewReader([]byte("{}"))); err == nil {
		t.Error("uncompressed input accepted")
	}
}
