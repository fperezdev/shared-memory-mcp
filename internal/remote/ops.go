package remote

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnsureProject upserts the projects row. Returns the (possibly existing)
// id. Project rows don't have updated_at/deleted_at — they're metadata.
func EnsureProject(ctx context.Context, pool *pgxpool.Pool, id, slug, name string) (string, error) {
	row := pool.QueryRow(ctx, `
		insert into projects (id, slug, name) values ($1, $2, $3)
		on conflict (slug) do update set name = excluded.name
		returning id::text
	`, id, slug, name)
	var got string
	if err := row.Scan(&got); err != nil {
		return "", fmt.Errorf("ensure project: %w", err)
	}
	return got, nil
}

// UpsertEntity inserts or updates by deterministic id. Re-upserting a
// previously deleted entity clears the tombstone. Returns the server's
// authoritative updated_at so the caller can sync the local row.
func UpsertEntity(ctx context.Context, pool *pgxpool.Pool, id, projectID, name, entityType, deviceID string) (time.Time, error) {
	var t time.Time
	err := pool.QueryRow(ctx, `
		insert into entities (id, project_id, name, entity_type, last_writer_device)
		values ($1, $2, $3, $4, $5)
		on conflict (id) do update set
		  entity_type        = excluded.entity_type,
		  deleted_at         = NULL,
		  last_writer_device = excluded.last_writer_device,
		  updated_at         = now()
		returning updated_at
	`, id, projectID, name, entityType, deviceID).Scan(&t)
	return t, err
}

// AddObservation appends a new observation. observationID is the client's
// random UUID. Returns server updated_at.
func AddObservation(ctx context.Context, pool *pgxpool.Pool, id, entityID, content, deviceID string) (time.Time, error) {
	var t time.Time
	err := pool.QueryRow(ctx, `
		insert into observations (id, entity_id, content, last_writer_device)
		values ($1, $2, $3, $4)
		on conflict (id) do update set
		  deleted_at         = NULL,
		  last_writer_device = excluded.last_writer_device,
		  updated_at         = now()
		returning updated_at
	`, id, entityID, content, deviceID).Scan(&t)
	return t, err
}

// UpsertRelation creates the directed edge. ON CONFLICT (id) handles
// re-creation of the same relation across devices (deterministic id).
func UpsertRelation(ctx context.Context, pool *pgxpool.Pool, id, projectID, fromID, toID, relationType, deviceID string) (time.Time, error) {
	var t time.Time
	err := pool.QueryRow(ctx, `
		insert into relations (id, project_id, from_entity_id, to_entity_id, relation_type, last_writer_device)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (id) do update set
		  deleted_at         = NULL,
		  last_writer_device = excluded.last_writer_device,
		  updated_at         = now()
		returning updated_at
	`, id, projectID, fromID, toID, relationType, deviceID).Scan(&t)
	return t, err
}

// DeleteEntity soft-deletes by id. No-op if already deleted (returns the
// existing updated_at). Returns zero time if not found.
func DeleteEntity(ctx context.Context, pool *pgxpool.Pool, id, deviceID string) (time.Time, error) {
	var t time.Time
	err := pool.QueryRow(ctx, `
		update entities
		set deleted_at = coalesce(deleted_at, now()), last_writer_device = $2
		where id = $1
		returning updated_at
	`, id, deviceID).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	return t, err
}

func DeleteObservation(ctx context.Context, pool *pgxpool.Pool, id, deviceID string) (time.Time, error) {
	var t time.Time
	err := pool.QueryRow(ctx, `
		update observations
		set deleted_at = coalesce(deleted_at, now()), last_writer_device = $2
		where id = $1
		returning updated_at
	`, id, deviceID).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	return t, err
}

func DeleteRelation(ctx context.Context, pool *pgxpool.Pool, id, deviceID string) (time.Time, error) {
	var t time.Time
	err := pool.QueryRow(ctx, `
		update relations
		set deleted_at = coalesce(deleted_at, now()), last_writer_device = $2
		where id = $1
		returning updated_at
	`, id, deviceID).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	return t, err
}

// WriteAudit persists one mutation record. occurred_at is the device's
// local clock; received_at is server-stamped (default value).
func WriteAudit(ctx context.Context, pool *pgxpool.Pool, id, projectID, deviceID, toolName, argsHash, occurredAt string) error {
	_, err := pool.Exec(ctx, `
		insert into audit_log (id, project_id, device_id, tool_name, args_hash, occurred_at)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (id) do nothing
	`, id, projectID, deviceID, toolName, argsHash, occurredAt)
	return err
}

// ---- Pull side --------------------------------------------------------

// EntityRow mirrors the Postgres entities columns we need locally.
type EntityRow struct {
	ID, ProjectID, Name, EntityType string
	CreatedAt, UpdatedAt            time.Time
	DeletedAt                       *time.Time
	LastWriterDevice                *string
}

type ObservationRow struct {
	ID, EntityID, Content string
	CreatedAt, UpdatedAt  time.Time
	DeletedAt             *time.Time
	LastWriterDevice      *string
}

type RelationRow struct {
	ID, ProjectID, FromEntityID, ToEntityID, RelationType string
	CreatedAt, UpdatedAt                                  time.Time
	DeletedAt                                             *time.Time
	LastWriterDevice                                      *string
}

// PullEntities returns rows updated after `since`, keyset-ordered. Pass
// the previous page's max (updated_at, id) to continue.
func PullEntities(ctx context.Context, pool *pgxpool.Pool, projectID string, since time.Time, sinceID string, limit int) ([]EntityRow, error) {
	rows, err := pool.Query(ctx, `
		select id::text, project_id::text, name, entity_type, created_at, updated_at, deleted_at, last_writer_device
		from entities
		where project_id = $1
		  and (updated_at > $2 or (updated_at = $2 and id::text > $3))
		order by updated_at asc, id asc
		limit $4
	`, projectID, since, sinceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EntityRow{}
	for rows.Next() {
		var r EntityRow
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Name, &r.EntityType, &r.CreatedAt, &r.UpdatedAt, &r.DeletedAt, &r.LastWriterDevice); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PullObservations returns observation rows for a project (joined via
// entities) updated after `since`.
func PullObservations(ctx context.Context, pool *pgxpool.Pool, projectID string, since time.Time, sinceID string, limit int) ([]ObservationRow, error) {
	rows, err := pool.Query(ctx, `
		select o.id::text, o.entity_id::text, o.content, o.created_at, o.updated_at, o.deleted_at, o.last_writer_device
		from observations o
		join entities e on e.id = o.entity_id
		where e.project_id = $1
		  and (o.updated_at > $2 or (o.updated_at = $2 and o.id::text > $3))
		order by o.updated_at asc, o.id asc
		limit $4
	`, projectID, since, sinceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ObservationRow{}
	for rows.Next() {
		var r ObservationRow
		if err := rows.Scan(&r.ID, &r.EntityID, &r.Content, &r.CreatedAt, &r.UpdatedAt, &r.DeletedAt, &r.LastWriterDevice); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func PullRelations(ctx context.Context, pool *pgxpool.Pool, projectID string, since time.Time, sinceID string, limit int) ([]RelationRow, error) {
	rows, err := pool.Query(ctx, `
		select id::text, project_id::text, from_entity_id::text, to_entity_id::text, relation_type, created_at, updated_at, deleted_at, last_writer_device
		from relations
		where project_id = $1
		  and (updated_at > $2 or (updated_at = $2 and id::text > $3))
		order by updated_at asc, id asc
		limit $4
	`, projectID, since, sinceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RelationRow{}
	for rows.Next() {
		var r RelationRow
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.FromEntityID, &r.ToEntityID, &r.RelationType, &r.CreatedAt, &r.UpdatedAt, &r.DeletedAt, &r.LastWriterDevice); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LookupProjectByID fetches the project's slug and name. Used to verify
// the pinned id resolves to something sensible.
func LookupProjectByID(ctx context.Context, pool *pgxpool.Pool, id string) (slug, name string, err error) {
	err = pool.QueryRow(ctx, `select slug, name from projects where id = $1`, id).Scan(&slug, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return
}
