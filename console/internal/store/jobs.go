package store

import (
	"database/sql"
	"errors"
)

const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
)

// Job is work that takes time, and outlives a restart of the daemon or the unit. BootID is the boot
// that last touched it, which is how a reboot job knows it finished.
type Job struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	Actor     string `json:"actor"`
	State     string `json:"state"`
	Result    string `json:"result"`
	BootID    string `json:"boot_id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

const jobColumns = `id, kind, target, actor, state, result, boot_id, created_at, updated_at`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Kind, &j.Target, &j.Actor, &j.State, &j.Result, &j.BootID, &j.CreatedAt, &j.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	return j, err
}

// CreateJob queues a job, or returns the one already made with this idempotency key, with created
// false. The audit line goes in the same transaction: work that can't be recorded doesn't happen.
func (s *Store) CreateJob(j Job, key string, a Audit) (Job, bool, error) {
	created := false
	err := s.tx(func(tx *sql.Tx) error {
		var err error
		existing, err := scanJob(tx.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE idempotency_key = ?`, key))
		if err == nil {
			j = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		t := now()
		res, err := tx.Exec(`INSERT INTO jobs (kind, target, actor, idempotency_key, state, boot_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, j.Kind, j.Target, j.Actor, key, JobQueued, j.BootID, t, t)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		j, err = scanJob(tx.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id))
		if err != nil {
			return err
		}
		created = true
		return addAudit(tx, a)
	})
	return j, created, err
}

func (s *Store) Job(id int64) (Job, error) {
	return scanJob(s.db.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id))
}

func (s *Store) Jobs(limit int) ([]Job, error) {
	return s.queryJobs(`SELECT `+jobColumns+` FROM jobs ORDER BY id DESC LIMIT ?`, limit)
}

// OpenJobs are those not finished, oldest first.
func (s *Store) OpenJobs() ([]Job, error) {
	return s.queryJobs(`SELECT ` + jobColumns + ` FROM jobs WHERE state IN ('queued', 'running') ORDER BY id`)
}

func (s *Store) queryJobs(query string, args ...any) ([]Job, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, j)
	}
	return list, rows.Err()
}

func (s *Store) SetJob(id int64, state, result, bootID string) error {
	_, err := s.db.Exec(`UPDATE jobs SET state = ?, result = ?, boot_id = ?, updated_at = ? WHERE id = ?`,
		state, result, bootID, now(), id)
	return err
}
