package port

import (
	"context"

	"Medical-Web-Backend/internal/domain/user"
)

// UserRepository contains the persistence operations used by authentication.
// Implementations should map Authenticate to the MIS_USER username/password
// lookup and Permissions to the user-role-role-permission joins.
type UserRepository interface {
	Authenticate(ctx context.Context, username, passwordHash string) (*user.User, error)
	Permissions(ctx context.Context, userID int64) ([]string, error)
}
