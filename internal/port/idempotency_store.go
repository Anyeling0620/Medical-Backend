package port

import (
	"context"
	"errors"
)

// ErrIdempotencyStoreUnavailable 表示幂等存储（Redis）当前不可用；
// 调用方不得静默降级为无保护写入，应返回 503 IDEMPOTENCY_STORE_UNAVAILABLE。
var ErrIdempotencyStoreUnavailable = errors.New("idempotency store unavailable")

// IdempotencyRecord 保存一次写请求的完整可重放结果。
// StatusCode 与 Body 用于原样返回第一次结果；Headers 仅需保留 Location 等自定义头。
type IdempotencyRecord struct {
	StatusCode int
	Headers    map[string]string
	Body       []byte
}

// IdempotencyStore 描述排班创建类写接口的幂等协调存储：
// Claim 原子占位防止并发重复创建，Save/Load 保存并重放第一次结果。
type IdempotencyStore interface {
	// Claim 原子占位（SET NX）。claimed=false 表示已有其他同 key 请求在途或已完成。
	Claim(ctx context.Context, key string) (bool, error)
	// Save 保存可重放结果，覆盖保存须使用首次执行的完整结果。
	Save(ctx context.Context, key string, record IdempotencyRecord) error
	// Load 读取已保存的结果；无记录返回 (nil, nil)。
	Load(ctx context.Context, key string) (*IdempotencyRecord, error)
	// Release 释放未产生可重放结果的占位（如内部 5xx），允许客户端按契约对 5xx 重试。
	Release(ctx context.Context, key string) error
}
