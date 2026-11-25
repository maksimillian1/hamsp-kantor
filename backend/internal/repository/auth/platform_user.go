package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/kana-consultant/kantor/backend/internal/repository"
)

// platformPasswordHash is not a bcrypt hash, so no password ever matches a
// user created in platform mode; the platform signs users in.
const platformPasswordHash = "!platform"

// EnsurePlatformUser returns the ID of the tenant's user with this platform
// user ID, creating the user on first sight. The tenant's first user becomes
// its super admin; later users get no roles until a KANTOR admin grants them.
func (r *Repository) EnsurePlatformUser(ctx context.Context, externalID string) (string, error) {
	db := repository.DB(ctx, r.db)

	var userID string
	err := db.QueryRow(ctx, `SELECT id::text FROM users WHERE external_id = $1`, externalID).Scan(&userID)
	if err == nil {
		return userID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("look up platform user: %w", err)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Serializes first sign-ins within the tenant, across replicas, so only one user becomes the first.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('kantor-platform-user:' || current_setting('app.current_tenant')))`,
	); err != nil {
		return "", fmt.Errorf("lock platform users: %w", err)
	}
	err = tx.QueryRow(ctx, `SELECT id::text FROM users WHERE external_id = $1`, externalID).Scan(&userID)
	if err == nil {
		return userID, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("look up platform user: %w", err)
	}

	var hasUsers bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users)`).Scan(&hasUsers); err != nil {
		return "", fmt.Errorf("count users: %w", err)
	}

	user, err := r.CreateUserWithRoles(repository.WithConn(ctx, tx), CreateUserParams{
		Email:        externalID + "@users.hamsp.invalid",
		PasswordHash: platformPasswordHash,
		FullName:     externalID,
	}, nil)
	if err != nil {
		return "", fmt.Errorf("create platform user: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE users SET external_id = $2, is_super_admin = $3 WHERE id = $1`,
		user.ID, externalID, !hasUsers,
	); err != nil {
		return "", fmt.Errorf("link platform user: %w", err)
	}
	return user.ID, tx.Commit(ctx)
}
