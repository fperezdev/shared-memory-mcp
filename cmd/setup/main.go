// shared-memory-mcp-setup runs one-shot administrative tasks against
// Supabase Postgres using the project's superuser/service-role credentials.
//
// Subcommands:
//
//	init                Apply migrations, create the scoped role, seed __defaults/__policy,
//	                    print a ready-to-paste config.json fragment.
//	migrate             Apply pending migrations only.
//	rotate-credentials  Generate a new password for the scoped role and print the new
//	                    connection string.
//
// Required env: SHARED_MEMORY_SETUP_DB_URL (a superuser or service-role
// connection string). This URL is used by the CLI only and never persists
// on the runtime devices.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/fperez/shared-memory-mcp/internal/local"
	"github.com/fperez/shared-memory-mcp/internal/remote"
)

const (
	roleName            = "memory_mcp_user"
	defaultsProjectSlug = "__defaults"
	policyEntityName    = "__policy"
)

var defaultPolicy = []string{
	"Guarda decisiones arquitectónicas y trade-offs explícitos (entityType: decision).",
	"Guarda features y módulos como entidades (entityType: feature o module).",
	"Después de resolver un bug, almacena la causa raíz como observación en el feature afectado (prefijo entityType: gotcha:).",
	"Nunca guardes credenciales, tokens, paths absolutos del filesystem local, ni PII.",
	"Prefiere observaciones cortas y atómicas (un dato por observación) en lugar de párrafos largos.",
	"Usa relations para capturar dependencias, ownership y vínculos de 'esto causó aquello'.",
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "init":
		mustDoInit(args)
	case "migrate":
		mustDoMigrate(args)
	case "rotate-credentials":
		mustDoRotate(args)
	case "reset-policy":
		mustDoResetPolicy(args)
	case "dump":
		mustDoDump(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `shared-memory-mcp-setup

Commands:
  init                Apply migrations, create scoped role, seed __defaults/__policy,
                      print a ready-to-paste config.json fragment.
  migrate             Apply pending migrations only.
  rotate-credentials  Regenerate the scoped role's password and print the new connection string.
  reset-policy        Wipe __defaults/__policy observations and reseed from the binary's
                      defaultPolicy (use after editing the policy in source).
  dump                Show projects, recent entities and per-device activity from Supabase.
                      Pass -project <slug> to drill into one project.

Required env:
  SHARED_MEMORY_SETUP_DB_URL  Superuser or service-role connection string (used only by this CLI).`)
	os.Exit(1)
}

func setupURL() string {
	u := os.Getenv("SHARED_MEMORY_SETUP_DB_URL")
	if u == "" {
		fmt.Fprintln(os.Stderr, "error: SHARED_MEMORY_SETUP_DB_URL is required")
		usage()
	}
	return u
}

func connect(ctx context.Context, url string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	// Setup uses the direct host, prepared statements work fine here.
	return pgx.ConnectConfig(ctx, cfg)
}

func mustDoInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	rawURL := setupURL()
	conn, err := connect(ctx, rawURL)
	if err != nil {
		fail("connect: %v", err)
	}
	defer conn.Close(ctx)

	fmt.Fprintln(os.Stderr, "applying migrations...")
	applied, err := remote.Migrate(ctx, conn, func(s string) { fmt.Fprintln(os.Stderr, "  "+s) })
	if err != nil {
		fail("migrate: %v", err)
	}
	if len(applied) == 0 {
		fmt.Fprintln(os.Stderr, "  (already up to date)")
	}

	password := newPassword()
	fmt.Fprintf(os.Stderr, "creating role %q with scoped privileges...\n", roleName)
	if err := applyRoleAndGrants(ctx, conn, password); err != nil {
		fail("apply role: %v", err)
	}

	fmt.Fprintln(os.Stderr, "seeding default __policy template...")
	if err := seedDefaultPolicy(ctx, conn); err != nil {
		fail("seed policy: %v", err)
	}

	scoped, err := buildScopedURL(rawURL, password)
	if err != nil {
		fail("build connection string: %v", err)
	}

	deviceID := uuid.New().String()
	configBlock, err := json.MarshalIndent(map[string]any{
		"db": map[string]any{
			"connectionString": scoped,
			"caCertPath":       nil,
		},
		"device":  map[string]any{"id": deviceID},
		"project": map[string]any{"slug": nil, "name": nil},
		"sync":    map[string]any{"intervalSeconds": 60, "pageSize": 1000, "localDbPath": nil},
	}, "", "  ")
	if err != nil {
		fail("marshal config: %v", err)
	}

	out := strings.Builder{}
	out.WriteString("\n================================================================\n")
	out.WriteString("Setup complete.\n\n")
	out.WriteString("Paste this into ~/.config/shared-memory-mcp/config.json (Unix)\n")
	out.WriteString("or %APPDATA%\\shared-memory-mcp\\config.json (Windows):\n\n")
	out.Write(configBlock)
	out.WriteString("\n\n")
	out.WriteString("On Unix:    chmod 600 ~/.config/shared-memory-mcp/config.json\n")
	out.WriteString("On Windows: icacls \"%APPDATA%\\shared-memory-mcp\\config.json\" /inheritance:r /grant:r \"%USERNAME%:F\"\n\n")
	out.WriteString("Important: edit the host/port of connectionString to the *pooler* endpoint\n")
	out.WriteString("(aws-0-<region>.pooler.supabase.com:6543) if setup ran against the direct host.\n")
	out.WriteString("================================================================\n\n")
	fmt.Print(out.String())
}

func mustDoMigrate(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	conn, err := connect(ctx, setupURL())
	if err != nil {
		fail("connect: %v", err)
	}
	defer conn.Close(ctx)

	applied, err := remote.Migrate(ctx, conn, func(s string) { fmt.Fprintln(os.Stderr, s) })
	if err != nil {
		fail("migrate: %v", err)
	}
	if len(applied) == 0 {
		fmt.Fprintln(os.Stderr, "already up to date")
	} else {
		fmt.Fprintf(os.Stderr, "applied: %s\n", strings.Join(applied, ", "))
	}
}

func mustDoRotate(args []string) {
	fs := flag.NewFlagSet("rotate-credentials", flag.ExitOnError)
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	rawURL := setupURL()
	conn, err := connect(ctx, rawURL)
	if err != nil {
		fail("connect: %v", err)
	}
	defer conn.Close(ctx)

	password := newPassword()
	if _, err := conn.Exec(ctx, fmt.Sprintf(`alter role %s with password '%s'`,
		roleName, escapeSQL(password))); err != nil {
		fail("alter role: %v", err)
	}

	scoped, err := buildScopedURL(rawURL, password)
	if err != nil {
		fail("build scoped url: %v", err)
	}
	fmt.Print("\n================================================================\n")
	fmt.Println("Password rotated. Replace db.connectionString in every device's")
	fmt.Println("config.json with the new value below:\n")
	fmt.Println(scoped)
	fmt.Println("================================================================\n")
}

func applyRoleAndGrants(ctx context.Context, conn *pgx.Conn, password string) error {
	var exists bool
	if err := conn.QueryRow(ctx, `select exists (select 1 from pg_roles where rolname = $1)`, roleName).Scan(&exists); err != nil {
		return fmt.Errorf("check role: %w", err)
	}
	if exists {
		if _, err := conn.Exec(ctx, fmt.Sprintf(`alter role %s with login password '%s'`, roleName, escapeSQL(password))); err != nil {
			return fmt.Errorf("alter role: %w", err)
		}
	} else {
		if _, err := conn.Exec(ctx, fmt.Sprintf(`create role %s with login password '%s'`, roleName, escapeSQL(password))); err != nil {
			return fmt.Errorf("create role: %w", err)
		}
	}

	dbName, err := currentDB(ctx, conn)
	if err != nil {
		return err
	}
	grants := fmt.Sprintf(`
		grant connect on database "%s" to %s;
		grant usage on schema public to %s;
		grant select, insert, update, delete on
		  projects, entities, observations, relations, audit_log
		  to %s;
		grant usage, select on all sequences in schema public to %s;
		grant execute on function search_observations(uuid, text, int) to %s;
		grant execute on function read_graph(uuid, int) to %s;
		grant execute on function open_nodes(uuid, text[]) to %s;
		grant execute on function upsert_entity_with_observations(uuid, text, text, text[]) to %s;
	`,
		strings.ReplaceAll(dbName, `"`, `""`),
		roleName, roleName, roleName, roleName, roleName, roleName, roleName, roleName)
	if _, err := conn.Exec(ctx, grants); err != nil {
		return fmt.Errorf("grants: %w", err)
	}
	return nil
}

func currentDB(ctx context.Context, conn *pgx.Conn) (string, error) {
	var name string
	if err := conn.QueryRow(ctx, `select current_database()`).Scan(&name); err != nil {
		return "", err
	}
	return name, nil
}

func seedDefaultPolicy(ctx context.Context, conn *pgx.Conn) error {
	return ensurePolicy(ctx, conn, false)
}

// ensurePolicy upserts the __defaults project + __policy entity, then
// inserts the defaultPolicy observations. If force=true, existing
// observations are soft-deleted first so the result reflects the current
// binary's defaultPolicy exactly. If force=false (used by `init`), an
// entity that already has observations is left untouched.
func ensurePolicy(ctx context.Context, conn *pgx.Conn, force bool) error {
	projectID := local.ProjectID(defaultsProjectSlug)
	if _, err := conn.Exec(ctx, `
		insert into projects (id, slug, name) values ($1, $2, $3)
		on conflict (slug) do update set name = excluded.name
	`, projectID, defaultsProjectSlug, "Default policy template"); err != nil {
		return err
	}

	entityID := local.EntityID(projectID, policyEntityName)
	if _, err := conn.Exec(ctx, `
		insert into entities (id, project_id, name, entity_type)
		values ($1, $2, $3, 'policy')
		on conflict (id) do nothing
	`, entityID, projectID, policyEntityName); err != nil {
		return err
	}

	var liveCount int
	if err := conn.QueryRow(ctx, `select count(*) from observations where entity_id = $1 and deleted_at is null`, entityID).Scan(&liveCount); err != nil {
		return err
	}

	if liveCount > 0 {
		if !force {
			return nil
		}
		// Soft-delete the existing rules so sync sees the change and
		// propagates the tombstones to every device.
		if _, err := conn.Exec(ctx,
			`update observations set deleted_at = now() where entity_id = $1 and deleted_at is null`,
			entityID); err != nil {
			return err
		}
	}

	for _, line := range defaultPolicy {
		if _, err := conn.Exec(ctx,
			`insert into observations (id, entity_id, content) values ($1, $2, $3)`,
			uuid.New().String(), entityID, line); err != nil {
			return err
		}
	}
	return nil
}

func mustDoDump(args []string) {
	fs := flag.NewFlagSet("dump", flag.ExitOnError)
	projectSlug := fs.String("project", "", "limit output to one project slug")
	limit := fs.Int("limit", 20, "max entities to list per project")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := connect(ctx, setupURL())
	if err != nil {
		fail("connect: %v", err)
	}
	defer conn.Close(ctx)

	if *projectSlug != "" {
		dumpProject(ctx, conn, *projectSlug, *limit)
		return
	}
	dumpAllProjects(ctx, conn, *limit)
}

func dumpAllProjects(ctx context.Context, conn *pgx.Conn, perProjectLimit int) {
	rows, err := conn.Query(ctx, `
		select p.slug, p.name,
		       (select count(*) from entities     e where e.project_id = p.id and e.deleted_at is null),
		       (select count(*) from observations o join entities e on e.id = o.entity_id where e.project_id = p.id and o.deleted_at is null),
		       (select count(*) from relations    r where r.project_id = p.id and r.deleted_at is null),
		       (select max(updated_at) from entities e where e.project_id = p.id)
		from projects p
		order by p.slug
	`)
	if err != nil {
		fail("list projects: %v", err)
	}
	defer rows.Close()

	type row struct {
		slug, name           string
		ents, obs, rels      int
		lastWrite            *time.Time
	}
	all := []row{}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.slug, &r.name, &r.ents, &r.obs, &r.rels, &r.lastWrite); err != nil {
			fail("scan: %v", err)
		}
		all = append(all, r)
	}

	fmt.Printf("%-40s %8s %8s %8s  %s\n", "PROJECT (slug)", "ents", "obs", "rels", "last write")
	fmt.Println(strings.Repeat("-", 90))
	for _, r := range all {
		last := "—"
		if r.lastWrite != nil {
			last = r.lastWrite.UTC().Format(time.RFC3339)
		}
		fmt.Printf("%-40s %8d %8d %8d  %s\n", r.slug, r.ents, r.obs, r.rels, last)
	}
	fmt.Println()
	fmt.Println("(use `dump -project <slug>` to drill into a project)")
}

func dumpProject(ctx context.Context, conn *pgx.Conn, slug string, limit int) {
	var projectID, name string
	if err := conn.QueryRow(ctx, `select id::text, name from projects where slug = $1`, slug).Scan(&projectID, &name); err != nil {
		fail("project %q: %v", slug, err)
	}
	fmt.Printf("Project: %s (%s)\n", slug, name)
	fmt.Printf("ID:      %s\n\n", projectID)

	// Per-device activity in the last 7 days.
	rows, err := conn.Query(ctx, `
		select device_id, count(*) as ops, max(occurred_at) as last_op
		from audit_log
		where project_id = $1 and occurred_at > now() - interval '7 days'
		group by device_id
		order by last_op desc
	`, projectID)
	if err != nil {
		fail("audit: %v", err)
	}
	defer rows.Close()
	fmt.Println("Recent activity (last 7 days):")
	fmt.Printf("  %-40s %6s  %s\n", "device_id", "ops", "last")
	any := false
	for rows.Next() {
		var dev string
		var ops int
		var last time.Time
		if err := rows.Scan(&dev, &ops, &last); err != nil {
			fail("scan audit: %v", err)
		}
		fmt.Printf("  %-40s %6d  %s\n", dev, ops, last.UTC().Format(time.RFC3339))
		any = true
	}
	if !any {
		fmt.Println("  (none)")
	}
	fmt.Println()

	// Most recent entities (live only).
	rows2, err := conn.Query(ctx, `
		select e.name, e.entity_type, e.updated_at, e.last_writer_device,
		       (select count(*) from observations o where o.entity_id = e.id and o.deleted_at is null) as obs_count
		from entities e
		where e.project_id = $1 and e.deleted_at is null
		order by e.updated_at desc
		limit $2
	`, projectID, limit)
	if err != nil {
		fail("entities: %v", err)
	}
	defer rows2.Close()
	fmt.Printf("Most recent entities (top %d, live only):\n", limit)
	fmt.Printf("  %-40s %-12s %6s  %s\n", "name", "type", "obs", "updated_at  // last_writer_device")
	for rows2.Next() {
		var n, t string
		var u time.Time
		var dev *string
		var obs int
		if err := rows2.Scan(&n, &t, &u, &dev, &obs); err != nil {
			fail("scan entity: %v", err)
		}
		devStr := "—"
		if dev != nil {
			devStr = *dev
		}
		fmt.Printf("  %-40s %-12s %6d  %s  //  %s\n",
			truncate(n, 40), truncate(t, 12), obs, u.UTC().Format(time.RFC3339), devStr)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 1 {
		return s
	}
	return s[:n-1] + "…"
}

func mustDoResetPolicy(args []string) {
	fs := flag.NewFlagSet("reset-policy", flag.ExitOnError)
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn, err := connect(ctx, setupURL())
	if err != nil {
		fail("connect: %v", err)
	}
	defer conn.Close(ctx)

	fmt.Fprintln(os.Stderr, "resetting __defaults/__policy to the binary's defaultPolicy...")
	if err := ensurePolicy(ctx, conn, true); err != nil {
		fail("reset policy: %v", err)
	}
	fmt.Fprintf(os.Stderr, "done — %d rule(s) seeded.\n", len(defaultPolicy))
}

func newPassword() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		fail("rand: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func buildScopedURL(rawURL, password string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	// Supabase poolers route by tenant via the username suffix:
	//   postgres.<project_ref>  →  user "postgres" for project <project_ref>
	// Preserve that suffix when swapping in our scoped role name so the
	// printed connection string targets the right tenant out of the box.
	scopedUser := roleName
	if oldUser := u.User.Username(); oldUser != "" {
		if i := strings.IndexByte(oldUser, '.'); i >= 0 {
			scopedUser = roleName + oldUser[i:]
		}
	}
	u.User = url.UserPassword(scopedUser, password)

	// Default the runtime URL to the transaction pooler if setup ran
	// against the session pooler — pgx with simple_protocol expects port
	// 6543.
	if u.Port() == "5432" && strings.Contains(u.Host, "pooler.supabase.com") {
		u.Host = strings.TrimSuffix(u.Host, ":5432") + ":6543"
	}

	q := u.Query()
	if q.Get("sslmode") == "" {
		q.Set("sslmode", "verify-full")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func escapeSQL(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
