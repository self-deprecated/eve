package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/self-deprecated/eve/internal/api"
)

// Summarizer describes a failure no rule recognized (PLAN.md §9, "Luna"). It only ever writes
// the summary and hint of an unknown failure: never the class, and never a control decision.
type Summarizer interface {
	Summarize(ctx context.Context, excerpt []string, phase api.State) (summary, hint string, confidence float64, err error)
}

// MinConfidence is the confidence below which a model's summary is discarded.
const MinConfidence = 0.3

// Refine returns f with a Summarizer's description when f is unknown and s is set; any other
// failure, a nil s, an error or a low-confidence answer returns f unchanged. f is not modified.
// excerpt is what the model sees: the caller decides how much (error lines plus the last 200
// lines, redacted), falling back to f.Excerpt.
func Refine(ctx context.Context, s Summarizer, f *api.Failure, phase api.State, excerpt []string) *api.Failure {
	if s == nil || f == nil || f.Class != ClassUnknown {
		return f
	}
	if len(excerpt) == 0 {
		excerpt = f.Excerpt
	}
	summary, hint, conf, err := s.Summarize(ctx, excerpt, phase)
	summary = strings.TrimSpace(summary)
	if err != nil || conf < MinConfidence || summary == "" {
		return f
	}
	out := *f
	out.Summary = truncate(summary, maxSummary)
	out.Hint = truncate(strings.TrimSpace(hint), maxHint)
	out.By = "model"
	return &out
}

// Responses is a Summarizer for an OpenAI-compatible Responses API endpoint, such as
// CLIProxyAPI in front of gpt-6-luna. Every field is injected; nothing is read from a default
// configuration, so it is never used unless the server config builds one.
type Responses struct {
	BaseURL   string // e.g. "https://cliproxyapi.tail6e682f.ts.net/v1"
	Model     string // e.g. "gpt-6-luna"
	APIKeyEnv string // name of the environment variable holding the key, e.g. "CLIPROXY_API_KEY"
	Client    *http.Client
	// Getenv reads APIKeyEnv; nil means os.Getenv.
	Getenv func(string) string
}

const instructions = `You explain why an iOS build job failed, for a coding agent that will fix it.
You get the failed phase and a redacted excerpt of the xcodebuild/devicectl/App Store Connect log.
Reply with JSON only: {"summary": one sentence (max 200 characters) saying what failed and why,
"hint": one sentence (max 300 characters) with the most likely fix, "confidence": 0 to 1}.
Quote exact error text where it helps. Never invent file names, keys or numbers that are not in
the excerpt. If the excerpt does not show the cause, say so and use a confidence below 0.3.`

var schema = json.RawMessage(`{"type":"object","additionalProperties":false,` +
	`"required":["summary","hint","confidence"],"properties":{"summary":{"type":"string"},` +
	`"hint":{"type":"string"},"confidence":{"type":"number"}}}`)

// Summarize implements Summarizer.
func (r Responses) Summarize(ctx context.Context, excerpt []string, phase api.State) (string, string, float64, error) {
	getenv := r.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	key := getenv(r.APIKeyEnv)
	if key == "" {
		return "", "", 0, fmt.Errorf("classify: %s is not set", r.APIKeyEnv)
	}
	body, err := json.Marshal(map[string]any{
		"model":             r.Model,
		"instructions":      instructions,
		"input":             fmt.Sprintf("Phase: %s\nExcerpt:\n%s", phase, strings.Join(excerpt, "\n")),
		"max_output_tokens": 400,
		"store":             false,
		"text": map[string]any{"format": map[string]any{
			"type": "json_schema", "name": "failure_summary", "strict": true, "schema": schema,
		}},
	})
	if err != nil {
		return "", "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.BaseURL, "/")+"/responses", bytes.NewReader(body))
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", 0, err
	}
	if resp.StatusCode/100 != 2 {
		return "", "", 0, fmt.Errorf("classify: model endpoint: %s: %s", resp.Status, truncate(strings.TrimSpace(string(data)), 200))
	}
	text, err := outputText(data)
	if err != nil {
		return "", "", 0, err
	}
	var out struct {
		Summary    string  `json:"summary"`
		Hint       string  `json:"hint"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return "", "", 0, fmt.Errorf("classify: model reply is not the requested JSON: %w", err)
	}
	return out.Summary, out.Hint, min(max(out.Confidence, 0), 1), nil
}

// outputText extracts the assistant text from a Responses API reply: the output_text parts of
// its message items, or the convenience output_text field some proxies add.
func outputText(data []byte) (string, error) {
	var resp struct {
		Status     string `json:"status"`
		OutputText string `json:"output_text"`
		Output     []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("classify: model endpoint returned invalid JSON: %w", err)
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return "", fmt.Errorf("classify: model error: %s", resp.Error.Message)
	}
	var b strings.Builder
	for _, item := range resp.Output {
		if item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			if c.Type == "output_text" {
				b.WriteString(c.Text)
			}
		}
	}
	if b.Len() == 0 {
		b.WriteString(resp.OutputText)
	}
	if b.Len() == 0 {
		return "", errors.New("classify: model reply has no output text")
	}
	return b.String(), nil
}
