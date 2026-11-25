package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kana-consultant/kantor/backend/internal/repository"
	"github.com/kana-consultant/kantor/backend/internal/tenant"
)

func platformTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("KANTOR_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KANTOR_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newExternalID(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	id := "test" + hex.EncodeToString(b)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM system_settings WHERE tenant_id IN (SELECT id FROM tenants WHERE external_id = $1)`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE external_id = $1`, id)
	})
	return id
}

type platformTenantFixture struct {
	pool     *pgxpool.Pool
	handler  http.Handler
	created  atomic.Int32
	reached  atomic.Int32
	lastSeen atomic.Value
}

// The handler under the middleware writes a setting on POST and returns the
// number of settings with that key it can see on GET.
func newPlatformTenantFixture(t *testing.T) *platformTenantFixture {
	t.Helper()
	f := &platformTenantFixture{pool: platformTestPool(t)}
	resolver := tenant.NewExternalResolver(f.pool, func(context.Context, tenant.Info) error {
		f.created.Add(1)
		return nil
	})
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.reached.Add(1)
		info := tenant.MustFromContext(r.Context())
		f.lastSeen.Store(info.ID)
		db := repository.DB(r.Context(), nil)
		var guc string
		if err := db.QueryRow(r.Context(), `SELECT current_setting('app.current_tenant', true)`).Scan(&guc); err != nil || guc != info.ID {
			http.Error(w, "tenant GUC does not match", http.StatusInternalServerError)
			return
		}
		key := r.URL.Query().Get("key")
		if r.Method == http.MethodPost {
			if _, err := db.Exec(r.Context(), `INSERT INTO system_settings (key, value) VALUES ($1, '"x"')`, key); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			return
		}
		var n int
		if err := db.QueryRow(r.Context(), `SELECT count(*) FROM system_settings WHERE key = $1`, key).Scan(&n); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-Count", strconv.Itoa(n))
		w.WriteHeader(http.StatusOK)
	})
	f.handler = PlatformTenantMiddleware(f.pool, resolver)(inner)
	return f
}

func (f *platformTenantFixture) do(method, externalID, host, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/v1/things?key="+key, nil)
	if host != "" {
		req.Host = host
	}
	if externalID != "" {
		req.Header.Set(TenantHeader, externalID)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// Verifies: a request without x-tenant-id gets 401 and reaches no handler.
func TestPlatformTenantMissingHeader(t *testing.T) {
	f := newPlatformTenantFixture(t)

	rec := f.do(http.MethodGet, "", "localhost", "k")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if f.reached.Load() != 0 {
		t.Fatal("handler was reached without a tenant")
	}
}

// Verifies: a malformed x-tenant-id gets 401 and creates no tenant.
func TestPlatformTenantMalformedHeader(t *testing.T) {
	f := newPlatformTenantFixture(t)

	for _, bad := range []string{"a b", "x'; DROP TABLE tenants;--", "../etc", string(make([]byte, 200))} {
		rec := f.do(http.MethodGet, bad, "", "k")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("header %q: status = %d, want 401", bad, rec.Code)
		}
	}
	if f.reached.Load() != 0 || f.created.Load() != 0 {
		t.Fatal("a malformed header reached the handler or created a tenant")
	}
}

// Verifies: tenant A's data is invisible to tenant B (T-01).
func TestPlatformTenantIsolation(t *testing.T) {
	f := newPlatformTenantFixture(t)
	a, b := newExternalID(t, f.pool), newExternalID(t, f.pool)
	key := "isolation-" + a

	if rec := f.do(http.MethodPost, a, "", key); rec.Code != http.StatusCreated {
		t.Fatalf("write as A: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(http.MethodGet, a, "", key); rec.Header().Get("X-Count") != "1" {
		t.Fatalf("A sees %q of its own settings, want 1 (%d %s)", rec.Header().Get("X-Count"), rec.Code, rec.Body)
	}
	if rec := f.do(http.MethodGet, b, "", key); rec.Header().Get("X-Count") != "0" {
		t.Fatalf("B sees %q of A's settings, want 0 (%d %s)", rec.Header().Get("X-Count"), rec.Code, rec.Body)
	}
}

// Verifies: the Host header does not choose the tenant in platform mode.
func TestPlatformTenantIgnoresHost(t *testing.T) {
	f := newPlatformTenantFixture(t)
	a := newExternalID(t, f.pool)

	if rec := f.do(http.MethodGet, a, "localhost", "k"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var want string
	if err := f.pool.QueryRow(context.Background(), `SELECT id::text FROM tenants WHERE external_id = $1`, a).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if got := f.lastSeen.Load(); got != want {
		t.Fatalf("tenant = %v, want the one of x-tenant-id %s", got, want)
	}
}

// Verifies: an unknown tenant is created once, with its defaults hook run once, even under concurrent first requests.
func TestPlatformTenantCreatedOnce(t *testing.T) {
	f := newPlatformTenantFixture(t)
	a := newExternalID(t, f.pool)

	done := make(chan int, 8)
	for range 8 {
		go func() { done <- f.do(http.MethodGet, a, "", "k").Code }()
	}
	for range 8 {
		if code := <-done; code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM tenants WHERE external_id = $1`, a).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d tenants for one external id, want 1", n)
	}
	if c := f.created.Load(); c != 1 {
		t.Fatalf("defaults hook ran %d times, want 1", c)
	}
}
