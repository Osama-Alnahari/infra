package client

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/lib/pq" //nolint:blank-imports

	dashboardqueries "github.com/e2b-dev/infra/packages/db/pkg/dashboard/queries"
	"github.com/e2b-dev/infra/packages/db/pkg/pool"
	database "github.com/e2b-dev/infra/packages/db/queries"
)

const snapshotLockNamespace = "e2b:snapshot:"
const snapshotGlobalLockKey = "e2b:snapshot:global-roots"

const poolName = "main"

type Client struct {
	*database.Queries

	Dashboard *dashboardqueries.Queries
	conn      *pgxpool.Pool
}

func NewClient(ctx context.Context, databaseURL string, options ...pool.Option) (*Client, error) {
	dbClient, connPool, err := pool.New(ctx, databaseURL, poolName, options...)
	if err != nil {
		return nil, err
	}

	return &Client{
		Queries:   database.New(dbClient),
		Dashboard: dashboardqueries.New(dbClient),
		conn:      connPool,
	}, nil
}

func (db *Client) Close() error {
	db.conn.Close()

	return nil
}

// WithTx starts a read-write transaction and returns a transactional Client.
func (db *Client) WithTx(ctx context.Context) (*Client, pgx.Tx, error) {
	tx, err := db.conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, nil, err
	}

	client := &Client{
		Queries:   db.Queries.WithTx(tx),
		Dashboard: db.Dashboard.WithTx(tx),
		conn:      db.conn,
	}

	return client, tx, nil
}

// WithSnapshotLock takes the shared side of the global snapshot fence, allowing
// independent pauses/build assignments to proceed concurrently while excluding
// the GC writer. The per-sandbox lock remains exclusive.
func (db *Client) WithSnapshotLock(ctx context.Context, sandboxID string, fn func(*Client) error) (err error) {
	return db.withSnapshotLock(ctx, sandboxID, false, fn)
}

// WithSnapshotGCLock takes the exclusive side of the global snapshot fence, so
// no root assignment can change between GC's final reachability read and delete.
func (db *Client) WithSnapshotGCLock(ctx context.Context, sandboxID string, fn func(*Client) error) (err error) {
	return db.withSnapshotLock(ctx, sandboxID, true, fn)
}

func (db *Client) withSnapshotLock(ctx context.Context, sandboxID string, exclusiveGlobal bool, fn func(*Client) error) (err error) {
	if sandboxID == "" {
		return fmt.Errorf("snapshot lock sandbox ID is empty")
	}
	txClient, tx, err := db.WithTx(ctx)
	if err != nil {
		return fmt.Errorf("begin snapshot lock transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil && rollbackErr != pgx.ErrTxClosed && err == nil {
			err = fmt.Errorf("rollback snapshot lock transaction: %w", rollbackErr)
		}
	}()

	// Lock ordering is always global then sandbox.
	globalLock := "SELECT pg_advisory_xact_lock_shared(hashtextextended($1, 0))"
	if exclusiveGlobal {
		globalLock = "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))"
	}
	if _, err = tx.Exec(ctx, globalLock, snapshotGlobalLockKey); err != nil {
		return fmt.Errorf("acquire global snapshot advisory lock: %w", err)
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", snapshotLockNamespace+sandboxID); err != nil {
		return fmt.Errorf("acquire sandbox snapshot advisory lock: %w", err)
	}
	if err = fn(txClient); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit snapshot lock transaction: %w", err)
	}
	return nil
}
