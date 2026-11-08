package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Simulator records exact ownership, an explicit job lease and one pending shutdown.
// Zero ReleasedAt is a lease, not an age-based expiry. Settled devices are never shut down
// again: a later boot may belong to another workflow or a person. A settled device that is
// still stopped and unbooted since SettledAt may be deleted once; DeletedAt records that.
type Simulator struct {
	UDID            string    `json:"udid"`
	Name            string    `json:"name"`
	JobID           string    `json:"jobId"`
	ReleasedAt      time.Time `json:"releasedAt"`
	Settled         bool      `json:"settled"`
	SettledAt       time.Time `json:"settledAt"`
	DeletedAt       time.Time `json:"deletedAt"`
	LegacyProtected bool      `json:"legacyProtected,omitempty"` // old indefinite pins need explicit conversion
	LeaseUntil      time.Time `json:"leaseUntil"`
	LastAttemptAt   time.Time `json:"lastAttemptAt"`
	LastError       string    `json:"lastError,omitempty"`
}

func (s *Store) RegisterSimulator(ctx context.Context, id, name, job string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO simulators (udid,name,job_id) VALUES (?,?,?)`, id, name, job)
	return err
}

func (s *Store) Simulators(ctx context.Context) ([]Simulator, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT udid,name,job_id,released_at,settled,settled_at,deleted_at,protected,lease_until,last_attempt_at,last_error FROM simulators ORDER BY last_attempt_at,udid`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Simulator{}
	for rows.Next() {
		var d Simulator
		var released, settledAt, deletedAt, attempted, until int64
		if err := rows.Scan(&d.UDID, &d.Name, &d.JobID, &released, &d.Settled, &settledAt, &deletedAt, &d.LegacyProtected, &until, &attempted, &d.LastError); err != nil {
			return nil, err
		}
		d.ReleasedAt = fromMS(released)
		d.SettledAt = fromMS(settledAt)
		d.DeletedAt = fromMS(deletedAt)
		d.LastAttemptAt = fromMS(attempted)
		d.LeaseUntil = fromMS(until)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ReleaseSimulator records activity once, when the job actually stops using the device.
func (s *Store) ReleaseSimulator(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE simulators SET released_at=? WHERE udid=? AND released_at=0 AND settled=0`, ms(at), id)
	return err
}

// SimulatorCleanup records a shutdown attempt. The first success also sets settled_at, the
// start of the deletion grace period.
func (s *Store) SimulatorCleanup(ctx context.Context, id string, settled bool, detail string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE simulators SET settled=?,last_error=?,last_attempt_at=?,
		settled_at=CASE WHEN ? AND settled_at=0 THEN ? ELSE settled_at END WHERE udid=?`,
		settled, detail, ms(at), settled, ms(at), id)
	return err
}

// SimulatorDeletion records a deletion attempt on a settled device: deleted (or already gone)
// ends its lifecycle; otherwise detail says why it was kept. Unchanged details are not rewritten.
func (s *Store) SimulatorDeletion(ctx context.Context, id string, deleted bool, detail string, at time.Time) error {
	if deleted {
		_, err := s.db.ExecContext(ctx, `UPDATE simulators SET deleted_at=?,last_error='',last_attempt_at=? WHERE udid=? AND deleted_at=0`, ms(at), ms(at), id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE simulators SET last_error=?,last_attempt_at=? WHERE udid=? AND deleted_at=0 AND last_error<>?`, detail, ms(at), id, detail)
	return err
}

var ErrSimulatorSettled = errors.New("Simulator cleanup already settled; only devices awaiting Eve cleanup can be leased")

// RenewSimulator converts legacy pins explicitly and never rearms settled cleanup.
func (s *Store) RenewSimulator(ctx context.Context, id string, until time.Time) error {
	r, err := s.db.ExecContext(ctx, `UPDATE simulators SET protected=0,lease_until=? WHERE udid=? AND settled=0`, ms(until), id)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil || n > 0 {
		return err
	}
	var settled bool
	if err := s.db.QueryRowContext(ctx, `SELECT settled FROM simulators WHERE udid=?`, id).Scan(&settled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return ErrSimulatorSettled
}

// ReleaseSimulatorLease ends borrowing, without expiring the independent job lease or
// changing its idle timestamp. Already-idle devices can become eligible on the next sweep.
func (s *Store) ReleaseSimulatorLease(ctx context.Context, id string) error {
	r, err := s.db.ExecContext(ctx, `UPDATE simulators SET protected=0,lease_until=0 WHERE udid=?`, id)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}
