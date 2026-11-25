package files

import (
	"context"
	"errors"
	"testing"

	"github.com/kana-consultant/kantor/backend/internal/filestore"
	"github.com/kana-consultant/kantor/backend/internal/tenant"
)

// Verifies: T-01, tenant B cannot fetch tenant A's avatar even with A's user ID and file name.
func TestProfileAvatarIsTenantScoped(t *testing.T) {
	store := filestore.NewTenant(filestore.NewLocal(t.TempDir()), true)
	service := New(t.TempDir(), store, nil, nil, nil)
	ctxA := tenant.WithInfo(context.Background(), tenant.Info{ID: "11111111-1111-1111-1111-111111111111"})
	ctxB := tenant.WithInfo(context.Background(), tenant.Info{ID: "22222222-2222-2222-2222-222222222222"})
	const userID, name = "33333333-3333-3333-3333-333333333333", "1-avatar.png"

	if err := store.Put(ctxA, "profiles/"+userID+"/"+name, []byte("png"), "image/png"); err != nil {
		t.Fatal(err)
	}

	file, err := service.Resolve(ctxA, "profiles", userID, name)
	if err != nil {
		t.Fatalf("tenant A resolves its avatar: %v", err)
	}
	if data, err := service.Read(ctxA, file.Key); err != nil || string(data) != "png" {
		t.Fatalf("tenant A reads its avatar: %q, %v", data, err)
	}
	if file.OwnerUserID == nil || *file.OwnerUserID != userID {
		t.Fatalf("owner = %v, want %s", file.OwnerUserID, userID)
	}

	if _, err := service.Resolve(ctxB, "profiles", userID, name); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("tenant B resolves A's avatar: %v, want ErrFileNotFound", err)
	}
	if _, err := service.Read(ctxB, file.Key); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("tenant B reads A's avatar: %v, want ErrFileNotFound", err)
	}
	if err := store.Put(ctxA, "branding/logo.png", []byte("logo"), "image/png"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resolve(ctxA, "profiles", "..", "branding/logo.png"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("path traversal: %v, want ErrFileNotFound", err)
	}
}
