// Package server is eve's HTTP API (PLAN.md §11). It listens on loopback only and is published
// to the tailnet by the svc:eve Tailscale Service. Bearer tokens are the only authorization;
// Tailscale-User-Login is recorded for audit and never trusted (PLAN.md §4.2, §14).
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	"github.com/self-deprecated/eve/internal/config"
	"github.com/self-deprecated/eve/internal/queue"
	"github.com/self-deprecated/eve/internal/store"
)

// Sources stores uploaded source archives by digest.
type Sources interface {
	Has(sha string) bool
	Put(sha string, r io.Reader) error
}

// Server holds the API's dependencies. Optional hooks may be nil.
type Server struct {
	Config  *config.Server
	Store   *store.Store
	Queue   *queue.Queue
	Sources Sources
	Version string
	Log     *slog.Logger

	// AllowURL validates a --notify-url against notify.allowHosts.
	AllowURL func(raw string, allowHosts []string) error
	// SizeDetails returns the full size diff for a job (eve size).
	SizeDetails func(ctx context.Context, j store.Job) (any, error)
	// Builds lists an app's latest App Store Connect builds (eve builds).
	Builds func(ctx context.Context, app string) (any, error)
	// Doctor runs host checks (eve doctor --host).
	Doctor func(ctx context.Context) any
	// ErrorLines filters a log down to its relevant lines (?errors=1).
	ErrorLines func(log []byte, max int) []string
	// MaxUpload caps a source upload (compressed).
	MaxUpload int64
}

type ctxKey int

const tokenKey ctxKey = 0

// Handler returns the routed, authenticated handler.
func (s *Server) Handler() http.Handler {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.MaxUpload == 0 {
		s.MaxUpload = 1 << 30
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", s.version)
	mux.HandleFunc("HEAD /v1/sources/{sha}", s.headSource)
	mux.HandleFunc("PUT /v1/sources/{sha}", s.putSource)
	mux.HandleFunc("POST /v1/jobs", s.createJob)
	mux.HandleFunc("GET /v1/jobs", s.listJobs)
	mux.HandleFunc("GET /v1/jobs/{id}", s.getJob)
	mux.HandleFunc("GET /v1/jobs/{id}/events", s.events)
	mux.HandleFunc("GET /v1/jobs/{id}/logs", s.listLogs)
	mux.HandleFunc("GET /v1/jobs/{id}/logs/{phase}", s.getLog)
	mux.HandleFunc("GET /v1/jobs/{id}/artifacts/{name}", s.getArtifact)
	mux.HandleFunc("GET /v1/jobs/{id}/size", s.size)
	mux.HandleFunc("GET /v1/jobs/{id}/deliveries", s.deliveries)
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", s.cancel)
	mux.HandleFunc("POST /v1/jobs/{id}/confirm", s.confirm)
	mux.HandleFunc("POST /v1/jobs/{id}/reject", s.reject)
	mux.HandleFunc("GET /v1/apps", s.apps)
	mux.HandleFunc("GET /v1/apps/{app}/builds", s.builds)
	mux.HandleFunc("GET /v1/apps/{app}/source-stats", s.sourceStats)
	mux.HandleFunc("GET /v1/apps/{app}/baseline", s.getBaseline)
	mux.HandleFunc("PUT /v1/apps/{app}/baseline", s.putBaseline)
	mux.HandleFunc("GET /v1/doctor", s.doctor)
	return s.auth(mux)
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing bearer token; run `eve login`")
			return
		}
		ti, err := s.Store.VerifyToken(r.Context(), tok)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "unknown or revoked token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenKey, ti)))
	})
}

func tokenOf(r *http.Request) store.TokenInfo {
	ti, _ := r.Context().Value(tokenKey).(store.TokenInfo)
	return ti
}

func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.Version})
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Server) headSource(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if !shaRe.MatchString(sha) || !s.Sources.Has(sha) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) putSource(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if !shaRe.MatchString(sha) {
		writeErr(w, http.StatusBadRequest, "bad_digest", "digest must be 64 lowercase hex characters")
		return
	}
	if s.Sources.Has(sha) {
		writeJSON(w, http.StatusOK, map[string]string{"sha256": sha})
		return
	}
	body := http.MaxBytesReader(w, r.Body, s.MaxUpload)
	if err := s.Sources.Put(sha, body); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("source upload exceeds %d bytes", s.MaxUpload))
			return
		}
		writeErr(w, http.StatusBadRequest, "bad_source", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"sha256": sha})
}

var threadRe = regexp.MustCompile(`^[A-Za-z0-9._:%-]{1,200}$`)

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var req api.JobRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	ti := tokenOf(r)
	app, ok := s.Config.Apps[req.App]
	switch {
	case !ok:
		writeErr(w, http.StatusBadRequest, "unknown_app", fmt.Sprintf("app %q is not configured on the server (server.yml apps)", req.App))
		return
	case !req.Verb.Valid():
		writeErr(w, http.StatusBadRequest, "bad_verb", fmt.Sprintf("unknown verb %q", req.Verb))
		return
	case req.Verb == api.VerbExec && !app.AllowExec:
		writeErr(w, http.StatusForbidden, "exec_not_allowed", fmt.Sprintf("app %s does not allow exec (allowExec in server.yml)", req.App))
		return
	case req.Source.SHA256 == "" && req.Source.Path == "":
		writeErr(w, http.StatusBadRequest, "no_source", "a job needs an uploaded source (sha256) or a path")
		return
	case req.Source.SHA256 != "" && !s.Sources.Has(req.Source.SHA256):
		writeErr(w, http.StatusBadRequest, "unknown_source", "upload the source first (PUT /v1/sources/{sha256})")
		return
	case req.Source.Path != "" && !ti.Owner:
		writeErr(w, http.StatusForbidden, "path_needs_owner", "building a path in place needs the owner token on Quartz")
		return
	}
	if req.Source.Path != "" && !filepath.IsAbs(req.Source.Path) {
		writeErr(w, http.StatusBadRequest, "bad_path", "path must be absolute")
		return
	}
	for _, t := range req.NotifyT3 {
		if !threadRe.MatchString(t) {
			writeErr(w, http.StatusBadRequest, "bad_thread", fmt.Sprintf("invalid T3 thread id %q", t))
			return
		}
	}
	for _, u := range req.NotifyURLs {
		if s.AllowURL == nil {
			writeErr(w, http.StatusBadRequest, "notify_url_disabled", "webhook notifications are not configured")
			return
		}
		if err := s.AllowURL(u, s.Config.Notify.AllowHosts); err != nil {
			writeErr(w, http.StatusBadRequest, "notify_url_not_allowed", err.Error())
			return
		}
	}
	j, existing, err := s.Store.CreateJob(r.Context(), store.Job{
		App: req.App, Verb: req.Verb, SourceSHA: req.Source.SHA256, IdempotencyKey: req.IdempotencyKey,
		Request: req, TokenName: ti.Name, Requester: r.Header.Get("Tailscale-User-Login"),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store", err.Error())
		return
	}
	if !existing {
		s.Queue.Kick()
	}
	status := http.StatusCreated
	if existing {
		status = http.StatusOK
	}
	writeJSON(w, status, api.JobCreated{ID: j.ID, IdempotencyKey: j.IdempotencyKey, Existing: existing, State: j.State})
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	jobs, err := s.Store.List(r.Context(), store.Filter{App: r.URL.Query().Get("app"), State: api.State(r.URL.Query().Get("state")), Limit: limit})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store", err.Error())
		return
	}
	out := make([]api.Result, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.Result)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) (store.Job, bool) {
	j, err := s.Store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "no such job")
		return j, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store", err.Error())
		return j, false
	}
	return j, true
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	if j, ok := s.job(w, r); ok {
		writeJSON(w, http.StatusOK, j.Result)
	}
}

// events streams a job's events as SSE: replay after Last-Event-ID (or ?after=), then follow.
// The stream ends after a terminal event; awaiting_confirmation is sent and the stream stays open.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "no_streaming", "streaming unsupported")
		return
	}
	after, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if v := r.URL.Query().Get("after"); v != "" {
		after, _ = strconv.ParseInt(v, 10, 64)
	}
	// Subscribe before replaying so nothing committed in between is missed.
	live, stop := s.Queue.Hub.Subscribe(j.ID)
	defer stop()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(ev api.Event) bool {
		b, _ := json.Marshal(ev)
		_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, b)
		fl.Flush()
		after = ev.Seq
		return ev.State.Terminal() && ev.Kind == api.EventTransition
	}
	past, err := s.Store.Events(r.Context(), j.ID, after)
	if err != nil {
		return
	}
	for _, ev := range past {
		if send(ev) {
			return
		}
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-live:
			if !open {
				return // fell behind; the client reconnects with Last-Event-ID
			}
			if ev.Seq <= after {
				continue
			}
			if send(ev) {
				return
			}
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")
			fl.Flush()
		}
	}
}

func (s *Server) jobDir(id string) string { return filepath.Join(s.Config.DataDir, "jobs", id) }

func (s *Server) listLogs(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	entries, _ := os.ReadDir(filepath.Join(s.jobDir(j.ID), "logs"))
	var phases []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".log"); ok {
			phases = append(phases, name)
		}
	}
	sort.Strings(phases)
	writeJSON(w, http.StatusOK, phases)
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,120}$`)

func (s *Server) getLog(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	phase := r.PathValue("phase")
	if !nameRe.MatchString(phase) {
		writeErr(w, http.StatusBadRequest, "bad_phase", "invalid phase")
		return
	}
	b, err := os.ReadFile(filepath.Join(s.jobDir(j.ID), "logs", phase+".log"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "no log for phase "+phase)
		return
	}
	q := r.URL.Query()
	if q.Get("errors") == "1" && s.ErrorLines != nil {
		b = []byte(strings.Join(s.ErrorLines(b, 80), "\n") + "\n")
	} else if n, _ := strconv.Atoi(q.Get("tail")); n > 0 {
		b = tail(b, n)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) getArtifact(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !nameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "bad_name", "invalid artifact name")
		return
	}
	p := filepath.Join(s.jobDir(j.ID), "artifacts", name)
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		writeErr(w, http.StatusNotFound, "not_found", "no such artifact")
		return
	}
	http.ServeFile(w, r, p)
}

func (s *Server) size(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	if s.SizeDetails == nil {
		writeJSON(w, http.StatusOK, j.Result.Size)
		return
	}
	d, err := s.SizeDetails(r.Context(), j)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no_size", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) deliveries(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(w, r)
	if !ok {
		return
	}
	rows, err := s.Store.OutboxForJob(r.Context(), j.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store", err.Error())
		return
	}
	type row struct {
		Event, Target, Status, LastError string
		Attempts                         int
	}
	out := make([]row, 0, len(rows))
	for _, d := range rows {
		out = append(out, row{d.Event, d.TargetKind + ":" + d.TargetAddress, d.Status, d.LastError, d.Attempts})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	j, err := s.Queue.Cancel(r.Context(), r.PathValue("id"), r.URL.Query().Get("force") == "1")
	s.respondJob(w, j, err)
}

func (s *Server) confirm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body)
	j, err := s.Queue.Confirm(r.Context(), r.PathValue("id"), strings.TrimSpace(body.Reason))
	s.respondJob(w, j, err)
}

func (s *Server) reject(w http.ResponseWriter, r *http.Request) {
	j, err := s.Queue.Reject(r.Context(), r.PathValue("id"))
	s.respondJob(w, j, err)
}

func (s *Server) respondJob(w http.ResponseWriter, j store.Job, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "no such job")
	case errors.Is(err, queue.ErrNotCancellable):
		writeErr(w, http.StatusConflict, "delivering", err.Error())
	case err != nil:
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeJSON(w, http.StatusOK, j.Result)
	}
}

func (s *Server) apps(w http.ResponseWriter, r *http.Request) {
	type app struct {
		Name, BundleID, Signing string
		InternalOnly, AllowExec bool
	}
	out := []app{}
	for name, a := range s.Config.Apps {
		out = append(out, app{name, a.BundleID, a.SigningMode(), a.InternalOnly(), a.AllowExec})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) knownApp(w http.ResponseWriter, r *http.Request) (string, bool) {
	app := r.PathValue("app")
	if _, ok := s.Config.Apps[app]; !ok {
		writeErr(w, http.StatusNotFound, "unknown_app", fmt.Sprintf("app %q is not configured", app))
		return "", false
	}
	return app, true
}

func (s *Server) builds(w http.ResponseWriter, r *http.Request) {
	app, ok := s.knownApp(w, r)
	if !ok {
		return
	}
	if s.Builds == nil {
		writeErr(w, http.StatusNotImplemented, "no_asc", "App Store Connect is not configured")
		return
	}
	b, err := s.Builds(r.Context(), app)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "asc", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) sourceStats(w http.ResponseWriter, r *http.Request) {
	app, ok := s.knownApp(w, r)
	if !ok {
		return
	}
	st, err := s.Store.SourceStats(r.Context(), app)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st) // null when there is no accepted source yet
}

func (s *Server) getBaseline(w http.ResponseWriter, r *http.Request) {
	app, ok := s.knownApp(w, r)
	if !ok {
		return
	}
	b, err := s.Store.Baseline(r.Context(), app)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) putBaseline(w http.ResponseWriter, r *http.Request) {
	app, ok := s.knownApp(w, r)
	if !ok {
		return
	}
	var body struct {
		From string `json:"from"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil || body.From == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", `body must be {"from": "<job id>"}`)
		return
	}
	j, err := s.Store.Get(r.Context(), body.From)
	if err != nil || j.App != app {
		writeErr(w, http.StatusNotFound, "not_found", "no such job for this app")
		return
	}
	manifest := filepath.Join(s.jobDir(j.ID), "bundle-manifest.json.zst")
	if _, err := os.Stat(manifest); err != nil {
		writeErr(w, http.StatusConflict, "no_manifest", "that job has no bundle manifest")
		return
	}
	base := store.Baseline{App: app, JobID: j.ID, BuildNumber: j.BuildNumber, Path: manifest}
	if err := s.Store.SetBaseline(r.Context(), base); err != nil {
		writeErr(w, http.StatusInternalServerError, "store", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, base)
}

func (s *Server) doctor(w http.ResponseWriter, r *http.Request) {
	if s.Doctor == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "no host checks configured"})
		return
	}
	writeJSON(w, http.StatusOK, s.Doctor(r.Context()))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, api.ErrorBody{Error: msg, Code: code})
}

func tail(b []byte, n int) []byte {
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}
