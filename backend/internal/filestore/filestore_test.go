package filestore

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/kana-consultant/kantor/backend/internal/tenant"
)

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
)

func inTenant(id string) context.Context {
	return tenant.WithInfo(context.Background(), tenant.Info{ID: id})
}

func backends(t *testing.T) map[string]Backend {
	t.Helper()
	out := map[string]Backend{"local": NewLocal(t.TempDir())}
	if endpoint := os.Getenv("KANTOR_TEST_S3_ENDPOINT"); endpoint != "" {
		s3, err := NewS3(S3Config{
			Endpoint:        endpoint,
			Bucket:          os.Getenv("KANTOR_TEST_S3_BUCKET"),
			AccessKeyID:     os.Getenv("KANTOR_TEST_S3_ACCESS_KEY"),
			SecretAccessKey: os.Getenv("KANTOR_TEST_S3_SECRET_KEY"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s3.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
		out["s3"] = s3
	}
	return out
}

// Verifies: keys that could leave their prefix are refused.
func TestKeysAreChecked(t *testing.T) {
	store := NewTenant(NewLocal(t.TempDir()), true)
	for _, key := range []string{"", "/abs", "../x", "a/../../x", "a//b", "a/./b", "a b", "a/b\x00"} {
		if err := store.Put(inTenant(tenantA), key, []byte("x"), ""); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("key %q: %v, want ErrInvalidKey", key, err)
		}
	}
}

// Verifies: a tenant-prefixed store cannot reach another tenant's object, and refuses a context without a tenant (T-01).
func TestTenantPrefixIsolates(t *testing.T) {
	for name, backend := range backends(t) {
		t.Run(name, func(t *testing.T) {
			store := NewTenant(backend, true)
			key := "profiles/u1/avatar.png"
			t.Cleanup(func() { _ = store.Delete(inTenant(tenantA), key) })

			if err := store.Put(inTenant(tenantA), key, []byte("a-data"), "image/png"); err != nil {
				t.Fatal(err)
			}
			if got, err := store.Get(inTenant(tenantA), key); err != nil || string(got) != "a-data" {
				t.Fatalf("A reads its own file: %q, %v", got, err)
			}
			if _, err := store.Get(inTenant(tenantB), key); !errors.Is(err, ErrNotFound) {
				t.Fatalf("B reads A's file: %v, want ErrNotFound", err)
			}
			if _, err := store.Size(inTenant(tenantB), key); !errors.Is(err, ErrNotFound) {
				t.Fatalf("B stats A's file: %v, want ErrNotFound", err)
			}
			if err := store.Delete(inTenant(tenantB), key); !errors.Is(err, ErrNotFound) {
				t.Fatalf("B deletes A's file: %v, want ErrNotFound", err)
			}
			if _, err := store.Get(context.Background(), key); !errors.Is(err, ErrTenantMissing) {
				t.Fatalf("no tenant: %v, want ErrTenantMissing", err)
			}
			if got, err := backend.Get(context.Background(), tenantA+"/"+key); err != nil || string(got) != "a-data" {
				t.Fatalf("object not stored under the tenant: %q, %v", got, err)
			}
		})
	}
}

// Verifies: Put, Get, Size and Delete behave the same on every backend.
func TestBackendRoundTrip(t *testing.T) {
	for name, backend := range backends(t) {
		t.Run(name, func(t *testing.T) {
			store := NewTenant(backend, true)
			ctx := inTenant(tenantA)
			key := "branding/logo.png"
			if err := store.Put(ctx, key, []byte("v1"), "image/png"); err != nil {
				t.Fatal(err)
			}
			if err := store.Put(ctx, key, []byte("v2-longer"), "image/png"); err != nil {
				t.Fatal(err)
			}
			if size, err := store.Size(ctx, key); err != nil || size != 9 {
				t.Fatalf("size %d, %v", size, err)
			}
			if err := store.Delete(ctx, key); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Get(ctx, key); !errors.Is(err, ErrNotFound) {
				t.Fatalf("after delete: %v", err)
			}
			if err := store.Delete(ctx, key); !errors.Is(err, ErrNotFound) {
				t.Fatalf("second delete: %v", err)
			}
		})
	}
}
