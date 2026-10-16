package t3

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/self-deprecated/eve/internal/api"
)

func TestHeader(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    api.Notification
		want string
	}{
		{"testflight succeeded", api.Notification{Event: api.NotifySucceeded, Result: api.Result{
			ID: "j_01", App: "driver", Verb: api.VerbTestflight, Outcome: api.StateSucceeded,
			Build: &api.BuildInfo{Version: "1.4", Number: "202610161512"},
		}}, "[eve] testflight driver j_01 → succeeded (build 1.4 (202610161512), ready for testers)"},
		{"awaiting confirmation", api.Notification{Event: api.NotifyAwaitingConfirmation, Result: api.Result{
			ID: "j_02", App: "driver", Verb: api.VerbTestflight, Phase: api.StateAwaitingConfirmation,
			Size: &api.SizeReport{Summary: "bundle grew +166 MB, repo files in bundle"},
		}}, "[eve] testflight driver j_02 → awaiting_confirmation: bundle grew +166 MB, repo files in bundle — run `eve size j_02`, then `eve confirm j_02 --reason \"…\"` or `eve reject j_02`"},
		{"needs human", api.Notification{Event: api.NotifyNeedsHuman, Result: api.Result{
			ID: "j_03", App: "lineage", Verb: api.VerbDevice, Human: &api.Human{Action: api.HumanUnlockPhone},
		}}, "[eve] device lineage j_03 → needs_human: unlock_phone"},
		{"failed, multi-line summary", api.Notification{Event: api.NotifyFailed, Result: api.Result{
			ID: "j_04", App: "lineage", Verb: api.VerbCheck,
			Failure: &api.Failure{Class: "compile", Summary: "Foo.swift:12: error\nmore"},
		}}, "[eve] check lineage j_04 → failed: compile — Foo.swift:12: error more"},
		{
			"cancelled",
			api.Notification{Event: api.NotifyCancelled, Result: api.Result{ID: "j_05", App: "a", Verb: api.VerbExec}},
			"[eve] exec a j_05 → cancelled",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Header(tc.n); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestFormatMessageFencesResult(t *testing.T) {
	n := api.Notification{Event: api.NotifySucceeded, Result: api.Result{ID: "j_1", App: "a", Verb: api.VerbCheck, Outcome: api.StateSucceeded}}
	msg := FormatMessage(n)
	head, rest, ok := strings.Cut(msg, "\n\n```json\n")
	if !ok || head != Header(n) || !strings.HasSuffix(rest, "\n```") {
		t.Fatalf("message shape:\n%s", msg)
	}
	var r api.Result
	if err := json.Unmarshal([]byte(strings.TrimSuffix(rest, "\n```")), &r); err != nil || r.ID != "j_1" {
		t.Errorf("fenced JSON: %v %+v", err, r)
	}
}

func TestFormatMessageStaysSmall(t *testing.T) {
	excerpt := make([]string, 200)
	for i := range excerpt {
		excerpt[i] = strings.Repeat("x", 80)
	}
	largest := make([]api.SizeEntry, 50)
	for i := range largest {
		largest[i] = api.SizeEntry{Path: strings.Repeat("p", 60), Bytes: 1, Change: "new"}
	}
	n := api.Notification{Event: api.NotifyFailed, Result: api.Result{
		ID: "j_1", App: "a", Verb: api.VerbTestflight,
		Failure:   &api.Failure{Class: "compile", Summary: strings.Repeat("s", 2000), Excerpt: excerpt},
		Size:      &api.SizeReport{Summary: "ok", Largest: largest},
		Artifacts: []string{strings.Repeat("a", 3000)},
	}}
	msg := FormatMessage(n)
	if len(msg) >= MaxMessage {
		t.Fatalf("message is %d bytes", len(msg))
	}
	if !strings.Contains(msg, "```json") || !strings.Contains(msg, `"class": "compile"`) {
		t.Errorf("trimmed message lost the result:\n%s", msg)
	}
	if len(n.Result.Failure.Excerpt) != 200 || len(n.Result.Size.Largest) != 50 {
		t.Error("FormatMessage modified the caller's result")
	}
	if first, _, _ := strings.Cut(msg, "\n"); len(first) > maxHeader {
		t.Errorf("header is %d bytes", len(first))
	}
}
