package notify

import (
	"fmt"
	"strings"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
)

// BuildDeliveries decides who receives event for a job (PLAN.md §8.1a, §8.1b). The requesting
// agents (T3 threads and notify URLs) get every event. The human hook gets only events a person
// must act on: needs_human and waiting_for_device. awaiting_confirmation never goes to the
// human; size confirmations are the agent's call. Exhausted deliveries reach the human through
// ExhaustedCopy instead. event defaults to n.Event; duplicate and blank addresses are dropped.
//
// Deduplicating one human message per job and action across repeated events is the store's
// job (a unique key on job, event and target).
func BuildDeliveries(n api.Notification, t3Threads, urls []string, human *config.HumanHook, event string) []Target {
	if event == "" {
		event = n.Event
	}
	var out []Target
	seen := map[Target]bool{}
	add := func(t Target) {
		t.Address = strings.TrimSpace(t.Address)
		if t.Address != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, id := range t3Threads {
		add(Target{Kind: KindT3, Address: id})
	}
	for _, u := range urls {
		add(Target{Kind: KindWebhook, Address: u})
	}
	if human != nil && HumanEvent(event) {
		add(Target{Kind: KindHuman, Address: human.URL})
	}
	return out
}

// HumanEvent reports whether a job event goes to the human hook. Exhausted-delivery copies
// are routed separately, by ExhaustedCopy.
func HumanEvent(event string) bool {
	switch event {
	case api.NotifyNeedsHuman, api.NotifyWaitingForDevice:
		return true
	}
	return false
}

// ExhaustedCopy returns the human-hook target for a copy of an exhausted delivery, if one
// should be sent: never for a human delivery itself (that would loop), nor without a hook. The
// server enqueues it with Event EventDeliveryExhausted and the original Payload.
func ExhaustedCopy(row Delivery, human *config.HumanHook) (Target, bool) {
	if human == nil || strings.TrimSpace(human.URL) == "" || row.Target.Kind == KindHuman {
		return Target{}, false
	}
	return Target{Kind: KindHuman, Address: strings.TrimSpace(human.URL)}, true
}

// HumanMessage is the human hook's JSON body (see the package documentation).
type HumanMessage struct {
	Text    string           `json:"text"`
	Event   string           `json:"event"`
	Payload api.Notification `json:"payload"`
}

// HumanBody builds the human hook body for an outbox row.
func HumanBody(row Delivery) HumanMessage {
	return HumanMessage{Text: HumanText(row.Event, row.Payload), Event: row.Event, Payload: row.Payload}
}

// humanActions phrases api.Human actions for a person.
var humanActions = map[string]string{
	api.HumanUnlockPhone:            "unlock the iPhone",
	api.HumanXcodeSignIn:            "sign in to Xcode (Settings → Accounts) on Quartz",
	api.HumanAcceptAgreement:        "accept the new agreement in App Store Connect",
	api.HumanExportCompliance:       "answer export compliance in App Store Connect",
	api.HumanCreateAppRecord:        "create the app record in App Store Connect",
	api.HumanConsoleLogin:           "log in to the Mac's console session",
	api.HumanAcceptTestflightInvite: "accept the TestFlight invitation",
	api.HumanT3Login:                "run `eve t3 login` on Quartz",
}

// HumanText is the one-line chat message for event about n.
func HumanText(event string, n api.Notification) string {
	r := n.Result
	id := r.ID
	if id == "" {
		id = n.ID
	}
	subject := fmt.Sprintf("eve: %s %s %s", r.App, r.Verb, id)
	var text string
	switch event {
	case EventDeliveryExhausted:
		text = fmt.Sprintf("%s: could not deliver %s to the requesting agent within %.0f h; see `eve status %s`",
			subject, n.Event, MaxAge.Hours(), id)
	case api.NotifyNeedsHuman, api.NotifyWaitingForDevice:
		verb := "needs you"
		if event == api.NotifyWaitingForDevice {
			verb = "is waiting for the device"
		}
		text = subject + " " + verb
		if h := r.Human; h != nil {
			action := humanActions[h.Action]
			if action == "" {
				action = h.Action
			}
			text += ": " + action
			if h.Detail != "" {
				text += " — " + h.Detail
			}
			if h.URL != "" {
				text += " " + h.URL
			}
		}
	default:
		text = fmt.Sprintf("%s → %s", subject, strings.TrimPrefix(event, "job."))
	}
	return truncate(strings.Join(strings.Fields(text), " "), 500)
}
