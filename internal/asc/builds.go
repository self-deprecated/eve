package asc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// allProcessingStates names every build processing state in filters: the default listing is
// not documented to include all of them.
const allProcessingStates = "PROCESSING,FAILED,INVALID,VALID"

// App is an App Store Connect app record.
type App struct {
	ID       string
	BundleID string
	Name     string
}

// Build is an App Store Connect build with its TestFlight beta detail when known.
type Build struct {
	ID string
	// Number is the build number (CFBundleVersion; Apple's attribute "version").
	Number                  string
	ProcessingState         string // PROCESSING, FAILED, INVALID, VALID
	BuildAudienceType       string // INTERNAL_ONLY, APP_STORE_ELIGIBLE
	UsesNonExemptEncryption *bool
	Expired                 bool
	ExpirationDate          time.Time
	UploadedDate            time.Time
	// BetaDetail is nil while App Store Connect does not show it yet.
	BetaDetail *BetaDetail
}

// BetaDetail is a build's buildBetaDetail resource.
type BetaDetail struct {
	ID                 string
	InternalBuildState string
	ExternalBuildState string
}

type buildAttributes struct {
	Version                 string    `json:"version"`
	ProcessingState         string    `json:"processingState"`
	BuildAudienceType       string    `json:"buildAudienceType"`
	UsesNonExemptEncryption *bool     `json:"usesNonExemptEncryption"`
	Expired                 bool      `json:"expired"`
	ExpirationDate          time.Time `json:"expirationDate"`
	UploadedDate            time.Time `json:"uploadedDate"`
}

func decodeBuild(r resource) (Build, error) {
	var a buildAttributes
	if err := json.Unmarshal(r.Attributes, &a); err != nil {
		return Build{}, fmt.Errorf("asc: decode build %s: %w", r.ID, err)
	}
	return Build{
		ID:                      r.ID,
		Number:                  a.Version,
		ProcessingState:         a.ProcessingState,
		BuildAudienceType:       a.BuildAudienceType,
		UsesNonExemptEncryption: a.UsesNonExemptEncryption,
		Expired:                 a.Expired,
		ExpirationDate:          a.ExpirationDate,
		UploadedDate:            a.UploadedDate,
	}, nil
}

func decodeBetaDetail(r resource) (*BetaDetail, error) {
	var a struct {
		InternalBuildState string `json:"internalBuildState"`
		ExternalBuildState string `json:"externalBuildState"`
	}
	if err := json.Unmarshal(r.Attributes, &a); err != nil {
		return nil, fmt.Errorf("asc: decode buildBetaDetail %s: %w", r.ID, err)
	}
	return &BetaDetail{ID: r.ID, InternalBuildState: a.InternalBuildState, ExternalBuildState: a.ExternalBuildState}, nil
}

// AppByBundleID finds the app record for a bundle id. A missing record wraps ErrNotFound
// (needs_human create_app_record).
func (c *Client) AppByBundleID(ctx context.Context, bundleID string) (App, error) {
	q := url.Values{
		"filter[bundleId]": {bundleID},
		"fields[apps]":     {"bundleId,name"},
		"limit":            {"20"},
	}
	doc, err := c.get(ctx, "/v1/apps?"+q.Encode())
	if err != nil {
		return App{}, err
	}
	apps, err := doc.list()
	if err != nil {
		return App{}, err
	}
	// The filter matches prefixes too (a.b also returns a.b.widget); insist on equality.
	for _, r := range apps {
		var a struct {
			BundleID string `json:"bundleId"`
			Name     string `json:"name"`
		}
		if json.Unmarshal(r.Attributes, &a) == nil && a.BundleID == bundleID {
			return App{ID: r.ID, BundleID: a.BundleID, Name: a.Name}, nil
		}
	}
	return App{}, fmt.Errorf("%w: App Store Connect has no app record for %s; create it once in App Store Connect", ErrNotFound, bundleID)
}

// LatestBuildNumber returns the highest build number App Store Connect knows for this app,
// platform and marketing version, for the build-number allocator's floor (PLAN.md §6.3
// step 1). Build numbers compare numerically ("9" < "10" < "202610161458"); ok is false when
// the version has no builds.
// An empty version considers every version of the app.
func (c *Client) LatestBuildNumber(ctx context.Context, appID, version string) (string, bool, error) {
	q := url.Values{
		"filter[app]":                        {appID},
		"filter[preReleaseVersion.version]":  {version},
		"filter[preReleaseVersion.platform]": {Platform},
		"filter[processingState]":            {allProcessingStates},
		"fields[builds]":                     {"version"},
		"limit":                              {"200"},
	}
	if version == "" {
		// Any version: eve's build numbers are monotonic across versions (PLAN.md §6.3).
		q.Del("filter[preReleaseVersion.version]")
	}
	var best string
	err := c.getAll(ctx, "/v1/builds?"+q.Encode(), func(doc *document) (bool, error) {
		builds, err := doc.list()
		if err != nil {
			return false, err
		}
		for _, r := range builds {
			var a struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(r.Attributes, &a) == nil && a.Version != "" &&
				(best == "" || CompareBuildNumbers(a.Version, best) > 0) {
				best = a.Version
			}
		}
		return false, nil
	})
	if err != nil {
		return "", false, err
	}
	return best, best != "", nil
}

// CompareBuildNumbers compares two CFBundleVersion strings numerically, component by
// component ("1.10" > "1.9", "202610161458" > "42"). It returns -1, 0 or 1. Non-numeric
// components compare as strings after numeric ones.
func CompareBuildNumbers(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(pa), len(pb)) {
		x, y := "0", "0"
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if c := compareNumeric(x, y); c != 0 {
			return c
		}
	}
	return 0
}

// compareNumeric compares decimal strings of any length without overflow.
func compareNumeric(x, y string) int {
	isNum := func(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }
	nx, ny := isNum(x), isNum(y)
	switch {
	case nx && !ny:
		return -1
	case !nx && ny:
		return 1
	case !nx && !ny:
		return strings.Compare(x, y)
	}
	x, y = strings.TrimLeft(x, "0"), strings.TrimLeft(y, "0")
	if len(x) != len(y) {
		if len(x) < len(y) {
			return -1
		}
		return 1
	}
	return strings.Compare(x, y)
}

// FindBuild returns the build for this app, version and number with its beta detail, or nil
// while App Store Connect does not show it. The detail comes from include=buildBetaDetail and,
// when the listing omits it for a VALID build, from the relationship endpoint.
func (c *Client) FindBuild(ctx context.Context, appID, version, number string) (*Build, error) {
	q := url.Values{
		"filter[app]":                        {appID},
		"filter[version]":                    {number},
		"filter[preReleaseVersion.version]":  {version},
		"filter[preReleaseVersion.platform]": {Platform},
		"filter[processingState]":            {allProcessingStates},
		"include":                            {"buildBetaDetail"},
		"limit":                              {"10"},
	}
	doc, err := c.get(ctx, "/v1/builds?"+q.Encode())
	if err != nil {
		return nil, err
	}
	builds, err := doc.list()
	if err != nil {
		return nil, err
	}
	var found *resource
	for i := range builds {
		var a struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(builds[i].Attributes, &a) == nil && a.Version == number {
			found = &builds[i]
			break
		}
	}
	if found == nil {
		return nil, nil
	}
	b, err := decodeBuild(*found)
	if err != nil {
		return nil, err
	}
	_, refID, hasRef := found.ref("buildBetaDetail")
	for _, inc := range doc.Included {
		if inc.Type == "buildBetaDetails" && (!hasRef || inc.ID == refID) {
			if b.BetaDetail, err = decodeBetaDetail(inc); err != nil {
				return nil, err
			}
			break
		}
	}
	if b.BetaDetail == nil && b.ProcessingState == "VALID" {
		d, err := c.get(ctx, "/v1/builds/"+url.PathEscape(b.ID)+"/buildBetaDetail")
		if err != nil {
			return nil, err
		}
		r, err := d.one()
		if err != nil {
			return nil, err
		}
		if r != nil {
			if b.BetaDetail, err = decodeBetaDetail(*r); err != nil {
				return nil, err
			}
		}
	}
	return &b, nil
}

// Upload states, from buildUploads plus the summary's "none seen".
const (
	UploadNone       = ""
	UploadAwaiting   = "AWAITING_UPLOAD"
	UploadProcessing = "PROCESSING"
	UploadComplete   = "COMPLETE"
	UploadFailed     = "FAILED"
)

// UploadSummary condenses every upload record for one version and build number.
type UploadSummary struct {
	// State is UploadComplete or UploadProcessing when any record is live, UploadFailed only
	// when every record failed, UploadAwaiting otherwise, and UploadNone with no records.
	State   string
	Reasons []string // Apple's errors when State is UploadFailed, deduplicated
	Records int
}

// uploadRecord is a buildUploads resource's attributes. The state is an object
// {state, errors[]} in current API versions and was a plain string before; both decode.
type uploadRecord struct {
	State  json.RawMessage `json:"state"`
	Errors []uploadError   `json:"errors"`
}

type uploadError struct {
	Code        string `json:"code"`
	Description string `json:"description"`
}

// uploadVerdict extracts one record's state and Apple's reasons.
func uploadVerdict(attrs json.RawMessage) (state string, reasons []string) {
	var rec uploadRecord
	if json.Unmarshal(attrs, &rec) != nil {
		return "", nil
	}
	errs := rec.Errors
	if json.Unmarshal(rec.State, &state) != nil {
		var nested struct {
			State  string        `json:"state"`
			Errors []uploadError `json:"errors"`
		}
		if json.Unmarshal(rec.State, &nested) == nil {
			state = nested.State
			if len(nested.Errors) > 0 {
				errs = nested.Errors
			}
		}
	}
	for _, e := range errs {
		var parts []string
		for _, s := range []string{e.Code, e.Description} {
			if s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			reasons = append(reasons, strings.Join(parts, ": "))
		}
	}
	return state, reasons
}

// summarizeUploads condenses upload records. altool and Xcode retries reuse the build number,
// so one build can have several records: any live record wins, and FAILED counts only when
// every record failed.
func summarizeUploads(records []json.RawMessage) UploadSummary {
	type verdict struct {
		state   string
		reasons []string
	}
	var vs []verdict
	for _, r := range records {
		if s, reasons := uploadVerdict(r); s != "" {
			vs = append(vs, verdict{s, reasons})
		}
	}
	sum := UploadSummary{Records: len(vs)}
	if len(vs) == 0 {
		return sum
	}
	for _, want := range []string{UploadComplete, UploadProcessing} {
		for _, v := range vs {
			if v.state == want {
				sum.State = want
				return sum
			}
		}
	}
	for _, v := range vs {
		if v.state != UploadFailed {
			sum.State = UploadAwaiting
			return sum
		}
	}
	sum.State = UploadFailed
	for _, v := range vs {
		for _, r := range v.reasons {
			if !slices.Contains(sum.Reasons, r) {
				sum.Reasons = append(sum.Reasons, r)
			}
		}
	}
	return sum
}

// FindUploads summarizes the buildUploads records for this app, version and number. Upload
// records (API 4.1+) carry machine-readable errors the build record lacks.
func (c *Client) FindUploads(ctx context.Context, appID, version, number string) (UploadSummary, error) {
	q := url.Values{
		"filter[cfBundleVersion]":            {number},
		"filter[cfBundleShortVersionString]": {version},
		"filter[platform]":                   {Platform},
		"limit":                              {"20"},
	}
	var attrs []json.RawMessage
	err := c.getAll(ctx, "/v1/apps/"+url.PathEscape(appID)+"/buildUploads?"+q.Encode(), func(doc *document) (bool, error) {
		recs, err := doc.list()
		for _, r := range recs {
			attrs = append(attrs, r.Attributes)
		}
		return false, err
	})
	if err != nil {
		return UploadSummary{}, err
	}
	return summarizeUploads(attrs), nil
}
