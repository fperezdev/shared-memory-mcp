// Package migrations bundles the numbered Postgres migration files into
// the binary at build time. The migrate runner in internal/remote reads
// from FS, so a single source of truth lives next to the .sql files.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
