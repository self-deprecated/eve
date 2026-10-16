package t3

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/self-deprecated/eve/internal/api"
)

// MaxMessage bounds a thread message. The result document is already small (PLAN.md §5.3), but
// excerpts and size listings can grow, and a thread message should stay glanceable.
const MaxMessage = 6000

// maxHeader bounds the header line, so a long failure summary cannot crowd out the JSON.
const maxHeader = 400

// FormatMessage renders a notification for a T3 thread: one header line an agent understands at
// a glance, then the result document in a json fence. The whole message stays under MaxMessage
// bytes; when the result is too large, its bulkiest optional fields are dropped first, and
// `eve status <id>` has the rest.
func FormatMessage(n api.Notification) string {
	header := Header(n)
	res := n.Result
	for _, trim := range resultTrims {
		msg, ok := fenced(header, res)
		if ok {
			return msg
		}
		trim(&res)
	}
	msg, ok := fenced(header, res)
	if ok {
		return msg
	}
	// Even the trimmed document is too large (a pathological summary): say so instead.
	return header + "\n\nThe result document is too large for a message; run `eve status " + n.Result.ID + " --json`."
}

// resultTrims shrink a result step by step, least useful first. Each works on a copy's fields,
// never on slices shared with the caller.
var resultTrims = []func(*api.Result){
	func(r *api.Result) { r.Artifacts = nil },
	func(r *api.Result) {
		if r.Size != nil {
			s := *r.Size
			s.Largest = firstN(s.Largest, 3)
			r.Size = &s
		}
	},
	func(r *api.Result) {
		if r.Failure != nil {
			f := *r.Failure
			f.Excerpt = firstN(f.Excerpt, 5)
			r.Failure = &f
		}
	},
	func(r *api.Result) { r.Notes = nil },
	func(r *api.Result) {
		if r.Failure != nil {
			f := *r.Failure
			f.Excerpt = nil
			r.Failure = &f
		}
		if r.Size != nil {
			s := *r.Size
			s.Largest, s.Details = nil, ""
			r.Size = &s
		}
	},
}

func firstN[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n:n]
	}
	return s
}

// fenced assembles the message and reports whether it fits.
func fenced(header string, r api.Result) (string, bool) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", false
	}
	msg := header + "\n\n```json\n" + string(b) + "\n```"
	return msg, len(msg) < MaxMessage
}

// Header is the one-line summary of a notification, e.g.
//
//	[eve] testflight driver j_01JB… → succeeded (build 1.4 (202610161512), ready for testers)
//	[eve] testflight driver j_01JB… → awaiting_confirmation: bundle grew +166 MB — run `eve size j_01JB…`, then `eve confirm j_01JB… --reason "…"` or `eve reject j_01JB…`
//	[eve] device lineage j_01JB… → needs_human: unlock_phone
func Header(n api.Notification) string {
	r := n.Result
	id := r.ID
	if id == "" {
		id = n.ID
	}
	state := strings.TrimPrefix(n.Event, "job.")
	if state == "" {
		state = string(r.Outcome)
	}
	h := fmt.Sprintf("[eve] %s %s %s → %s", r.Verb, r.App, id, state)
	if d := detail(api.State(state), r); d != "" {
		h += d
	}
	return truncate(strings.Join(strings.Fields(h), " "), maxHeader) // one line, whatever the summary holds
}

// detail is the state-specific tail of the header, including its leading separator.
func detail(s api.State, r api.Result) string {
	switch s {
	case api.StateSucceeded:
		var parts []string
		if b := r.Build; b != nil && (b.Number != "" || b.Version != "") {
			parts = append(parts, "build "+buildLabel(b))
		}
		if r.Verb == api.VerbTestflight {
			parts = append(parts, "ready for testers")
		}
		if len(parts) == 0 {
			return ""
		}
		return " (" + strings.Join(parts, ", ") + ")"
	case api.StateAwaitingConfirmation:
		why := "size guard paused delivery"
		if r.Size != nil {
			switch {
			case r.Size.Summary != "":
				why = r.Size.Summary
			case len(r.Size.Reasons) > 0:
				why = strings.Join(r.Size.Reasons, ", ")
			}
		}
		return fmt.Sprintf(": %s — run `eve size %s`, then `eve confirm %s --reason \"…\"` or `eve reject %s`", why, r.ID, r.ID, r.ID)
	case api.StateNeedsHuman, api.StateWaitingForDevice:
		if r.Human == nil {
			return ""
		}
		d := ": " + r.Human.Action
		if r.Human.Detail != "" {
			d += " — " + r.Human.Detail
		}
		return d
	case api.StateFailed, api.StateRejected, api.StateUnavailable, api.StateInterrupted:
		if r.Failure == nil {
			return ""
		}
		d := ": " + r.Failure.Class
		if r.Failure.Summary != "" {
			d += " — " + r.Failure.Summary
		}
		return d
	}
	return ""
}

func buildLabel(b *api.BuildInfo) string {
	switch {
	case b.Version != "" && b.Number != "":
		return b.Version + " (" + b.Number + ")"
	case b.Number != "":
		return b.Number
	}
	return b.Version
}

// truncate shortens s to at most n bytes on a rune boundary, marking the cut with an ellipsis.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
