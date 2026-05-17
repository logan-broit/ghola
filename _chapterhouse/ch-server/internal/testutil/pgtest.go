package testutil

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// EphemeralPostgres is a throwaway Postgres instance with pgvector +
// pgcrypto available. Tests call NewEphemeralPostgres(t) for a
// dedicated database pinned to their test. The container stops when
// the test ends.
type EphemeralPostgres struct {
	Pool      *pgxpool.Pool
	DSN       string
	container testcontainers.Container
}

// NewEphemeralPostgres boots a pgvector-enabled Postgres container
// and returns a connected pool. Callers that exercise migrations
// should set EMBEDDING_DIM before calling the runner so the
// ${EMBEDDING_DIM} placeholder resolves deterministically.
func NewEphemeralPostgres(t *testing.T) *EphemeralPostgres {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// pgvector/pgvector ships Postgres + the vector extension
	// pre-installed. Pin a known tag so CI stays reproducible.
	container, err := postgres.Run(
		ctx,
		"pgvector/pgvector:pg17",
		postgres.WithDatabase("episodic_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err, "start pgvector container")

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err, "build DSN")

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err, "open pgxpool")

	require.NoError(t, pool.Ping(ctx), "ping pg")

	t.Cleanup(func() {
		pool.Close()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		if err := container.Terminate(stopCtx); err != nil {
			// Best-effort; container will be GC'd by the testcontainers reaper.
			t.Logf("container terminate: %v", err)
		}
	})

	return &EphemeralPostgres{Pool: pool, DSN: dsn, container: container}
}

// QueryTables returns the names of user tables in a schema, sorted.
func QueryTables(t *testing.T, pool *pgxpool.Pool, schema string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := pool.Query(ctx,
		`SELECT tablename FROM pg_tables WHERE schemaname = $1 ORDER BY tablename`,
		schema,
	)
	require.NoError(t, err, "query pg_tables")
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		out = append(out, name)
	}
	require.NoError(t, rows.Err())
	return out
}

// ColumnVectorDim returns the declared pgvector dimension for a column
// of type `vector(N)`. Returns 0 if the column isn't a vector.
func ColumnVectorDim(t *testing.T, pool *pgxpool.Pool, schema, table, column string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// format_type(atttypid, atttypmod) renders the full 'vector(<N>)'.
	var formatted string
	err := pool.QueryRow(ctx, `
		SELECT format_type(a.atttypid, a.atttypmod)
		FROM pg_attribute a
		JOIN pg_class c    ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attname = $3`,
		schema, table, column,
	).Scan(&formatted)
	require.NoError(t, err, "query pg_attribute format_type")

	var dim int
	if _, err := fmt.Sscanf(formatted, "vector(%d)", &dim); err != nil {
		return 0
	}
	return dim
}
