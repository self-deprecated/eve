package cli

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func artifactsCmd(env *Env) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "artifacts <job>",
		Short: "Download an exec job's artifacts (directories are unpacked)",
		Long: "Download every artifact of an exec job into --out (default: the current directory).\n" +
			"A file artifact is written as DIR/<name>; a directory artifact replaces DIR/<name> entirely,\n" +
			"so `eve artifacts <job> --out docs/screenshots` refreshes docs/screenshots/current/.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client()
			if err != nil {
				return err
			}
			r, err := c.Job(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if len(r.Artifacts) == 0 {
				return fmt.Errorf("job %s has no artifacts", args[0])
			}
			var written []string
			for _, name := range r.Artifacts {
				body, err := c.Artifact(cmd.Context(), args[0], name)
				if err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				base := strings.TrimSuffix(name, "/")
				dest := filepath.Join(out, base)
				if strings.HasSuffix(name, "/") {
					err = replaceDir(dest, body)
				} else {
					err = writeFile(dest, body)
				}
				_ = body.Close()
				if err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				written = append(written, dest)
			}
			if env.JSON {
				return env.printJSON(written)
			}
			for _, p := range written {
				_, _ = fmt.Fprintln(env.Out, p)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", ".", "directory to write the artifacts into")
	return cmd
}

func writeFile(dest string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".eve-tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// replaceDir unpacks a tar stream next to dest, then swaps it in, so a failed download leaves
// dest as it was.
func replaceDir(dest string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".eve-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := untar(tmp, r); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// untar writes a stream of directories and regular files under dir, refusing anything that
// would land outside it.
func untar(dir string, r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(strings.TrimSuffix(h.Name, "/"))
		if name == "" || filepath.IsAbs(name) || !filepath.IsLocal(name) {
			return fmt.Errorf("unsafe path %q in the artifact", h.Name)
		}
		p := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFile(p, tr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected entry %q in the artifact", h.Name)
		}
	}
}
