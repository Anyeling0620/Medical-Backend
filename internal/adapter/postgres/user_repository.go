package postgres

import (
	"context"
	"database/sql"

	"Medical-Web-Backend/internal/domain/user"
)

type UserRepository struct{ db *sql.DB }

func NewUserRepository(db *sql.DB) *UserRepository { return &UserRepository{db: db} }

func (r *UserRepository) Authenticate(ctx context.Context, username, passwordHash string) (*user.User, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	query := `SELECT "id", "username" FROM hospital.mis_user WHERE "username" = $1 AND "password" = $2 LIMIT 1`
	result := &user.User{}
	if err := r.db.QueryRowContext(ctx, query, username, passwordHash).Scan(&result.ID, &result.Username); err != nil {
		return nil, err
	}
	result.PasswordHash = passwordHash
	return result, nil
}

func (r *UserRepository) Permissions(ctx context.Context, userID int64) ([]string, error) {
	if r == nil || r.db == nil {
		return nil, sql.ErrConnDone
	}
	query := `SELECT p."permission_code" AS "permission" FROM hospital.mis_user u JOIN hospital.mis_user_role ur ON u."id" = ur."user_id" JOIN hospital.mis_role_permission rp ON rp."role_id" = ur."role_id" JOIN hospital.mis_permission p ON rp."permission_id" = p."id" WHERE u."id" = $1`
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
