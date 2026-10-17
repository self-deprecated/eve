// Package store is eve's SQLite database: jobs, their events, the notification outbox, build
// numbers, tokens and size-guard bookkeeping. A state transition, its event and its outbox rows
// commit in one transaction, so a crash can never lose a notification (PLAN.md §5.1).
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/self-deprecated/eve/internal/api"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a single connection keeps transactions simple and serial.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// SetClock replaces the clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

var migrations = []string{
	`CREATE TABLE jobs (
		id TEXT PRIMARY KEY,
		app TEXT NOT NULL,
		verb TEXT NOT NULL,
		state TEXT NOT NULL,
		phase TEXT NOT NULL,
		idempotency_key TEXT NOT NULL,
		source_sha TEXT NOT NULL DEFAULT '',
		request TEXT NOT NULL,
		result TEXT NOT NULL DEFAULT '{}',
		build_version TEXT NOT NULL DEFAULT '',
		build_number TEXT NOT NULL DEFAULT '',
		upload_intent TEXT NOT NULL DEFAULT '',
		pgid INTEGER NOT NULL DEFAULT 0,
		priority INTEGER NOT NULL DEFAULT 0,
		phase_attempts INTEGER NOT NULL DEFAULT 0,
		deadline_at INTEGER NOT NULL DEFAULT 0,
		token_name TEXT NOT NULL DEFAULT '',
		requester TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		started_at INTEGER NOT NULL DEFAULT 0,
		finished_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE UNIQUE INDEX jobs_idem ON jobs(idempotency_key)`,
	`CREATE INDEX jobs_state ON jobs(state, priority DESC, created_at)`,
	`CREATE INDEX jobs_app ON jobs(app, created_at DESC)`,
	`CREATE TABLE events (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id TEXT NOT NULL REFERENCES jobs(id),
		time INTEGER NOT NULL,
		kind TEXT NOT NULL,
		state TEXT NOT NULL,
		message TEXT NOT NULL DEFAULT '',
		data TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX events_job ON events(job_id, seq)`,
	`CREATE TABLE outbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id TEXT NOT NULL REFERENCES jobs(id),
		event TEXT NOT NULL,
		target_kind TEXT NOT NULL,
		target_address TEXT NOT NULL,
		delivery_id TEXT NOT NULL,
		payload TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending',
		attempts INTEGER NOT NULL DEFAULT 0,
		next_at INTEGER NOT NULL,
		last_error TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL
	)`,
	`CREATE INDEX outbox_due ON outbox(status, next_at)`,
	`CREATE TABLE build_numbers (app TEXT PRIMARY KEY, last TEXT NOT NULL)`,
	`CREATE TABLE tokens (
		name TEXT PRIMARY KEY,
		hash TEXT NOT NULL UNIQUE,
		owner INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		last_used_at INTEGER NOT NULL DEFAULT 0,
		revoked_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE source_stats (app TEXT PRIMARY KEY, stats TEXT NOT NULL, job_id TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
	`CREATE TABLE baselines (app TEXT PRIMARY KEY, job_id TEXT NOT NULL, build_number TEXT NOT NULL, path TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
	`ALTER TABLE jobs ADD COLUMN resume_from TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN confirmation TEXT NOT NULL DEFAULT ''`,
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (v INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT v FROM schema_version`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_version (v) VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		if _, err := s.db.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE schema_version SET v = ?`, i+1); err != nil {
			return err
		}
	}
	return nil
}

// Job is a row of the jobs table.
type Job struct {
	ID             string
	App            string
	Verb           api.Verb
	State          api.State
	Phase          api.State // last non-terminal state; where recovery resumes
	IdempotencyKey string
	SourceSHA      string
	Request        api.JobRequest
	Result         api.Result
	BuildVersion   string
	BuildNumber    string
	UploadIntent   string    // non-empty once delivery was attempted (PLAN.md §5.5)
	ResumeFrom     api.State // where a requeued job continues (restart recovery, size confirmation)
	Confirmation   string    // the agent's reason when it confirmed a size-guard pause
	Pgid           int
	Priority       int
	PhaseAttempts  int
	DeadlineAt     time.Time
	TokenName      string
	Requester      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	StartedAt      time.Time
	FinishedAt     time.Time
}

// NewJobID returns a sortable, unique job id.
func NewJobID(now time.Time) string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("j_%s%s", strings.ToLower(big.NewInt(now.UnixMilli()).Text(36)), hex.EncodeToString(b[:]))
}

// NewKey returns a random idempotency key.
func NewKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// CreateJob inserts a queued job. If the request carries an idempotency key that already exists,
// the existing job is returned with existing=true. Without a key, a non-terminal job with the
// same app, verb and source digest is returned instead of starting a duplicate (PLAN.md §5.4).
func (s *Store) CreateJob(ctx context.Context, j Job) (Job, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	if j.IdempotencyKey != "" {
		if existing, err := getJob(ctx, tx, `idempotency_key = ?`, j.IdempotencyKey); err == nil {
			return existing, true, nil
		} else if !errors.Is(err, ErrNotFound) {
			return Job{}, false, err
		}
	} else {
		if j.SourceSHA != "" {
			live, err := listJobs(ctx, tx, `app = ? AND verb = ? AND source_sha = ? AND state NOT IN (`+terminalList+`) ORDER BY created_at DESC LIMIT 1`,
				j.App, string(j.Verb), j.SourceSHA)
			if err != nil {
				return Job{}, false, err
			}
			if len(live) == 1 {
				return live[0], true, nil
			}
		}
		j.IdempotencyKey = NewKey()
	}

	now := s.now()
	if j.ID == "" {
		j.ID = NewJobID(now)
	}
	j.State, j.Phase = api.StateQueued, api.StateQueued
	j.CreatedAt, j.UpdatedAt = now, now
	j.Result = api.Result{ID: j.ID, App: j.App, Verb: j.Verb, Outcome: api.StateQueued, Phase: api.StateQueued}
	req, _ := json.Marshal(j.Request)
	res, _ := json.Marshal(j.Result)
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs (id, app, verb, state, phase, idempotency_key, source_sha, request, result,
		token_name, requester, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.App, string(j.Verb), string(j.State), string(j.Phase), j.IdempotencyKey, j.SourceSHA, string(req), string(res),
		j.TokenName, j.Requester, ms(now), ms(now)); err != nil {
		return Job{}, false, err
	}
	if _, err := insertEvent(ctx, tx, j.ID, now, api.EventTransition, api.StateQueued, "queued", nil); err != nil {
		return Job{}, false, err
	}
	return j, false, tx.Commit()
}

// Get returns one job.
func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	return getJob(ctx, s.db, `id = ?`, id)
}

// Filter narrows List.
type Filter struct {
	App   string
	State api.State
	Limit int
}

// List returns jobs, newest first.
func (s *Store) List(ctx context.Context, f Filter) ([]Job, error) {
	where, args := []string{"1=1"}, []any{}
	if f.App != "" {
		where, args = append(where, "app = ?"), append(args, f.App)
	}
	if f.State != "" {
		where, args = append(where, "state = ?"), append(args, string(f.State))
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	args = append(args, limit)
	return listJobs(ctx, s.db, strings.Join(where, " AND ")+` ORDER BY created_at DESC LIMIT ?`, args...)
}

// NextQueued returns the queued job to run next (highest priority, then oldest), or ErrNotFound.
func (s *Store) NextQueued(ctx context.Context) (Job, error) {
	jobs, err := listJobs(ctx, s.db, `state = ? ORDER BY priority DESC, created_at ASC LIMIT 1`, string(api.StateQueued))
	if err != nil {
		return Job{}, err
	}
	if len(jobs) == 0 {
		return Job{}, ErrNotFound
	}
	return jobs[0], nil
}

// Unfinished returns every non-terminal job, oldest first (restart recovery).
func (s *Store) Unfinished(ctx context.Context) ([]Job, error) {
	return listJobs(ctx, s.db, `state NOT IN (`+terminalList+`) ORDER BY created_at ASC`)
}

// FinishedBefore returns terminal jobs that finished before t, oldest first.
func (s *Store) FinishedBefore(ctx context.Context, t time.Time) ([]Job, error) {
	return listJobs(ctx, s.db, `state IN (`+terminalList+`) AND finished_at > 0 AND finished_at < ? ORDER BY finished_at ASC`, ms(t))
}

// BaselineJobs returns the ids of the jobs whose bundle manifests are an app's size baseline.
func (s *Store) BaselineJobs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id FROM baselines`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// NewDelivery is an outbox row to create with a transition.
type NewDelivery struct {
	Event         string
	TargetKind    string
	TargetAddress string
	Payload       api.Notification
}

// Transition describes a state change.
type Transition struct {
	To      api.State
	Message string
	Data    any
	// Mutate may change other job fields inside the same transaction (build number, pgid, result…).
	Mutate func(*Job)
	// Deliveries are created atomically with the change; their payload's DeliveryID is filled in.
	Deliveries func(j Job) []NewDelivery
}

// Transition applies t to job id in one transaction and returns the updated job and its event.
// Transitions out of a terminal state are refused.
func (s *Store) Transition(ctx context.Context, id string, t Transition) (Job, api.Event, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, api.Event{}, err
	}
	defer func() { _ = tx.Rollback() }()
	j, err := getJob(ctx, tx, `id = ?`, id)
	if err != nil {
		return Job{}, api.Event{}, err
	}
	if j.State.Terminal() {
		return j, api.Event{}, fmt.Errorf("job %s is already %s", id, j.State)
	}
	now := s.now()
	prev := j.State
	if t.To != "" {
		j.State = t.To
		if !t.To.Terminal() {
			if t.To != prev {
				j.PhaseAttempts = 0
			}
			j.Phase = t.To
		} else {
			j.FinishedAt = now
		}
		if prev == api.StateQueued && t.To != api.StateQueued && j.StartedAt.IsZero() {
			j.StartedAt = now
		}
	}
	if t.Mutate != nil {
		t.Mutate(&j)
	}
	j.UpdatedAt = now
	j.Result.ID, j.Result.App, j.Result.Verb = j.ID, j.App, j.Verb
	j.Result.Outcome = j.State
	j.Result.Phase = j.Phase
	if j.State == api.StateSucceeded {
		j.Result.Phase = "" // a finished job has no phase; failures keep the one they stopped in
	}
	if !j.StartedAt.IsZero() {
		end := now
		if !j.FinishedAt.IsZero() {
			end = j.FinishedAt
		}
		j.Result.DurationS = int(end.Sub(j.StartedAt).Seconds())
	}
	if err := updateJob(ctx, tx, j); err != nil {
		return Job{}, api.Event{}, err
	}
	var data []byte
	if t.Data != nil {
		data, _ = json.Marshal(t.Data)
	}
	kind := api.EventTransition
	if t.To == "" || t.To == prev {
		kind = api.EventMessage
	}
	ev, err := insertEvent(ctx, tx, j.ID, now, kind, j.State, t.Message, data)
	if err != nil {
		return Job{}, api.Event{}, err
	}
	if t.Deliveries != nil {
		for _, d := range t.Deliveries(j) {
			d.Payload.DeliveryID = fmt.Sprintf("%s:%d:%s:%s", j.ID, ev.Seq, d.TargetKind, shortHash(d.TargetAddress))
			payload, _ := json.Marshal(d.Payload)
			if _, err := tx.ExecContext(ctx, `INSERT INTO outbox (job_id, event, target_kind, target_address, delivery_id, payload, next_at, created_at)
				VALUES (?,?,?,?,?,?,?,?)`, j.ID, d.Event, d.TargetKind, d.TargetAddress, d.Payload.DeliveryID, string(payload), ms(now), ms(now)); err != nil {
				return Job{}, api.Event{}, err
			}
		}
	}
	return j, ev, tx.Commit()
}

// Update changes job fields without a state change or event (pgid, upload intent, …).
func (s *Store) Update(ctx context.Context, id string, mutate func(*Job)) (Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer func() { _ = tx.Rollback() }()
	j, err := getJob(ctx, tx, `id = ?`, id)
	if err != nil {
		return Job{}, err
	}
	mutate(&j)
	j.UpdatedAt = s.now()
	if err := updateJob(ctx, tx, j); err != nil {
		return Job{}, err
	}
	return j, tx.Commit()
}

// Message appends an informational event without changing state.
func (s *Store) Message(ctx context.Context, id, msg string) (api.Event, error) {
	j, err := s.Get(ctx, id)
	if err != nil {
		return api.Event{}, err
	}
	return insertEvent(ctx, s.db, id, s.now(), api.EventMessage, j.State, msg, nil)
}

// Events returns a job's events after seq.
func (s *Store) Events(ctx context.Context, id string, after int64) ([]api.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, job_id, time, kind, state, message, data FROM events WHERE job_id = ? AND seq > ? ORDER BY seq`, id, after)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []api.Event
	for rows.Next() {
		var e api.Event
		var t int64
		var state, data string
		if err := rows.Scan(&e.Seq, &e.JobID, &t, &e.Kind, &state, &e.Message, &data); err != nil {
			return nil, err
		}
		e.Time, e.State = fromMS(t), api.State(state)
		if data != "" {
			e.Data = json.RawMessage(data)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// OutboxRow is a pending or finished delivery.
type OutboxRow struct {
	ID            int64
	JobID         string
	Event         string
	TargetKind    string
	TargetAddress string
	DeliveryID    string
	Payload       api.Notification
	Status        string // pending | delivered | exhausted
	Attempts      int
	NextAt        time.Time
	LastError     string
	CreatedAt     time.Time
}

// OutboxDue returns pending deliveries whose next attempt is due.
func (s *Store) OutboxDue(ctx context.Context, now time.Time, limit int) ([]OutboxRow, error) {
	return s.outbox(ctx, `status = 'pending' AND next_at <= ? ORDER BY next_at LIMIT ?`, ms(now), limit)
}

// OutboxForJob returns all deliveries of a job.
func (s *Store) OutboxForJob(ctx context.Context, jobID string) ([]OutboxRow, error) {
	return s.outbox(ctx, `job_id = ? ORDER BY id`, jobID)
}

func (s *Store) outbox(ctx context.Context, where string, args ...any) ([]OutboxRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, job_id, event, target_kind, target_address, delivery_id, payload, status, attempts,
		next_at, last_error, created_at FROM outbox WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []OutboxRow
	for rows.Next() {
		var r OutboxRow
		var payload string
		var next, created int64
		if err := rows.Scan(&r.ID, &r.JobID, &r.Event, &r.TargetKind, &r.TargetAddress, &r.DeliveryID, &payload, &r.Status,
			&r.Attempts, &next, &r.LastError, &created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(payload), &r.Payload)
		r.NextAt, r.CreatedAt = fromMS(next), fromMS(created)
		out = append(out, r)
	}
	return out, rows.Err()
}

// OutboxDelivered marks a delivery done.
func (s *Store) OutboxDelivered(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbox SET status = 'delivered', attempts = attempts + 1, last_error = '' WHERE id = ?`, id)
	return err
}

// OutboxRetry schedules the next attempt.
func (s *Store) OutboxRetry(ctx context.Context, id int64, attempts int, next time.Time, lastErr string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbox SET attempts = ?, next_at = ?, last_error = ? WHERE id = ?`, attempts, ms(next), lastErr, id)
	return err
}

// OutboxExhausted gives up on a delivery; it stays visible in `eve status`.
func (s *Store) OutboxExhausted(ctx context.Context, id int64, lastErr string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbox SET status = 'exhausted', attempts = attempts + 1, last_error = ? WHERE id = ?`, lastErr, id)
	return err
}

// AllocateBuildNumber returns the next build number for app: the largest of the UTC minute
// stamp, the last allocated number + 1 and floor (e.g. App Store Connect's latest + 1). With
// requested set, it is validated against the same floor and reserved instead (PLAN.md §6.3).
func (s *Store) AllocateBuildNumber(ctx context.Context, app, floor, requested string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var last string
	err = tx.QueryRowContext(ctx, `SELECT last FROM build_numbers WHERE app = ?`, app).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	min := maxNum(incr(last), floor)
	var n string
	if requested != "" {
		if !isDigits(requested) {
			return "", fmt.Errorf("build number %q must be digits", requested)
		}
		if min != "" && cmpNum(requested, min) < 0 {
			return "", fmt.Errorf("build number %s is not above the last used (%s)", requested, last+"/"+floor)
		}
		n = requested
	} else {
		n = maxNum(s.now().UTC().Format("200601021504"), min)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO build_numbers (app, last) VALUES (?, ?) ON CONFLICT(app) DO UPDATE SET last = excluded.last`, app, n); err != nil {
		return "", err
	}
	return n, tx.Commit()
}

// CreateToken stores a new named token and returns its plaintext once.
func (s *Store) CreateToken(ctx context.Context, name string, owner bool) (string, error) {
	var b [24]byte
	_, _ = rand.Read(b[:])
	tok := "eve_" + hex.EncodeToString(b[:])
	o := 0
	if owner {
		o = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tokens (name, hash, owner, created_at) VALUES (?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET hash = excluded.hash, owner = excluded.owner, created_at = excluded.created_at, revoked_at = 0`,
		name, hashToken(tok), o, ms(s.now()))
	if err != nil {
		return "", err
	}
	return tok, nil
}

// TokenInfo describes a stored token.
type TokenInfo struct {
	Name       string
	Owner      bool
	CreatedAt  time.Time
	LastUsedAt time.Time
	Revoked    bool
}

// VerifyToken returns the token's info, or ErrNotFound for unknown or revoked tokens.
func (s *Store) VerifyToken(ctx context.Context, tok string) (TokenInfo, error) {
	var ti TokenInfo
	var owner int
	var created, used, revoked int64
	err := s.db.QueryRowContext(ctx, `SELECT name, owner, created_at, last_used_at, revoked_at FROM tokens WHERE hash = ?`, hashToken(tok)).
		Scan(&ti.Name, &owner, &created, &used, &revoked)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && revoked != 0) {
		return TokenInfo{}, ErrNotFound
	}
	if err != nil {
		return TokenInfo{}, err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE name = ?`, ms(s.now()), ti.Name)
	ti.Owner, ti.CreatedAt, ti.LastUsedAt = owner == 1, fromMS(created), fromMS(used)
	return ti, nil
}

// ListTokens returns all tokens.
func (s *Store) ListTokens(ctx context.Context) ([]TokenInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, owner, created_at, last_used_at, revoked_at FROM tokens ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TokenInfo
	for rows.Next() {
		var ti TokenInfo
		var owner int
		var created, used, revoked int64
		if err := rows.Scan(&ti.Name, &owner, &created, &used, &revoked); err != nil {
			return nil, err
		}
		ti.Owner, ti.CreatedAt, ti.LastUsedAt, ti.Revoked = owner == 1, fromMS(created), fromMS(used), revoked != 0
		out = append(out, ti)
	}
	return out, rows.Err()
}

// RevokeToken revokes a token by name.
func (s *Store) RevokeToken(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tokens SET revoked_at = ? WHERE name = ? AND revoked_at = 0`, ms(s.now()), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SourceStats returns the last accepted source stats for app.
func (s *Store) SourceStats(ctx context.Context, app string) (*api.SourceStats, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT stats FROM source_stats WHERE app = ?`, app).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st api.SourceStats
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// SetSourceStats records the source stats of an accepted job.
func (s *Store) SetSourceStats(ctx context.Context, app, jobID string, st api.SourceStats) error {
	raw, _ := json.Marshal(st)
	_, err := s.db.ExecContext(ctx, `INSERT INTO source_stats (app, stats, job_id, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(app) DO UPDATE SET stats = excluded.stats, job_id = excluded.job_id, updated_at = excluded.updated_at`,
		app, string(raw), jobID, ms(s.now()))
	return err
}

// Baseline is the accepted bundle manifest for an app.
type Baseline struct {
	App         string
	JobID       string
	BuildNumber string
	Path        string
	UpdatedAt   time.Time
}

// Baseline returns app's baseline, or nil if there is none.
func (s *Store) Baseline(ctx context.Context, app string) (*Baseline, error) {
	var b Baseline
	var t int64
	err := s.db.QueryRowContext(ctx, `SELECT app, job_id, build_number, path, updated_at FROM baselines WHERE app = ?`, app).
		Scan(&b.App, &b.JobID, &b.BuildNumber, &b.Path, &t)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.UpdatedAt = fromMS(t)
	return &b, nil
}

// SetBaseline records a new baseline.
func (s *Store) SetBaseline(ctx context.Context, b Baseline) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO baselines (app, job_id, build_number, path, updated_at) VALUES (?,?,?,?,?)
		ON CONFLICT(app) DO UPDATE SET job_id = excluded.job_id, build_number = excluded.build_number, path = excluded.path, updated_at = excluded.updated_at`,
		b.App, b.JobID, b.BuildNumber, b.Path, ms(s.now()))
	return err
}

// --- helpers ---

var terminalList = func() string {
	states := []api.State{
		api.StateSucceeded, api.StateFailed, api.StateRejected, api.StateNeedsHuman,
		api.StateUnavailable, api.StateCancelled, api.StateInterrupted,
	}
	q := make([]string, len(states))
	for i, s := range states {
		q[i] = "'" + string(s) + "'"
	}
	return strings.Join(q, ",")
}()

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

const jobColumns = `id, app, verb, state, phase, idempotency_key, source_sha, request, result, build_version, build_number,
	upload_intent, pgid, priority, phase_attempts, deadline_at, token_name, requester, created_at, updated_at, started_at, finished_at,
	resume_from, confirmation`

func scanJob(sc interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var verb, state, phase, req, res, resume string
	var deadline, created, updated, started, finished int64
	if err := sc.Scan(&j.ID, &j.App, &verb, &state, &phase, &j.IdempotencyKey, &j.SourceSHA, &req, &res, &j.BuildVersion,
		&j.BuildNumber, &j.UploadIntent, &j.Pgid, &j.Priority, &j.PhaseAttempts, &deadline, &j.TokenName, &j.Requester,
		&created, &updated, &started, &finished, &resume, &j.Confirmation); err != nil {
		return Job{}, err
	}
	j.Verb, j.State, j.Phase, j.ResumeFrom = api.Verb(verb), api.State(state), api.State(phase), api.State(resume)
	_ = json.Unmarshal([]byte(req), &j.Request)
	_ = json.Unmarshal([]byte(res), &j.Result)
	j.DeadlineAt, j.CreatedAt, j.UpdatedAt = fromMS(deadline), fromMS(created), fromMS(updated)
	j.StartedAt, j.FinishedAt = fromMS(started), fromMS(finished)
	return j, nil
}

func getJob(ctx context.Context, q querier, where string, args ...any) (Job, error) {
	j, err := scanJob(q.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return j, err
}

func listJobs(ctx context.Context, q querier, where string, args ...any) ([]Job, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func updateJob(ctx context.Context, q querier, j Job) error {
	req, _ := json.Marshal(j.Request)
	res, _ := json.Marshal(j.Result)
	_, err := q.ExecContext(ctx, `UPDATE jobs SET state=?, phase=?, request=?, result=?, build_version=?, build_number=?, upload_intent=?,
		pgid=?, priority=?, phase_attempts=?, deadline_at=?, updated_at=?, started_at=?, finished_at=?, resume_from=?, confirmation=? WHERE id=?`,
		string(j.State), string(j.Phase), string(req), string(res), j.BuildVersion, j.BuildNumber, j.UploadIntent,
		j.Pgid, j.Priority, j.PhaseAttempts, ms(j.DeadlineAt), ms(j.UpdatedAt), ms(j.StartedAt), ms(j.FinishedAt),
		string(j.ResumeFrom), j.Confirmation, j.ID)
	return err
}

func insertEvent(ctx context.Context, q querier, jobID string, t time.Time, kind string, state api.State, msg string, data []byte) (api.Event, error) {
	res, err := q.ExecContext(ctx, `INSERT INTO events (job_id, time, kind, state, message, data) VALUES (?,?,?,?,?,?)`,
		jobID, ms(t), kind, string(state), msg, string(data))
	if err != nil {
		return api.Event{}, err
	}
	seq, _ := res.LastInsertId()
	ev := api.Event{Seq: seq, JobID: jobID, Time: t, Kind: kind, State: state, Message: msg}
	if len(data) > 0 {
		ev.Data = data
	}
	return ev, nil
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// cmpNum compares two non-negative decimal strings numerically.
func cmpNum(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

func maxNum(a, b string) string {
	switch {
	case !isDigits(a):
		if isDigits(b) {
			return b
		}
		return ""
	case !isDigits(b):
		return a
	case cmpNum(a, b) >= 0:
		return a
	}
	return b
}

func incr(n string) string {
	if !isDigits(n) {
		return ""
	}
	v, _ := new(big.Int).SetString(n, 10)
	return v.Add(v, big.NewInt(1)).String()
}

// Enqueue adds a delivery outside a state transition (e.g. the human copy of an exhausted one).
func (s *Store) Enqueue(ctx context.Context, jobID string, d NewDelivery) error {
	now := s.now()
	d.Payload.DeliveryID = fmt.Sprintf("%s:x%d:%s:%s", jobID, now.UnixNano(), d.TargetKind, shortHash(d.TargetAddress))
	payload, _ := json.Marshal(d.Payload)
	_, err := s.db.ExecContext(ctx, `INSERT INTO outbox (job_id, event, target_kind, target_address, delivery_id, payload, next_at, created_at)
		VALUES (?,?,?,?,?,?,?,?)`, jobID, d.Event, d.TargetKind, d.TargetAddress, d.Payload.DeliveryID, string(payload), ms(now), ms(now))
	return err
}
