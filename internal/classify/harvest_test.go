//go:build harvest

package classify

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/self-deprecated/eve/internal/redact"
)

// TestHarvest redacts staged raw logs into testdata/logs. It is how the real fixtures were made
// (testdata/README.md) and only runs on demand:
//
//	EVE_HARVEST_SRC=/tmp/eve-harvest/staged EVE_HARVEST_SECRETS='First,Last Name' \
//		go test -tags harvest -run Harvest ./internal/classify/
//
// The staging directory mirrors testdata/logs (<class>/<name>.log) and holds unredacted
// selections from build logs, IDEDistribution logs and session transcripts.
func TestHarvest(t *testing.T) {
	src := os.Getenv("EVE_HARVEST_SRC")
	if src == "" {
		t.Skip("EVE_HARVEST_SRC not set")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// The owner's name appears in device names ("…’s iPhone") and signing identities; pass it
	// and any other personal strings, comma-separated, in EVE_HARVEST_SECRETS.
	var secrets []string
	if s := os.Getenv("EVE_HARVEST_SECRETS"); s != "" {
		secrets = strings.Split(s, ",")
	}
	r := redact.New(redact.Options{Home: home, Secrets: secrets})
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join("testdata", "logs", rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		t.Logf("%s (%d bytes)", rel, len(raw))
		return os.WriteFile(dst, r.Redact(raw), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
