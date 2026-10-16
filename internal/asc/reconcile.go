package asc

import (
	"context"
	"errors"
	"time"
)

// ReconcileStatus is what App Store Connect shows for an upload whose outcome eve does not
// know (PLAN.md §5.5).
type ReconcileStatus string

const (
	// Found means the build record exists: go to processing.
	Found ReconcileStatus = "found"
	// UploadLive means no build yet, but an upload record is COMPLETE or PROCESSING: go to
	// processing.
	UploadLive ReconcileStatus = "upload_live"
	// AllFailed means every upload record FAILED and no build exists: re-run delivery from
	// the kept archive. Reasons carries Apple's errors.
	AllFailed ReconcileStatus = "all_failed"
	// NotVisible means neither a build nor a live or failed upload shows (no records, or only
	// AWAITING_UPLOAD ones, which a finished upload has left behind).
	NotVisible ReconcileStatus = "not_visible"
)

// Reconciliation is one reconciliation check.
type Reconciliation struct {
	Status  ReconcileStatus
	Build   *Build        // set when Status is Found
	Upload  UploadSummary // zero when the build was found or upload records are unreadable
	Reasons []string      // Apple's upload errors when Status is AllFailed
	// UploadsUnreadable is set when this key's role (or API version) refuses buildUploads;
	// only the build record then decides.
	UploadsUnreadable bool
}

// Reconcile checks once whether an upload of this version and build number reached App Store
// Connect. Once the build record exists it alone decides, even if every upload record failed:
// stale failed records from an earlier attempt must not abort a build that is processing.
func (c *Client) Reconcile(ctx context.Context, appID, version, number string) (Reconciliation, error) {
	b, err := c.FindBuild(ctx, appID, version, number)
	if err != nil {
		return Reconciliation{}, err
	}
	if b != nil {
		return Reconciliation{Status: Found, Build: b}, nil
	}
	up, err := c.FindUploads(ctx, appID, version, number)
	switch {
	case err != nil && (isRoleRefusal(err) || errors.Is(err, ErrNotFound)):
		// Upload records only add diagnostics; without them, wait for the build record.
		return Reconciliation{Status: NotVisible, UploadsUnreadable: true}, nil
	case err != nil:
		return Reconciliation{}, err
	}
	r := Reconciliation{Status: NotVisible, Upload: up}
	switch up.State {
	case UploadComplete, UploadProcessing:
		r.Status = UploadLive
	case UploadFailed:
		r.Status, r.Reasons = AllFailed, up.Reasons
	}
	return r, nil
}

// ReconcileUntil repeats Reconcile every interval until it sees something other than
// NotVisible or window has elapsed, riding out App Store Connect outages (ErrUnavailable)
// meanwhile. It returns the last result; NotVisible after the full window means the upload
// probably never arrived. An outage that lasts until the window ends returns its error.
func (c *Client) ReconcileUntil(ctx context.Context, appID, version, number string, window, interval time.Duration) (Reconciliation, error) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	deadline := c.now().Add(window)
	for {
		r, err := c.Reconcile(ctx, appID, version, number)
		if err != nil && !errors.Is(err, ErrUnavailable) {
			return r, err
		}
		if err == nil && r.Status != NotVisible {
			return r, nil
		}
		left := deadline.Sub(c.now())
		if left <= 0 {
			return r, err
		}
		if werr := c.wait(ctx, min(interval, left)); werr != nil {
			return r, werr
		}
	}
}
