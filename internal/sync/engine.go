// Package sync runs the local-first <-> Supabase reconciliation.
//
// Three responsibilities, single-flighted under one mutex:
//  1. Push: drain pending_writes against Postgres with idempotent upserts.
//  2. Pull: keyset-paginated delta from Postgres into the local cache.
//  3. Bootstrap: same as pull but called explicitly at startup.
//
// The engine owns one goroutine selecting on three channels:
// ticker (periodic), trigger (opportunistic), and ctx.Done() (shutdown).
package sync

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Engine struct {
	db        *sql.DB
	pool      *pgxpool.Pool
	projectID string
	deviceID  string
	interval  time.Duration
	pageSize  int

	mu        sync.Mutex
	triggerCh chan struct{}
	logger    io.Writer
}

// New builds an engine. pool may be nil for local-only mode; engine
// silently no-ops in that case.
func New(db *sql.DB, pool *pgxpool.Pool, projectID, deviceID string, intervalSeconds, pageSize int) *Engine {
	return &Engine{
		db:        db,
		pool:      pool,
		projectID: projectID,
		deviceID:  deviceID,
		interval:  time.Duration(intervalSeconds) * time.Second,
		pageSize:  pageSize,
		triggerCh: make(chan struct{}, 1),
		logger:    os.Stderr,
	}
}

// Run blocks until ctx is cancelled. Each tick: drain queue, then pull.
// Drains are also triggered opportunistically by Trigger().
func (e *Engine) Run(ctx context.Context) {
	if e.pool == nil {
		e.logf("local-only mode: sync engine idle")
		<-ctx.Done()
		return
	}
	t := time.NewTicker(e.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.cycle(ctx, false)
		case <-e.triggerCh:
			e.cycle(ctx, true)
		}
	}
}

// Trigger asks the engine to run a sync soon (non-blocking; drops if a
// trigger is already queued). Call this after a tool successfully wrote
// to the queue.
func (e *Engine) Trigger() {
	select {
	case e.triggerCh <- struct{}{}:
	default:
	}
}

// BootstrapPull does the initial paginated pull. Call before serving
// tools so the first tool call sees current data. Returns on first error
// or when all three resources have been fully pulled.
func (e *Engine) BootstrapPull(ctx context.Context) error {
	if e.pool == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pullAll(ctx)
}

func (e *Engine) cycle(ctx context.Context, triggered bool) {
	if !e.mu.TryLock() {
		// A previous cycle is still running.
		return
	}
	defer e.mu.Unlock()

	pushErr := e.drainQueue(ctx)
	if pushErr != nil {
		e.logf("push: %v", pushErr)
	}
	if !triggered {
		// Only pull on the periodic tick. Trigger() is for write-side
		// drains; a fresh pull on every write is unnecessary chatter.
		if err := e.pullAll(ctx); err != nil {
			e.logf("pull: %v", err)
		}
	}
}

func (e *Engine) drainQueue(ctx context.Context) error {
	return drainOnce(ctx, e.db, e.pool, e.deviceID, e.logger)
}

func (e *Engine) pullAll(ctx context.Context) error {
	return pullAll(ctx, e.db, e.pool, e.projectID, e.pageSize, e.logger)
}

func (e *Engine) logf(format string, args ...any) {
	fmt.Fprintf(e.logger, "[sync] "+format+"\n", args...)
}
