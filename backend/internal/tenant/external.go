package tenant

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrInvalidExternalID is returned for an external ID that is empty or not
// of the allowed form.
var ErrInvalidExternalID = errors.New("tenant: invalid external id")

var externalIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ExternalResolver maps the platform's tenant ID (tenants.external_id) to a
// KANTOR tenant, creating the tenant on its first request. A tenant is served
// only after onCreate (its idempotent defaults seed) has succeeded once.
type ExternalResolver struct {
	pool     *pgxpool.Pool
	onCreate func(ctx context.Context, info Info) error

	mu      sync.RWMutex
	cache   map[string]cachedInfo
	ttl     time.Duration
	createM sync.Mutex
}

type cachedInfo struct {
	info Info
	at   time.Time
}

func NewExternalResolver(pool *pgxpool.Pool, onCreate func(ctx context.Context, info Info) error) *ExternalResolver {
	return &ExternalResolver{pool: pool, onCreate: onCreate, cache: map[string]cachedInfo{}, ttl: 5 * time.Minute}
}

func (r *ExternalResolver) Resolve(ctx context.Context, externalID string) (Info, error) {
	if !externalIDPattern.MatchString(externalID) {
		return Info{}, ErrInvalidExternalID
	}

	r.mu.RLock()
	cached, ok := r.cache[externalID]
	r.mu.RUnlock()
	if ok && time.Since(cached.at) < r.ttl {
		return cached.info, nil
	}

	var info Info

	t, err := r.lookup(ctx, externalID)
	if err != nil {
		return Info{}, err
	}
	if !t.found || !t.seeded {
		if info, err = r.provision(ctx, externalID); err != nil {
			return Info{}, err
		}
	} else {
		info = t.info
	}

	r.mu.Lock()
	r.cache[externalID] = cachedInfo{info: info, at: time.Now()}
	r.mu.Unlock()
	return info, nil
}

type externalTenant struct {
	info   Info
	found  bool
	seeded bool
}

func (r *ExternalResolver) lookup(ctx context.Context, externalID string) (externalTenant, error) {
	var t externalTenant
	var active bool
	err := r.pool.QueryRow(ctx,
		`SELECT id::text, slug, name, is_active, defaults_seeded_at IS NOT NULL FROM tenants WHERE external_id = $1`, externalID,
	).Scan(&t.info.ID, &t.info.Slug, &t.info.Name, &active, &t.seeded)
	if errors.Is(err, pgx.ErrNoRows) {
		return externalTenant{}, nil
	}
	if err != nil {
		return externalTenant{}, fmt.Errorf("tenant: look up external id: %w", err)
	}
	if !active {
		return externalTenant{}, fmt.Errorf("tenant: tenant %s is inactive", t.info.Slug)
	}
	t.found = true
	return t, nil
}

func (r *ExternalResolver) provision(ctx context.Context, externalID string) (Info, error) {
	// One provisioner per process keeps concurrent first requests from
	// serving the tenant before its defaults exist.
	r.createM.Lock()
	defer r.createM.Unlock()

	t, err := r.lookup(ctx, externalID)
	if err != nil {
		return Info{}, err
	}
	if t.seeded {
		return t.info, nil
	}
	if !t.found {
		_, err := r.pool.Exec(ctx,
			`INSERT INTO tenants (name, slug, external_id) VALUES ($1, $2, $1)
			 ON CONFLICT (external_id) DO NOTHING`,
			externalID, "ext-"+externalID)
		if err != nil {
			return Info{}, fmt.Errorf("tenant: create: %w", err)
		}
		if t, err = r.lookup(ctx, externalID); err != nil {
			return Info{}, err
		}
		if !t.found {
			return Info{}, errors.New("tenant: created tenant not found")
		}
	}

	if r.onCreate != nil {
		if err := r.onCreate(ctx, t.info); err != nil {
			return Info{}, fmt.Errorf("tenant: seed defaults: %w", err)
		}
	}
	if _, err := r.pool.Exec(ctx, `UPDATE tenants SET defaults_seeded_at = NOW() WHERE id = $1`, t.info.ID); err != nil {
		return Info{}, fmt.Errorf("tenant: mark defaults seeded: %w", err)
	}
	return t.info, nil
}
