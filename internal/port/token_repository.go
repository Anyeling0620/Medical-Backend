package port

import "context"

// RefreshSession is the server-side state associated with one refresh token.
type RefreshSession struct {
	TokenHash string `json:"-"`
	SessionID string `json:"sessionId"`
	UserID    int64  `json:"userId"`
	Username  string `json:"username"`
}

// TokenRepository stores refresh sessions and revoked access-token IDs.
type TokenRepository interface {
	SaveRefreshSession(ctx context.Context, session RefreshSession, ttlSeconds int64) error
	GetRefreshSession(ctx context.Context, tokenHash string) (*RefreshSession, error)
	DeleteRefreshSession(ctx context.Context, tokenHash string) error
	RevokeAccessToken(ctx context.Context, jti string, ttlSeconds int64) error
	IsAccessTokenRevoked(ctx context.Context, jti string) (bool, error)
}
