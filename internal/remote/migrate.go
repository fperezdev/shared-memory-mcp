package remote

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/fperez/shared-memory-mcp/migrations"
)

// Migrate applies any pending migration files in alphabetical order.
// Uses schema_migrations to track what's been run. Each file runs in its
// own transaction.
//
// The first migration (001) creates schema_migrations itself; we
// special-case that by checking whether the table exists before reading
// the applied set.
func Migrate(ctx context.Context, conn *pgx.Conn, log func(string)) (applied []string, err error) {
	if log == nil {
		log = func(string) {}
	}

	files, err := migrations.FS.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	names := []string{}
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".sql") {
			names = append(names, f.Name())
		}
	}
	sort.Strings(names)

	tableExists := false
	_ = conn.QueryRow(ctx, `
		select exists (
			select 1 from information_schema.tables
			where table_schema='public' and table_name='schema_migrations'
		)`).Scan(&tableExists)

	alreadyApplied := map[string]bool{}
	if tableExists {
		rows, err := conn.Query(ctx, `select version from schema_migrations`)
		if err != nil {
			return nil, fmt.Errorf("read schema_migrations: %w", err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return nil, err
			}
			alreadyApplied[v] = true
		}
		rows.Close()
	}

	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		if alreadyApplied[version] {
			log(fmt.Sprintf("skip %s (already applied)", version))
			continue
		}
		log(fmt.Sprintf("apply %s", version))

		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return applied, fmt.Errorf("read %s: %w", name, err)
		}

		tx, err := conn.Begin(ctx)
		if err != nil {
			return applied, fmt.Errorf("begin tx for %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("apply %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx,
			`insert into schema_migrations (version) values ($1) on conflict (version) do nothing`,
			version); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("record %s: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return applied, fmt.Errorf("commit %s: %w", version, err)
		}
		applied = append(applied, version)
	}
	return applied, nil
}
