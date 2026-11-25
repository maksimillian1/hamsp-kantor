package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kana-consultant/kantor/backend/internal/rbac"
	authrepo "github.com/kana-consultant/kantor/backend/internal/repository/auth"
	"github.com/kana-consultant/kantor/backend/internal/tenant"
)

type platformAuthFixture struct {
	pool    *pgxpool.Pool
	handler http.Handler
	mu      sync.Mutex
	seen    []Principal
}

// The full platform chain: tenant from X-Tenant-Id, user from X-User-Id.
func newPlatformAuthFixture(t *testing.T) *platformAuthFixture {
	t.Helper()
	f := &platformAuthFixture{pool: platformTestPool(t)}
	resolver := tenant.NewExternalResolver(f.pool, func(ctx context.Context, info tenant.Info) error {
		return RunForTenant(ctx, f.pool, info, func(ctx context.Context, _ tenant.Info) error {
			return rbac.SeedDefaults(ctx, f.pool)
		})
	})
	cache := rbac.NewPermissionCache(f.pool, time.Minute)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := PrincipalFromContext(r.Context())
		f.mu.Lock()
		f.seen = append(f.seen, p)
		f.mu.Unlock()
	})
	auth := PlatformAuthMiddleware(authrepo.New(f.pool).EnsurePlatformUser, cache.Load)
	f.handler = PlatformTenantMiddleware(f.pool, resolver)(auth(inner))
	t.Cleanup(func() {
		ctx := context.Background()
		for _, p := range f.seen {
			_, _ = f.pool.Exec(ctx, `DELETE FROM employees WHERE user_id = $1`, p.UserID)
			_, _ = f.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, p.UserID)
		}
	})
	return f
}

func (f *platformAuthFixture) do(tenantExt, userExt string) (int, Principal) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(TenantHeader, tenantExt)
	if userExt != "" {
		req.Header.Set(UserHeader, userExt)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	f.mu.Lock()
	defer f.mu.Unlock()
	if rec.Code != http.StatusOK || len(f.seen) == 0 {
		return rec.Code, Principal{}
	}
	return rec.Code, f.seen[len(f.seen)-1]
}

func (f *platformAuthFixture) tenantID(t *testing.T, ext string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), `SELECT id::text FROM tenants WHERE external_id = $1`, ext).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Verifies: a request without a valid x-user-id gets 401 and reaches no handler.
func TestPlatformAuthMissingUser(t *testing.T) {
	f := newPlatformAuthFixture(t)
	a := newExternalID(t, f.pool)

	for _, bad := range []string{"", "a b", "x'--"} {
		if code, _ := f.do(a, bad); code != http.StatusUnauthorized {
			t.Errorf("user %q: status = %d, want 401", bad, code)
		}
	}
	if len(f.seen) != 0 {
		t.Fatal("handler reached without a user")
	}
}

// Verifies: the tenant's first user becomes super admin, later users get nothing, a returning user is the same user.
func TestPlatformAuthFirstUserIsAdmin(t *testing.T) {
	f := newPlatformAuthFixture(t)
	a := newExternalID(t, f.pool)

	code, first := f.do(a, "user1")
	if code != http.StatusOK || !first.IsSuperAdmin {
		t.Fatalf("first user: %d, super admin %v", code, first.IsSuperAdmin)
	}
	code, second := f.do(a, "user2")
	if code != http.StatusOK || second.IsSuperAdmin || len(second.Permissions) != 0 {
		t.Fatalf("second user: %d, super admin %v, permissions %v", code, second.IsSuperAdmin, second.Permissions)
	}
	if _, again := f.do(a, "user1"); again.UserID != first.UserID {
		t.Fatalf("returning user got %s, want %s", again.UserID, first.UserID)
	}
	if first.TenantID != f.tenantID(t, a) {
		t.Fatalf("principal tenant %s, want %s", first.TenantID, f.tenantID(t, a))
	}
}

// Verifies: one platform user ID in two tenants is two separate users, each bound to its tenant (T-01, T-04).
func TestPlatformAuthUserPerTenant(t *testing.T) {
	f := newPlatformAuthFixture(t)
	a, b := newExternalID(t, f.pool), newExternalID(t, f.pool)

	_, inA := f.do(a, "same-user")
	_, inB := f.do(b, "same-user")

	if inA.UserID == "" || inB.UserID == "" || inA.UserID == inB.UserID {
		t.Fatalf("users %q and %q, want two different users", inA.UserID, inB.UserID)
	}
	if inA.TenantID != f.tenantID(t, a) || inB.TenantID != f.tenantID(t, b) {
		t.Fatal("principal bound to the wrong tenant")
	}
}

// Verifies: concurrent first sign-ins of different users make exactly one super admin.
func TestPlatformAuthOneFirstAdmin(t *testing.T) {
	f := newPlatformAuthFixture(t)
	a := newExternalID(t, f.pool)
	if code, _ := f.do(a, ""); code != http.StatusUnauthorized {
		t.Fatalf("provisioning request: %d", code)
	}

	var wg sync.WaitGroup
	for _, u := range []string{"c1", "c2", "c3", "c4", "c5", "c6"} {
		wg.Add(1)
		go func() { defer wg.Done(); f.do(a, u) }()
	}
	wg.Wait()

	var admins int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE tenant_id = $1 AND is_super_admin`, f.tenantID(t, a),
	).Scan(&admins); err != nil {
		t.Fatal(err)
	}
	if admins != 1 {
		t.Fatalf("%d super admins, want 1", admins)
	}
}
