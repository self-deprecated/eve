package asc

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

// Port: "preflight resolves the app and refuses external groups" (the group half), against the
// recorded Driver group.
func TestBetaGroupByName(t *testing.T) {
	recorded := fixture(t, "beta_groups.json")
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("filter[name]") {
		case "Driver":
			reply(w, 200, recorded)
		case "Friends":
			reply(w, 200, list(map[string]any{"type": "betaGroups", "id": "g2", "attributes": map[string]any{"name": "Friends", "isInternalGroup": false}}))
		default:
			reply(w, 200, list(map[string]any{"type": "betaGroups", "id": "g3", "attributes": map[string]any{"name": "Driver Beta", "isInternalGroup": true}}))
		}
	})
	ctx := context.Background()
	g, err := f.c.BetaGroupByName(ctx, "6823618186", "Driver")
	if err != nil {
		t.Fatal(err)
	}
	if g.ID != "f3312b11-08f6-47a6-9cc0-3a38fc1566df" || !g.Internal || !g.AutoDistribution || g.FeedbackEnabled == nil || !*g.FeedbackEnabled {
		t.Errorf("group %+v", g)
	}
	if !strings.Contains(f.calls[0].Query, "filter%5Bapp%5D=6823618186") {
		t.Errorf("query %s", f.calls[0].Query)
	}
	g, err = f.c.BetaGroupByName(ctx, "app1", "Friends")
	if !errors.Is(err, ErrExternalGroup) || g.ID != "g2" {
		t.Errorf("external: %+v %v", g, err)
	}
	if _, err := f.c.BetaGroupByName(ctx, "app1", "Driv"); !errors.Is(err, ErrNotFound) {
		t.Errorf("inexact name: %v", err)
	}
}

// Port: "adding to the group retries through an outage", plus idempotency.
func TestAddBuildToGroup(t *testing.T) {
	ctx := context.Background()
	groupPath := "/v1/betaGroups/g1/relationships/builds"
	withFailures := func(failures int32, final int) (*fake, *atomic.Int32) {
		var n atomic.Int32
		f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			if n.Add(1) <= failures {
				reply(w, 503, appleError("SERVICE_UNAVAILABLE", ""))
				return
			}
			if final == 409 {
				reply(w, 409, appleError("ENTITY_ERROR.RELATIONSHIP.INVALID", "The build is already in the group"))
				return
			}
			reply(w, final, nil)
		})
		return f, &n
	}

	f, n := withFailures(3, 204)
	if err := f.c.AddBuildToGroup(ctx, "g1", "b1"); err != nil {
		t.Fatal(err)
	}
	c, _ := f.find("POST", groupPath)
	data, _ := c.Body["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != "b1" || data[0].(map[string]any)["type"] != "builds" {
		t.Errorf("body %v", c.Body)
	}
	if n.Load() != 4 {
		t.Errorf("attempts %d, want 4", n.Load())
	}

	// A retried POST meeting its own earlier success, or a build already in the group.
	f, _ = withFailures(1, 409)
	if err := f.c.AddBuildToGroup(ctx, "g1", "b1"); err != nil {
		t.Errorf("already added: %v", err)
	}

	f, _ = withFailures(1_000, 204)
	if err := f.c.AddBuildToGroup(ctx, "g1", "b1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("stuck: %v", err)
	}
}

// notesFake serves betaBuildLocalizations; write is the status for PATCH/POST (0 = success).
func notesFake(t *testing.T, existing []map[string]any, write int) *fake {
	return newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/builds/b1/betaBuildLocalizations":
			reply(w, 200, list(existing...))
		case strings.HasPrefix(r.URL.Path, "/v1/betaBuildLocalizations"):
			switch write {
			case 0:
				status := 200
				if r.Method == "POST" {
					status = 201
				}
				reply(w, status, map[string]any{"data": map[string]any{}})
			default:
				reply(w, write, appleError("FORBIDDEN_ERROR", "This request is forbidden for security reasons"))
			}
		default:
			reply(w, 404, appleError("NOT_FOUND", ""))
		}
	})
}

func loc(id, locale string) map[string]any {
	return map[string]any{"type": "betaBuildLocalizations", "id": id, "attributes": map[string]any{"locale": locale}}
}

// Port of the notes half of "finish waits through processing and a full outage".
func TestSetWhatToTestCreatesLocalization(t *testing.T) {
	f := notesFake(t, nil, 0)
	locale, err := f.c.SetWhatToTest(context.Background(), "b1", "Try the side view.")
	if err != nil || locale != "en-US" {
		t.Fatalf("got %q %v", locale, err)
	}
	c, ok := f.find("POST", "/v1/betaBuildLocalizations")
	if !ok {
		t.Fatal("no POST")
	}
	data := c.Body["data"].(map[string]any)
	a := data["attributes"].(map[string]any)
	rel := data["relationships"].(map[string]any)["build"].(map[string]any)["data"].(map[string]any)
	if a["whatsNew"] != "Try the side view." || a["locale"] != "en-US" || rel["id"] != "b1" {
		t.Errorf("body %v", c.Body)
	}
}

// Port: "finish updates existing notes instead of creating a second localization".
func TestSetWhatToTestUpdatesExisting(t *testing.T) {
	f := notesFake(t, []map[string]any{loc("l0", "de-DE"), loc("l1", "en-US")}, 0)
	locale, err := f.c.SetWhatToTest(context.Background(), "b1", "x")
	if err != nil || locale != "en-US" {
		t.Fatalf("got %q %v", locale, err)
	}
	c, ok := f.find("PATCH", "/v1/betaBuildLocalizations/l1")
	if !ok || c.Body["data"].(map[string]any)["attributes"].(map[string]any)["whatsNew"] != "x" {
		t.Errorf("patch %+v", c)
	}
	if f.count("POST", "/v1/betaBuildLocalizations") != 0 {
		t.Error("created a second localization")
	}

	// Without en-US, the first existing localization is updated.
	g := notesFake(t, []map[string]any{loc("l0", "de-DE")}, 0)
	if locale, err := g.c.SetWhatToTest(context.Background(), "b1", "x"); err != nil || locale != "de-DE" {
		t.Errorf("got %q %v", locale, err)
	}
	if _, ok := g.find("PATCH", "/v1/betaBuildLocalizations/l0"); !ok {
		t.Error("did not patch de-DE")
	}
}

func TestSetWhatToTestTruncates(t *testing.T) {
	f := notesFake(t, nil, 0)
	long := strings.Repeat("é", WhatToTestLimit+50)
	if _, err := f.c.SetWhatToTest(context.Background(), "b1", long); err != nil {
		t.Fatal(err)
	}
	c, _ := f.find("POST", "/v1/betaBuildLocalizations")
	got := c.Body["data"].(map[string]any)["attributes"].(map[string]any)["whatsNew"].(string)
	if utf8.RuneCountInString(got) != WhatToTestLimit || !utf8.ValidString(got) {
		t.Errorf("truncated to %d runes", utf8.RuneCountInString(got))
	}
}

// Port: "notes never stop the run: a refused role and an outage both become warnings". The
// caller makes them notes; the client keeps the two distinguishable.
func TestSetWhatToTestErrors(t *testing.T) {
	_, err := notesFake(t, nil, 403).c.SetWhatToTest(context.Background(), "b1", "x")
	if !errors.Is(err, ErrWhatToTestRefused) || !errors.Is(err, ErrForbidden) {
		t.Errorf("403: %v", err)
	}
	_, err = notesFake(t, nil, 503).c.SetWhatToTest(context.Background(), "b1", "x")
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrWhatToTestRefused) {
		t.Errorf("503: %v", err)
	}
}

func TestSetWhatToTestConflictUpdates(t *testing.T) {
	var lists atomic.Int32
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/builds/b1/betaBuildLocalizations":
			if lists.Add(1) == 1 {
				reply(w, 200, list())
			} else {
				reply(w, 200, list(loc("l1", "en-US")))
			}
		case r.Method == "POST":
			reply(w, 409, appleError("ENTITY_ERROR.ATTRIBUTE.INVALID.DUPLICATE", "duplicate locale"))
		default:
			reply(w, 200, map[string]any{"data": map[string]any{}})
		}
	})
	if locale, err := f.c.SetWhatToTest(context.Background(), "b1", "x"); err != nil || locale != "en-US" {
		t.Fatalf("got %q %v", locale, err)
	}
	if _, ok := f.find("PATCH", "/v1/betaBuildLocalizations/l1"); !ok {
		t.Error("did not update after the conflict")
	}
}

func TestTesterInviteState(t *testing.T) {
	recorded := fixture(t, "beta_testers.json")
	tester := func(state string) map[string]any {
		return map[string]any{"type": "betaTesters", "id": "t-" + state, "attributes": map[string]any{"email": "me@example.com", "state": state}}
	}
	cases := []struct {
		name  string
		body  any
		state InviteState
		apple string
	}{
		{"recorded installed", recorded, InviteAccepted, "INSTALLED"},
		{"accepted", list(tester("ACCEPTED")), InviteAccepted, "ACCEPTED"},
		{"invited", list(tester("INVITED")), InvitePending, "INVITED"},
		{"not invited yet", list(tester("NOT_INVITED")), InvitePending, "NOT_INVITED"},
		{"revoked", list(tester("REVOKED")), NotATester, "REVOKED"},
		{"absent", list(), NotATester, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, tc.body) })
			inv, err := f.c.TesterInviteState(context.Background(), "app1", "Me@Example.com")
			if err != nil || inv.State != tc.state || inv.AppleState != tc.apple {
				t.Errorf("got %+v %v", inv, err)
			}
			q := f.calls[0].Query
			if f.calls[0].Path != "/v1/betaTesters" || !strings.Contains(q, "filter%5Bapps%5D=app1") || !strings.Contains(q, "filter%5Bemail%5D=Me%40Example.com") {
				t.Errorf("request %s?%s", f.calls[0].Path, q)
			}
		})
	}
}

func TestPing(t *testing.T) {
	agreement := fixture(t, "error_agreement.json")
	cases := []struct {
		status int
		body   any
		want   Health
	}{
		{200, list(), HealthOK},
		{401, appleError("NOT_AUTHORIZED", ""), HealthAuth},
		{403, agreement, HealthAgreement},
		{403, appleError("FORBIDDEN_ERROR", ""), HealthForbidden},
		{503, nil, HealthUnavailable},
		{400, appleError("PARAMETER_ERROR.INVALID", ""), HealthError},
	}
	for _, tc := range cases {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request) { reply(w, tc.status, tc.body) })
		h, err := f.c.Ping(context.Background())
		if h != tc.want || (err == nil) != (tc.want == HealthOK) {
			t.Errorf("status %d: %s %v, want %s", tc.status, h, err, tc.want)
		}
		if f.calls[0].Path != "/v1/apps" || !strings.Contains(f.calls[0].Query, "limit=1") {
			t.Errorf("request %s?%s", f.calls[0].Path, f.calls[0].Query)
		}
	}
}
