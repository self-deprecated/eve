package asc

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors. Match them with errors.Is; *APIError carries the details.
var (
	// ErrAuth means App Store Connect rejected the token (HTTP 401): wrong key id, issuer or
	// key file, a revoked key, or a skewed clock (docs/design.md §9 asc.auth).
	ErrAuth = errors.New("asc: authentication failed")
	// ErrForbidden means the key may not make the request (HTTP 403). The *APIError's
	// Agreement flag tells an expired agreement (needs_human accept_agreement) from a role
	// that is too narrow.
	ErrForbidden = errors.New("asc: forbidden")
	// ErrNotFound means the resource does not exist (HTTP 404), or a lookup found no match.
	ErrNotFound = errors.New("asc: not found")
	// ErrConflict means the request conflicts with existing state (HTTP 409).
	ErrConflict = errors.New("asc: conflict")
	// ErrUnavailable means App Store Connect or the network stayed unavailable through every
	// retry (network errors, unreadable responses, 429, 5xx).
	ErrUnavailable = errors.New("asc: unavailable")
	// ErrWhatToTestRefused wraps a role refusal from SetWhatToTest. A Developer-role key may
	// upload and use internal groups but not edit What to Test; callers record a note rather
	// than failing the job (docs/design.md §6.3 step 6).
	ErrWhatToTestRefused = errors.New("asc: what to test refused")
	// ErrExternalGroup is returned by BetaGroupByName for a group that is not internal: eve
	// only distributes to internal groups.
	ErrExternalGroup = errors.New("asc: beta group is external")
	// ErrAudienceMismatch is returned by CheckAudience when a build exported for internal
	// testing only arrives store-eligible.
	ErrAudienceMismatch = errors.New("asc: build audience mismatch")
)

// ErrorItem is one entry of Apple's errors[] array.
type ErrorItem struct {
	Status string `json:"status,omitempty"`
	Code   string `json:"code,omitempty"`
	Title  string `json:"title,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// APIError is a non-2xx response from App Store Connect.
type APIError struct {
	Method string
	Path   string // without the query
	Status int
	Errors []ErrorItem
	// Agreement is set for 403 FORBIDDEN.REQUIRED_AGREEMENTS_MISSING_OR_EXPIRED: the Account
	// Holder has to accept an agreement before the API answers again.
	Agreement bool
}

func newAPIError(method, path string, status int, items []ErrorItem) *APIError {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	e := &APIError{Method: method, Path: path, Status: status, Errors: items}
	if status == 403 {
		for _, it := range items {
			if strings.Contains(strings.ToUpper(it.Code), "AGREEMENT") {
				e.Agreement = true
			}
		}
	}
	return e
}

// Reason is Apple's first error as "code: title: detail", or "HTTP <status>".
func (e *APIError) Reason() string {
	if len(e.Errors) > 0 {
		var parts []string
		for _, s := range []string{e.Errors[0].Code, e.Errors[0].Title, e.Errors[0].Detail} {
			if s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, ": ")
		}
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

func (e *APIError) Error() string {
	where := e.Method + " " + e.Path
	switch {
	case e.Status == 401:
		return fmt.Sprintf("asc: App Store Connect rejected the API key on %s (%s); check the key id, issuer id and key file, or whether the key was revoked", where, e.Reason())
	case e.Agreement:
		return fmt.Sprintf("asc: an App Store Connect agreement needs the Account Holder (%s)", e.Reason())
	case e.Status == 403:
		return fmt.Sprintf("asc: the API key's role does not allow %s (%s)", where, e.Reason())
	case e.Status == 429 || e.Status >= 500:
		return fmt.Sprintf("asc: App Store Connect failed on %s (%s)", where, e.Reason())
	}
	return fmt.Sprintf("asc: App Store Connect rejected %s (%s)", where, e.Reason())
}

// Is matches the sentinel errors by status.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrAuth:
		return e.Status == 401
	case ErrForbidden:
		return e.Status == 403
	case ErrNotFound:
		return e.Status == 404
	case ErrConflict:
		return e.Status == 409
	case ErrUnavailable:
		return e.Status == 429 || e.Status >= 500
	}
	return false
}

// IsAgreement reports whether err is a 403 for a missing or expired agreement.
func IsAgreement(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Agreement
}

// isRoleRefusal reports whether err is a 403 that is not about agreements.
func isRoleRefusal(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == 403 && !ae.Agreement
}
