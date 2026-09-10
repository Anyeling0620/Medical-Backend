package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"Medical-Web-Backend/internal/port"
)

const (
	refreshKeyPrefix = "medical:auth:refresh:"
	revokedKeyPrefix = "medical:auth:revoked:access:"
	// refreshSessionIndexKeyPrefix 是 sessionID -> tokenHash 的反查索引前缀。
	// 客户端只提交 access token 登出时，服务端凭 claims.sid 定位本次登录的
	// refresh 会话并撤销，否则登出后 refresh 令牌仍能续期（契约 §7.2）。
	refreshSessionIndexKeyPrefix = "medical:auth:refresh:session:"
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
	ttl := time.Duration(ttlSeconds) * time.Second
	// 主键与反查索引在同一次事务管道中写入，避免出现「查得到会话却按 sid 撤不掉」
	// 或「撤掉会话但索引仍指向已失效摘要」的中间状态。
	pipe := r.client.TxPipeline()
	pipe.Set(ctx, refreshKey(session.TokenHash), payload, ttl)
	if session.SessionID != "" {
		pipe.Set(ctx, refreshSessionKey(session.SessionID), session.TokenHash, ttl)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (r *RedisTokenRepository) GetRefreshSession(ctx context.Context, tokenHash string) (*port.RefreshSession, error) {
	if r == nil || r.client == nil {
		return nil, redis.ErrClosed
	}
	payload, err := r.client.Get(ctx, refreshKey(tokenHash)).Bytes()
	if err != nil {
		// 键不存在表示会话已过期或已被轮换，用哨兵错误与真正的读取失败区分。
		if errors.Is(err, redis.Nil) {
			return nil, port.ErrRefreshSessionNotFound
		}
		return nil, err
	}
	var session port.RefreshSession
	if err := json.Unmarshal(payload, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

func (r *RedisTokenRepository) DeleteRefreshSession(ctx context.Context, tokenHash string, sessionID string) error {
	if r == nil || r.client == nil {
		return redis.ErrClosed
	}
	// 主键与反查索引在同一次事务管道中删除。sessionID 由调用方从已读取的会话传入，
	// 这里不再为了反查索引多做一次读：读取失败会被静默吞掉并留下悬挂索引，
	// 而多余的读还会拉长并发轮换的读-删窗口。
	pipe := r.client.TxPipeline()
	pipe.Del(ctx, refreshKey(tokenHash))
	if sessionID != "" {
		pipe.Del(ctx, refreshSessionKey(sessionID))
	}
	_, err := pipe.Exec(ctx)
	return err
}

// DeleteRefreshSessionBySessionID 按会话 ID 撤销 refresh 会话，供「只带 access token」
// 的登出路径使用：先查反查索引拿到令牌摘要，再在同一管道里删除会话主键与索引键。
// 索引不存在表示会话已过期或已被轮换，返回 (false, nil) 保持登出幂等。
func (r *RedisTokenRepository) DeleteRefreshSessionBySessionID(
	ctx context.Context,
	sessionID string,
) (bool, error) {
	if r == nil || r.client == nil {
		return false, redis.ErrClosed
	}
	if sessionID == "" {
		return false, nil
	}
	tokenHash, err := r.client.Get(ctx, refreshSessionKey(sessionID)).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	pipe := r.client.TxPipeline()
	mainDeleted := pipe.Del(ctx, refreshKey(tokenHash))
	pipe.Del(ctx, refreshSessionKey(sessionID))
	if _, err := pipe.Exec(ctx); err != nil {
		return false, err
	}
	// 以会话主键是否真的被删除为准：索引可能残留（主键已过期或已被轮换），
	// 此时不能报告「已撤销」，否则调用方会误判会话仍然存在。
	return mainDeleted.Val() > 0, nil
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

// refreshSessionKey 返回 sessionID 维度的反查索引键。
func refreshSessionKey(sessionID string) string {
	return refreshSessionIndexKeyPrefix + sessionID
}
