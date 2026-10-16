package asc

import (
	"fmt"

	"github.com/self-deprecated/eve/internal/api"
)

// Kind is what the pipeline does with a build state.
type Kind string

const (
	// Wait means keep polling.
	Wait Kind = "wait"
	// Done means processing finished (VALID); move on to distributing.
	Done Kind = "done"
	// Ready means internal testers can install the build; distribute it.
	Ready Kind = "ready"
	// Rejected means Apple refused the build (job outcome rejected).
	Rejected Kind = "rejected"
	// Failed means the build cannot be used (job outcome failed).
	Failed Kind = "failed"
	// NeedsHuman means a person has to act; Verdict.Human names the action.
	NeedsHuman Kind = "needs_human"
	// Unknown means Apple reported a state this mapping does not know. It is surfaced
	// verbatim in Verdict.State rather than guessed; the caller decides.
	Unknown Kind = "unknown"
)

// Verdict is the pipeline's view of one App Store Connect state.
type Verdict struct {
	Kind Kind
	// State is Apple's state verbatim; empty when the build or its beta detail is not
	// visible yet.
	State   string
	Human   string   // api.Human* action when Kind is NeedsHuman
	Reasons []string // Apple's reasons, when known
	Summary string
}

// ProcessingVerdict maps a build's processingState (PLAN.md §6.3 step 5). A nil build is
// still on its way and means Wait.
func ProcessingVerdict(b *Build) Verdict {
	if b == nil {
		return Verdict{Kind: Wait, Summary: "waiting for the build to appear in App Store Connect"}
	}
	s := b.ProcessingState
	switch s {
	case "PROCESSING":
		return Verdict{Kind: Wait, State: s, Summary: "Apple is processing the build"}
	case "VALID":
		return Verdict{Kind: Done, State: s, Summary: "Apple finished processing the build"}
	case "FAILED", "INVALID":
		return Verdict{
			Kind:    Rejected,
			State:   s,
			Summary: fmt.Sprintf("Apple marked the build %s. Apple's email to the account lists the ITMS errors; fix them and upload a new build.", s),
		}
	}
	return Verdict{Kind: Unknown, State: s, Summary: fmt.Sprintf("unexpected processing state %q", s)}
}

// InternalVerdict maps a build's buildBetaDetail internalBuildState (PLAN.md §6.3 step 6). A
// nil detail is not visible yet and means Wait.
func InternalVerdict(d *BetaDetail) Verdict {
	if d == nil || d.InternalBuildState == "" {
		return Verdict{Kind: Wait, Summary: "waiting for TestFlight to show the build"}
	}
	s := d.InternalBuildState
	switch s {
	case "READY_FOR_BETA_TESTING", "IN_BETA_TESTING":
		return Verdict{Kind: Ready, State: s, Summary: "the build is ready for internal testers"}
	case "MISSING_EXPORT_COMPLIANCE":
		return Verdict{
			Kind:    NeedsHuman,
			State:   s,
			Human:   api.HumanExportCompliance,
			Summary: "The build is waiting for an export compliance answer. Set ITSAppUsesNonExemptEncryption in the app's Info.plist (the developer decides), then upload a new build.",
		}
	case "IN_EXPORT_COMPLIANCE_REVIEW", "PROCESSING":
		return Verdict{Kind: Wait, State: s, Summary: "TestFlight is still preparing the build"}
	case "PROCESSING_EXCEPTION":
		return Verdict{Kind: Rejected, State: s, Summary: "TestFlight reports PROCESSING_EXCEPTION for this build."}
	case "EXPIRED":
		return Verdict{Kind: Failed, State: s, Summary: "TestFlight reports the build EXPIRED."}
	}
	return Verdict{Kind: Unknown, State: s, Summary: fmt.Sprintf("unexpected TestFlight state %q", s)}
}

// BuildVerdict combines both mappings: the processing verdict until the build is VALID, then
// the TestFlight verdict.
func BuildVerdict(b *Build) Verdict {
	if v := ProcessingVerdict(b); v.Kind != Done {
		return v
	}
	return InternalVerdict(b.BetaDetail)
}

// CheckAudience refuses a build that arrived store-eligible although the app exports for
// internal testing only: it must not be distributed quietly (PLAN.md §6.3 step 6). It only
// judges VALID builds whose audience Apple reports; the error wraps ErrAudienceMismatch.
func CheckAudience(b *Build, internalOnly bool) error {
	if !internalOnly || b == nil || b.ProcessingState != "VALID" {
		return nil
	}
	if b.BuildAudienceType != "" && b.BuildAudienceType != "INTERNAL_ONLY" {
		return fmt.Errorf("%w: build %s arrived as %s although it was exported for internal testing only; it was not distributed",
			ErrAudienceMismatch, b.Number, b.BuildAudienceType)
	}
	return nil
}
