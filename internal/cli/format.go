package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/self-deprecated/eve/internal/api"
)

// FormatResult renders a result document for people. Agents should use --json.
func FormatResult(r api.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s %s  %s", r.ID, r.Verb, r.App, strings.ToUpper(string(r.Outcome)))
	if !r.Outcome.Terminal() && r.Phase != r.Outcome {
		fmt.Fprintf(&b, " (%s)", r.Phase)
	}
	if r.DurationS > 0 {
		fmt.Fprintf(&b, "  %s", duration(r.DurationS))
	}
	b.WriteString("\n")
	if r.Build != nil && r.Build.Number != "" {
		fmt.Fprintf(&b, "  build    %s %s\n", r.Build.Version, r.Build.Number)
	}
	if r.Source.Commit != "" || r.Source.SHA256 != "" {
		src := r.Source.Commit
		if src == "" && len(r.Source.SHA256) >= 12 {
			src = "upload " + r.Source.SHA256[:12]
		}
		if r.Source.Dirty {
			src += " (dirty)"
		}
		if r.Source.Files > 0 {
			src += fmt.Sprintf(", %d files, %s", r.Source.Files, mb(r.Source.Bytes))
		}
		fmt.Fprintf(&b, "  source   %s\n", src)
	}
	if s := r.Size; s != nil {
		fmt.Fprintf(&b, "  size     %s  [%s]\n", s.Summary, s.Gate)
		for _, reason := range s.Reasons {
			fmt.Fprintf(&b, "           - %s\n", reason)
		}
		for _, e := range s.Largest {
			fmt.Fprintf(&b, "           %-48s %10s  %s\n", e.Path, mb(e.Bytes), e.Change)
		}
		if s.Hint != "" {
			fmt.Fprintf(&b, "  hint     %s\n", s.Hint)
		}
	}
	if t := r.Tests; t != nil {
		fmt.Fprintf(&b, "  tests    %d passed, %d failed", t.Passed, t.Failed)
		if t.Skipped > 0 {
			fmt.Fprintf(&b, ", %d skipped", t.Skipped)
		}
		if t.Simulator != "" {
			fmt.Fprintf(&b, "  (%s)", t.Simulator)
		}
		b.WriteString("\n")
	}
	if f := r.Failure; f != nil {
		fmt.Fprintf(&b, "  failure  %s: %s\n", f.Class, f.Summary)
		if f.Hint != "" {
			fmt.Fprintf(&b, "  hint     %s\n", f.Hint)
		}
		for _, l := range f.Excerpt {
			fmt.Fprintf(&b, "    | %s\n", l)
		}
		if f.Log != "" {
			fmt.Fprintf(&b, "  more     %s\n", f.Log)
		}
	}
	if h := r.Human; h != nil {
		fmt.Fprintf(&b, "  NEEDS YOU  %s: %s\n", h.Action, h.Detail)
		if h.URL != "" {
			fmt.Fprintf(&b, "             %s\n", h.URL)
		}
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "  note     %s: %s\n", n.Kind, n.Detail)
	}
	switch r.Outcome {
	case api.StateAwaitingConfirmation:
		fmt.Fprintf(&b, "  next     eve size %s, then eve confirm %s --reason \"…\" or eve reject %s\n", r.ID, r.ID, r.ID)
	case api.StateQueued, api.StateArchiving, api.StateBuilding, api.StatePreparing, api.StateDelivering,
		api.StateProcessing, api.StateDistributing, api.StateMeasuring, api.StateWaitingForDevice:
		fmt.Fprintf(&b, "  next     eve wait %s\n", r.ID)
	}
	return b.String()
}

// FormatJobs renders a job list.
func FormatJobs(jobs []api.Result) string {
	if len(jobs) == 0 {
		return "no jobs\n"
	}
	var b strings.Builder
	for _, r := range jobs {
		build := ""
		if r.Build != nil {
			build = r.Build.Number
		}
		state := string(r.Outcome)
		if r.Failure != nil {
			state += " (" + r.Failure.Class + ")"
		}
		fmt.Fprintf(&b, "%-22s %-10s %-11s %-12s %s\n", r.ID, r.Verb, r.App, build, state)
	}
	return b.String()
}

// FormatDeliveries renders a job's notification deliveries.
func FormatDeliveries(raw json.RawMessage) string {
	var rows []struct {
		Event, Target, Status, LastError string
		Attempts                         int
	}
	if json.Unmarshal(raw, &rows) != nil || len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("  notifications\n")
	for _, r := range rows {
		line := fmt.Sprintf("    %-26s %-40s %s", r.Event, r.Target, r.Status)
		if r.LastError != "" {
			line += " (" + r.LastError + ")"
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// FormatSize renders the size endpoint's JSON. Its shape is owned by the sizeguard package, so
// this prints the common fields and falls back to indented JSON.
func FormatSize(raw json.RawMessage) string {
	var rep struct {
		Report *api.SizeReport `json:"report"`
		Diff   []api.SizeEntry `json:"diff"`
	}
	if json.Unmarshal(raw, &rep) == nil && (rep.Report != nil || len(rep.Diff) > 0) {
		var b strings.Builder
		if rep.Report != nil {
			b.WriteString(FormatResult(api.Result{Size: rep.Report, Outcome: api.StateSucceeded})[0:0])
			fmt.Fprintf(&b, "%s  [%s]\n", rep.Report.Summary, rep.Report.Gate)
			for _, r := range rep.Report.Reasons {
				fmt.Fprintf(&b, "  - %s\n", r)
			}
			if rep.Report.Hint != "" {
				fmt.Fprintf(&b, "hint: %s\n", rep.Report.Hint)
			}
		}
		for _, e := range rep.Diff {
			fmt.Fprintf(&b, "%-60s %10s  %s\n", e.Path, mb(e.Bytes), e.Change)
		}
		return b.String()
	}
	return indent(raw)
}

// FormatDoctor renders host checks: [{name, status, detail}] or any JSON.
func FormatDoctor(raw json.RawMessage) string {
	var checks []struct {
		Name, Status, Detail string
	}
	if json.Unmarshal(raw, &checks) != nil || len(checks) == 0 {
		return indent(raw)
	}
	var b strings.Builder
	for _, c := range checks {
		mark := map[string]string{"ok": "✓", "warn": "!", "fail": "✗"}[c.Status]
		if mark == "" {
			mark = "?"
		}
		fmt.Fprintf(&b, "%s %-28s %s\n", mark, c.Name, c.Detail)
	}
	return b.String()
}

func indent(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw) + "\n"
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b) + "\n"
}

func mb(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1f KB", float64(n)/1e3)
	}
	return fmt.Sprintf("%d B", n)
}

func duration(s int) string {
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}
