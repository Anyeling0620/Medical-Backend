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
	// idemClaimSuffix 占位键后缀：在结果键上追加，SET NX 原子占位防止并发重复创建。
	idemClaimSuffix = ":claim"
	// idempotencyTTL 幂等记录保留时长（契约要求不少于 24 小时）。
	idempotencyTTL = 24 * time.Hour
)

// RedisIdempotencyStore 用 Redis 实现幂等存储：claim 键做原子占位，
// result 键保存首次执行的响应，重试请求据此原样重放。Redis 不可用时不静默降级。
type RedisIdempotencyStore struct {
	client redis.UniversalClient
}

// NewRedisIdempotencyStore 构造幂等存储。
func NewRedisIdempotencyStore(client redis.UniversalClient) *RedisIdempotencyStore {
	return &RedisIdempotencyStore{client: client}
}

func (r *RedisIdempotencyStore) Claim(ctx context.Context, key string) (bool, error) {
	if r == nil || r.client == nil {
		return false, port.ErrIdempotencyStoreUnavailable
	}
	claimed, err := r.client.SetNX(ctx, claimKey(key), "1", idempotencyTTL).Result()
	if err != nil {
		return false, port.ErrIdempotencyStoreUnavailable
	}
	return claimed, nil
}

func (r *RedisIdempotencyStore) Save(ctx context.Context, key string, record port.IdempotencyRecord) error {
	if r == nil || r.client == nil {
		return port.ErrIdempotencyStoreUnavailable
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := r.client.Set(ctx, key, payload, idempotencyTTL).Err(); err != nil {
		return port.ErrIdempotencyStoreUnavailable
	}
	return nil
}

func (r *RedisIdempotencyStore) Load(ctx context.Context, key string) (*port.IdempotencyRecord, error) {
	if r == nil || r.client == nil {
		return nil, port.ErrIdempotencyStoreUnavailable
	}
	payload, err := r.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, port.ErrIdempotencyStoreUnavailable
	}
	var record port.IdempotencyRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		return nil, err
	}
	return &record, nil
}

// Release 释放占位键：内部错误（5xx）不产生可重放结果，删除占位以允许客户端重试。
func (r *RedisIdempotencyStore) Release(ctx context.Context, key string) error {
	if r == nil || r.client == nil {
		return port.ErrIdempotencyStoreUnavailable
	}
	if err := r.client.Del(ctx, claimKey(key)).Err(); err != nil {
		return port.ErrIdempotencyStoreUnavailable
	}
	return nil
}

// claimKey 返回原子占位键：结果键 + :claim 后缀。
func claimKey(key string) string {
	return key + idemClaimSuffix
}
