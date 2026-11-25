package middleware

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TenantConn is a tenant-scoped repository.DBTX. Every statement runs in a
// transaction of its own that sets app.current_tenant with SET LOCAL
// semantics, so no tenant state outlives the transaction on the server
// connection. That keeps it correct behind PgBouncer in transaction mode,
// where consecutive transactions of one client may use different server
// connections, and keeps KANTOR's per-statement commits.
type TenantConn struct {
	conn     *pgxpool.Conn
	tenantID string
	setupSQL string
}

const (
	tenantSetupSQL         = "SELECT set_config('app.current_tenant', $1, true)"
	tenantSetupWithRoleSQL = "SELECT set_config('app.current_tenant', $1, true), set_config('role', 'kantor_app', true)"
)

func newTenantConn(ctx context.Context, conn *pgxpool.Conn, tenantID string) (*TenantConn, error) {
	if _, err := uuid.Parse(tenantID); err != nil {
		return nil, fmt.Errorf("invalid tenant id: %w", err)
	}
	// A superuser (Docker dev) bypasses RLS, so each transaction switches to
	// kantor_app; the platform's login role is never a superuser.
	var isSuperuser bool
	if err := conn.QueryRow(ctx, "SELECT current_setting('is_superuser') = 'on'").Scan(&isSuperuser); err != nil {
		return nil, fmt.Errorf("check superuser: %w", err)
	}
	setupSQL := tenantSetupSQL
	if isSuperuser {
		setupSQL = tenantSetupWithRoleSQL
	}
	return &TenantConn{conn: conn, tenantID: tenantID, setupSQL: setupSQL}, nil
}

func (c *TenantConn) begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := c.conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, c.setupSQL, c.tenantID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("set tenant guc: %w", err)
	}
	return tx, nil
}

func finish(ctx context.Context, tx pgx.Tx, err error) error {
	if err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	return tx.Commit(ctx)
}

// Begin starts a tenant-scoped transaction the caller commits or rolls back;
// nested Begin calls on it are savepoints.
func (c *TenantConn) Begin(ctx context.Context) (pgx.Tx, error) {
	return c.begin(ctx)
}

func (c *TenantConn) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tx, err := c.begin(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := tx.Exec(ctx, sql, args...)
	return tag, finish(ctx, tx, err)
}

func (c *TenantConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx, err := c.begin(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		_ = finish(ctx, tx, err)
		return nil, err
	}
	return &tenantRows{Rows: rows, ctx: ctx, tx: tx}, nil
}

func (c *TenantConn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	tx, err := c.begin(ctx)
	if err != nil {
		return errRow{err: err}
	}
	return &tenantRow{row: tx.QueryRow(ctx, sql, args...), ctx: ctx, tx: tx}
}

func (c *TenantConn) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	tx, err := c.begin(ctx)
	if err != nil {
		return errBatch{err: err}
	}
	return &tenantBatch{BatchResults: tx.SendBatch(ctx, b), ctx: ctx, tx: tx}
}

// tenantRows ends its transaction when the rows are exhausted or closed.
type tenantRows struct {
	pgx.Rows
	ctx  context.Context
	tx   pgx.Tx
	done bool
	err  error
}

func (r *tenantRows) end() {
	if r.done {
		return
	}
	r.done = true
	r.Rows.Close()
	r.err = finish(r.ctx, r.tx, r.Rows.Err())
}

func (r *tenantRows) Next() bool {
	if r.done {
		return false
	}
	if r.Rows.Next() {
		return true
	}
	r.end()
	return false
}

func (r *tenantRows) Close() { r.end() }

func (r *tenantRows) Err() error {
	if err := r.Rows.Err(); err != nil {
		return err
	}
	return r.err
}

type tenantRow struct {
	row pgx.Row
	ctx context.Context
	tx  pgx.Tx
}

func (r *tenantRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	var txErr error
	if errors.Is(err, pgx.ErrNoRows) {
		txErr = finish(r.ctx, r.tx, nil)
	} else {
		txErr = finish(r.ctx, r.tx, err)
	}
	if err != nil {
		return err
	}
	return txErr
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

type tenantBatch struct {
	pgx.BatchResults
	ctx  context.Context
	tx   pgx.Tx
	done bool
}

func (b *tenantBatch) Close() error {
	if b.done {
		return nil
	}
	b.done = true
	err := b.BatchResults.Close()
	if txErr := finish(b.ctx, b.tx, err); err == nil {
		err = txErr
	}
	return err
}

type errBatch struct{ err error }

func (b errBatch) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, b.err }
func (b errBatch) Query() (pgx.Rows, error)         { return nil, b.err }
func (b errBatch) QueryRow() pgx.Row                { return errRow(b) }
func (b errBatch) Close() error                     { return b.err }
