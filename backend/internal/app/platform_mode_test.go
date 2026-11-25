package app_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/kana-consultant/kantor/backend/internal/app"
	"github.com/kana-consultant/kantor/backend/internal/config"
)

var platformReplacedRoutes = []string{
	"POST /api/v1/auth/register",
	"POST /api/v1/auth/login",
	"POST /api/v1/auth/forgot-password",
	"GET /api/v1/auth/reset-password/validate",
	"POST /api/v1/auth/reset-password",
	"POST /api/v1/auth/refresh",
	"POST /api/v1/auth/logout",
	"POST /api/v1/auth/change-password",
	"PUT /api/v1/auth/profile/email",
	"GET /api/v1/auth/pat/",
	"POST /api/v1/oauth/grant",
	"GET /.well-known/oauth-authorization-server",
	"GET /.well-known/oauth-protected-resource",
	"POST /oauth/register",
	"GET /oauth/authorize",
	"POST /oauth/token",
	"POST /mcp",
	"GET /api/v1/tracker/extension/download",
	"POST /api/v1/tracker/heartbeat",
}

var trialOffModulePrefixes = []string{
	"/api/v1/wa/",
	"/api/v1/discord-reminder/",
	"/api/v1/operational/domains",
	"/api/v1/operational/vps",
	"/api/v1/operational/legal-",
	"/api/v1/hris/",
	"/api/v1/marketing/",
	"/api/v1/tracker/",
}

var trialOnRoutes = []string{
	"GET /api/v1/auth/me",
	"GET /api/v1/operational/projects/",
	"GET /api/v1/operational/projects/{projectID}/tasks/",
	"GET /api/v1/operational/projects/{projectID}/columns/",
	"GET /api/v1/operational/tasks/mine",
	"GET /api/v1/notifications/",
	"GET /api/v1/admin/users",
	"GET /healthz",
}

func routeTable(t *testing.T, platformMode bool) map[string]bool {
	t.Helper()
	routes, err := app.BuildRouteTableForInspection(config.Config{PlatformMode: platformMode})
	if err != nil {
		t.Fatalf("build route table: %v", err)
	}
	table := map[string]bool{}
	err = chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		table[method+" "+strings.ReplaceAll(route, "/*/", "/")] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	return table
}

// Verifies: platform mode removes the routes the platform replaces and the modules that are off for the trial.
func TestPlatformModeRemovesReplacedRoutesAndModules(t *testing.T) {
	table := routeTable(t, true)

	for _, route := range platformReplacedRoutes {
		if table[route] {
			t.Errorf("platform mode still serves %s", route)
		}
	}
	for route := range table {
		path := route[strings.Index(route, " ")+1:]
		for _, prefix := range trialOffModulePrefixes {
			if strings.HasPrefix(path, prefix) {
				t.Errorf("platform mode still serves %s", route)
			}
		}
	}
	for _, route := range trialOnRoutes {
		if !table[route] {
			t.Errorf("platform mode lost %s", route)
		}
	}
}

// Verifies: with the flag off every route is still served.
func TestPlatformModeOffKeepsAllRoutes(t *testing.T) {
	table := routeTable(t, false)

	for _, route := range append(append([]string{}, platformReplacedRoutes...), trialOnRoutes...) {
		if !table[route] {
			t.Errorf("flag off lost %s", route)
		}
	}
	for _, prefix := range trialOffModulePrefixes {
		found := false
		for route := range table {
			if strings.HasPrefix(route[strings.Index(route, " ")+1:], prefix) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("flag off lost every route under %s", prefix)
		}
	}
}
