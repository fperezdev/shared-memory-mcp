// shared-memory-mcp serves the project's knowledge graph over MCP/stdio,
// with a SQLite cache and a write-behind queue that drains to Supabase
// Postgres in the background.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/server"

	"github.com/fperez/shared-memory-mcp/internal/config"
	"github.com/fperez/shared-memory-mcp/internal/local"
	"github.com/fperez/shared-memory-mcp/internal/project"
	"github.com/fperez/shared-memory-mcp/internal/remote"
	mysync "github.com/fperez/shared-memory-mcp/internal/sync"
	"github.com/fperez/shared-memory-mcp/internal/tools"
)

const (
	bootstrapTimeout      = 10 * time.Second
	policyDefaultsProject = "__defaults"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "[shared-memory-mcp] fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "[shared-memory-mcp] "+format+"\n", args...)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	proj, err := project.Resolve(project.Input{
		ConfigSlug: cfg.Project.Slug,
		ConfigName: cfg.Project.Name,
	})
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}
	logf("project: slug=%q source=%s", proj.Slug, proj.Source)

	db, err := local.Open(cfg.Sync.LocalDBPath)
	if err != nil {
		return fmt.Errorf("open local cache: %w", err)
	}
	defer db.Close()
	logf("local cache: %s", cfg.Sync.LocalDBPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := remote.Open(ctx, cfg.DB.ConnectionString, cfg.DB.CACertPath)
	if err != nil {
		return fmt.Errorf("open remote pool: %w", err)
	}
	if pool != nil {
		defer pool.Close()
		logf("remote: connected")
	} else {
		logf("remote: local-only mode (no db.connectionString)")
	}

	projectID, err := local.EnsureProject(ctx, db, proj.Slug, proj.Name)
	if err != nil {
		return err
	}
	if pool != nil {
		if _, err := remote.EnsureProject(ctx, pool, projectID, proj.Slug, proj.Name); err != nil {
			// Don't fail startup; we may be offline. Sync will retry later.
			logf("remote: ensure project failed: %v", err)
		}
	}

	engine := mysync.New(db, pool, projectID, cfg.Device.ID, cfg.Sync.IntervalSeconds, cfg.Sync.PageSize)

	if pool != nil {
		bctx, bcancel := context.WithTimeout(ctx, bootstrapTimeout)
		if err := engine.BootstrapPull(bctx); err != nil {
			logf("bootstrap pull: %v (continuing with partial cache)", err)
		}
		bcancel()
	}

	go engine.Run(ctx)

	policy := loadPolicy(ctx, db, pool, projectID, logf)
	instructions := defaultInstructions
	if policy != "" {
		instructions = policy
	}

	srv := server.NewMCPServer(
		"shared-memory-mcp",
		"0.2.0",
		server.WithToolCapabilities(true),
		server.WithInstructions(instructions),
	)

	tools.Register(srv, &tools.Ctx{
		DB:        db,
		ProjectID: projectID,
		DeviceID:  cfg.Device.ID,
		Sync:      engine,
	})

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		logf("shutdown requested")
		cancel()
	}()

	logf("ready")
	if err := server.ServeStdio(srv); err != nil && ctx.Err() == nil {
		return fmt.Errorf("serve stdio: %w", err)
	}
	return nil
}

const defaultInstructions = "No __policy entity exists for this project and no __defaults template was seeded. " +
	"Run `shared-memory-mcp-admin init` against your Supabase to seed the default policy, " +
	"or add observations to an entity named `__policy` (entityType `policy`) to define how memory should be used here."

// loadPolicy returns the __policy text to inject as MCP instructions:
//   - prefer this project's own __policy entity observations,
//   - otherwise fall back to the __defaults project (seeded by setup),
//   - otherwise return "" so the server uses its built-in default.
//
// If the local cache has neither, we try a one-shot remote read against
// the __defaults project and seed the local cache with the result, so
// subsequent startups (and offline use) see the policy without a network.
func loadPolicy(ctx context.Context, db *sql.DB, pool *pgxpool.Pool, projectID string, logf func(string, ...any)) string {
	obs, err := local.PolicyObservations(ctx, db, projectID)
	if err == nil && len(obs) > 0 {
		logf("policy: source=project (%d rules)", len(obs))
		return formatPolicy(obs, false)
	}

	defaultsID := local.ProjectID(policyDefaultsProject)
	obs, err = local.PolicyObservations(ctx, db, defaultsID)
	if err == nil && len(obs) > 0 {
		logf("policy: source=defaults (%d rules)", len(obs))
		return formatPolicy(obs, true)
	}

	if pool != nil {
		if obs, err := fetchRemoteDefaultsPolicy(ctx, pool, db); err == nil && len(obs) > 0 {
			logf("policy: source=defaults-remote (%d rules)", len(obs))
			return formatPolicy(obs, true)
		} else if err != nil {
			logf("policy: remote fetch failed: %v", err)
		}
	}

	logf("policy: none")
	return ""
}

// fetchRemoteDefaultsPolicy reads the __defaults/__policy observations
// directly from Postgres and inserts them into the local cache so the
// next startup sees them locally. Best-effort — failures don't abort.
func fetchRemoteDefaultsPolicy(ctx context.Context, pool *pgxpool.Pool, db *sql.DB) ([]string, error) {
	defaultsID := local.ProjectID(policyDefaultsProject)
	entityID := local.EntityID(defaultsID, "__policy")

	// Ensure local project + entity rows exist so the FK from observations
	// resolves cleanly when we mirror them.
	if _, err := local.EnsureProject(ctx, db, policyDefaultsProject, "Default policy template"); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `
		insert into entities (id, project_id, name, entity_type, created_at, updated_at, sync_state)
		values (?, ?, '__policy', 'policy', ?, ?, 'synced')
		on conflict(id) do nothing
	`, entityID, defaultsID, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, `
		select o.id::text, o.content, o.created_at, o.updated_at
		from observations o
		where o.entity_id = $1 and o.deleted_at is null
		order by o.created_at asc
	`, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	contents := []string{}
	for rows.Next() {
		var id, content string
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&id, &content, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		contents = append(contents, content)
		_, _ = db.ExecContext(ctx, `
			insert into observations (id, entity_id, content, created_at, updated_at, sync_state)
			values (?, ?, ?, ?, ?, 'synced')
			on conflict(id) do nothing
		`, id, entityID, content,
			createdAt.UTC().Format(time.RFC3339Nano),
			updatedAt.UTC().Format(time.RFC3339Nano))
	}
	return contents, rows.Err()
}

func formatPolicy(observations []string, fromDefaults bool) string {
	header := "Memory policy for this project (from its __policy entity):"
	if fromDefaults {
		header = "Memory policy (default template — add observations to this project's __policy to override):"
	}
	bullets := make([]string, 0, len(observations))
	for _, o := range observations {
		bullets = append(bullets, "- "+o)
	}
	return header + "\n" + strings.Join(bullets, "\n")
}
