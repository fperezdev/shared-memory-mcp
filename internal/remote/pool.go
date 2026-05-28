// Package remote talks to Supabase Postgres via pgx.
//
// All runtime traffic goes through the transaction pooler (port 6543),
// which requires queries to use the simple protocol — prepared statements
// don't survive PgBouncer in transaction mode. Setting QueryExecMode in
// the pool config or default_query_exec_mode=simple_protocol in the URL
// is non-negotiable; without it you get intermittent "prepared statement
// does not exist" errors that look like network flakes.
package remote

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Open returns a pgx pool configured for Supabase pooler use.
// Returns (nil, nil) if connStr is empty — local-only mode.
//
// If caCertPath is non-empty, its contents are loaded into RootCAs and
// VerifyConnection is left as default (full chain verification using only
// the provided CA). Otherwise pgx uses its parsed TLS config from the URL
// — typically the system trust store for sslmode=verify-full.
func Open(ctx context.Context, connStr, caCertPath string) (*pgxpool.Pool, error) {
	if connStr == "" {
		return nil, nil
	}
	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("parse db url: %w", err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	cfg.MaxConns = 2
	cfg.MinConns = 0
	cfg.MaxConnLifetime = 25 * time.Minute  // pooler kills at ~30
	cfg.MaxConnIdleTime = 2 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	if caCertPath != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pem, err := os.ReadFile(caCertPath)
		if err != nil {
			return nil, fmt.Errorf("read caCertPath %q: %w", caCertPath, err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("caCertPath %q: no usable PEM certs", caCertPath)
		}
		if cfg.ConnConfig.TLSConfig == nil {
			cfg.ConnConfig.TLSConfig = &tls.Config{}
		}
		cfg.ConnConfig.TLSConfig.RootCAs = pool
		cfg.ConnConfig.TLSConfig.ServerName = cfg.ConnConfig.Host
	}

	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	return p, nil
}
