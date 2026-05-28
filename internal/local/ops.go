package local

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SyncStatePending marks rows that have local mutations not yet on Postgres.
// SyncStateSynced marks rows whose state matches what Postgres last reported.
const (
	SyncStatePending = "pending_push"
	SyncStateSynced  = "synced"
)

func nowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// EnsureProject creates the projects row for this slug if it doesn't exist.
// Project ID is deterministic from the slug so multiple devices agree.
func EnsureProject(ctx context.Context, db *sql.DB, slug, name string) (string, error) {
	id := ProjectID(slug)
	_, err := db.ExecContext(ctx, `
		insert into projects (id, slug, name, created_at)
		values (?, ?, ?, ?)
		on conflict(slug) do update set name = excluded.name
	`, id, slug, name, nowISO())
	if err != nil {
		return "", fmt.Errorf("ensure project: %w", err)
	}
	return id, nil
}

// UpsertEntity inserts-or-updates an entity by (project_id, name).
// Marks the row pending_push so the sync engine knows to forward it.
func UpsertEntity(ctx context.Context, db *sql.DB, projectID, name, entityType, deviceID string) (string, error) {
	id := EntityID(projectID, name)
	now := nowISO()
	_, err := db.ExecContext(ctx, `
		insert into entities (id, project_id, name, entity_type, created_at, updated_at, deleted_at, last_writer_device, sync_state)
		values (?, ?, ?, ?, ?, ?, NULL, ?, ?)
		on conflict(id) do update set
		  entity_type        = excluded.entity_type,
		  updated_at         = excluded.updated_at,
		  deleted_at         = NULL,
		  last_writer_device = excluded.last_writer_device,
		  sync_state         = excluded.sync_state
	`, id, projectID, name, entityType, now, now, deviceID, SyncStatePending)
	if err != nil {
		return "", fmt.Errorf("upsert entity: %w", err)
	}
	return id, nil
}

// AddObservation appends a new observation. Observations are append-only;
// duplicates of identical content are tolerated (each gets its own id).
func AddObservation(ctx context.Context, db *sql.DB, entityID, content, deviceID string) (string, error) {
	id := uuid.New().String()
	now := nowISO()
	_, err := db.ExecContext(ctx, `
		insert into observations (id, entity_id, content, created_at, updated_at, deleted_at, last_writer_device, sync_state)
		values (?, ?, ?, ?, ?, NULL, ?, ?)
	`, id, entityID, content, now, now, deviceID, SyncStatePending)
	if err != nil {
		return "", fmt.Errorf("add observation: %w", err)
	}
	return id, nil
}

// UpsertRelation creates a directed relation. Deterministic id makes the
// "create the same relation on two devices" case a no-op on push.
func UpsertRelation(ctx context.Context, db *sql.DB, projectID, fromName, toName, relationType, deviceID string) (string, error) {
	fromID := EntityID(projectID, fromName)
	toID := EntityID(projectID, toName)
	id := RelationID(projectID, fromName, toName, relationType)
	now := nowISO()
	_, err := db.ExecContext(ctx, `
		insert into relations (id, project_id, from_entity_id, to_entity_id, relation_type, created_at, updated_at, deleted_at, last_writer_device, sync_state)
		values (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)
		on conflict(id) do update set
		  updated_at         = excluded.updated_at,
		  deleted_at         = NULL,
		  last_writer_device = excluded.last_writer_device,
		  sync_state         = excluded.sync_state
	`, id, projectID, fromID, toID, relationType, now, now, deviceID, SyncStatePending)
	if err != nil {
		return "", fmt.Errorf("upsert relation: %w", err)
	}
	return id, nil
}

// DeleteEntity soft-deletes by setting deleted_at. Returns whether a row
// was affected (false if no live entity existed by that name).
func DeleteEntity(ctx context.Context, db *sql.DB, projectID, name, deviceID string) (bool, error) {
	now := nowISO()
	res, err := db.ExecContext(ctx, `
		update entities
		set deleted_at = ?, updated_at = ?, last_writer_device = ?, sync_state = ?
		where project_id = ? and name = ? and deleted_at is null
	`, now, now, deviceID, SyncStatePending, projectID, name)
	if err != nil {
		return false, fmt.Errorf("delete entity: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteObservations removes specific contents from an entity. Returns the
// count actually marked.
func DeleteObservations(ctx context.Context, db *sql.DB, projectID, entityName string, contents []string, deviceID string) (int, error) {
	if len(contents) == 0 {
		return 0, nil
	}
	now := nowISO()
	// Build the IN clause dynamically with placeholders.
	placeholders := ""
	args := []any{now, now, deviceID, SyncStatePending, projectID, entityName}
	for i, c := range contents {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, c)
	}
	q := fmt.Sprintf(`
		update observations
		set deleted_at = ?, updated_at = ?, last_writer_device = ?, sync_state = ?
		where entity_id in (
			select e.id from entities e
			where e.project_id = ? and e.name = ? and e.deleted_at is null
		)
		  and content in (%s) and deleted_at is null
	`, placeholders)
	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("delete observations: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func DeleteRelation(ctx context.Context, db *sql.DB, projectID, fromName, toName, relationType, deviceID string) (bool, error) {
	now := nowISO()
	res, err := db.ExecContext(ctx, `
		update relations
		set deleted_at = ?, updated_at = ?, last_writer_device = ?, sync_state = ?
		where id = ? and deleted_at is null
	`, now, now, deviceID, SyncStatePending, RelationID(projectID, fromName, toName, relationType))
	if err != nil {
		return false, fmt.Errorf("delete relation: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ---- Read side --------------------------------------------------------

type GraphObservation struct {
	Content   string `json:"content"`
	CreatedAt string `json:"createdAt"`
}

type GraphRelation struct {
	To        string `json:"to"`
	Type      string `json:"type"`
	CreatedAt string `json:"createdAt"`
}

type GraphEntity struct {
	Name         string             `json:"name"`
	EntityType   string             `json:"entityType"`
	CreatedAt    string             `json:"createdAt"`
	Observations []GraphObservation `json:"observations"`
	Relations    []GraphRelation    `json:"relations"`
}

type GraphStats struct {
	EntityCount      int `json:"entityCount"`
	ObservationCount int `json:"observationCount"`
	RelationCount    int `json:"relationCount"`
}

type Graph struct {
	Entities []GraphEntity `json:"entities"`
	Stats    GraphStats    `json:"stats"`
}

// ReadGraph dumps every live entity in the project with its observations
// and outgoing relations. Capped at entityLimit (default 5000 if <=0).
func ReadGraph(ctx context.Context, db *sql.DB, projectID string, entityLimit int) (*Graph, error) {
	if entityLimit <= 0 {
		entityLimit = 5000
	}
	entities, err := loadEntitiesByProject(ctx, db, projectID, entityLimit)
	if err != nil {
		return nil, err
	}
	if err := hydrateGraphEntities(ctx, db, projectID, entities); err != nil {
		return nil, err
	}

	g := &Graph{Entities: entities, Stats: GraphStats{}}
	if err := db.QueryRowContext(ctx, `
		select
		  (select count(*) from entities where project_id = ? and deleted_at is null),
		  (select count(*) from observations o join entities e on e.id = o.entity_id where e.project_id = ? and e.deleted_at is null and o.deleted_at is null),
		  (select count(*) from relations where project_id = ? and deleted_at is null)
	`, projectID, projectID, projectID).Scan(&g.Stats.EntityCount, &g.Stats.ObservationCount, &g.Stats.RelationCount); err != nil {
		return nil, fmt.Errorf("read graph stats: %w", err)
	}
	return g, nil
}

func loadEntitiesByProject(ctx context.Context, db *sql.DB, projectID string, limit int) ([]GraphEntity, error) {
	rows, err := db.QueryContext(ctx, `
		select name, entity_type, created_at
		from entities
		where project_id = ? and deleted_at is null
		order by created_at desc
		limit ?
	`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("list entities: %w", err)
	}
	defer rows.Close()
	out := []GraphEntity{}
	for rows.Next() {
		var e GraphEntity
		if err := rows.Scan(&e.Name, &e.EntityType, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Observations = []GraphObservation{}
		e.Relations = []GraphRelation{}
		out = append(out, e)
	}
	return out, rows.Err()
}

func hydrateGraphEntities(ctx context.Context, db *sql.DB, projectID string, entities []GraphEntity) error {
	for i := range entities {
		obs, err := loadObservations(ctx, db, projectID, entities[i].Name)
		if err != nil {
			return err
		}
		rels, err := loadOutgoingRelations(ctx, db, projectID, entities[i].Name)
		if err != nil {
			return err
		}
		entities[i].Observations = obs
		entities[i].Relations = rels
	}
	return nil
}

func loadObservations(ctx context.Context, db *sql.DB, projectID, entityName string) ([]GraphObservation, error) {
	rows, err := db.QueryContext(ctx, `
		select o.content, o.created_at
		from observations o
		join entities e on e.id = o.entity_id
		where e.project_id = ? and e.name = ? and e.deleted_at is null and o.deleted_at is null
		order by o.created_at desc
	`, projectID, entityName)
	if err != nil {
		return nil, fmt.Errorf("list observations: %w", err)
	}
	defer rows.Close()
	out := []GraphObservation{}
	for rows.Next() {
		var o GraphObservation
		if err := rows.Scan(&o.Content, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func loadOutgoingRelations(ctx context.Context, db *sql.DB, projectID, fromName string) ([]GraphRelation, error) {
	rows, err := db.QueryContext(ctx, `
		select te.name, r.relation_type, r.created_at
		from relations r
		join entities fe on fe.id = r.from_entity_id
		join entities te on te.id = r.to_entity_id
		where r.project_id = ? and fe.name = ?
		  and r.deleted_at is null and fe.deleted_at is null and te.deleted_at is null
		order by r.created_at desc
	`, projectID, fromName)
	if err != nil {
		return nil, fmt.Errorf("list relations: %w", err)
	}
	defer rows.Close()
	out := []GraphRelation{}
	for rows.Next() {
		var r GraphRelation
		if err := rows.Scan(&r.To, &r.Type, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OpenNodes returns specific entities by name with full context. Missing
// names are silently skipped.
func OpenNodes(ctx context.Context, db *sql.DB, projectID string, names []string) ([]GraphEntity, error) {
	if len(names) == 0 {
		return []GraphEntity{}, nil
	}
	placeholders := ""
	args := []any{projectID}
	for i, n := range names {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, n)
	}
	q := fmt.Sprintf(`
		select name, entity_type, created_at
		from entities
		where project_id = ? and deleted_at is null and name in (%s)
		order by created_at desc
	`, placeholders)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("open nodes: %w", err)
	}
	defer rows.Close()
	out := []GraphEntity{}
	for rows.Next() {
		var e GraphEntity
		if err := rows.Scan(&e.Name, &e.EntityType, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Observations = []GraphObservation{}
		e.Relations = []GraphRelation{}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := hydrateGraphEntities(ctx, db, projectID, out); err != nil {
		return nil, err
	}
	return out, nil
}

// SearchResult is one ranked hit from SearchNodes.
type SearchResult struct {
	EntityName string  `json:"entityName"`
	EntityType string  `json:"entityType"`
	Content    string  `json:"content"`
	Rank       float64 `json:"rank"`
}

// SearchNodes runs an FTS5 query over observation content within this
// project. Rank is bm25 (lower = better) negated so higher = better, to
// match the Postgres ts_rank semantics callers may already expect.
func SearchNodes(ctx context.Context, db *sql.DB, projectID, query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.QueryContext(ctx, `
		select e.name, e.entity_type, o.content, -bm25(observations_fts) as rank
		from observations_fts
		join observations o on o.rowid = observations_fts.rowid
		join entities e on e.id = o.entity_id
		where observations_fts match ?
		  and e.project_id = ?
		  and e.deleted_at is null
		  and o.deleted_at is null
		order by rank desc
		limit ?
	`, query, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("search nodes: %w", err)
	}
	defer rows.Close()
	out := []SearchResult{}
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.EntityName, &r.EntityType, &r.Content, &r.Rank); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PolicyObservations returns the observation contents (in order) attached
// to the special `__policy` entity for this project. Empty slice if no
// policy entity exists.
func PolicyObservations(ctx context.Context, db *sql.DB, projectID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		select o.content
		from observations o
		join entities e on e.id = o.entity_id
		where e.project_id = ? and e.name = '__policy'
		  and e.deleted_at is null and o.deleted_at is null
		order by o.created_at asc
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// EntityIDByName looks up an entity by name within the project. Returns
// ("", false) if no live entity exists.
func EntityIDByName(ctx context.Context, db *sql.DB, projectID, name string) (string, bool, error) {
	var id string
	err := db.QueryRowContext(ctx, `
		select id from entities
		where project_id = ? and name = ? and deleted_at is null
	`, projectID, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// MarkSyncedJSON applies a server response to a local row: sets
// sync_state='synced' and rewrites updated_at to the server-stamped value.
// Used by the push path.
func MarkSyncedJSON(ctx context.Context, db *sql.DB, table, id, serverUpdatedAt string) error {
	q := fmt.Sprintf(`update %s set sync_state='synced', updated_at=? where id=?`, sanitizeTable(table))
	_, err := db.ExecContext(ctx, q, serverUpdatedAt, id)
	return err
}

func sanitizeTable(t string) string {
	switch t {
	case "entities", "observations", "relations":
		return t
	default:
		// Programmer error — fail loudly rather than silently injecting.
		panic("unknown table: " + t)
	}
}

// MarshalPayload is a small helper used by the queue layer.
func MarshalPayload(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
