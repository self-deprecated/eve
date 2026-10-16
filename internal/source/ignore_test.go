package source

import "testing"

func TestIgnore(t *testing.T) {
	tests := []struct {
		name     string
		base     string
		patterns []string
		path     string
		dir      bool
		want     bool
	}{
		{"plain name matches at any depth", "", []string{"build"}, "ios/build/x.o", false, true},
		{"plain name matches the file itself", "", []string{"notes.txt"}, "a/b/notes.txt", false, true},
		{"dir-only skips files", "", []string{"build/"}, "build", false, false},
		{"dir-only matches dirs", "", []string{"build/"}, "build", true, true},
		{"dir-only excludes contents", "", []string{"build/"}, "build/out/a", false, true},
		{"star stays in one segment", "", []string{"*.xcodeproj/"}, "ios/Driver.xcodeproj/project.pbxproj", false, true},
		{"star glob on files", "", []string{"*.log"}, "logs/today.log", false, true},
		{"star glob miss", "", []string{"*.log"}, "logs/today.txt", false, false},
		{"question mark", "", []string{"a?c"}, "abc", false, true},
		{"question mark not slash", "", []string{"a?c"}, "a/c", false, false},
		{"class", "", []string{"[ab].txt"}, "b.txt", false, true},
		{"negated class", "", []string{"[!ab].txt"}, "b.txt", false, false},
		{"negated class hit", "", []string{"[!ab].txt"}, "c.txt", false, true},
		{"leading slash anchors", "", []string{"/docs"}, "docs/a.md", false, true},
		{"leading slash anchors miss", "", []string{"/docs"}, "ios/docs/a.md", false, false},
		{"middle slash anchors", "", []string{"ios/build"}, "ios/build/a", false, true},
		{"middle slash anchors miss", "", []string{"ios/build"}, "x/ios/build/a", false, false},
		{"leading double star", "", []string{"**/cache"}, "a/b/cache/f", false, true},
		{"leading double star at root", "", []string{"**/cache"}, "cache", true, true},
		{"trailing double star", "", []string{"vendor/**"}, "vendor/a/b", false, true},
		{"trailing double star not dir itself", "", []string{"vendor/**"}, "vendor", true, false},
		{"middle double star", "", []string{"a/**/z"}, "a/b/c/z", false, true},
		{"middle double star zero dirs", "", []string{"a/**/z"}, "a/z", false, true},
		{"negation re-includes", "", []string{"*.log", "!keep.log"}, "keep.log", false, false},
		{"last match wins", "", []string{"!keep.log", "*.log"}, "keep.log", false, true},
		{"negation cannot rescue inside excluded dir", "", []string{"out/", "!out/keep"}, "out/keep", false, true},
		{"comment ignored", "", []string{"# build"}, "# build", false, false},
		{"escaped hash", "", []string{`\#notes`}, "#notes", false, true},
		{"escaped bang", "", []string{`\!important`}, "!important", false, true},
		{"trailing spaces trimmed", "", []string{"tmp   "}, "tmp", false, true},
		{"escaped star is literal", "", []string{`a\*b`}, "a*b", false, true},
		{"escaped star miss", "", []string{`a\*b`}, "axb", false, false},
		{"dot is literal", "", []string{"*.p8"}, "keyp8", false, false},
		{"nested base applies below", "ios", []string{"build/"}, "ios/build/x", false, true},
		{"nested base not above", "ios", []string{"build/"}, "build/x", false, false},
		{"nested anchored", "ios", []string{"/Pods"}, "ios/Pods/a", false, true},
		{"nested anchored miss deeper", "ios", []string{"/Pods"}, "ios/x/Pods/a", false, false},
		{"env prefix", "", []string{".env*"}, "config/.env.local", false, true},
		{"posix class", "", []string{"[[:digit:]].txt"}, "7.txt", false, true},
		{"unclosed bracket literal", "", []string{"a[b"}, "a[b", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ig := ParseIgnore(tt.base, tt.patterns)
			if got := ig.Ignored(tt.path, tt.dir); got != tt.want {
				t.Errorf("Ignored(%q) with %q at %q = %v, want %v", tt.path, tt.patterns, tt.base, got, tt.want)
			}
		})
	}
}

func TestIsSecret(t *testing.T) {
	for path, want := range map[string]bool{
		"AuthKey_ABC.p8":                 true,
		"ios/certs/dist.p12":             true,
		"profiles/App.mobileprovision":   true,
		".env":                           true,
		"web/.env.production":            true,
		"tls/server.pem":                 true,
		"keys/id.key":                    true,
		"ios/Driver/Info.plist":          false,
		"docs/keyboard.md":               false,
		"environment.swift":              false,
		"Sources/KeychainKeyStore.swift": false,
	} {
		if got := IsSecret(path); got != want {
			t.Errorf("IsSecret(%q) = %v, want %v", path, got, want)
		}
	}
}
