// Package filestore keeps uploaded files on local disk or in S3-compatible
// object storage. Tenant keys every object by the request's tenant, so one
// tenant can never address another tenant's files.
package filestore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/kana-consultant/kantor/backend/internal/tenant"
	"github.com/kana-consultant/kantor/backend/internal/uploads"
)

var (
	ErrNotFound      = errors.New("filestore: not found")
	ErrInvalidKey    = errors.New("filestore: invalid key")
	ErrTenantMissing = errors.New("filestore: tenant missing from context")
)

// Backend stores objects by key.
type Backend interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Get(ctx context.Context, key string) ([]byte, error)
	Size(ctx context.Context, key string) (int64, error)
	Delete(ctx context.Context, key string) error
}

var keySegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func checkKey(key string) error {
	if key == "" || path.Clean(key) != key || strings.HasPrefix(key, "/") {
		return ErrInvalidKey
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "." || segment == ".." || !keySegment.MatchString(segment) {
			return ErrInvalidKey
		}
	}
	return nil
}

// Tenant is the store the application uses. With prefix set, every key is
// stored under the tenant ID taken from the context; without it keys are used
// as given (KANTOR's original single-host layout).
type Tenant struct {
	backend Backend
	prefix  bool
}

func NewTenant(backend Backend, prefix bool) *Tenant {
	return &Tenant{backend: backend, prefix: prefix}
}

func (t *Tenant) key(ctx context.Context, key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	if !t.prefix {
		return key, nil
	}
	info, ok := tenant.FromContext(ctx)
	if !ok {
		return "", ErrTenantMissing
	}
	id, err := uuid.Parse(info.ID)
	if err != nil {
		return "", ErrTenantMissing
	}
	return id.String() + "/" + key, nil
}

func (t *Tenant) Put(ctx context.Context, key string, data []byte, contentType string) error {
	full, err := t.key(ctx, key)
	if err != nil {
		return err
	}
	return t.backend.Put(ctx, full, data, contentType)
}

func (t *Tenant) Get(ctx context.Context, key string) ([]byte, error) {
	full, err := t.key(ctx, key)
	if err != nil {
		return nil, err
	}
	return t.backend.Get(ctx, full)
}

func (t *Tenant) Size(ctx context.Context, key string) (int64, error) {
	full, err := t.key(ctx, key)
	if err != nil {
		return 0, err
	}
	return t.backend.Size(ctx, full)
}

func (t *Tenant) Delete(ctx context.Context, key string) error {
	full, err := t.key(ctx, key)
	if err != nil {
		return err
	}
	return t.backend.Delete(ctx, full)
}

// Local keeps objects as files under a directory.
type Local struct{ root string }

func NewLocal(root string) *Local { return &Local{root: root} }

func (l *Local) path(key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	return filepath.Join(l.root, filepath.FromSlash(key)), nil
}

func (l *Local) Put(_ context.Context, key string, data []byte, _ string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	return uploads.WriteFileAtomic(p, data, 0o640, 0o750)
}

func (l *Local) Get(_ context.Context, key string) ([]byte, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return data, err
}

func (l *Local) Size(_ context.Context, key string) (int64, error) {
	p, err := l.path(key)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

// S3Config configures an S3-compatible bucket (MinIO in the prototype).
type S3Config struct {
	Endpoint        string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	UseSSL          bool
	Region          string
}

// S3 keeps objects in one bucket.
type S3 struct {
	client *minio.Client
	bucket string
}

func NewS3(cfg S3Config) (*S3, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("filestore: s3 client: %w", err)
	}
	return &S3{client: client, bucket: cfg.Bucket}, nil
}

// Check fails when the bucket is missing or not reachable with these credentials.
func (s *S3) Check(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("filestore: s3 bucket %q: %w", s.bucket, err)
	}
	if !ok {
		return fmt.Errorf("filestore: s3 bucket %q does not exist", s.bucket)
	}
	return nil
}

func isS3NotFound(err error) bool {
	code := minio.ToErrorResponse(err).Code
	return code == "NoSuchKey" || code == "NotFound"
}

func (s *S3) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = obj.Close() }()
	data, err := io.ReadAll(obj)
	if err != nil && isS3NotFound(err) {
		return nil, ErrNotFound
	}
	return data, err
}

func (s *S3) Size(ctx context.Context, key string) (int64, error) {
	if err := checkKey(key); err != nil {
		return 0, err
	}
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isS3NotFound(err) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return info.Size, nil
}

// Delete reports ErrNotFound for a missing object, as Local does; S3 itself
// does not tell a delete of a missing key apart.
func (s *S3) Delete(ctx context.Context, key string) error {
	if _, err := s.Size(ctx, key); err != nil {
		return err
	}
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}
