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
	// DeleteRefreshSession 按令牌摘要删除 refresh 会话。
	//
	// sessionID 用于一并清除 sessionID -> tokenHash 反查索引，由调用方从已读取的
	// 会话原样传入。实现不应为了反查索引再做一次读取：多余读取会拉长并发轮换的
	// 读-删窗口，把「同一 refresh token 被并发使用」的概率放大。
	DeleteRefreshSession(ctx context.Context, tokenHash string, sessionID string) error
	// DeleteRefreshSessionBySessionID 按会话 ID 撤销 refresh 会话。
	//
	// 登出接口允许客户端只提交 access token（spec/04-api-contract.md §7.2 要求
	// 登出必须同时撤销 refresh 会话），而 refresh 会话在存储中只按令牌摘要索引，
	// 因此实现必须自行维护 sessionID -> tokenHash 的反查能力。
	// 会话不存在（已过期或已被轮换）时返回 (false, nil)，让登出保持幂等。
	DeleteRefreshSessionBySessionID(ctx context.Context, sessionID string) (bool, error)
	RevokeAccessToken(ctx context.Context, jti string, ttlSeconds int64) error
	IsAccessTokenRevoked(ctx context.Context, jti string) (bool, error)
}
