package repo

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"Medical-Web-Backend/internal/port"
)

const (
	refreshKeyPrefix = "medical:auth:refresh:"
	revokedKeyPrefix = "medical:auth:revoked:access:"
)

// RedisTokenRepository implements server-side token state in Redis.
type RedisTokenRepository struct {
	client redis.UniversalClient
}

func NewRedisTokenRepository(client redis.UniversalClient) *RedisTokenRepository {
	return &RedisTokenRepository{client: client}
}

func (r *RedisTokenRepository) SaveRefreshSession(ctx context.Context, session port.RefreshSession, ttlSeconds int64) error {
	if r == nil || r.client == nil {
		return redis.ErrClosed
	}
	if ttlSeconds <= 0 {
		return nil
	}
	payload, err := json.Marshal(session)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, refreshKey(session.TokenHash), payload, time.Duration(ttlSeconds)*time.Second).Err()
}

func (r *RedisTokenRepository) GetRefreshSession(ctx context.Context, tokenHash string) (*port.RefreshSession, error) {
	if r == nil || r.client == nil {
		return nil, redis.ErrClosed
	}
	payload, err := r.client.Get(ctx, refreshKey(tokenHash)).Bytes()
	if err != nil {
		return nil, err
	}
	var session port.RefreshSession
	if err := json.Unmarshal(payload, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

func (r *RedisTokenRepository) DeleteRefreshSession(ctx context.Context, tokenHash string) error {
	if r == nil || r.client == nil {
		return redis.ErrClosed
	}
	return r.client.Del(ctx, refreshKey(tokenHash)).Err()
}

func (r *RedisTokenRepository) RevokeAccessToken(ctx context.Context, jti string, ttlSeconds int64) error {
	if r == nil || r.client == nil {
		return redis.ErrClosed
	}
	if ttlSeconds <= 0 {
		return nil
	}
	return r.client.Set(ctx, revokedKey(jti), "1", time.Duration(ttlSeconds)*time.Second).Err()
}

func (r *RedisTokenRepository) IsAccessTokenRevoked(ctx context.Context, jti string) (bool, error) {
	if r == nil || r.client == nil {
		return false, redis.ErrClosed
	}
	count, err := r.client.Exists(ctx, revokedKey(jti)).Result()
	return count > 0, err
}

func refreshKey(tokenHash string) string {
	return refreshKeyPrefix + tokenHash
}

func revokedKey(jti string) string {
	return revokedKeyPrefix + jti
}
