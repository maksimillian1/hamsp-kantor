package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kana-consultant/kantor/backend/internal/repository"
	"github.com/kana-consultant/kantor/backend/internal/response"
	"github.com/kana-consultant/kantor/backend/internal/tenant"
)

const notificationsStreamPath = "/api/v1/notifications/stream"

type tenantPoolCtxKey struct{}

func withTenantPool(ctx context.Context, pool *pgxpool.Pool) context.Context {
	return context.WithValue(ctx, tenantPoolCtxKey{}, pool)
}

func tenantPoolFromContext(ctx context.Context) (*pgxpool.Pool, bool) {
	pool, ok := ctx.Value(tenantPoolCtxKey{}).(*pgxpool.Pool)
	return pool, ok && pool != nil
}

// DetachTenantContext creates a fresh background context that preserves tenant
// identity and tenant pool access without carrying a request-scoped DB
// connection. Use this before launching background goroutines from a request.
func DetachTenantContext(ctx context.Context) context.Context {
	detached := context.Background()

	if info, ok := tenant.FromContext(ctx); ok {
		detached = tenant.WithInfo(detached, info)
	}
	if pool, ok := tenantPoolFromContext(ctx); ok {
		detached = withTenantPool(detached, pool)
	}

	return detached
}

// WithScopedTenantConn ensures the callback runs with a tenant-scoped
// connection. If the context already carries a tenant-scoped DB handle, it is
// reused as-is.
func WithScopedTenantConn[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
	var zero T

	if repository.DB(ctx, nil) != nil {
		return fn(ctx)
	}

	pool, ok := tenantPoolFromContext(ctx)
	if !ok {
		return zero, errors.New("tenant pool is missing from context")
	}

	info, ok := tenant.FromContext(ctx)
	if !ok {
		return zero, errors.New("tenant info is missing from context")
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return zero, err
	}
	defer conn.Release()

	db, err := newTenantConn(ctx, conn, info.ID)
	if err != nil {
		return zero, err
	}

	return fn(repository.WithConn(ctx, db))
}

// AcquireTenantConn acquires a private tenant-scoped connection for a unit of
// work that runs in parallel with other workers (sharing one pgxpool.Conn
// across goroutines is not safe). Release it with ReleaseTenantConn.
func AcquireTenantConn(ctx context.Context, pool *pgxpool.Pool, tenantID string) (*TenantConn, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	db, err := newTenantConn(ctx, conn, tenantID)
	if err != nil {
		conn.Release()
		return nil, err
	}
	return db, nil
}

func ReleaseTenantConn(conn *TenantConn) {
	if conn == nil {
		return
	}
	conn.conn.Release()
}

// TenantMiddleware resolves the tenant from the Host header, acquires a
// dedicated connection from the pool, and configures RLS by setting the
// app.current_tenant GUC and (when superuser) downgrading to kantor_app.
func TenantMiddleware(pool *pgxpool.Pool, resolver *tenant.Resolver) func(http.Handler) http.Handler {
	return tenantMiddleware(pool, func(r *http.Request) (tenant.Info, bool) {
		info, err := resolver.Resolve(r.Context(), r.Host)
		return info, err == nil
	}, http.StatusBadRequest, "UNKNOWN_TENANT", "Could not resolve tenant for this domain")
}

// TenantHeader carries the platform's tenant ID, set only by the HAMSP API
// Gateway after it has validated the user's token.
const TenantHeader = "X-Tenant-Id"

// PlatformTenantMiddleware is TenantMiddleware for platform mode: the tenant
// comes only from TenantHeader; the Host header is ignored.
func PlatformTenantMiddleware(pool *pgxpool.Pool, resolver *tenant.ExternalResolver) func(http.Handler) http.Handler {
	return tenantMiddleware(pool, func(r *http.Request) (tenant.Info, bool) {
		values := r.Header.Values(TenantHeader)
		if len(values) != 1 {
			return tenant.Info{}, false
		}
		info, err := resolver.Resolve(r.Context(), values[0])
		if err != nil && !errors.Is(err, tenant.ErrInvalidExternalID) {
			slog.ErrorContext(r.Context(), "resolve platform tenant", "error", err)
		}
		return info, err == nil
	}, http.StatusUnauthorized, "UNKNOWN_TENANT", "Missing or unknown tenant")
}

func tenantMiddleware(pool *pgxpool.Pool, resolve func(*http.Request) (tenant.Info, bool), failStatus int, failCode, failMessage string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info, ok := resolve(r)
			if !ok {
				response.WriteError(w, failStatus, failCode, failMessage, nil)
				return
			}

			ctx := tenant.WithInfo(r.Context(), info)
			ctx = withTenantPool(ctx, pool)

			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, notificationsStreamPath) {
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			conn, err := pool.Acquire(r.Context())
			if err != nil {
				response.WriteError(w, http.StatusServiceUnavailable, "DB_UNAVAILABLE", "Could not acquire database connection", nil)
				return
			}
			defer conn.Release()

			db, err := newTenantConn(r.Context(), conn, info.ID)
			if err != nil {
				response.WriteError(w, http.StatusInternalServerError, "TENANT_SETUP_FAILED", "Failed to configure tenant context", nil)
				return
			}
			ctx = repository.WithConn(ctx, db)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ForEachTenant runs fn once per active tenant, setting up proper RLS context.
// Used for background jobs that need to iterate over all tenants.
func ForEachTenant(ctx context.Context, pool *pgxpool.Pool, fn func(ctx context.Context, t tenant.Info) error) error {
	rows, err := pool.Query(ctx, "SELECT id::text, slug, name FROM tenants WHERE is_active = true")
	if err != nil {
		return err
	}
	defer rows.Close()

	var tenants []tenant.Info
	for rows.Next() {
		var t tenant.Info
		if err := rows.Scan(&t.ID, &t.Slug, &t.Name); err != nil {
			return err
		}
		tenants = append(tenants, t)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, t := range tenants {
		if err := RunForTenant(ctx, pool, t, fn); err != nil {
			return err
		}
	}

	return nil
}

// RunForTenant runs fn on a connection scoped to tenant t.
func RunForTenant(ctx context.Context, pool *pgxpool.Pool, t tenant.Info, fn func(ctx context.Context, t tenant.Info) error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	db, err := newTenantConn(ctx, conn, t.ID)
	if err != nil {
		return err
	}
	return fn(repository.WithConn(tenant.WithInfo(ctx, t), db), t)
}
