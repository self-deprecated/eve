package source

// AlwaysExclude lists what is never uploaded, whatever the ignore files say (docs/design.md §6.1):
// build output, dependency caches, and credential files.
var AlwaysExclude = []string{
	".git/",
	"build/",
	"DerivedData/",
	".build/",
	".tools/",
	"node_modules/",
	".DS_Store",
}

// SecretPatterns name credential files that must never leave the client. The server refuses an
// upload containing any of them (defence in depth; the client already excludes them).
var SecretPatterns = []string{
	".env*",
	"*.p8",
	"*.p12",
	"*.mobileprovision",
	"*.pem",
	"*.key",
}

var (
	alwaysExclude = ParseIgnore("", append(append([]string{}, AlwaysExclude...), SecretPatterns...))
	secrets       = ParseIgnore("", SecretPatterns)
)

// IsSecret reports whether the slash-separated path rel names, or lies inside, a credential
// file or directory matching SecretPatterns.
func IsSecret(rel string) bool {
	return secrets.Ignored(rel, false)
}
