package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/middleware"
	userservice "Medical-Web-Backend/internal/usecase/misuser"
)

// idempotencyProcessingCode 表示“相同幂等键的请求仍在处理中”，配合 503 让客户端可重试。
const idempotencyProcessingCode = "IDEMPOTENCY_PROCESSING"

// idemConfig 幂等协调可调参数：poll 控制同 key 并发在途时的轮询节奏，save 控制业务结果
// 落库（Save）的有限重试。默认约 3 秒（150×20ms）与 3 次/50ms，测试可调小以加速超时断言。
type idemConfig struct {
	pollAttempts int
	pollInterval time.Duration
	saveAttempts int
	saveInterval time.Duration
}

// scheduleIdem 封装排班计划/时段两类写接口共享的幂等协调流程：
// Claim 原子占位 -> 读历史结果重放 -> 执行首次业务 -> Save 落库。
// plan/slot handler 通过嵌入本结构复用同一实现，避免两套占位/轮询/重放逻辑漂移。
type scheduleIdem struct {
	store port.IdempotencyStore
	idemConfig
}

// newScheduleIdem 构造幂等执行器，并应用默认参数。
func newScheduleIdem(store port.IdempotencyStore) *scheduleIdem {
	return &scheduleIdem{
		store: store,
		idemConfig: idemConfig{
			pollAttempts: 150,
			pollInterval: 20 * time.Millisecond,
			saveAttempts: 3,
			saveInterval: 50 * time.Millisecond,
		},
	}
}

// claimAndRun 实现“占位 -> 重放 -> 执行业务 -> 保存”的幂等协调流程：
// 幂等存储不可用返回 503；同 key 重复请求原样重放第一次结果（成功与业务错误都重放）；
// 首次请求执行期间到达的并发重复轮询约 3 秒等待第一次结果，期间读到即重放，仍无结果时返回
// 503 IDEMPOTENCY_PROCESSING（契约可重试错误，避免 409 造成客户端死区）。抢到占位后也会先读
// 历史结果：占位按短 TTL 过期而 24h 结果仍在时直接重放，杜绝同 key 重跑业务造成重复创建/修改。
func (e *scheduleIdem) claimAndRun(c *gin.Context, idemKey string, run func(ctx context.Context) *opResult) {
	if e.store == nil {
		e.writeUnavailable(c)
		return
	}
	claimed, err := e.store.Claim(c.Request.Context(), idemKey)
	if err != nil {
		e.writeUnavailable(c)
		return
	}
	if !claimed {
		// 未抢到占位：优先重放已保存的第一次结果；首请求仍在途时按 pollAttempts 轮询等待。
		for attempt := 0; attempt < e.pollAttempts; attempt++ {
			record, loadErr := e.store.Load(c.Request.Context(), idemKey)
			if loadErr != nil {
				e.writeUnavailable(c)
				return
			}
			if record != nil {
				e.replay(c, record)
				return
			}
			// 仅非末次迭代后 sleep，避免总时长超出约 3 秒一拍的误差。
			if attempt < e.pollAttempts-1 {
				time.Sleep(e.pollInterval)
			}
		}
		e.writeProcessing(c)
		return
	}
	// 抢到占位：先查一次历史结果。旧 claim 已按短 TTL（10 分钟）过期而 24h 结果仍存在时，
	// 必须直接重放并释放本次占位，而不是重跑业务（同 key 重复创建风险）。
	record, loadErr := e.store.Load(c.Request.Context(), idemKey)
	if loadErr != nil {
		// 读不到结果状态时不冒险执行：释放本次占位并返回 503，客户端可安全重试；若历史
		// 结果实际已落库，重试会再次走“占位 -> 读历史 -> 重放”路径自行恢复。
		_ = e.store.Release(c.Request.Context(), idemKey)
		e.writeUnavailable(c)
		return
	}
	if record != nil {
		e.replay(c, record)
		// 结果已落库无需占位：释放本次新 claim，避免残留占位阻塞后续请求直到 TTL 到期。
		_ = e.store.Release(c.Request.Context(), idemKey)
		return
	}
	// 无历史结果：执行首次业务（创建/更新/删除）。
	result := run(c.Request.Context())
	if result == nil {
		// 业务内部错误不会产生可重放结果：释放本次占位，避免同 key 重试被占位阻塞至 TTL 到期。
		_ = e.store.Release(c.Request.Context(), idemKey)
		e.writeInternal(c)
		return
	}
	// 5xx 不入幂等存储，允许网络超时后客户端重试并重新创建/修改。
	if result.status >= 500 {
		e.writeResult(c, result)
		// 释放占位键，避免后续重试被残留占位挡住（占位 TTL 10 分钟内本也应自愈）。
		_ = e.store.Release(c.Request.Context(), idemKey)
		return
	}
	// 业务成功（或业务错误，如 409/404/422）：保存结果后原样重放给并发重复请求。
	// Save 做有限次重试（默认 3 次、间隔 50ms），覆盖“业务已提交但结果尚未落库”的窄窗口。
	var saveErr error
	for attempt := 0; attempt < e.saveAttempts; attempt++ {
		saveErr = e.store.Save(c.Request.Context(), idemKey, port.IdempotencyRecord{
			StatusCode: result.status,
			Headers:    result.headers,
			Body:       marshalBody(result.body),
		})
		if saveErr == nil {
			break
		}
		if attempt < e.saveAttempts-1 {
			time.Sleep(e.saveInterval)
		}
	}
	if saveErr != nil {
		// 有限重试仍失败（生产应记录告警日志/指标）：业务已执行无法回滚，只能释放占位并
		// 返回真实结果；此后同 key 重试可能重跑业务，属窄窗口已知取舍，不做后台 goroutine。
		_ = e.store.Release(c.Request.Context(), idemKey)
	}
	e.writeResult(c, result)
}

// replay 按保存的第一次结果原样重放响应。
func (e *scheduleIdem) replay(c *gin.Context, record *port.IdempotencyRecord) {
	for name, value := range record.Headers {
		c.Header(name, value)
	}
	if len(record.Body) == 0 {
		c.Status(record.StatusCode)
		return
	}
	c.Data(record.StatusCode, "application/json; charset=utf-8", record.Body)
}

// writeResult 把 opResult 写回客户端（Content-Type 与响应体序列化保持一致）。
func (e *scheduleIdem) writeResult(c *gin.Context, result *opResult) {
	if result == nil {
		e.writeInternal(c)
		return
	}
	for name, value := range result.headers {
		c.Header(name, value)
	}
	body := marshalBody(result.body)
	if len(body) == 0 {
		c.Status(result.status)
		return
	}
	c.Data(result.status, "application/json; charset=utf-8", body)
}

// writeError 直接输出统一契约错误体 {code, message}（无分页/资源 body）。
func (e *scheduleIdem) writeError(c *gin.Context, status int, code, message string) {
	payload, _ := json.Marshal(map[string]any{"code": code, "message": message})
	c.Data(status, "application/json; charset=utf-8", payload)
}

// writeInternal 返回 500 内部错误结果。
func (e *scheduleIdem) writeInternal(c *gin.Context) {
	e.writeError(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "服务器内部错误")
}

// writeUnavailable 返回 503 幂等存储不可用结果（不静默降级）。
func (e *scheduleIdem) writeUnavailable(c *gin.Context) {
	e.writeError(c, http.StatusServiceUnavailable, "IDEMPOTENCY_STORE_UNAVAILABLE", "幂等存储不可用，请稍后重试")
}

// writeProcessing 返回 503 IDEMPOTENCY_PROCESSING（契约可重试错误）。
func (e *scheduleIdem) writeProcessing(c *gin.Context) {
	e.writeError(c, http.StatusServiceUnavailable, idempotencyProcessingCode, "相同幂等键的请求正在处理中，请稍后重试")
}

// currentUserID 从 access token claims 中提取用户 ID，用于幂等键隔离不同操作者。
func currentUserID(c *gin.Context) (int64, error) {
	value, exists := c.Get(middleware.ClaimsKey)
	claims, ok := value.(*userservice.AccessClaims)
	if !exists || !ok || claims == nil {
		return 0, errors.New("访问令牌无效或已过期")
	}
	return claims.UserID, nil
}

// bindRequestResult 把请求体绑定错误映射为统一 HTTP 结果：JSON 语法错误、字段类型错误与
// 截断的 JSON（io.ErrUnexpectedEOF）返回 400 REQUEST_INVALID_JSON（避免泄漏内部英文错误）；
// 其余（空请求体、未知字段与语义错误）委托给调用方的 validation 回调转 422 中文文案。
// plan/slot 两类写接口共用，保证幂等“处理中”与“非法 JSON”语义一致。
func bindRequestResult(e error, validation func(error) *opResult) *opResult {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(e, &syntaxErr) || errors.As(e, &typeErr) || errors.Is(e, io.ErrUnexpectedEOF) {
		return &opResult{
			status: http.StatusBadRequest,
			body:   map[string]any{"code": "REQUEST_INVALID_JSON", "message": "请求体不是合法的 JSON"},
		}
	}
	return validation(e)
}
