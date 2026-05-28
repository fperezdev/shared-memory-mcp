// Package audit records every mutation to a local table and enqueues
// the same record for eventual replay to Postgres. Failures here never
// block a tool call — auditing is best-effort.
package audit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/fperez/shared-memory-mcp/internal/local"
)

type Entry struct {
	ID         string `json:"id"`
	ProjectID  string `json:"projectId"`
	DeviceID   string `json:"deviceId"`
	ToolName   string `json:"toolName"`
	ArgsHash   string `json:"argsHash"`
	OccurredAt string `json:"occurredAt"`
}

// Record writes one audit row to local.audit_log and enqueues an op for
// the sync engine to replay against Postgres. Returns the args hash for
// callers that want to log it.
func Record(ctx context.Context, db *sql.DB, projectID, deviceID, toolName string, args any) (string, error) {
	hash := hashArgs(args)
	entry := Entry{
		ID:         uuid.New().String(),
		ProjectID:  projectID,
		DeviceID:   deviceID,
		ToolName:   toolName,
		ArgsHash:   hash,
		OccurredAt: time.Now().UTC().Format(time.RFC3339Nano),
	}

	if _, err := db.ExecContext(ctx, `
		insert into audit_log (id, project_id, device_id, tool_name, args_hash, occurred_at, sync_state)
		values (?, ?, ?, ?, ?, ?, ?)
	`, entry.ID, entry.ProjectID, entry.DeviceID, entry.ToolName, entry.ArgsHash, entry.OccurredAt, local.SyncStatePending); err != nil {
		// Best-effort: log and move on. Do not propagate.
		fmt.Fprintf(stderrSink, "[audit] local insert failed: %v\n", err)
		return hash, nil
	}

	if _, err := local.Enqueue(ctx, db, local.OpAudit, entry); err != nil {
		fmt.Fprintf(stderrSink, "[audit] enqueue failed: %v\n", err)
	}
	return hash, nil
}

func hashArgs(args any) string {
	b, err := json.Marshal(args)
	if err != nil {
		b = []byte(fmt.Sprintf("%v", args))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:32]
}
