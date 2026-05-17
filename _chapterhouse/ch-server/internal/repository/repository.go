package repository

import (
	"context"
	"fmt"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository/sqlc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository wraps sqlc queries with transaction support.
type Repository struct {
	pool    *pgxpool.Pool
	queries *sqlc.Queries
}

// New creates a new repository with the given connection pool.
func New(pool *pgxpool.Pool) *Repository {
	return &Repository{
		pool:    pool,
		queries: sqlc.New(pool),
	}
}

// Queries returns the underlying sqlc queries interface.
func (r *Repository) Queries() *sqlc.Queries {
	return r.queries
}

// Pool returns the underlying connection pool.
func (r *Repository) Pool() *pgxpool.Pool {
	return r.pool
}

// WithTx executes the given function within a transaction.
// If the function returns an error, the transaction is rolled back.
// If the function succeeds, the transaction is committed.
func (r *Repository) WithTx(ctx context.Context, fn func(*sqlc.Queries) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
	}()

	qtx := r.queries.WithTx(tx)
	if err := fn(qtx); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			return fmt.Errorf("rollback failed: %w (original error: %v)", rbErr, err)
		}
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

// WithTxRaw executes fn inside a pgx transaction and returns the raw
// pgx.Tx to the callback. Counterpart to WithTx, which hands a sqlc
// *Queries — some repo paths (associations, episodic batch upsert)
// build SQL by hand on r.pool and need a pgx.Tx of the same shape to
// stay transactional. Auto-rollback on error or panic; commit on
// successful return.
func (r *Repository) WithTxRaw(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			return fmt.Errorf("rollback failed: %w (original error: %v)", rbErr, err)
		}
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// TxQuerier provides a queries interface within a transaction.
type TxQuerier struct {
	tx      pgx.Tx
	queries *sqlc.Queries
}

// BeginTx starts a new transaction and returns a TxQuerier.
func (r *Repository) BeginTx(ctx context.Context) (*TxQuerier, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}

	return &TxQuerier{
		tx:      tx,
		queries: r.queries.WithTx(tx),
	}, nil
}

// Queries returns the queries interface for this transaction.
func (tq *TxQuerier) Queries() *sqlc.Queries {
	return tq.queries
}

// Commit commits the transaction.
func (tq *TxQuerier) Commit(ctx context.Context) error {
	return tq.tx.Commit(ctx)
}

// Rollback rolls back the transaction.
func (tq *TxQuerier) Rollback(ctx context.Context) error {
	return tq.tx.Rollback(ctx)
}

// Close rolls back the transaction if not already committed.
// Safe to call multiple times.
func (tq *TxQuerier) Close(ctx context.Context) error {
	return tq.tx.Rollback(ctx)
}
