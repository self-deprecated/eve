package apple

import (
	"context"
	"regexp"
	"slices"
)

var (
	bundleIDRE   = regexp.MustCompile(`^[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$`)
	extensionRE  = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	capabilityRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	skuRE        = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	localeRE     = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z]{2,4})?$`)
	versionRE    = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}$`)
)

// AppSpec is what `eve apple app create` asks for.
type AppSpec struct {
	BundleID string
	Name     string
	SKU      string
	Locale   string
	Version  string
	// Like is an existing app whose capabilities (with settings) and app-group use are copied,
	// for the app and for each extension with the same suffix.
	Like       string
	Extensions []string // suffixes: "monitor" registers <bundle-id>.monitor
	// Capabilities are enabled on the app, ExtensionCapabilities on every extension, on top of
	// anything copied from Like.
	Capabilities          []string
	ExtensionCapabilities []string
	// AppGroup creates group.<bundle-id> and adds the app and its extensions to it (implied
	// when Like uses an app group).
	AppGroup bool
	DryRun   bool
}

// Validate checks the spec before anything talks to Apple.
func (s AppSpec) Validate() error {
	switch {
	case !bundleIDRE.MatchString(s.BundleID):
		return usage("bundle id %q must look like com.example.app (letters, digits, hyphens and dots)", s.BundleID)
	case s.Name == "":
		return usage("--name is required: the app's name in App Store Connect")
	case !skuRE.MatchString(s.SKU):
		return usage("--sku is required (letters, digits, dots, hyphens and underscores)")
	case !localeRE.MatchString(s.Locale):
		return usage("--locale %q must be a locale code such as en-US or en-GB", s.Locale)
	case !versionRE.MatchString(s.Version):
		return usage("--version %q must look like 1.0", s.Version)
	case s.Like != "" && !bundleIDRE.MatchString(s.Like):
		return usage("--like %q must be a bundle id", s.Like)
	case s.Like == s.BundleID:
		return usage("--like must name another app")
	}
	for _, e := range s.Extensions {
		if !extensionRE.MatchString(e) {
			return usage("--extension %q must be one bundle id component, such as widget", e)
		}
	}
	for _, c := range slices.Concat(s.Capabilities, s.ExtensionCapabilities) {
		if !capabilityRE.MatchString(c) {
			return usage("capability %q must be Apple's capability type, such as PUSH_NOTIFICATIONS", c)
		}
	}
	return nil
}

// AppResult is what `eve apple app create` did (or, with DryRun, would do).
type AppResult struct {
	Account
	BundleID string `json:"bundleId"`
	AppID    string `json:"appId,omitempty"`
	DryRun   bool   `json:"dryRun,omitempty"`
	// Changes is empty when everything already existed.
	Changes []string `json:"changes"`
	Notes   []string `json:"notes,omitempty"`
}

// CreateApp creates whatever the app is missing on Apple's side: the app group, the bundle id and
// its capabilities, the App Store Connect record, then the extensions' bundle ids. Anything that
// exists is left alone, so it is safe to run again. On error the result still lists what changed.
func (c *Client) CreateApp(ctx context.Context, team string, s AppSpec) (AppResult, error) {
	res := AppResult{BundleID: s.BundleID, DryRun: s.DryRun}
	if err := s.Validate(); err != nil {
		return res, err
	}
	req, err := c.request(team, map[string]any{
		"bundleId": s.BundleID, "name": s.Name, "sku": s.SKU, "locale": s.Locale, "version": s.Version,
		"like": s.Like, "extensions": nonNil(s.Extensions), "capabilities": nonNil(s.Capabilities),
		"extensionCapabilities": nonNil(s.ExtensionCapabilities), "appGroup": s.AppGroup, "dryRun": s.DryRun,
	})
	if err != nil {
		return res, err
	}
	err = c.lane(ctx, "app_create", req, &res)
	res.DryRun = s.DryRun
	return res, err
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}
