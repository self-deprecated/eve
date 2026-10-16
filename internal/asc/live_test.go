package asc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/self-deprecated/eve/internal/config"
)

// Live tests make read-only GET calls to the real App Store Connect API. They run only with
// EVE_ASC_LIVE=1 and the key in ~/.appstoreconnect; `go test` skips them otherwise.
// EVE_ASC_LIVE_TESTER names a tester email to check; EVE_ASC_LIVE_DUMP=1 logs raw pages.
func liveClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("EVE_ASC_LIVE") != "1" {
		t.Skip("set EVE_ASC_LIVE=1 to call App Store Connect")
	}
	c, err := New(config.ASC{
		KeyID:    "J9584QW2TF",
		IssuerID: "25a2d76f-91fe-49e0-9551-09af06f209dc",
		KeyPath:  "~/.appstoreconnect/private_keys/AuthKey_J9584QW2TF.p8",
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLive(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if h, err := c.Ping(ctx); h != HealthOK {
		t.Fatalf("ping: %s %v", h, err)
	}
	for _, bundle := range []string{"ai.selfdeprecated.driver", "ai.selfdeprecated.lineage", "ai.selfdeprecated.screengate", "ai.selfdeprecated.bodydiff"} {
		app, err := c.AppByBundleID(ctx, bundle)
		if err != nil {
			t.Errorf("%s: %v", bundle, err)
			continue
		}
		t.Logf("%s: app %s %q", bundle, app.ID, app.Name)
		dump(t, c, ctx, "/v1/builds?filter%5Bapp%5D="+app.ID+"&include=preReleaseVersion,buildBetaDetail&sort=-uploadedDate&limit=2")
		dump(t, c, ctx, "/v1/apps/"+app.ID+"/buildUploads?limit=2&sort=-uploadedDate")
		dump(t, c, ctx, "/v1/betaGroups?filter%5Bapp%5D="+app.ID+"&limit=5")

		doc, err := c.get(ctx, "/v1/builds?filter%5Bapp%5D="+app.ID+"&include=preReleaseVersion&sort=-uploadedDate&limit=1")
		if err != nil {
			t.Fatal(err)
		}
		var version string
		for _, inc := range doc.Included {
			var a struct{ Version string }
			_ = json.Unmarshal(inc.Attributes, &a)
			version = a.Version
		}
		if version == "" {
			continue
		}
		num, ok, err := c.LatestBuildNumber(ctx, app.ID, version)
		t.Logf("  latest %s: %q %v %v", version, num, ok, err)
		if !ok {
			continue
		}
		b, err := c.FindBuild(ctx, app.ID, version, num)
		if err != nil || b == nil {
			t.Errorf("  find build: %v %v", b, err)
			continue
		}
		t.Logf("  build %+v detail %+v -> %+v", *b, b.BetaDetail, BuildVerdict(b))
		up, err := c.FindUploads(ctx, app.ID, version, num)
		t.Logf("  uploads %+v %v", up, err)
		r, err := c.Reconcile(ctx, app.ID, version, num)
		t.Logf("  reconcile %s %v", r.Status, err)
		if none, err := c.FindBuild(ctx, app.ID, version, "999999999999999"); none != nil || err != nil {
			t.Errorf("  missing build: %v %v", none, err)
		}
		if _, err := c.BetaGroupByName(ctx, app.ID, "no such group, eve live test"); !errors.Is(err, ErrNotFound) {
			t.Errorf("  missing group: %v", err)
		}
		if email := os.Getenv("EVE_ASC_LIVE_TESTER"); email != "" {
			inv, err := c.TesterInviteState(ctx, app.ID, email)
			t.Logf("  tester: %+v %v", inv, err)
			dump(t, c, ctx, "/v1/betaTesters?filter%5Bapps%5D="+app.ID+"&limit=5")
		}
	}
	if _, err := c.AppByBundleID(ctx, "ai.selfdeprecated"); !errors.Is(err, ErrNotFound) {
		t.Errorf("prefix bundle id: %v", err)
	}
}

func dump(t *testing.T, c *Client, ctx context.Context, path string) {
	t.Helper()
	if os.Getenv("EVE_ASC_LIVE_DUMP") != "1" {
		return
	}
	var raw json.RawMessage
	err := c.do(ctx, "GET", path, nil, &raw)
	t.Logf("GET %s: %v\n%s", path, err, raw)
}
