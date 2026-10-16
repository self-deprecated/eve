package asc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/api"
)

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func attrs(t *testing.T, upload map[string]any) json.RawMessage {
	return raw(t, upload["attributes"])
}

// Port: "upload records: nested and flat states, and stale retries" (uploadVerdict half).
func TestUploadVerdict(t *testing.T) {
	if s, r := uploadVerdict(nil); s != "" || r != nil {
		t.Errorf("nil record: %q %v", s, r)
	}
	failed := uploadJSON("FAILED", [2]string{"ITMS-90717", "Invalid large app icon"})
	if s, r := uploadVerdict(attrs(t, failed)); s != "FAILED" || !slices.Equal(r, []string{"ITMS-90717: Invalid large app icon"}) {
		t.Errorf("nested failed: %q %v", s, r)
	}
	if s, r := uploadVerdict(raw(t, map[string]any{"state": "COMPLETE"})); s != "COMPLETE" || len(r) != 0 {
		t.Errorf("flat complete: %q %v", s, r)
	}
	// Flat state with top-level errors, as early API versions reported them.
	flat := map[string]any{"state": "FAILED", "errors": []map[string]string{{"code": "ITMS-90683"}}}
	if s, r := uploadVerdict(raw(t, flat)); s != "FAILED" || !slices.Equal(r, []string{"ITMS-90683"}) {
		t.Errorf("flat failed: %q %v", s, r)
	}
}

// Port: "upload records: nested and flat states, and stale retries" (summarizeUploads half).
func TestSummarizeUploads(t *testing.T) {
	failed := attrs(t, uploadJSON("FAILED", [2]string{"ITMS-90717", "Invalid large app icon"}))
	processing := attrs(t, uploadJSON("PROCESSING"))
	awaiting := attrs(t, uploadJSON("AWAITING_UPLOAD"))
	complete := attrs(t, uploadJSON("COMPLETE"))
	cases := []struct {
		name    string
		records []json.RawMessage
		state   string
		reasons []string
	}{
		{"no records", nil, UploadNone, nil},
		{"stateless records", []json.RawMessage{raw(t, map[string]any{})}, UploadNone, nil},
		{"a live retry wins over a failed one", []json.RawMessage{failed, processing}, UploadProcessing, nil},
		{"complete wins over processing", []json.RawMessage{processing, complete, failed}, UploadComplete, nil},
		{"every record failed", []json.RawMessage{failed, failed}, UploadFailed, []string{"ITMS-90717: Invalid large app icon"}},
		{"failed and awaiting", []json.RawMessage{failed, awaiting}, UploadAwaiting, nil},
	}
	for _, tc := range cases {
		got := summarizeUploads(tc.records)
		if got.State != tc.state || !slices.Equal(got.Reasons, tc.reasons) {
			t.Errorf("%s: %+v, want %s %v", tc.name, got, tc.state, tc.reasons)
		}
	}
}

// Port: "classify waits, succeeds and stops on the right states", split across both verdicts.
func TestProcessingVerdict(t *testing.T) {
	cases := []struct {
		build *Build
		kind  Kind
		state string
	}{
		{nil, Wait, ""},
		{&Build{ProcessingState: "PROCESSING"}, Wait, "PROCESSING"},
		{&Build{ProcessingState: "VALID"}, Done, "VALID"},
		{&Build{ProcessingState: "INVALID"}, Rejected, "INVALID"},
		{&Build{ProcessingState: "FAILED"}, Rejected, "FAILED"},
		{&Build{ProcessingState: "QUANTUM"}, Unknown, "QUANTUM"},
	}
	for _, tc := range cases {
		v := ProcessingVerdict(tc.build)
		if v.Kind != tc.kind || v.State != tc.state || v.Summary == "" {
			t.Errorf("%+v: %+v, want %s %q", tc.build, v, tc.kind, tc.state)
		}
	}
}

func TestInternalVerdict(t *testing.T) {
	cases := []struct {
		state string
		kind  Kind
		human string
	}{
		{"READY_FOR_BETA_TESTING", Ready, ""},
		{"IN_BETA_TESTING", Ready, ""},
		{"MISSING_EXPORT_COMPLIANCE", NeedsHuman, api.HumanExportCompliance},
		{"IN_EXPORT_COMPLIANCE_REVIEW", Wait, ""},
		{"PROCESSING", Wait, ""},
		{"PROCESSING_EXCEPTION", Rejected, ""},
		{"EXPIRED", Failed, ""},
		{"", Wait, ""},
		{"IN_BETA_REVIEW", Unknown, ""},
	}
	for _, tc := range cases {
		v := InternalVerdict(&BetaDetail{InternalBuildState: tc.state})
		if v.Kind != tc.kind || v.Human != tc.human || v.State != tc.state {
			t.Errorf("%q: %+v, want %s %q", tc.state, v, tc.kind, tc.human)
		}
	}
	if v := InternalVerdict(nil); v.Kind != Wait {
		t.Errorf("missing detail: %+v", v)
	}
}

func TestBuildVerdict(t *testing.T) {
	valid := func(internal string) *Build {
		b := &Build{ProcessingState: "VALID"}
		if internal != "" {
			b.BetaDetail = &BetaDetail{InternalBuildState: internal}
		}
		return b
	}
	cases := []struct {
		build *Build
		kind  Kind
	}{
		{nil, Wait},
		{&Build{ProcessingState: "PROCESSING"}, Wait},
		{&Build{ProcessingState: "INVALID"}, Rejected},
		{&Build{ProcessingState: "FAILED"}, Rejected},
		{valid("IN_BETA_TESTING"), Ready},
		{valid("READY_FOR_BETA_TESTING"), Ready},
		{valid("MISSING_EXPORT_COMPLIANCE"), NeedsHuman},
		{valid("PROCESSING"), Wait},
		{valid("IN_EXPORT_COMPLIANCE_REVIEW"), Wait},
		{valid(""), Wait},
	}
	for _, tc := range cases {
		if v := BuildVerdict(tc.build); v.Kind != tc.kind {
			t.Errorf("%+v: %s, want %s", tc.build, v.Kind, tc.kind)
		}
	}
}

// Port: "an internal-only build that arrives store-eligible stops before distribution".
func TestCheckAudience(t *testing.T) {
	internal := &Build{Number: "42", ProcessingState: "VALID", BuildAudienceType: "INTERNAL_ONLY"}
	eligible := &Build{Number: "42", ProcessingState: "VALID", BuildAudienceType: "APP_STORE_ELIGIBLE"}
	if err := CheckAudience(internal, true); err != nil {
		t.Errorf("matching audience: %v", err)
	}
	err := CheckAudience(eligible, true)
	if !errors.Is(err, ErrAudienceMismatch) || !strings.Contains(err.Error(), "APP_STORE_ELIGIBLE") {
		t.Errorf("mismatch: %v", err)
	}
	if err := CheckAudience(eligible, false); err != nil {
		t.Errorf("store-eligible app: %v", err)
	}
	for _, b := range []*Build{nil, {ProcessingState: "PROCESSING", BuildAudienceType: "APP_STORE_ELIGIBLE"}, {ProcessingState: "VALID"}} {
		if err := CheckAudience(b, true); err != nil {
			t.Errorf("%+v: %v", b, err)
		}
	}
}

func TestCompareBuildNumbers(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2", "2", 0},
		{"9", "10", -1},
		{"202610161458", "42", 1},
		{"202610161458", "202610161459", -1},
		{"99999999999999999999999", "100000000000000000000000", -1}, // beyond uint64
		{"007", "7", 0},
		{"1.10", "1.9", 1},
		{"1.0", "1", 0},
		{"1.2.3", "1.2", 1},
		{"1a", "2", 1}, // non-numeric after numeric
	}
	for _, tc := range cases {
		if got := CompareBuildNumbers(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareBuildNumbers(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := CompareBuildNumbers(tc.b, tc.a); got != -tc.want {
			t.Errorf("CompareBuildNumbers(%q, %q) = %d, want %d", tc.b, tc.a, got, -tc.want)
		}
	}
}

func TestLatestBuildNumberNoBuilds(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, list()) })
	if num, ok, err := f.c.LatestBuildNumber(context.Background(), "app1", "9.9"); num != "" || ok || err != nil {
		t.Errorf("got %q %v %v", num, ok, err)
	}
}

// Port: "preflight resolves the app" (the app half).
func TestAppByBundleID(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, list(
			map[string]any{"type": "apps", "id": "app2", "attributes": map[string]any{"bundleId": "dev.bodydiff.app.widget", "name": "Widget"}},
			map[string]any{"type": "apps", "id": "app1", "attributes": map[string]any{"bundleId": "dev.bodydiff.app", "name": "BodyDiff"}},
		))
	})
	ctx := context.Background()
	app, err := f.c.AppByBundleID(ctx, "dev.bodydiff.app")
	if err != nil || app != (App{ID: "app1", BundleID: "dev.bodydiff.app", Name: "BodyDiff"}) {
		t.Fatalf("got %+v %v", app, err)
	}
	if !strings.Contains(f.calls[0].Query, "filter%5BbundleId%5D=dev.bodydiff.app") {
		t.Errorf("query %s", f.calls[0].Query)
	}
	if _, err := f.c.AppByBundleID(ctx, "dev.bodydiff"); !errors.Is(err, ErrNotFound) {
		t.Errorf("prefix match: %v", err)
	}
}

// Recorded response (Driver, 2026-10-16): the beta detail is matched through the build's
// relationship, not confused with the included preReleaseVersion.
func TestFindBuildRecorded(t *testing.T) {
	body := fixture(t, "builds_valid.json")
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, body) })
	b, err := f.c.FindBuild(context.Background(), "6823618186", "0.1", "202610161458")
	if err != nil || b == nil {
		t.Fatalf("got %v %v", b, err)
	}
	if b.ID != "5552ea26-dc5e-4b82-bf21-d8e79ca644b3" || b.Number != "202610161458" || b.ProcessingState != "VALID" ||
		b.BuildAudienceType != "APP_STORE_ELIGIBLE" || b.UsesNonExemptEncryption == nil || *b.UsesNonExemptEncryption ||
		b.UploadedDate.IsZero() || b.ExpirationDate.Before(b.UploadedDate) {
		t.Errorf("build %+v", b)
	}
	if b.BetaDetail == nil || b.BetaDetail.InternalBuildState != "IN_BETA_TESTING" || b.BetaDetail.ExternalBuildState != "READY_FOR_BETA_SUBMISSION" {
		t.Errorf("detail %+v", b.BetaDetail)
	}
	if f.count("", "/v1/builds/"+b.ID+"/buildBetaDetail") != 0 {
		t.Error("fetched the detail although it was included")
	}
}

// Port of the query assertions in "finish waits through processing and a full outage".
func TestFindBuildQuery(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, list()) })
	b, err := f.c.FindBuild(context.Background(), "app1", "0.1.0", "42")
	if b != nil || err != nil {
		t.Fatalf("no build yet: %v %v", b, err)
	}
	q := f.calls[0].Query
	for _, want := range []string{
		"filter%5Bversion%5D=42",
		"filter%5BpreReleaseVersion.version%5D=0.1.0",
		"filter%5BpreReleaseVersion.platform%5D=IOS",
		"filter%5BprocessingState%5D=PROCESSING%2CFAILED%2CINVALID%2CVALID",
		"include=buildBetaDetail",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query %s lacks %s", q, want)
		}
	}
}

// Port: "finish fetches the beta detail when the build listing omits it".
func TestFindBuildFetchesDetailWhenOmitted(t *testing.T) {
	state := "VALID"
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/builds":
			reply(w, 200, list(buildJSON("b1", "42", state, "INTERNAL_ONLY")))
		case "/v1/builds/b1/buildBetaDetail":
			reply(w, 200, map[string]any{"data": detailJSON("d1", "IN_BETA_TESTING")})
		default:
			reply(w, 404, appleError("NOT_FOUND", ""))
		}
	})
	b, err := f.c.FindBuild(context.Background(), "app1", "0.1.0", "42")
	if err != nil || b.BetaDetail == nil || b.BetaDetail.InternalBuildState != "IN_BETA_TESTING" {
		t.Fatalf("got %+v %v", b, err)
	}
	if BuildVerdict(b).Kind != Ready {
		t.Errorf("verdict %+v", BuildVerdict(b))
	}
	// While processing, the detail is not worth a second request.
	state = "PROCESSING"
	before := f.count("", "/v1/builds/b1/buildBetaDetail")
	if b, err := f.c.FindBuild(context.Background(), "app1", "0.1.0", "42"); err != nil || b.BetaDetail != nil {
		t.Fatalf("processing: %+v %v", b, err)
	}
	if f.count("", "/v1/builds/b1/buildBetaDetail") != before {
		t.Error("fetched the detail for a processing build")
	}
}

func TestFindBuildIgnoresOtherNumbers(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, list(buildJSON("b9", "420", "VALID", "")))
	})
	if b, err := f.c.FindBuild(context.Background(), "app1", "0.1.0", "42"); b != nil || err != nil {
		t.Errorf("got %+v %v", b, err)
	}
}

func TestFindUploadsRecorded(t *testing.T) {
	body := fixture(t, "build_uploads.json")
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, body) })
	got, err := f.c.FindUploads(context.Background(), "6823618186", "0.1", "202610161458")
	if err != nil || got.State != UploadComplete || got.Records != 2 {
		t.Fatalf("got %+v %v", got, err)
	}
	q := f.calls[0].Query
	if f.calls[0].Path != "/v1/apps/6823618186/buildUploads" ||
		!strings.Contains(q, "filter%5BcfBundleVersion%5D=202610161458") ||
		!strings.Contains(q, "filter%5BcfBundleShortVersionString%5D=0.1") {
		t.Errorf("request %s?%s", f.calls[0].Path, q)
	}
}

// reconcileFake serves builds and buildUploads; nextBuild returns the build for each poll
// (nil for none, "outage" for a 503 through every retry).
func reconcileFake(t *testing.T, nextBuild func() any, uploads any) *fake {
	return newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/builds":
			switch b := nextBuild().(type) {
			case nil:
				reply(w, 200, list())
			case string:
				reply(w, 503, appleError("SERVICE_UNAVAILABLE", b))
			case map[string]any:
				reply(w, 200, map[string]any{"data": []any{b}, "included": []any{detailJSON("d1", "PROCESSING")}})
			}
		case "/v1/apps/app1/buildUploads":
			switch u := uploads.(type) {
			case int:
				reply(w, u, appleError("FORBIDDEN_ERROR", "forbidden"))
			case string:
				reply(w, 403, u)
			default:
				reply(w, 200, u)
			}
		default:
			reply(w, 404, appleError("NOT_FOUND", ""))
		}
	})
}

func always(v any) func() any { return func() any { return v } }

// Ports: "finish reports a failed upload with Apple's reasons while no build exists" and "a
// stale failed upload record does not abort a build that is processing".
func TestReconcile(t *testing.T) {
	failed := uploadJSON("FAILED", [2]string{"ITMS-90683", "Missing purpose string"})
	stale := uploadJSON("FAILED", [2]string{"NETWORK", "interrupted"})
	processing := buildJSON("b1", "42", "PROCESSING", "INTERNAL_ONLY")
	cases := []struct {
		name       string
		build      any
		uploads    any
		status     ReconcileStatus
		reasons    []string
		unreadable bool
	}{
		{"build found", processing, list(), Found, nil, false},
		{"build decides over failed uploads", processing, list(stale), Found, nil, false},
		{"live retry over a stale failure", nil, list(stale, uploadJSON("PROCESSING")), UploadLive, nil, false},
		{"complete upload", nil, list(uploadJSON("COMPLETE")), UploadLive, nil, false},
		{"every upload failed", nil, list(failed, failed), AllFailed, []string{"ITMS-90683: Missing purpose string"}, false},
		{"nothing yet", nil, list(), NotVisible, nil, false},
		{"only awaiting upload", nil, list(uploadJSON("AWAITING_UPLOAD")), NotVisible, nil, false},
		{"uploads refused by role", nil, 403, NotVisible, nil, true},
		{"uploads endpoint missing", nil, 404, NotVisible, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := reconcileFake(t, always(tc.build), tc.uploads)
			r, err := f.c.Reconcile(context.Background(), "app1", "0.1.0", "42")
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != tc.status || !slices.Equal(r.Reasons, tc.reasons) || r.UploadsUnreadable != tc.unreadable {
				t.Errorf("got %+v", r)
			}
			if (r.Status == Found) != (r.Build != nil) {
				t.Errorf("build %+v with status %s", r.Build, r.Status)
			}
			if tc.status == Found && f.count("", "/v1/apps/app1/buildUploads") != 0 {
				t.Error("consulted uploads although the build exists")
			}
		})
	}

	// An agreement lapse is a human action, never "not visible".
	f := reconcileFake(t, always(nil), fixture(t, "error_agreement.json"))
	if _, err := f.c.Reconcile(context.Background(), "app1", "0.1.0", "42"); !IsAgreement(err) {
		t.Errorf("agreement: %v", err)
	}
}

// Ports: "finish stops when an upload never shows up" and the outage riding of "finish waits
// through processing and a full outage"; plus visibility lag (PLAN.md §15).
func TestReconcileUntil(t *testing.T) {
	ctx := context.Background()
	seq := func(items ...any) func() any {
		var i atomic.Int32
		return func() any {
			n := int(i.Add(1)) - 1
			return items[min(n, len(items)-1)]
		}
	}
	processing := buildJSON("b1", "42", "PROCESSING", "INTERNAL_ONLY")

	t.Run("visibility lag", func(t *testing.T) {
		f := reconcileFake(t, seq(nil, nil, processing), list())
		r, err := f.c.ReconcileUntil(ctx, "app1", "0.1.0", "42", 10*time.Minute, 30*time.Second)
		if err != nil || r.Status != Found {
			t.Fatalf("got %+v %v", r, err)
		}
		if !slices.Equal(f.sleeps, []time.Duration{30 * time.Second, 30 * time.Second}) {
			t.Errorf("sleeps %v", f.sleeps)
		}
	})
	t.Run("never shows up", func(t *testing.T) {
		f := reconcileFake(t, always(nil), list(uploadJSON("AWAITING_UPLOAD")))
		r, err := f.c.ReconcileUntil(ctx, "app1", "0.1.0", "42", 10*time.Minute, 4*time.Minute)
		if err != nil || r.Status != NotVisible {
			t.Fatalf("got %+v %v", r, err)
		}
		// Polls at 0, 4, 8 and once more at the window's end.
		if !slices.Equal(f.sleeps, []time.Duration{4 * time.Minute, 4 * time.Minute, 2 * time.Minute}) {
			t.Errorf("sleeps %v", f.sleeps)
		}
	})
	t.Run("zero window checks once", func(t *testing.T) {
		f := reconcileFake(t, always(nil), list())
		r, err := f.c.ReconcileUntil(ctx, "app1", "0.1.0", "42", 0, time.Second)
		if err != nil || r.Status != NotVisible || f.count("", "/v1/builds") != 1 {
			t.Errorf("got %+v %v after %d polls", r, err, f.count("", "/v1/builds"))
		}
	})
	t.Run("rides out an outage", func(t *testing.T) {
		f := reconcileFake(t, seq("outage", "outage", "outage", "outage", processing), list())
		r, err := f.c.ReconcileUntil(ctx, "app1", "0.1.0", "42", 10*time.Minute, 30*time.Second)
		if err != nil || r.Status != Found {
			t.Fatalf("got %+v %v", r, err)
		}
	})
	t.Run("outage to the end", func(t *testing.T) {
		f := reconcileFake(t, always("outage"), list())
		if _, err := f.c.ReconcileUntil(ctx, "app1", "0.1.0", "42", time.Minute, 30*time.Second); !errors.Is(err, ErrUnavailable) {
			t.Errorf("got %v", err)
		}
	})
	t.Run("all failed stops at once", func(t *testing.T) {
		f := reconcileFake(t, always(nil), list(uploadJSON("FAILED", [2]string{"ITMS-90189", "Redundant Binary Upload"})))
		r, err := f.c.ReconcileUntil(ctx, "app1", "0.1.0", "42", 10*time.Minute, 30*time.Second)
		if err != nil || r.Status != AllFailed || len(f.sleeps) != 0 {
			t.Errorf("got %+v %v sleeps %v", r, err, f.sleeps)
		}
	})
}
