package port

import (
	"context"

	"Medical-Web-Backend/internal/domain/user"
)

// UserRepository contains persistence operations used by MIS authentication.
type UserRepository interface {
	FindByUsername(ctx context.Context, username string) (*user.User, error)
	FindByID(ctx context.Context, userID int64) (*user.User, error)
	Permissions(ctx context.Context, userID int64) ([]string, error)
}
