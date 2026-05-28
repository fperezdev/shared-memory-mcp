package local

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"
)

// Operation types stored in pending_writes.op.
const (
	OpUpsertEntity      = "upsert_entity"
	OpAddObservation    = "add_observation"
	OpUpsertRelation    = "upsert_relation"
	OpDeleteEntity      = "delete_entity"
	OpDeleteObservation = "delete_observation"
	OpDeleteRelation    = "delete_relation"
	OpAudit             = "audit"
)

// Pending is one queued mutation waiting to be applied to Postgres.
type Pending struct {
	ID         int64
	Op         string
	Payload    string // JSON
	EnqueuedAt string
	Attempts   int
}

// Enqueue appends an operation. Returns the pending_writes row id.
func Enqueue(ctx context.Context, db *sql.DB, op string, payload any) (int64, error) {
	body, err := MarshalPayload(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal payload: %w", err)
	}
	res, err := db.ExecContext(ctx, `
		insert into pending_writes (op, payload, enqueued_at)
		values (?, ?, ?)
	`, op, body, nowISO())
	if err != nil {
		return 0, fmt.Errorf("enqueue: %w", err)
	}
	return res.LastInsertId()
}

// PeekDue returns up to limit operations whose next_attempt_at has passed,
// oldest first. The push loop iterates these and applies each in order.
func PeekDue(ctx context.Context, db *sql.DB, limit int) ([]Pending, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.QueryContext(ctx, `
		select id, op, payload, enqueued_at, attempts
		from pending_writes
		where datetime(next_attempt_at) <= datetime('now')
		order by id asc
		limit ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Pending{}
	for rows.Next() {
		var p Pending
		if err := rows.Scan(&p.ID, &p.Op, &p.Payload, &p.EnqueuedAt, &p.Attempts); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkSuccess deletes the queue entry after a successful push.
func MarkSuccess(ctx context.Context, db *sql.DB, id int64) error {
	_, err := db.ExecContext(ctx, `delete from pending_writes where id = ?`, id)
	return err
}

// MarkFailure increments attempts and schedules the next try via
// exponential backoff capped at 5 minutes. last_error stores the message
// for diagnostic queries.
func MarkFailure(ctx context.Context, db *sql.DB, id int64, attempts int, errMsg string) error {
	delay := backoff(attempts + 1)
	next := time.Now().UTC().Add(delay).Format(time.RFC3339Nano)
	_, err := db.ExecContext(ctx, `
		update pending_writes
		set attempts = attempts + 1, next_attempt_at = ?, last_error = ?
		where id = ?
	`, next, errMsg, id)
	return err
}

// CountPending returns how many ops are waiting (regardless of due time).
func CountPending(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `select count(*) from pending_writes`).Scan(&n)
	return n, err
}

// backoff returns 2^attempts seconds, capped at 5min. attempt=1 → 2s,
// attempt=2 → 4s, … attempt=8 → 256s, attempt>=9 → 300s.
func backoff(attempts int) time.Duration {
	const cap = 5 * time.Minute
	d := time.Duration(math.Pow(2, float64(attempts))) * time.Second
	if d > cap || d <= 0 {
		return cap
	}
	return d
}
