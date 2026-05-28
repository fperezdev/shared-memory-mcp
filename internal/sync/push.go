package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fperez/shared-memory-mcp/internal/local"
	"github.com/fperez/shared-memory-mcp/internal/remote"
)

// drainOnce pops up to N due ops and applies each to Postgres. Successful
// ops are removed from pending_writes; failures get backoff via MarkFailure.
//
// The caller holds the engine mutex (single-flight), so no concurrent
// drains are possible from this process.
func drainOnce(ctx context.Context, db *sql.DB, pool *pgxpool.Pool, deviceID string, logger io.Writer) error {
	if pool == nil {
		return nil
	}
	const batch = 50
	for {
		ops, err := local.PeekDue(ctx, db, batch)
		if err != nil {
			return fmt.Errorf("peek queue: %w", err)
		}
		if len(ops) == 0 {
			return nil
		}
		for _, op := range ops {
			if err := applyOp(ctx, db, pool, deviceID, op); err != nil {
				_ = local.MarkFailure(ctx, db, op.ID, op.Attempts, err.Error())
				fmt.Fprintf(logger, "[sync] push op %d (%s) failed: %v\n", op.ID, op.Op, err)
				// Stop batching on failure — next cycle retries with backoff.
				return nil
			}
			_ = local.MarkSuccess(ctx, db, op.ID)
		}
	}
}

// applyOp dispatches a single queued op against Postgres and writes the
// server-stamped updated_at back into the local row.
func applyOp(ctx context.Context, db *sql.DB, pool *pgxpool.Pool, deviceID string, op local.Pending) error {
	switch op.Op {
	case local.OpUpsertEntity:
		var p struct {
			ID, ProjectID, Name, EntityType string
		}
		if err := json.Unmarshal([]byte(op.Payload), &p); err != nil {
			return fmt.Errorf("decode %s: %w", op.Op, err)
		}
		t, err := remote.UpsertEntity(ctx, pool, p.ID, p.ProjectID, p.Name, p.EntityType, deviceID)
		if err != nil {
			return err
		}
		return local.MarkSyncedJSON(ctx, db, "entities", p.ID, isoUTC(t))

	case local.OpAddObservation:
		var p struct {
			ID, EntityID, Content string
		}
		if err := json.Unmarshal([]byte(op.Payload), &p); err != nil {
			return fmt.Errorf("decode %s: %w", op.Op, err)
		}
		t, err := remote.AddObservation(ctx, pool, p.ID, p.EntityID, p.Content, deviceID)
		if err != nil {
			return err
		}
		return local.MarkSyncedJSON(ctx, db, "observations", p.ID, isoUTC(t))

	case local.OpUpsertRelation:
		var p struct {
			ID, ProjectID, FromID, ToID, RelationType string
		}
		if err := json.Unmarshal([]byte(op.Payload), &p); err != nil {
			return fmt.Errorf("decode %s: %w", op.Op, err)
		}
		t, err := remote.UpsertRelation(ctx, pool, p.ID, p.ProjectID, p.FromID, p.ToID, p.RelationType, deviceID)
		if err != nil {
			return err
		}
		return local.MarkSyncedJSON(ctx, db, "relations", p.ID, isoUTC(t))

	case local.OpDeleteEntity:
		var p struct{ ID string }
		if err := json.Unmarshal([]byte(op.Payload), &p); err != nil {
			return fmt.Errorf("decode %s: %w", op.Op, err)
		}
		t, err := remote.DeleteEntity(ctx, pool, p.ID, deviceID)
		if err != nil {
			return err
		}
		if t.IsZero() {
			return nil // row not on server — nothing to mark
		}
		return local.MarkSyncedJSON(ctx, db, "entities", p.ID, isoUTC(t))

	case local.OpDeleteObservation:
		var p struct{ ID string }
		if err := json.Unmarshal([]byte(op.Payload), &p); err != nil {
			return fmt.Errorf("decode %s: %w", op.Op, err)
		}
		t, err := remote.DeleteObservation(ctx, pool, p.ID, deviceID)
		if err != nil {
			return err
		}
		if t.IsZero() {
			return nil
		}
		return local.MarkSyncedJSON(ctx, db, "observations", p.ID, isoUTC(t))

	case local.OpDeleteRelation:
		var p struct{ ID string }
		if err := json.Unmarshal([]byte(op.Payload), &p); err != nil {
			return fmt.Errorf("decode %s: %w", op.Op, err)
		}
		t, err := remote.DeleteRelation(ctx, pool, p.ID, deviceID)
		if err != nil {
			return err
		}
		if t.IsZero() {
			return nil
		}
		return local.MarkSyncedJSON(ctx, db, "relations", p.ID, isoUTC(t))

	case local.OpAudit:
		var e struct {
			ID, ProjectID, DeviceID, ToolName, ArgsHash, OccurredAt string
		}
		if err := json.Unmarshal([]byte(op.Payload), &e); err != nil {
			return fmt.Errorf("decode %s: %w", op.Op, err)
		}
		if err := remote.WriteAudit(ctx, pool, e.ID, e.ProjectID, e.DeviceID, e.ToolName, e.ArgsHash, e.OccurredAt); err != nil {
			return err
		}
		_, err := db.ExecContext(ctx, `update audit_log set sync_state='synced' where id = ?`, e.ID)
		return err

	default:
		return fmt.Errorf("unknown op %q", op.Op)
	}
}

func isoUTC(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
