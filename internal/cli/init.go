package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func initCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Write a starter eve.yml for the Xcode project here (or one directory down)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := config.RepoFile
			if _, err := os.Stat(path); err == nil {
				return &ExitError{Code: api.ExitUsage, Err: fmt.Errorf("%s already exists", path)}
			}
			p, err := findProject(".")
			if err != nil {
				return &ExitError{Code: api.ExitUsage, Err: err}
			}
			if err := os.WriteFile(path, []byte(starterRepo(p)), 0o644); err != nil {
				return err
			}
			if _, err := config.LoadRepo("."); err != nil {
				return fmt.Errorf("the generated %s does not load: %w", path, err)
			}
			_, _ = fmt.Fprintf(env.Out, "wrote %s for scheme %s in %s\n", path, p.Scheme, p.Dir)
			_, _ = fmt.Fprintf(env.Out, "Check that app: %s matches a name in `eve apps`, and list large non-source paths under ignore.\n", strings.ToLower(p.Scheme))
			return nil
		},
	}
}

// findProject looks for an XcodeGen spec or an .xcodeproj in dir, then in its direct subdirectories.
func findProject(dir string) (config.RepoProject, error) {
	dirs := []string{"."}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && !strings.HasSuffix(e.Name(), ".xcodeproj") {
			dirs = append(dirs, e.Name())
		}
	}
	for _, d := range dirs {
		abs := filepath.Join(dir, d)
		if b, err := os.ReadFile(filepath.Join(abs, "project.yml")); err == nil {
			var spec struct {
				Name string `yaml:"name"`
			}
			if err := yaml.Unmarshal(b, &spec); err == nil && spec.Name != "" {
				return config.RepoProject{Dir: d, Xcodegen: "project.yml", Scheme: spec.Name}, nil
			}
		}
		if m, _ := filepath.Glob(filepath.Join(abs, "*.xcodeproj")); len(m) == 1 {
			name := filepath.Base(m[0])
			return config.RepoProject{Dir: d, Xcodeproj: name, Scheme: strings.TrimSuffix(name, ".xcodeproj")}, nil
		}
	}
	return config.RepoProject{}, errors.New("no project.yml or single .xcodeproj here or one directory down; write eve.yml by hand (see the eve skill)")
}

func starterRepo(p config.RepoProject) string {
	var b strings.Builder
	fmt.Fprintf(&b, "app: %-28s # the app's name on the server (eve apps)\n", strings.ToLower(p.Scheme))
	b.WriteString("project:\n")
	fmt.Fprintf(&b, "  dir: %s\n", p.Dir)
	if p.Xcodegen != "" {
		fmt.Fprintf(&b, "  xcodegen: %s\n", p.Xcodegen)
	} else {
		fmt.Fprintf(&b, "  xcodeproj: %s\n", p.Xcodeproj)
	}
	fmt.Fprintf(&b, "  scheme: %s\n", p.Scheme)
	b.WriteString(`# ignore:                         # gitignore-style paths never uploaded (on top of .gitignore)
#   - docs/screenshots/
# pre:                            # commands run in the workspace before building
#   - swift test --package-path Core
# check:                          # eve submit check
#   test: true                    # also run the scheme's tests (default: build only)
#   simulator: iPhone 17 Pro      # default: the newest iPhone Pro
# xcode: "26"                     # refuse to build with another Xcode version
`)
	return b.String()
}
