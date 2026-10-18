package asc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// What to Test limits: Apple accepts 4000 characters per localization; eve writes en-US
// unless the build already has another localization.
const (
	WhatToTestLimit  = 4000
	WhatToTestLocale = "en-US"
)

// BetaGroup is a TestFlight group.
type BetaGroup struct {
	ID       string
	Name     string
	Internal bool
	// AutoDistribution is set when the group receives every build automatically
	// (hasAccessToAllBuilds); adding the build explicitly is then unnecessary.
	AutoDistribution bool
	FeedbackEnabled  *bool
}

// BetaGroupByName finds the app's TestFlight group with exactly this name. A missing group
// wraps ErrNotFound. An external group is returned together with an error wrapping
// ErrExternalGroup: eve only distributes to internal groups.
func (c *Client) BetaGroupByName(ctx context.Context, appID, name string) (BetaGroup, error) {
	q := url.Values{
		"filter[app]":  {appID},
		"filter[name]": {name},
		"limit":        {"50"},
	}
	var group *BetaGroup
	err := c.getAll(ctx, "/v1/betaGroups?"+q.Encode(), func(doc *document) (bool, error) {
		groups, err := doc.list()
		if err != nil {
			return false, err
		}
		for _, r := range groups {
			var a struct {
				Name                 string `json:"name"`
				IsInternalGroup      bool   `json:"isInternalGroup"`
				HasAccessToAllBuilds bool   `json:"hasAccessToAllBuilds"`
				FeedbackEnabled      *bool  `json:"feedbackEnabled"`
			}
			// filter[name] is not documented as exact; insist on equality.
			if json.Unmarshal(r.Attributes, &a) == nil && a.Name == name {
				group = &BetaGroup{
					ID:               r.ID,
					Name:             a.Name,
					Internal:         a.IsInternalGroup,
					AutoDistribution: a.HasAccessToAllBuilds,
					FeedbackEnabled:  a.FeedbackEnabled,
				}
				return true, nil
			}
		}
		return false, nil
	})
	switch {
	case err != nil:
		return BetaGroup{}, err
	case group == nil:
		return BetaGroup{}, fmt.Errorf("%w: the app has no TestFlight group named %q", ErrNotFound, name)
	case !group.Internal:
		return *group, fmt.Errorf("%w: TestFlight group %q is external; eve only distributes to internal groups", ErrExternalGroup, name)
	}
	return *group, nil
}

type linkage struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// AddBuildToGroup adds a build to a TestFlight group. It is idempotent: a 409 (already in
// the group, the group receives every build automatically, or a retried POST meeting its own
// earlier success) counts as success.
func (c *Client) AddBuildToGroup(ctx context.Context, groupID, buildID string) error {
	body := map[string]any{"data": []linkage{{Type: "builds", ID: buildID}}}
	err := c.do(ctx, "POST", "/v1/betaGroups/"+url.PathEscape(groupID)+"/relationships/builds", body, nil)
	if errors.Is(err, ErrConflict) {
		return nil
	}
	return err
}

// SetWhatToTest writes a build's What to Test text, truncated to WhatToTestLimit characters,
// updating the existing localization (en-US, else the first one) instead of creating a second.
// It returns the locale written. A role refusal wraps ErrWhatToTestRefused so the caller can
// record a note instead of failing (docs/design.md §6.3 step 6); outages stay ErrUnavailable.
func (c *Client) SetWhatToTest(ctx context.Context, buildID, text string) (string, error) {
	if r := []rune(text); len(r) > WhatToTestLimit {
		text = string(r[:WhatToTestLimit])
	}
	locale, err := c.setWhatToTest(ctx, buildID, text)
	if isRoleRefusal(err) {
		return "", fmt.Errorf("%w: %w", ErrWhatToTestRefused, err)
	}
	return locale, err
}

func (c *Client) setWhatToTest(ctx context.Context, buildID, text string) (string, error) {
	for attempt := 0; ; attempt++ {
		doc, err := c.get(ctx, "/v1/builds/"+url.PathEscape(buildID)+"/betaBuildLocalizations?limit=50")
		if err != nil {
			return "", err
		}
		locs, err := doc.list()
		if err != nil {
			return "", err
		}
		var current *resource
		var currentLocale string
		for i, r := range locs {
			var a struct {
				Locale string `json:"locale"`
			}
			_ = json.Unmarshal(r.Attributes, &a)
			if current == nil || a.Locale == WhatToTestLocale {
				current, currentLocale = &locs[i], a.Locale
			}
			if a.Locale == WhatToTestLocale {
				break
			}
		}
		if current != nil {
			body := map[string]any{"data": map[string]any{
				"type":       "betaBuildLocalizations",
				"id":         current.ID,
				"attributes": map[string]string{"whatsNew": text},
			}}
			return currentLocale, c.do(ctx, "PATCH", "/v1/betaBuildLocalizations/"+url.PathEscape(current.ID), body, nil)
		}
		body := map[string]any{"data": map[string]any{
			"type":       "betaBuildLocalizations",
			"attributes": map[string]string{"locale": WhatToTestLocale, "whatsNew": text},
			"relationships": map[string]any{
				"build": map[string]any{"data": linkage{Type: "builds", ID: buildID}},
			},
		}}
		err = c.do(ctx, "POST", "/v1/betaBuildLocalizations", body, nil)
		// A retried POST, or another writer, created the localization first: update it.
		if errors.Is(err, ErrConflict) && attempt == 0 {
			continue
		}
		return WhatToTestLocale, err
	}
}

// InviteState is a tester's TestFlight invitation state for one app.
type InviteState string

const (
	// InviteAccepted means the tester accepted (or installed from) the invitation.
	InviteAccepted InviteState = "accepted"
	// InvitePending means the tester is invited but has not accepted: the result carries an
	// accept_testflight_invite note.
	InvitePending InviteState = "invited_pending"
	// NotATester means no beta tester with this email exists for the app (or it was revoked).
	NotATester InviteState = "not_a_tester"
)

// TesterInvite is TesterInviteState's answer.
type TesterInvite struct {
	State InviteState
	// AppleState is Apple's betaTester state verbatim (NOT_INVITED, INVITED, ACCEPTED,
	// INSTALLED, REVOKED); unknown values map to InvitePending and stay visible here.
	AppleState string
	TesterID   string
}

// TesterInviteState reports whether the configured tester has accepted the app's TestFlight
// invitation, for the accept_testflight_invite note (docs/design.md §6.3 step 6).
func (c *Client) TesterInviteState(ctx context.Context, appID, email string) (TesterInvite, error) {
	q := url.Values{
		"filter[email]":       {email},
		"filter[apps]":        {appID},
		"fields[betaTesters]": {"email,state,inviteType"},
		"limit":               {"20"},
	}
	doc, err := c.get(ctx, "/v1/betaTesters?"+q.Encode())
	if err != nil {
		return TesterInvite{}, err
	}
	testers, err := doc.list()
	if err != nil {
		return TesterInvite{}, err
	}
	for _, r := range testers {
		var a struct {
			Email string `json:"email"`
			State string `json:"state"`
		}
		if json.Unmarshal(r.Attributes, &a) != nil || !strings.EqualFold(a.Email, email) {
			continue
		}
		inv := TesterInvite{AppleState: a.State, TesterID: r.ID}
		switch a.State {
		case "ACCEPTED", "INSTALLED":
			inv.State = InviteAccepted
		case "REVOKED":
			inv.State = NotATester
		default:
			inv.State = InvitePending
		}
		return inv, nil
	}
	return TesterInvite{State: NotATester}, nil
}

// Health is Ping's verdict for doctor.
type Health string

const (
	HealthOK          Health = "ok"
	HealthAuth        Health = "auth"      // 401: key id, issuer, key file or clock
	HealthAgreement   Health = "agreement" // needs_human accept_agreement
	HealthForbidden   Health = "forbidden" // the key's role cannot list apps
	HealthUnavailable Health = "unavailable"
	HealthError       Health = "error"
)

// Ping makes the cheapest authenticated call (GET /v1/apps?limit=1) and classifies the
// outcome for doctor. err is the underlying error, nil when the health is ok.
func (c *Client) Ping(ctx context.Context) (Health, error) {
	_, err := c.get(ctx, "/v1/apps?limit=1&fields%5Bapps%5D=bundleId")
	switch {
	case err == nil:
		return HealthOK, nil
	case errors.Is(err, ErrAuth):
		return HealthAuth, err
	case IsAgreement(err):
		return HealthAgreement, err
	case errors.Is(err, ErrForbidden):
		return HealthForbidden, err
	case errors.Is(err, ErrUnavailable):
		return HealthUnavailable, err
	}
	return HealthError, err
}
