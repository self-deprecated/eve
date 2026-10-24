// Package config loads server.yml, client.yml and a repository's eve.yml (docs/design.md §7).
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Server is ~/.config/eve/server.yml. Identity, signing, export options and secrets live here,
// so an uploaded tree cannot redirect a build to another app.
type Server struct {
	Listen     string            `yaml:"listen"`  // default 127.0.0.1:4790
	DataDir    string            `yaml:"dataDir"` // default ~/.local/share/eve
	Team       string            `yaml:"team"`
	ASC        ASC               `yaml:"asc"`
	Apps       map[string]App    `yaml:"apps"`
	AllowExec  bool              `yaml:"allowExec"` // every app may exec, as if each set allowExec
	Path       []string          `yaml:"path"`      // tool directories appended to every job's PATH
	SizeGuard  SizeGuard         `yaml:"sizeGuard"`
	Device     DeviceDefaults    `yaml:"device"`
	Notify     Notify            `yaml:"notify"`
	Xcode      XcodeSettings     `yaml:"xcode"`
	Simulators SimulatorSettings `yaml:"simulators"`
}

// SimulatorSettings applies only to devices created and recorded by this Eve installation.
type SimulatorSettings struct {
	IdleTimeout      *Duration `yaml:"idleTimeout"`      // nil: 15m; zero: disable automatic retry/recovery
	ShutdownTimeout  Duration  `yaml:"shutdownTimeout"`  // zero: 30s
	MaxLeaseDuration Duration  `yaml:"maxLeaseDuration"` // zero: 1h, per acquisition/renewal
	ProtectedDevices []string  `yaml:"protectedDevices"`
}

func (s SimulatorSettings) IdleAfter() time.Duration {
	if s.IdleTimeout == nil {
		return 15 * time.Minute
	}
	return time.Duration(*s.IdleTimeout)
}

func (s SimulatorSettings) ShutdownWithin() time.Duration {
	if s.ShutdownTimeout == 0 {
		return 30 * time.Second
	}
	return time.Duration(s.ShutdownTimeout)
}

func (s SimulatorSettings) LeaseLimit() time.Duration {
	if s.MaxLeaseDuration == 0 {
		return time.Hour
	}
	return time.Duration(s.MaxLeaseDuration)
}

// ASC is the App Store Connect API key.
type ASC struct {
	KeyID    string `yaml:"keyId"`
	IssuerID string `yaml:"issuerId"`
	KeyPath  string `yaml:"keyPath"`
}

// App is one allowlisted app.
type App struct {
	ASCAppID   string            `yaml:"ascAppId"`
	BundleID   string            `yaml:"bundleId"`
	Signing    string            `yaml:"signing"` // xcode-account (default) | api-key | manual
	Export     Export            `yaml:"export"`
	Testflight TestflightConfig  `yaml:"testflight"`
	Secrets    map[string]string `yaml:"secrets"` // build setting → "file:<path>" | "env:<NAME>"
	AllowExec  bool              `yaml:"allowExec"`
	SizeGuard  *SizeGuard        `yaml:"sizeGuard"` // overrides the server defaults field by field
	Device     string            `yaml:"device"`
}

// Export holds ExportOptions that differ per app.
type Export struct {
	InternalOnly  *bool `yaml:"internalOnly"`  // default true for every app (docs/design.md §18.6)
	UploadSymbols *bool `yaml:"uploadSymbols"` // default true
}

// TestflightConfig names the internal group and tester eve checks.
type TestflightConfig struct {
	Group  string `yaml:"group"`
	Tester string `yaml:"tester"`
}

// DeviceDefaults is the server-wide default device.
type DeviceDefaults struct {
	Default string `yaml:"default"`
}

// Notify configures outgoing notifications.
type Notify struct {
	AllowHosts []string    `yaml:"allowHosts"`
	Human      *HumanHook  `yaml:"human"`
	Secrets    []HookToken `yaml:"secrets"`
}

// HumanHook is the optional webhook for things only a person can do (e.g. a chat bot or push service).
type HumanHook struct {
	URL    string `yaml:"url"`
	Secret string `yaml:"secret"` // "file:<path>"; HMAC-SHA256 when set
}

// HookToken pairs a URL prefix with an HMAC secret for generic webhooks.
type HookToken struct {
	Prefix string `yaml:"prefix"`
	Secret string `yaml:"secret"`
}

// XcodeSettings pins tools.
type XcodeSettings struct {
	XcodegenVersion string `yaml:"xcodegenVersion"`
	XcodegenSHA256  string `yaml:"xcodegenSha256"`
}

// Signing modes.
const (
	SigningXcodeAccount = "xcode-account"
	SigningAPIKey       = "api-key"
	SigningManual       = "manual"
)

// SizeGuard holds the size-guard thresholds (docs/design.md §6.7). Zero values mean "use the default".
type SizeGuard struct {
	Source         SourceThresholds `yaml:"source"`
	Bundle         BundleThresholds `yaml:"bundle"`
	ConfirmTimeout Duration         `yaml:"confirmTimeout"`
}

// SourceThresholds apply to the uploaded tree before it leaves the client.
type SourceThresholds struct {
	Growth    Percent  `yaml:"growth"`
	GrowthMin Bytes    `yaml:"growthMin"`
	Files     Percent  `yaml:"files"`
	Ceiling   Bytes    `yaml:"ceiling"`
	LargeFile Bytes    `yaml:"largeFile"`
	AllowDirs []string `yaml:"allowDirs"`
}

// BundleThresholds apply to the archived .app before delivery.
type BundleThresholds struct {
	Growth        Percent  `yaml:"growth"`
	GrowthMin     Bytes    `yaml:"growthMin"`
	Files         Percent  `yaml:"files"`
	FilesMin      int      `yaml:"filesMin"`
	NewFile       Bytes    `yaml:"newFile"`
	Ceiling       Bytes    `yaml:"ceiling"`
	AllowPatterns []string `yaml:"allowPatterns"`
}

// DefaultSizeGuard returns the plan's defaults.
func DefaultSizeGuard() SizeGuard {
	return SizeGuard{
		Source: SourceThresholds{
			Growth: 50, GrowthMin: 20 * MB, Files: 50, Ceiling: 500 * MB, LargeFile: 50 * MB,
		},
		Bundle: BundleThresholds{
			Growth: 15, GrowthMin: 5 * MB, Files: 20, FilesMin: 200, NewFile: 5 * MB, Ceiling: 300 * MB,
		},
		ConfirmTimeout: Duration(24 * time.Hour),
	}
}

// Merge returns base with every non-zero field of over applied.
func (base SizeGuard) Merge(over *SizeGuard) SizeGuard {
	if over == nil {
		return base
	}
	out := base
	s, o := &out.Source, over.Source
	setP(&s.Growth, o.Growth)
	setB(&s.GrowthMin, o.GrowthMin)
	setP(&s.Files, o.Files)
	setB(&s.Ceiling, o.Ceiling)
	setB(&s.LargeFile, o.LargeFile)
	if o.AllowDirs != nil {
		s.AllowDirs = o.AllowDirs
	}
	b, ob := &out.Bundle, over.Bundle
	setP(&b.Growth, ob.Growth)
	setB(&b.GrowthMin, ob.GrowthMin)
	setP(&b.Files, ob.Files)
	if ob.FilesMin != 0 {
		b.FilesMin = ob.FilesMin
	}
	setB(&b.NewFile, ob.NewFile)
	setB(&b.Ceiling, ob.Ceiling)
	if ob.AllowPatterns != nil {
		b.AllowPatterns = ob.AllowPatterns
	}
	if over.ConfirmTimeout != 0 {
		out.ConfirmTimeout = over.ConfirmTimeout
	}
	return out
}

func setP(dst *Percent, v Percent) {
	if v != 0 {
		*dst = v
	}
}

func setB(dst *Bytes, v Bytes) {
	if v != 0 {
		*dst = v
	}
}

// SizeGuardFor returns the effective thresholds for app.
func (s *Server) SizeGuardFor(app string) SizeGuard {
	base := DefaultSizeGuard().Merge(&s.SizeGuard)
	if a, ok := s.Apps[app]; ok {
		return base.Merge(a.SizeGuard)
	}
	return base
}

// InternalOnly reports the app's TestFlight audience; true unless explicitly false.
func (a App) InternalOnly() bool { return a.Export.InternalOnly == nil || *a.Export.InternalOnly }

// UploadSymbols reports whether to upload dSYMs; true unless explicitly false.
func (a App) UploadSymbols() bool { return a.Export.UploadSymbols == nil || *a.Export.UploadSymbols }

// SigningMode returns the app's signing mode with the default applied.
func (a App) SigningMode() string {
	if a.Signing == "" {
		return SigningXcodeAccount
	}
	return a.Signing
}

var appName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,40}$`)

var simulatorID = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

// LoadServer reads and validates server.yml.
func LoadServer(path string) (*Server, error) {
	var s Server
	if err := readYAML(path, &s); err != nil {
		return nil, err
	}
	s.applyDefaults()
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

func (s *Server) applyDefaults() {
	if s.Listen == "" {
		s.Listen = "127.0.0.1:4790"
	}
	if s.DataDir == "" {
		s.DataDir = "~/.local/share/eve"
	}
	s.DataDir = ExpandHome(s.DataDir)
	s.ASC.KeyPath = ExpandHome(s.ASC.KeyPath)
	for i, dir := range s.Path {
		s.Path[i] = ExpandHome(dir)
	}
	if s.AllowExec {
		for name, a := range s.Apps {
			a.AllowExec = true
			s.Apps[name] = a
		}
	}
	if s.Xcode.XcodegenVersion == "" {
		s.Xcode.XcodegenVersion = "2.46.0"
		s.Xcode.XcodegenSHA256 = "4d9e34b62172d645eed6457cac13fc222569974098ef4ee9c3368bedf0196806"
	}
}

// Validate checks the configuration for mistakes that would only surface mid-build.
func (s *Server) Validate() error {
	var errs []error
	if s.Simulators.IdleAfter() < 0 {
		errs = append(errs, errors.New("simulators.idleTimeout must be non-negative (0s disables automatic cleanup)"))
	}
	if d := s.Simulators.ShutdownWithin(); d < time.Second || d > 2*time.Minute {
		errs = append(errs, errors.New("simulators.shutdownTimeout must be between 1s and 2m"))
	}
	if s.Simulators.LeaseLimit() < time.Second {
		errs = append(errs, errors.New("simulators.maxLeaseDuration must be at least 1s (omitted or 0s uses 1h)"))
	}
	for _, id := range s.Simulators.ProtectedDevices {
		if !simulatorID.MatchString(id) {
			errs = append(errs, fmt.Errorf("simulators.protectedDevices: invalid UDID %q", id))
		}
	}
	if s.Team == "" {
		errs = append(errs, errors.New("team is required"))
	}
	for _, dir := range s.Path {
		// Directories on the build Mac, whatever OS reads this file.
		if !strings.HasPrefix(dir, "/") || strings.Contains(dir, ":") {
			errs = append(errs, fmt.Errorf("path %q: want an absolute directory without ':'", dir))
		}
	}
	if !strings.HasPrefix(s.Listen, "127.0.0.1:") && !strings.HasPrefix(s.Listen, "localhost:") {
		errs = append(errs, fmt.Errorf("listen %q: eve listens on loopback only; publish it with the Tailscale Service", s.Listen))
	}
	names := make([]string, 0, len(s.Apps))
	for name := range s.Apps {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		a := s.Apps[name]
		if !appName.MatchString(name) {
			errs = append(errs, fmt.Errorf("apps.%s: app names are lowercase letters, digits and dashes", name))
		}
		if a.BundleID == "" {
			errs = append(errs, fmt.Errorf("apps.%s.bundleId is required", name))
		}
		switch a.SigningMode() {
		case SigningXcodeAccount, SigningAPIKey:
		case SigningManual:
			errs = append(errs, fmt.Errorf("apps.%s.signing: manual signing is not implemented yet (docs/design.md M7)", name))
		default:
			errs = append(errs, fmt.Errorf("apps.%s.signing %q: want xcode-account or api-key", name, a.Signing))
		}
		for k, v := range a.Secrets {
			if !strings.HasPrefix(v, "file:") && !strings.HasPrefix(v, "env:") {
				errs = append(errs, fmt.Errorf("apps.%s.secrets.%s: want file:<path> or env:<NAME>", name, k))
			}
		}
	}
	return errors.Join(errs...)
}

// Client is ~/.config/eve/client.yml. The token lives next to it in a 0600 file.
type Client struct {
	Server string `yaml:"server"`
}

// ClientDir returns ~/.config/eve (or $EVE_CONFIG_DIR).
func ClientDir() string {
	if d := os.Getenv("EVE_CONFIG_DIR"); d != "" {
		return d
	}
	return ExpandHome("~/.config/eve")
}

// LoadClient reads client.yml; a missing file yields an empty config.
func LoadClient() (*Client, error) {
	var c Client
	err := readYAML(filepath.Join(ClientDir(), "client.yml"), &c)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if env := os.Getenv("EVE_SERVER"); env != "" {
		c.Server = env
	}
	return &c, nil
}

// Repo is a repository's eve.yml.
type Repo struct {
	App     string      `yaml:"app"`
	Project RepoProject `yaml:"project"`
	Xcode   string      `yaml:"xcode"`
	Pre     []string    `yaml:"pre"`
	Check   RepoCheck   `yaml:"check"`
	Device  RepoDevice  `yaml:"device"`
	Ignore  []string    `yaml:"ignore"`
}

// RepoProject locates the Xcode project.
type RepoProject struct {
	Dir         string `yaml:"dir"`         // relative directory holding the project, default "."
	Xcodegen    string `yaml:"xcodegen"`    // project.yml path relative to Dir
	Xcodeproj   string `yaml:"xcodeproj"`   // e.g. Lineage.xcodeproj
	Xcworkspace string `yaml:"xcworkspace"` // e.g. ApproveAL.xcworkspace (CocoaPods, Expo)
	Scheme      string `yaml:"scheme"`
}

// RepoCheck configures the check verb.
type RepoCheck struct {
	Test      bool   `yaml:"test"`
	Simulator string `yaml:"simulator"` // device type name, e.g. "iPhone 17 Pro"; default: the newest Pro
}

// RepoDevice configures the device verb.
type RepoDevice struct {
	Configuration string `yaml:"configuration"`
}

// RepoFile is the conventional name of a repository's config.
const RepoFile = "eve.yml"

// LoadRepo reads eve.yml from dir.
func LoadRepo(dir string) (*Repo, error) {
	var r Repo
	if err := readYAML(filepath.Join(dir, RepoFile), &r); err != nil {
		return nil, err
	}
	if r.Project.Dir == "" {
		r.Project.Dir = "."
	}
	if r.Device.Configuration == "" {
		r.Device.Configuration = "Debug"
	}
	if r.App == "" {
		return nil, fmt.Errorf("%s: app is required", RepoFile)
	}
	if r.Project.Scheme == "" {
		return nil, fmt.Errorf("%s: project.scheme is required", RepoFile)
	}
	set := 0
	for _, v := range []string{r.Project.Xcodegen, r.Project.Xcodeproj, r.Project.Xcworkspace} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return nil, fmt.Errorf("%s: set exactly one of project.xcodegen, project.xcodeproj and project.xcworkspace", RepoFile)
	}
	if filepath.IsAbs(r.Project.Dir) || strings.Contains(r.Project.Dir, "..") {
		return nil, fmt.Errorf("%s: project.dir must be a relative path inside the repository", RepoFile)
	}
	return &r, nil
}

// FindRepoRoot walks up from dir to the directory holding eve.yml.
func FindRepoRoot(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, RepoFile)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s found here or in any parent directory", RepoFile)
		}
		dir = parent
	}
}

func readYAML(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// ExpandHome replaces a leading ~/ with the home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// ResolveSecret reads a "file:<path>" or "env:<NAME>" reference.
func ResolveSecret(ref string) (string, error) {
	switch {
	case strings.HasPrefix(ref, "file:"):
		b, err := os.ReadFile(ExpandHome(strings.TrimPrefix(ref, "file:")))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	case strings.HasPrefix(ref, "env:"):
		v, ok := os.LookupEnv(strings.TrimPrefix(ref, "env:"))
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", strings.TrimPrefix(ref, "env:"))
		}
		return v, nil
	}
	return "", fmt.Errorf("secret reference %q: want file:<path> or env:<NAME>", ref)
}

// Byte sizes.
const (
	KB Bytes = 1000
	MB Bytes = 1000 * KB
	GB Bytes = 1000 * MB
)

// Bytes is a size that unmarshals from "20MB", "1.5 GB" or a plain number of bytes.
type Bytes int64

// ParseBytes parses a size with an optional KB/MB/GB suffix (decimal units).
func ParseBytes(s string) (Bytes, error) {
	t := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	mult := Bytes(1)
	for _, u := range []struct {
		suffix string
		mult   Bytes
	}{{"GB", GB}, {"MB", MB}, {"KB", KB}, {"B", 1}} {
		if strings.HasSuffix(t, u.suffix) {
			mult, t = u.mult, strings.TrimSuffix(t, u.suffix)
			break
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return Bytes(f * float64(mult)), nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (b *Bytes) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseBytes(n.Value)
	if err != nil {
		return err
	}
	*b = v
	return nil
}

// Percent unmarshals from "15%" or 15.
type Percent float64

// UnmarshalYAML implements yaml.Unmarshaler.
func (p *Percent) UnmarshalYAML(n *yaml.Node) error {
	f, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(n.Value), "%"), 64)
	if err != nil || f < 0 {
		return fmt.Errorf("invalid percentage %q", n.Value)
	}
	*p = Percent(f)
	return nil
}

// Duration unmarshals from Go duration syntax ("24h").
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}
