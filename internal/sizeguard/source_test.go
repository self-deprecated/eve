package sizeguard

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
)

// files builds a source listing from path → size.
func files(m map[string]int64) []SourceFile {
	var out []SourceFile
	for p, s := range m {
		out = append(out, SourceFile{Path: p, Size: s})
	}
	return out
}

func stats(m map[string]int64) *api.SourceStats {
	st := SourceStatsOf(files(m))
	return &st
}

func TestCheckSource(t *testing.T) {
	// lineage-like tree: 100 MB in 1,000 files, so 50 % (50 MB) beats the 20 MB minimum. No file
	// is over the 50 MB large-file limit.
	lastTree := map[string]int64{
		"ios/App/Big0.bin": 25 * mb, "ios/App/Big1.bin": 25 * mb, "ios/App/Big2.bin": 25 * mb,
		"ios/App/Big3.bin": 25*mb - 996,
	}
	for i := range 996 {
		lastTree["ios/App/File"+strconv.Itoa(i)+".swift"] = 1
	}
	last := stats(lastTree)
	// small: 10 MB, where the 20 MB minimum applies.
	small := stats(map[string]int64{"ios/a.swift": 10 * mb})

	tests := []struct {
		name     string
		cur      map[string]int64
		excluded map[string]int64
		last     *api.SourceStats
		th       config.SourceThresholds
		tripped  bool
		reasons  []string
	}{
		{name: "unchanged", cur: lastTree, last: last},
		{name: "first submit", cur: lastTree},
		{name: "growth at 50 %", cur: with(lastTree, map[string]int64{"ios/App/Big0.bin": 50 * mb, "ios/App/Big1.bin": 50 * mb}), last: last},
		{name: "growth over 50 %", cur: with(lastTree, map[string]int64{"ios/App/Big0.bin": 50 * mb, "ios/App/Big1.bin": 50*mb + 1}), last: last, tripped: true, reasons: []string{"size 150.0 MB vs 100.0 MB last accepted (+50%"}},
		{name: "growth at 20 MB minimum", cur: map[string]int64{"ios/a.swift": 30 * mb}, last: small},
		{name: "growth over 20 MB minimum", cur: map[string]int64{"ios/a.swift": 30*mb + 1}, last: small, tripped: true, reasons: []string{"+200%"}},
		{name: "growth ignored on first submit", cur: many("ios/a%d", 16, 25*mb)},
		{name: "files at 50 %", cur: with(lastTree, many("ios/New%d.swift", 500, 0)), last: last},
		{name: "files over 50 %", cur: with(lastTree, many("ios/New%d.swift", 501, 0)), last: last, tripped: true, reasons: []string{"files 1,501 vs 1,000"}},
		{name: "at ceiling", cur: many("ios/a%d", 20, 25*mb)},
		{name: "over ceiling", cur: with(many("ios/a%d", 20, 25*mb), map[string]int64{"ios/z": 1}), tripped: true, reasons: []string{"over the 500.0 MB ceiling"}},
		{name: "large file at 50 MB", cur: map[string]int64{"ios/model.bin": 50 * mb}},
		{name: "large file over 50 MB", cur: map[string]int64{"ios/model.bin": 50*mb + 1}, tripped: true, reasons: []string{"files over 50.0 MB: ios/model.bin (50.0 MB)"}},
		{name: "Pods uploaded", cur: with(lastTree, map[string]int64{"ios/Pods/Alamofire/a.swift": 10}), last: last, tripped: true, reasons: []string{"contains ios/Pods/ (10 B, 1 files)"}},
		{name: "Pods allowed by name", cur: with(lastTree, map[string]int64{"ios/Pods/Alamofire/a.swift": 10}), last: last, th: config.SourceThresholds{AllowDirs: []string{"Pods"}}},
		{name: "Pods allowed by path", cur: with(lastTree, map[string]int64{"ios/Pods/Alamofire/a.swift": 10}), last: last, th: config.SourceThresholds{AllowDirs: []string{"ios/Pods/"}}},
		{name: "node_modules excluded but not gitignored", cur: lastTree, excluded: map[string]int64{"web/node_modules/x/i.js": 3 * mb, "web/node_modules/y/i.js": 1}, last: last, tripped: true, reasons: []string{"contains web/node_modules/ (3.0 MB, 2 files); eve leaves it out"}},
		{name: "build dir on first submit", cur: lastTree, excluded: map[string]int64{"ios/build/Driver.ipa": 9 * mb}, tripped: true, reasons: []string{"contains ios/build/"}},
		{name: "DerivedData", cur: lastTree, excluded: map[string]int64{"DerivedData/x": 1}, last: last, tripped: true},
		{name: "swiftpm build dir", cur: lastTree, excluded: map[string]int64{"Packages/Core/.build/debug/x": 1}, last: last, tripped: true, reasons: []string{"Packages/Core/.build/"}},
		{name: "excluded secret alone is fine", cur: lastTree, excluded: map[string]int64{".env": 10, "AuthKey.p8": 200}, last: last},
		{name: "file named build is fine", cur: with(lastTree, map[string]int64{"scripts/build": 10}), last: last},
		{name: "custom thresholds", cur: map[string]int64{"ios/a": 11 * mb}, th: config.SourceThresholds{Ceiling: 10 * config.MB}, tripped: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := SourceInput{Files: files(tt.cur), Excluded: files(tt.excluded)}
			v := CheckSource(in, tt.last, tt.th)
			if v.Tripped != tt.tripped {
				t.Fatalf("Tripped = %v, reasons %q", v.Tripped, v.Reasons)
			}
			for _, want := range tt.reasons {
				if !containsAny(v.Reasons, want) {
					t.Errorf("no reason contains %q: %q", want, v.Reasons)
				}
			}
			if !tt.tripped && len(v.Reasons) > 0 {
				t.Errorf("reasons without trip: %q", v.Reasons)
			}
			out := v.Render()
			if tt.tripped != strings.Contains(out, "--accept-source-size") {
				t.Errorf("render:\n%s", out)
			}
		})
	}
}

func TestCheckSourceReport(t *testing.T) {
	last := stats(map[string]int64{"ios/App.swift": 40 * mb, "README.md": 2000})
	cur := map[string]int64{
		"ios/App.swift":   40*mb + 1_200_000,
		"README.md":       2000,
		"docs/manual.pdf": 120 * mb,
		"docs/notes.md":   1000,
		"ios/mock[1].psd": 60 * mb,
		"a":               1, "b": 1, "c": 1,
	}
	in := SourceInput{
		Files:    files(cur),
		Excluded: files(map[string]int64{"node_modules/x/index.js": 5 * mb}),
	}
	in.Stats = SourceStatsOf(in.Files)
	v := CheckSource(in, last, config.SourceThresholds{})
	if !v.Tripped {
		t.Fatal("not tripped")
	}
	wantLargest := []api.SizeEntry{
		{Path: "docs/", Bytes: 120*mb + 1000, Change: "new"},
		{Path: "ios/", Bytes: 101_200_000, Change: "+61.2 MB"},
		{Path: "README.md", Bytes: 2000, Change: "+0 B"},
		{Path: "a", Bytes: 1, Change: "new"},
		{Path: "b", Bytes: 1, Change: "new"},
	}
	if !reflect.DeepEqual(v.Largest, wantLargest) {
		t.Errorf("Largest:\n got %+v\nwant %+v", v.Largest, wantLargest)
	}
	wantSuggest := []string{"node_modules/", "/docs/", `/ios/mock\[1].psd`}
	if !reflect.DeepEqual(v.Suggestions, wantSuggest) {
		t.Errorf("Suggestions %q, want %q", v.Suggestions, wantSuggest)
	}
	out := v.Render()
	for _, want := range []string{
		"source check failed: 221.2 MB, 8 files (last accepted: 40.0 MB, 2 files)",
		"  docs/       120.0 MB  new\n",
		"  /docs/\n",
		"contains node_modules/ (5.0 MB, 1 files)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	if pass := CheckSource(SourceInput{Files: files(map[string]int64{"a": 1})}, nil, config.SourceThresholds{}).Render(); pass != "source check passed: 1 B, 1 files (no accepted source yet)\n" {
		t.Errorf("pass render %q", pass)
	}
}
