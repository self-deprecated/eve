// Package api holds the types shared by the eve client and server: job states, the result
// document agents read, job requests and events (PLAN.md §5, §11).
package api

import (
	"encoding/json"
	"slices"
	"time"
)

// Verb is what a job does.
type Verb string

const (
	VerbTestflight Verb = "testflight"
	VerbDevice     Verb = "device"
	VerbCheck      Verb = "check"
	VerbExec       Verb = "exec"
)

// Verbs lists every verb in the order eve presents them.
var Verbs = []Verb{VerbTestflight, VerbCheck, VerbDevice, VerbExec}

// Valid reports whether v is a known verb.
func (v Verb) Valid() bool { return slices.Contains(Verbs, v) }

// State is a job's phase while it runs, or its outcome once terminal (PLAN.md §5.1).
type State string

const (
	StateQueued               State = "queued"
	StatePreparing            State = "preparing"
	StateBuilding             State = "building"
	StateArchiving            State = "archiving"
	StateMeasuring            State = "measuring"
	StateAwaitingConfirmation State = "awaiting_confirmation"
	StateDelivering           State = "delivering"
	StateProcessing           State = "processing"
	StateDistributing         State = "distributing"
	StateInstalling           State = "installing"
	StateWaitingForDevice     State = "waiting_for_device"
	StateLaunching            State = "launching"
	StateTesting              State = "testing"
	StateRunning              State = "running" // exec

	StateSucceeded   State = "succeeded"
	StateFailed      State = "failed"
	StateRejected    State = "rejected"
	StateNeedsHuman  State = "needs_human"
	StateUnavailable State = "unavailable"
	StateCancelled   State = "cancelled"
	StateInterrupted State = "interrupted"
)

// Terminal reports whether s is an outcome rather than a phase.
func (s State) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateRejected, StateNeedsHuman, StateUnavailable,
		StateCancelled, StateInterrupted:
		return true
	}
	return false
}

// Parked reports whether a job in state s is waiting off the worker (PLAN.md §5.6).
func (s State) Parked() bool {
	switch s {
	case StateAwaitingConfirmation, StateProcessing, StateDistributing:
		return true
	}
	return false
}

// Exit codes for `eve wait`, `eve run` and `eve submit --wait` (PLAN.md §5.2).
const (
	ExitOK                   = 0
	ExitFailed               = 1
	ExitUsage                = 64
	ExitRejected             = 65
	ExitUnavailable          = 69
	ExitWaitTimeout          = 75
	ExitConfirmationRequired = 76
	ExitNeedsHuman           = 78
	ExitCancelled            = 130
)

// ExitCode maps a job state to the CLI exit code. Non-terminal states other than
// awaiting_confirmation map to ExitWaitTimeout: the caller stopped waiting, the job did not.
func ExitCode(s State) int {
	switch s {
	case StateSucceeded:
		return ExitOK
	case StateFailed, StateInterrupted:
		return ExitFailed
	case StateRejected:
		return ExitRejected
	case StateUnavailable:
		return ExitUnavailable
	case StateNeedsHuman:
		return ExitNeedsHuman
	case StateCancelled:
		return ExitCancelled
	case StateAwaitingConfirmation:
		return ExitConfirmationRequired
	}
	return ExitWaitTimeout
}

// APILevel is the server behaviour this client relies on. Bump it whenever the client starts
// depending on something a server must have; a server without it (an older one reports 0) is
// refused before anything is uploaded, rather than misreading its answers. 2: apps list verbs.
const APILevel = 2

// VersionInfo is GET /v1/version.
type VersionInfo struct {
	Version string `json:"version"`
	API     int    `json:"api"`
}

// Result is the document an agent reads when it is woken (PLAN.md §5.3). Keep it small:
// everything else stays behind `eve logs`.
type Result struct {
	ID        string      `json:"id"`
	App       string      `json:"app"`
	Verb      Verb        `json:"verb"`
	Outcome   State       `json:"outcome"`
	Phase     State       `json:"phase,omitempty"` // where it is, or where it stopped; omitted once it succeeded
	DurationS int         `json:"durationS"`
	Source    SourceInfo  `json:"source"`
	Build     *BuildInfo  `json:"build,omitempty"`
	Failure   *Failure    `json:"failure,omitempty"`
	Human     *Human      `json:"human,omitempty"`
	Size      *SizeReport `json:"size,omitempty"`
	Tests     *Tests      `json:"tests,omitempty"`
	Notes     []Note      `json:"notes,omitempty"`
	Artifacts []string    `json:"artifacts,omitempty"`
}

// Tests summarizes a check's test run (PLAN.md §6.5): counts and the first failures, never the
// full log.
type Tests struct {
	Passed           int           `json:"passed"`
	Failed           int           `json:"failed"`
	Skipped          int           `json:"skipped,omitempty"`
	ExpectedFailures int           `json:"expectedFailures,omitempty"`
	Total            int           `json:"total"`
	Simulator        string        `json:"simulator,omitempty"` // e.g. "iPhone 17 Pro, iOS 26.5"
	Failures         []TestFailure `json:"failures,omitempty"`  // the first few
}

// TestFailure is one failing test.
type TestFailure struct {
	Test    string `json:"test"` // e.g. "LineageUITests/testOnboarding()"
	Message string `json:"message"`
}

// SourceInfo records what was built.
type SourceInfo struct {
	SHA256 string `json:"sha256,omitempty"`
	Path   string `json:"path,omitempty"`
	Commit string `json:"commit,omitempty"`
	Branch string `json:"branch,omitempty"`
	Dirty  bool   `json:"dirty,omitempty"`
	Files  int    `json:"files,omitempty"` // uploaded files (none for a path build)
	Bytes  int64  `json:"bytes,omitempty"` // their size, uncompressed
}

// BuildInfo identifies an App Store Connect build.
type BuildInfo struct {
	Version    string `json:"version,omitempty"`
	Number     string `json:"number,omitempty"`
	ASCBuildID string `json:"ascBuildId,omitempty"`
}

// Failure explains why a job did not succeed.
type Failure struct {
	Class   string   `json:"class"`
	Summary string   `json:"summary"`
	Hint    string   `json:"hint,omitempty"`
	Excerpt []string `json:"excerpt,omitempty"`
	By      string   `json:"by,omitempty"` // "rules" or "model"
	Log     string   `json:"log,omitempty"`
}

// Human actions a person must take (PLAN.md §5.1).
const (
	HumanUnlockPhone            = "unlock_phone"
	HumanConnectPhone           = "connect_phone" // Quartz can't reach it: Wi-Fi off, away, or asleep
	HumanXcodeSignIn            = "xcode_sign_in"
	HumanAcceptAgreement        = "accept_agreement"
	HumanExportCompliance       = "export_compliance"
	HumanCreateAppRecord        = "create_app_record"
	HumanConsoleLogin           = "console_login"
	HumanAcceptTestflightInvite = "accept_testflight_invite"
)

// Human describes what a person has to do.
type Human struct {
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
	URL    string `json:"url,omitempty"`
}

// Note is non-fatal information attached to a result.
type Note struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// Size-guard gate values (PLAN.md §6.7).
const (
	GatePassed               = "passed"
	GateBaselineCreated      = "baseline_created"
	GateAwaitingConfirmation = "awaiting_confirmation"
	GateConfirmed            = "confirmed"
	GateFailed               = "failed"
	GateReportOnly           = "report_only" // device and check builds never pause
)

// SizeReport is the size guard's summary in a result (PLAN.md §6.7.3).
type SizeReport struct {
	Gate          string      `json:"gate"`
	AppBytes      int64       `json:"appBytes"`
	Files         int         `json:"files"`
	BaselineBytes int64       `json:"baselineBytes,omitempty"`
	BaselineFiles int         `json:"baselineFiles,omitempty"`
	BaselineBuild string      `json:"baselineBuild,omitempty"`
	BaselineJob   string      `json:"baselineJob,omitempty"` // the job whose bundle was the baseline
	DeltaBytes    int64       `json:"deltaBytes,omitempty"`
	DeltaFiles    int         `json:"deltaFiles,omitempty"`
	Summary       string      `json:"summary"`
	Reasons       []string    `json:"reasons,omitempty"`
	Largest       []SizeEntry `json:"largest,omitempty"`
	Hint          string      `json:"hint,omitempty"`
	Confirmation  string      `json:"confirmation,omitempty"` // the agent's stated reason
	Details       string      `json:"details,omitempty"`
}

// SizeEntry is one path (file or directory aggregate) in a size report.
type SizeEntry struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Change string `json:"change"` // "new", "+1.2 MB", "removed"
}

// SourceStats summarizes an uploaded source tree (PLAN.md §6.7.1).
type SourceStats struct {
	Bytes  int64            `json:"bytes"`
	Files  int              `json:"files"`
	TopDir map[string]int64 `json:"topDirs"` // bytes per top-level entry
}

// App is one entry of GET /v1/apps.
type App struct {
	Name, BundleID, Signing string
	InternalOnly, AllowExec bool
	Verbs                   []Verb // what this server can run for the app
}

// JobRequest is the body of POST /v1/jobs.
type JobRequest struct {
	App            string     `json:"app"`
	Verb           Verb       `json:"verb"`
	Source         JobSource  `json:"source"`
	Options        JobOptions `json:"options"`
	NotifyURLs     []string   `json:"notifyUrls,omitempty"`
	IdempotencyKey string     `json:"idempotencyKey,omitempty"`
}

// JobSource says where the job's source comes from.
type JobSource struct {
	SHA256 string      `json:"sha256,omitempty"`
	Path   string      `json:"path,omitempty"` // owner token on Quartz only
	Stats  SourceStats `json:"stats"`
	Commit string      `json:"commit,omitempty"`
	Branch string      `json:"branch,omitempty"`
	Dirty  bool        `json:"dirty,omitempty"`
	// AcceptedSize carries --accept-source-size's reason when the client's source check tripped.
	AcceptedSize string `json:"acceptedSize,omitempty"`
}

// JobOptions are per-job switches.
type JobOptions struct {
	BuildNumber      string   `json:"buildNumber,omitempty"`
	Notes            string   `json:"notes,omitempty"`
	Device           string   `json:"device,omitempty"`
	Release          bool     `json:"release,omitempty"`
	ExpectSizeChange string   `json:"expectSizeChange,omitempty"`
	Command          []string `json:"command,omitempty"` // exec
}

// JobCreated is the response to POST /v1/jobs.
type JobCreated struct {
	ID             string `json:"id"`
	IdempotencyKey string `json:"idempotencyKey"`
	Existing       bool   `json:"existing,omitempty"`
	State          State  `json:"state"`
}

// Event is one entry of a job's history, also streamed over SSE.
type Event struct {
	Seq     int64           `json:"seq"`
	JobID   string          `json:"jobId"`
	Time    time.Time       `json:"time"`
	Kind    string          `json:"kind"` // EventTransition, EventMessage
	State   State           `json:"state"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Event kinds.
const (
	EventTransition = "transition"
	EventMessage    = "message"
)

// Notification event names (PLAN.md §8).
const (
	NotifySucceeded            = "job.succeeded"
	NotifyFailed               = "job.failed"
	NotifyRejected             = "job.rejected"
	NotifyNeedsHuman           = "job.needs_human"
	NotifyCancelled            = "job.cancelled"
	NotifyInterrupted          = "job.interrupted"
	NotifyUnavailable          = "job.unavailable"
	NotifyAwaitingConfirmation = "job.awaiting_confirmation"
	NotifyWaitingForDevice     = "job.waiting_for_device"
)

// NotifyEventFor returns the notification event for entering state s, or "" if none.
func NotifyEventFor(s State) string {
	switch s {
	case StateSucceeded:
		return NotifySucceeded
	case StateFailed:
		return NotifyFailed
	case StateRejected:
		return NotifyRejected
	case StateNeedsHuman:
		return NotifyNeedsHuman
	case StateCancelled:
		return NotifyCancelled
	case StateInterrupted:
		return NotifyInterrupted
	case StateUnavailable:
		return NotifyUnavailable
	case StateAwaitingConfirmation:
		return NotifyAwaitingConfirmation
	case StateWaitingForDevice:
		return NotifyWaitingForDevice
	}
	return ""
}

// Notification is what eve delivers to a webhook.
type Notification struct {
	Event      string `json:"event"`
	ID         string `json:"id"`
	DeliveryID string `json:"deliveryId"`
	Result     Result `json:"result"`
}

// ErrorBody is the JSON body of every non-2xx API response.
type ErrorBody struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}
