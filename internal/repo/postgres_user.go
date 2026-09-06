package repo

import (
	"context"
	"database/sql"

	"Medical-Web-Backend/internal/domain/user"
)

// PostgresUserRepository implements MIS user and permission persistence.
type PostgresUserRepository struct {
	db *sql.DB
}

func NewPostgresUserRepository(db *sql.DB) *PostgresUserRepository {
	return &PostgresUserRepository{db: db}
}

func (r *PostgresUserRepository) FindByUsername(ctx context.Context, username string) (*user.User, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}

	const query = `
SELECT id, username, password, COALESCE(status, 0)
FROM hospital.mis_user
WHERE username = $1
LIMIT 1`

	return r.scanUser(ctx, query, username)
}

func (r *PostgresUserRepository) FindByID(ctx context.Context, userID int64) (*user.User, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}

	const query = `
SELECT id, username, password, COALESCE(status, 0)
FROM hospital.mis_user
WHERE id = $1
LIMIT 1`

	return r.scanUser(ctx, query, userID)
}

func (r *PostgresUserRepository) scanUser(ctx context.Context, query string, arg any) (*user.User, error) {
	result := &user.User{}
	if err := r.db.QueryRowContext(ctx, query, arg).Scan(
		&result.ID,
		&result.Username,
		&result.PasswordHash,
		&result.Status,
	); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *PostgresUserRepository) Permissions(ctx context.Context, userID int64) ([]string, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}

	const query = `
SELECT DISTINCT p.permission_code
FROM hospital.mis_user u
JOIN hospital.mis_user_role ur ON u.id = ur.user_id
JOIN hospital.mis_role_permission rp ON rp.role_id = ur.role_id
JOIN hospital.mis_permission p ON rp.permission_id = p.id
WHERE u.id = $1
ORDER BY p.permission_code`

	rows, err := r.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	permissions := make([]string, 0)
	for rows.Next() {
		var permission string
		if err := rows.Scan(&permission); err != nil {
			return nil, err
		}
		permissions = append(permissions, permission)
	}
	return permissions, rows.Err()
}
