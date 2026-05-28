package sync

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fperez/shared-memory-mcp/internal/local"
	"github.com/fperez/shared-memory-mcp/internal/remote"
)

const (
	resourceEntities     = "entities"
	resourceObservations = "observations"
	resourceRelations    = "relations"
)

// pullAll runs the three resource pulls in order: entities first (so any
// new observations or relations referencing fresh entities have their FKs
// satisfied locally), then observations, then relations.
func pullAll(ctx context.Context, db *sql.DB, pool *pgxpool.Pool, projectID string, pageSize int, logger io.Writer) error {
	if pool == nil {
		return nil
	}
	if err := pullEntities(ctx, db, pool, projectID, pageSize, logger); err != nil {
		return fmt.Errorf("pull entities: %w", err)
	}
	if err := pullObservations(ctx, db, pool, projectID, pageSize, logger); err != nil {
		return fmt.Errorf("pull observations: %w", err)
	}
	if err := pullRelations(ctx, db, pool, projectID, pageSize, logger); err != nil {
		return fmt.Errorf("pull relations: %w", err)
	}
	return nil
}

func readCursor(ctx context.Context, db *sql.DB, projectID, resource string) (time.Time, string, error) {
	var raw string
	err := db.QueryRowContext(ctx, `
		select last_pulled_at from sync_cursor
		where project_id = ? and resource = ?
	`, projectID, resource).Scan(&raw)
	if err == sql.ErrNoRows {
		return time.Unix(0, 0), "", nil
	}
	if err != nil {
		return time.Time{}, "", err
	}
	t, perr := time.Parse(time.RFC3339Nano, raw)
	if perr != nil {
		return time.Unix(0, 0), "", nil
	}
	return t, "", nil // sinceID always starts empty within a cycle
}

func writeCursor(ctx context.Context, db *sql.DB, projectID, resource string, t time.Time) error {
	_, err := db.ExecContext(ctx, `
		insert into sync_cursor (project_id, resource, last_pulled_at)
		values (?, ?, ?)
		on conflict(project_id, resource) do update set last_pulled_at = excluded.last_pulled_at
	`, projectID, resource, t.UTC().Format(time.RFC3339Nano))
	return err
}

func pullEntities(ctx context.Context, db *sql.DB, pool *pgxpool.Pool, projectID string, pageSize int, logger io.Writer) error {
	since, sinceID, err := readCursor(ctx, db, projectID, resourceEntities)
	if err != nil {
		return err
	}
	pages := 0
	for {
		rows, err := remote.PullEntities(ctx, pool, projectID, since, sinceID, pageSize)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		if err := applyEntities(ctx, db, rows); err != nil {
			return err
		}
		last := rows[len(rows)-1]
		since = last.UpdatedAt
		sinceID = last.ID
		pages++
		if err := writeCursor(ctx, db, projectID, resourceEntities, since); err != nil {
			return err
		}
		if len(rows) < pageSize {
			break
		}
	}
	if pages > 0 {
		fmt.Fprintf(logger, "[sync] pulled %d entity page(s)\n", pages)
	}
	return nil
}

func pullObservations(ctx context.Context, db *sql.DB, pool *pgxpool.Pool, projectID string, pageSize int, logger io.Writer) error {
	since, sinceID, err := readCursor(ctx, db, projectID, resourceObservations)
	if err != nil {
		return err
	}
	pages := 0
	for {
		rows, err := remote.PullObservations(ctx, pool, projectID, since, sinceID, pageSize)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		if err := applyObservations(ctx, db, rows); err != nil {
			return err
		}
		last := rows[len(rows)-1]
		since = last.UpdatedAt
		sinceID = last.ID
		pages++
		if err := writeCursor(ctx, db, projectID, resourceObservations, since); err != nil {
			return err
		}
		if len(rows) < pageSize {
			break
		}
	}
	if pages > 0 {
		fmt.Fprintf(logger, "[sync] pulled %d observation page(s)\n", pages)
	}
	return nil
}

func pullRelations(ctx context.Context, db *sql.DB, pool *pgxpool.Pool, projectID string, pageSize int, logger io.Writer) error {
	since, sinceID, err := readCursor(ctx, db, projectID, resourceRelations)
	if err != nil {
		return err
	}
	pages := 0
	for {
		rows, err := remote.PullRelations(ctx, pool, projectID, since, sinceID, pageSize)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		if err := applyRelations(ctx, db, rows); err != nil {
			return err
		}
		last := rows[len(rows)-1]
		since = last.UpdatedAt
		sinceID = last.ID
		pages++
		if err := writeCursor(ctx, db, projectID, resourceRelations, since); err != nil {
			return err
		}
		if len(rows) < pageSize {
			break
		}
	}
	if pages > 0 {
		fmt.Fprintf(logger, "[sync] pulled %d relation page(s)\n", pages)
	}
	return nil
}

// applyEntities upserts each remote row, but ONLY if the local row is
// not pending push. The local row's pending edit wins until it drains.
func applyEntities(ctx context.Context, db *sql.DB, rows []remote.EntityRow) error {
	for _, r := range rows {
		deletedAt := nilOrISO(r.DeletedAt)
		_, err := db.ExecContext(ctx, `
			insert into entities (id, project_id, name, entity_type, created_at, updated_at, deleted_at, last_writer_device, sync_state)
			values (?, ?, ?, ?, ?, ?, ?, ?, 'synced')
			on conflict(id) do update set
			  name               = excluded.name,
			  entity_type        = excluded.entity_type,
			  updated_at         = excluded.updated_at,
			  deleted_at         = excluded.deleted_at,
			  last_writer_device = excluded.last_writer_device,
			  sync_state         = 'synced'
			where entities.sync_state = 'synced'
		`, r.ID, r.ProjectID, r.Name, r.EntityType,
			r.CreatedAt.UTC().Format(time.RFC3339Nano),
			r.UpdatedAt.UTC().Format(time.RFC3339Nano),
			deletedAt, ptrToString(r.LastWriterDevice))
		if err != nil {
			return err
		}
	}
	return nil
}

func applyObservations(ctx context.Context, db *sql.DB, rows []remote.ObservationRow) error {
	for _, r := range rows {
		deletedAt := nilOrISO(r.DeletedAt)
		_, err := db.ExecContext(ctx, `
			insert into observations (id, entity_id, content, created_at, updated_at, deleted_at, last_writer_device, sync_state)
			values (?, ?, ?, ?, ?, ?, ?, 'synced')
			on conflict(id) do update set
			  content            = excluded.content,
			  updated_at         = excluded.updated_at,
			  deleted_at         = excluded.deleted_at,
			  last_writer_device = excluded.last_writer_device,
			  sync_state         = 'synced'
			where observations.sync_state = 'synced'
		`, r.ID, r.EntityID, r.Content,
			r.CreatedAt.UTC().Format(time.RFC3339Nano),
			r.UpdatedAt.UTC().Format(time.RFC3339Nano),
			deletedAt, ptrToString(r.LastWriterDevice))
		if err != nil {
			return err
		}
	}
	return nil
}

func applyRelations(ctx context.Context, db *sql.DB, rows []remote.RelationRow) error {
	for _, r := range rows {
		deletedAt := nilOrISO(r.DeletedAt)
		_, err := db.ExecContext(ctx, `
			insert into relations (id, project_id, from_entity_id, to_entity_id, relation_type, created_at, updated_at, deleted_at, last_writer_device, sync_state)
			values (?, ?, ?, ?, ?, ?, ?, ?, ?, 'synced')
			on conflict(id) do update set
			  relation_type      = excluded.relation_type,
			  updated_at         = excluded.updated_at,
			  deleted_at         = excluded.deleted_at,
			  last_writer_device = excluded.last_writer_device,
			  sync_state         = 'synced'
			where relations.sync_state = 'synced'
		`, r.ID, r.ProjectID, r.FromEntityID, r.ToEntityID, r.RelationType,
			r.CreatedAt.UTC().Format(time.RFC3339Nano),
			r.UpdatedAt.UTC().Format(time.RFC3339Nano),
			deletedAt, ptrToString(r.LastWriterDevice))
		if err != nil {
			return err
		}
	}
	return nil
}

func nilOrISO(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func ptrToString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// Ensure unused import (kept for symmetry with future helpers).
var _ = local.SyncStateSynced
