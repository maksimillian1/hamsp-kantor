package notifications_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	notificationshandler "github.com/kana-consultant/kantor/backend/internal/handler/notifications"
	platformmiddleware "github.com/kana-consultant/kantor/backend/internal/middleware"
	"github.com/kana-consultant/kantor/backend/internal/rbac"
	authrepo "github.com/kana-consultant/kantor/backend/internal/repository/auth"
	notificationsrepo "github.com/kana-consultant/kantor/backend/internal/repository/notifications"
	notificationsservice "github.com/kana-consultant/kantor/backend/internal/service/notifications"
	"github.com/kana-consultant/kantor/backend/internal/tenant"
)

type streamState struct {
	UnreadCount int    `json:"unread_count"`
	LatestID    string `json:"latest_id"`
}

func externalID(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(b)
}

// firstEvent opens the stream as the given platform tenant and user and returns the first snapshot.
func firstEvent(t *testing.T, url, tenantExt, userExt string) streamState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/v1/notifications/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(platformmiddleware.TenantHeader, tenantExt)
	req.Header.Set(platformmiddleware.UserHeader, userExt)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
			var state streamState
			if err := json.Unmarshal([]byte(data), &state); err != nil {
				t.Fatal(err)
			}
			return state
		}
	}
	t.Fatalf("stream closed before the first event: %v", scanner.Err())
	return streamState{}
}

func ids(t *testing.T, pool *pgxpool.Pool, tenantExt, userExt string) (tenantID, userID string) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT t.id::text, u.id::text FROM tenants t JOIN users u ON u.tenant_id = t.id
		WHERE t.external_id = $1 AND u.external_id = $2`, tenantExt, userExt).Scan(&tenantID, &userID)
	if err != nil {
		t.Fatalf("look up %s/%s: %v", tenantExt, userExt, err)
	}
	return tenantID, userID
}

func insertNotification(t *testing.T, pool *pgxpool.Pool, tenantID, userID string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO notifications (tenant_id, user_id, type, title, message, created_at)
		VALUES ($1, $2, 'test', 'title', 'message', now()) RETURNING id::text`, tenantID, userID).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Verifies: T-01, a user of tenant A on the stream sees nothing of tenant B, even rows that name A's user ID.
func TestStreamIsTenantAndUserScoped(t *testing.T) {
	dsn := os.Getenv("KANTOR_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KANTOR_TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	resolver := tenant.NewExternalResolver(pool, func(ctx context.Context, info tenant.Info) error {
		return platformmiddleware.RunForTenant(ctx, pool, info, func(ctx context.Context, _ tenant.Info) error {
			return rbac.SeedDefaults(ctx, pool)
		})
	})
	handler := notificationshandler.New(notificationsservice.New(notificationsrepo.New(pool)))
	auth := platformmiddleware.PlatformAuthMiddleware(authrepo.New(pool).EnsurePlatformUser, rbac.NewPermissionCache(pool, time.Minute).Load)
	server := httptest.NewServer(platformmiddleware.PlatformTenantMiddleware(pool, resolver)(auth(http.HandlerFunc(handler.Stream))))
	t.Cleanup(server.Close)

	orgA, orgB := externalID(t, "testa"), externalID(t, "testb")
	t.Cleanup(func() {
		ctx := context.Background()
		for _, org := range []string{orgA, orgB} {
			_, _ = pool.Exec(ctx, `DELETE FROM notifications WHERE tenant_id IN (SELECT id FROM tenants WHERE external_id = $1)`, org)
			_, _ = pool.Exec(ctx, `DELETE FROM employees WHERE tenant_id IN (SELECT id FROM tenants WHERE external_id = $1)`, org)
			_, _ = pool.Exec(ctx, `DELETE FROM users WHERE tenant_id IN (SELECT id FROM tenants WHERE external_id = $1)`, org)
			_, _ = pool.Exec(ctx, `DELETE FROM system_settings WHERE tenant_id IN (SELECT id FROM tenants WHERE external_id = $1)`, org)
			_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE external_id = $1`, org)
		}
	})

	if got := firstEvent(t, server.URL, orgA, "alice"); got.UnreadCount != 0 {
		t.Fatalf("alice before any notification: %+v", got)
	}
	_ = firstEvent(t, server.URL, orgA, "carol")
	_ = firstEvent(t, server.URL, orgB, "bob")
	tenantA, alice := ids(t, pool, orgA, "alice")
	_, carol := ids(t, pool, orgA, "carol")
	tenantB, bob := ids(t, pool, orgB, "bob")

	own := insertNotification(t, pool, tenantA, alice)
	insertNotification(t, pool, tenantA, carol)
	insertNotification(t, pool, tenantB, bob)
	// A row of tenant B that names alice: only RLS keeps it from her stream.
	insertNotification(t, pool, tenantB, alice)

	if got := firstEvent(t, server.URL, orgA, "alice"); got.UnreadCount != 1 || got.LatestID != own {
		t.Fatalf("alice sees %+v, want 1 unread, latest %s", got, own)
	}
	if got := firstEvent(t, server.URL, orgB, "bob"); got.UnreadCount != 1 {
		t.Fatalf("bob sees %+v, want 1 unread", got)
	}
}
