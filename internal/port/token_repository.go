package port

import (
	"context"
	"errors"

	"Medical-Web-Backend/internal/domain/auth"
)

// ErrRefreshSessionNotFound 表示 refresh 会话不存在或已过期。
// 它把「没有这个会话」与「读取会话失败」区分开：登出时前者是幂等空操作，
// 后者必须如实上报，避免在不知道会话归属的情况下继续做删除动作。
var ErrRefreshSessionNotFound = errors.New("refresh session not found")

// RefreshSession is the server-side state associated with one refresh token.
type RefreshSession struct {
	TokenHash string     `json:"-"`
	SessionID string     `json:"sessionId"`
	UserID    int64      `json:"userId"`
	Username  string     `json:"username"`
	Realm     auth.Realm `json:"realm"`
}

// TokenRepository stores refresh sessions and revoked access-token IDs.
type TokenRepository interface {
	SaveRefreshSession(ctx context.Context, session RefreshSession, ttlSeconds int64) error
	GetRefreshSession(ctx context.Context, tokenHash string) (*RefreshSession, error)
	DeleteRefreshSession(ctx context.Context, tokenHash string) error
	RevokeAccessToken(ctx context.Context, jti string, ttlSeconds int64) error
	IsAccessTokenRevoked(ctx context.Context, jti string) (bool, error)
}
