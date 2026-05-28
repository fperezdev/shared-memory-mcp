package local

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Open creates (or opens) the local cache at path. Required pragmas are
// applied unconditionally on the open connection.
//
// WAL gives us concurrent reads while we write; synchronous=NORMAL trades
// the "fsync on every commit" of synchronous=FULL for ~100× write
// throughput, which is fine because Postgres is the durable store. foreign
// keys are off by default in SQLite — we want them on. busy_timeout makes
// transient locks block briefly instead of failing immediately.
func Open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir cache: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// Single writer; many concurrent connections under WAL are fine for
	// reads, but our process is intrinsically single-writer (one MCP).
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(SchemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	return db, nil
}
