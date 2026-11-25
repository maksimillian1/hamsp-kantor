package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kana-consultant/kantor/backend/internal/repository"
	"github.com/kana-consultant/kantor/backend/internal/tenant"
)

func createTestTenant(t *testing.T, pool *pgxpool.Pool) tenant.Info {
	t.Helper()
	ext := newExternalID(t, pool)
	info := tenant.Info{Slug: "ext-" + ext, Name: ext}
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO tenants (name, slug, external_id) VALUES ($1, $2, $1) RETURNING id::text`, ext, info.Slug,
	).Scan(&info.ID); err != nil {
		t.Fatal(err)
	}
	return info
}

func currentTenant(ctx context.Context, db repository.DBTX) (string, error) {
	var v *string
	err := db.QueryRow(ctx, `SELECT current_setting('app.current_tenant', true)`).Scan(&v)
	if v == nil {
		return "", err
	}
	return *v, err
}

// Verifies: every DBTX method of a tenant connection runs with the tenant set, and nothing stays on the server connection afterwards.
func TestTenantConnLeavesNoSessionState(t *testing.T) {
	base := platformTestPool(t)
	cfg := base.Config().Copy()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	a := createTestTenant(t, base)
	ctx := context.Background()

	err = RunForTenant(ctx, pool, a, func(ctx context.Context, _ tenant.Info) error {
		db := repository.DB(ctx, nil)
		if got, err := currentTenant(ctx, db); err != nil || got != a.ID {
			return fmt.Errorf("QueryRow: tenant %q, %v", got, err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO system_settings (key, value) VALUES ('conn-test', '1')`); err != nil {
			return fmt.Errorf("Exec: %w", err)
		}
		rows, err := db.Query(ctx, `SELECT current_setting('app.current_tenant', true) FROM generate_series(1, 3)`)
		if err != nil {
			return fmt.Errorf("Query: %w", err)
		}
		seen, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(seen) != 3 || seen[0] != a.ID {
			return fmt.Errorf("Query: %v, %v", seen, err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			return fmt.Errorf("Begin: %w", err)
		}
		nested, err := tx.Begin(ctx)
		if err != nil {
			return fmt.Errorf("nested Begin: %w", err)
		}
		if got, err := currentTenant(ctx, nested); err != nil || got != a.ID {
			return fmt.Errorf("nested tx: tenant %q, %v", got, err)
		}
		if err := nested.Commit(ctx); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		batch := &pgx.Batch{}
		batch.Queue(`SELECT current_setting('app.current_tenant', true)`)
		br := db.SendBatch(ctx, batch)
		var got string
		if err := br.QueryRow().Scan(&got); err != nil || got != a.ID {
			return fmt.Errorf("SendBatch: tenant %q, %v", got, err)
		}
		if err := br.Close(); err != nil {
			return err
		}
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM system_settings WHERE key = 'conn-test'`).Scan(&n); err != nil || n != 1 {
			return fmt.Errorf("committed row: %d, %v", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var guc *string
	var role string
	if err := conn.QueryRow(ctx, `SELECT current_setting('app.current_tenant', true), current_user`).Scan(&guc, &role); err != nil {
		t.Fatal(err)
	}
	if guc != nil && *guc != "" {
		t.Fatalf("app.current_tenant = %q left on the connection", *guc)
	}
	if role == "kantor_app" {
		t.Fatal("role kantor_app left on the connection")
	}
}

// Verifies: through PgBouncer in transaction mode, concurrent requests of two tenants sharing server connections never see each other's data or tenant (T-01).
func TestTenantMiddlewareThroughPgBouncer(t *testing.T) {
	dsn := os.Getenv("KANTOR_TEST_PGBOUNCER_URL")
	if dsn == "" {
		t.Skip("KANTOR_TEST_PGBOUNCER_URL not set")
	}
	direct := platformTestPool(t)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	a, b := createTestTenant(t, direct), createTestTenant(t, direct)
	byExt := map[string]tenant.Info{a.Name: a, b.Name: b}
	handler := tenantMiddleware(pool, func(r *http.Request) (tenant.Info, bool) {
		info, ok := byExt[r.Header.Get(TenantHeader)]
		return info, ok
	}, http.StatusUnauthorized, "UNKNOWN_TENANT", "")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := tenant.MustFromContext(r.Context())
		db := repository.DB(r.Context(), nil)
		for i := range 5 {
			got, err := currentTenant(r.Context(), db)
			if err != nil || got != info.ID {
				http.Error(w, fmt.Sprintf("statement %d: tenant %q, want %q (%v)", i, got, info.ID, err), http.StatusInternalServerError)
				return
			}
		}
		if r.Method == http.MethodPost {
			if _, err := db.Exec(r.Context(), `INSERT INTO system_settings (key, value) VALUES ('bouncer-'||$1, '1')`, info.ID); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		var n int
		if err := db.QueryRow(r.Context(), `SELECT count(*) FROM system_settings WHERE key LIKE 'bouncer-%'`).Scan(&n); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-Count", strconv.Itoa(n))
	}))

	call := func(method string, who tenant.Info) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/x", nil)
		req.Header.Set(TenantHeader, who.Name)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	for _, who := range []tenant.Info{a, b} {
		if rec := call(http.MethodPost, who); rec.Code != http.StatusOK {
			t.Fatalf("seed %s: %d %s", who.Slug, rec.Code, rec.Body)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan string, 400)
	for i := range 400 {
		who := a
		if i%2 == 1 {
			who = b
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ctx.Err() != nil {
				return
			}
			rec := call(http.MethodGet, who)
			if rec.Code != http.StatusOK {
				errs <- fmt.Sprintf("%s: %d %s", who.Slug, rec.Code, rec.Body)
			} else if c := rec.Header().Get("X-Count"); c != "1" {
				errs <- fmt.Sprintf("%s sees %s settings, want only its own 1", who.Slug, c)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	// A client with no tenant must find no tenant on any server connection.
	for range 10 {
		var guc *string
		if err := pool.QueryRow(context.Background(), `SELECT current_setting('app.current_tenant', true)`).Scan(&guc); err != nil {
			t.Fatal(err)
		}
		if guc != nil && *guc != "" {
			t.Fatalf("a client without a tenant got app.current_tenant = %q", *guc)
		}
	}
}
