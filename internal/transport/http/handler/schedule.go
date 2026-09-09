package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/domain/schedule"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/middleware"
	"Medical-Web-Backend/internal/transport/http/request"
	userservice "Medical-Web-Backend/internal/usecase/misuser"
	scheduleservice "Medical-Web-Backend/internal/usecase/schedule"
)

// ScheduleHandler 提供排班计划的四个 HTTP 接口。
// 幂等协调（Claim 原子占位 + 结果重放）位于 handler 层；业务规则在 use case/domain 层。
type ScheduleHandler struct {
	service *scheduleservice.Service
	store   port.IdempotencyStore
	// pollAttempts/pollInterval 控制同 key 并发在途时的轮询节奏；默认约 3 秒（150x20ms），
	// 期间读到已保存结果立即重放，耗尽仍无结果才返回 503。测试可调小以加速超时断言。
	pollAttempts int
	pollInterval time.Duration
	// saveAttempts/saveInterval 控制业务结果落库（Save）的有限重试；默认 3 次、间隔 50ms。
	// Save 全部失败才走降级（释放占位并返回真实结果），不启动后台 goroutine。测试可调小。
	saveAttempts int
	saveInterval time.Duration
}

// NewScheduleHandler 构造排班计划处理器。now 为空时回退到 time.Now。
func NewScheduleHandler(service *scheduleservice.Service, store port.IdempotencyStore) *ScheduleHandler {
	return &ScheduleHandler{
		service:      service,
		store:        store,
		pollAttempts: 150,
		pollInterval: 20 * time.Millisecond,
		saveAttempts: 3,
		saveInterval: 50 * time.Millisecond,
	}
}

// errorEnvelope 是契约统一的错误响应体 {code,message,details?}，现有实现不含 requestId。
type errorEnvelope struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// opResult 是单个写操作产生的 HTTP 结果：status + 自定义头 + 响应体。
type opResult struct {
	status  int
	headers map[string]string
	body    map[string]any
}

// ListPlans 处理 GET /api/v1/schedule/plans。
func (h *ScheduleHandler) ListPlans(c *gin.Context) {
	q, err := request.BindPlanList(c)
	if err != nil {
		h.validation(c, err)
		return
	}
	// fromDate 不得晚于 toDate（闭区间）。
	if q.FromDate != "" && q.ToDate != "" && q.FromDate > q.ToDate {
		h.writeError(c, http.StatusUnprocessableEntity, scheduleservice.CodeValidationFailed, "日期范围无效")
		return
	}
	items, total, err := h.service.ListPlans(c.Request.Context(), q.ToFilter(), q.Page, q.PageSize)
	if err != nil {
		h.internal(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "page": q.Page, "pageSize": q.PageSize, "total": total})
}

// CreatePlan 处理 POST /api/v1/schedule/plans（Idempotency-Key 幂等创建）。
func (h *ScheduleHandler) CreatePlan(c *gin.Context) {
	userID, serviceErr := h.currentUser(c)
	if serviceErr != nil {
		h.writeEnvelope(c, serviceErr)
		return
	}
	key, err := request.BindIdempotencyKey(c)
	if err != nil {
		h.validation(c, err)
		return
	}
	// scope 用“方法 + 实际 URL 路径”：POST 路径固定，天然隔离同一路径的不同请求体。
	idemKey := idempotencyKey(userID, c.Request.Method+"|"+c.Request.URL.Path, key)
	h.claimAndRun(c, idemKey, func(ctx context.Context) *opResult {
		body, bindErr := request.BindCreatePlan(c)
		if bindErr != nil {
			return h.validationResult(c, bindErr)
		}
		plan := schedule.WorkPlan{
			DoctorID:        body.DoctorID,
			SubdepartmentID: body.SubdepartmentID,
			Date:            body.Date,
			Maximum:         int16(body.Maximum),
		}
		result, err := h.service.CreatePlan(ctx, plan)
		if err != nil {
			return h.serviceErrorResult(c, err)
		}
		return &opResult{
			status:  http.StatusCreated,
			headers: map[string]string{"Location": planLocation(result.NewID)},
			body:    h.planBody(result.Plan),
		}
	})
}

// UpdatePlan 处理 PATCH /api/v1/schedule/plans/:planId（幂等键可选）。
func (h *ScheduleHandler) UpdatePlan(c *gin.Context) {
	planID, err := request.ParsePositiveID(c.Param("planId"))
	if err != nil {
		h.validation(c, err)
		return
	}
	run := func(ctx context.Context) *opResult {
		body, bindErr := request.BindUpdatePlanMaximum(c)
		if bindErr != nil {
			return h.validationResult(c, bindErr)
		}
		result, err := h.service.UpdatePlan(ctx, planID, int16(body.Maximum))
		if err != nil {
			return h.serviceErrorResult(c, err)
		}
		return &opResult{status: http.StatusOK, body: h.planBody(result.Plan)}
	}
	h.writeWithOptionalIdempotency(c, run)
}

// DeletePlan 处理 DELETE /api/v1/schedule/plans/:planId（物理删除，幂等键可选）。
func (h *ScheduleHandler) DeletePlan(c *gin.Context) {
	planID, err := request.ParsePositiveID(c.Param("planId"))
	if err != nil {
		h.validation(c, err)
		return
	}
	run := func(ctx context.Context) *opResult {
		if err := h.service.DeletePlan(ctx, planID); err != nil {
			return h.serviceErrorResult(c, err)
		}
		return &opResult{status: http.StatusNoContent}
	}
	h.writeWithOptionalIdempotency(c, run)
}

// runWriteOnce 幂等键缺失时（PATCH/DELETE 不强制要求键）直接执行一次并写出结果。
func (h *ScheduleHandler) runWriteOnce(c *gin.Context, run func(ctx context.Context) *opResult) {
	result := run(c.Request.Context())
	if result == nil {
		h.internal(c)
		return
	}
	h.writeResult(c, result)
}

// writeWithOptionalIdempotency 统一写接口的幂等协调入口：契约 1.5 仅要求 POST 携带
// Idempotency-Key；PATCH/DELETE 携带时走与创建一致的占位/重放流程，未携带时直接执行，
// 避免对只要求物理删除的删除强加必填头。
func (h *ScheduleHandler) writeWithOptionalIdempotency(c *gin.Context, run func(ctx context.Context) *opResult) {
	key, present, err := request.OptionalIdempotencyKey(c)
	if err != nil {
		h.validation(c, err)
		return
	}
	if !present {
		h.runWriteOnce(c, run)
		return
	}
	userID, serviceErr := h.currentUser(c)
	if serviceErr != nil {
		h.writeEnvelope(c, serviceErr)
		return
	}
	// scope 用“方法 + 实际 URL 路径”：模板 :planId 展开成真实 plan 编号，同一 key 对不同
	// 计划（以及同一计划的 PATCH/DELETE）落在不同幂等键，避免重放串到别的资源。
	h.claimAndRun(c, idempotencyKey(userID, c.Request.Method+"|"+c.Request.URL.Path, key), run)
}

// claimAndRun 实现“占位 -> 重放 -> 执行业务 -> 保存”的幂等协调流程：
// Redis 不可用返回 503；同 key 重复请求原样重放第一次结果（成功与业务错误都重放）；
// 首次请求执行期间到达的并发重复轮询约 3 秒等待第一次结果，期间读到即重放；仍无结果
// 时返回 503 IDEMPOTENCY_PROCESSING。503 属于契约可重试错误，409 则不可盲目重试，因此
// 不使用 409 表达“处理中”，避免形成客户端死区。抢到占位后也会先读历史结果：占位按短 TTL
// 过期而 24h 结果仍在时直接重放，杜绝同 key 重跑业务造成重复创建/修改。
func (h *ScheduleHandler) claimAndRun(c *gin.Context, idemKey string, run func(ctx context.Context) *opResult) {
	claimed, err := h.store.Claim(c.Request.Context(), idemKey)
	if err != nil {
		h.idempotencyUnavailable(c)
		return
	}
	if !claimed {
		// 未抢到占位：优先重放已保存的第一次结果；首请求仍在途时按 pollAttempts 轮询等待。
		for attempt := 0; attempt < h.pollAttempts; attempt++ {
			record, loadErr := h.store.Load(c.Request.Context(), idemKey)
			if loadErr != nil {
				h.idempotencyUnavailable(c)
				return
			}
			if record != nil {
				h.replay(c, record)
				return
			}
			// 仅非末次迭代后 sleep，避免总时长超出约 3 秒一拍的误差。
			if attempt < h.pollAttempts-1 {
				time.Sleep(h.pollInterval)
			}
		}
		// 轮询耗尽仍无结果（首请求仍在途或占位残留）：503 可安全重试。
		h.writeError(c, http.StatusServiceUnavailable, idempotencyProcessingCode, "相同幂等键的请求正在处理中，请稍后重试")
		return
	}
	// 抢到占位：先查一次历史结果。旧 claim 已按短 TTL（10 分钟）过期而 24h 结果仍存在时，
	// 必须直接重放并释放本次占位，而不是重跑业务（同 key 重复创建风险）。
	record, loadErr := h.store.Load(c.Request.Context(), idemKey)
	if loadErr != nil {
		// 读不到结果状态时不冒险执行：释放本次占位并返回 503，客户端可安全重试；若历史
		// 结果实际已落库，重试会再次走“占位 -> 读历史 -> 重放”路径自行恢复。
		_ = h.store.Release(c.Request.Context(), idemKey)
		h.idempotencyUnavailable(c)
		return
	}
	if record != nil {
		h.replay(c, record)
		// 结果已落库无需占位：释放本次新 claim，避免残留占位阻塞后续请求直到 TTL 到期。
		_ = h.store.Release(c.Request.Context(), idemKey)
		return
	}
	// 无历史结果：执行首次业务（创建/更新/删除）。
	result := run(c.Request.Context())
	if result == nil {
		h.internal(c)
		return
	}
	// 5xx 不入幂等存储，允许网络超时后客户端重试并重新创建/修改。
	if result.status >= 500 {
		h.writeResult(c, result)
		// 释放占位键，避免后续重试被残留占位挡住（占位 TTL 10 分钟内本也应自愈）。
		_ = h.store.Release(c.Request.Context(), idemKey)
		return
	}
	// 业务成功（或业务错误，如 409/404/422）：保存结果后原样重放给并发重复请求。
	// Save 做有限次重试（默认 3 次、间隔 50ms），覆盖“业务已提交但结果尚未落库”的窄窗口，
	// 尽量让同 key 重试能命中重放而不是重跑业务。
	var saveErr error
	for attempt := 0; attempt < h.saveAttempts; attempt++ {
		saveErr = h.store.Save(c.Request.Context(), idemKey, port.IdempotencyRecord{
			StatusCode: result.status,
			Headers:    result.headers,
			Body:       marshalBody(result.body),
		})
		if saveErr == nil {
			break
		}
		if attempt < h.saveAttempts-1 {
			time.Sleep(h.saveInterval)
		}
	}
	if saveErr != nil {
		// 有限重试仍失败（生产应记录告警日志/指标）：业务已执行无法回滚，只能释放占位并
		// 返回真实结果；此后同 key 重试可能重跑业务，属窄窗口已知取舍，不做后台 goroutine。
		_ = h.store.Release(c.Request.Context(), idemKey)
	}
	h.writeResult(c, result)
}

// replay 按保存的第一次结果原样重放响应。
func (h *ScheduleHandler) replay(c *gin.Context, record *port.IdempotencyRecord) {
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
func (h *ScheduleHandler) writeResult(c *gin.Context, result *opResult) {

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

// validation 输出 422 参数错误。
func (h *ScheduleHandler) validation(c *gin.Context, e error) {
	h.writeError(c, http.StatusUnprocessableEntity, scheduleservice.CodeValidationFailed, e.Error())
}

// validationResult 生成 422 参数错误的 opResult（绑定失败发生在占位之后，需一并重放）。
func (h *ScheduleHandler) validationResult(c *gin.Context, e error) *opResult {
	return &opResult{
		status: http.StatusUnprocessableEntity,
		body:   h.errorBody(scheduleservice.CodeValidationFailed, e.Error(), nil),
	}
}

// serviceErrorResult 把 use case 返回的错误映射为 HTTP opResult。
func (h *ScheduleHandler) serviceErrorResult(c *gin.Context, err error) *opResult {
	var serviceErr *scheduleservice.ServiceError
	if errors.As(err, &serviceErr) {
		return &opResult{status: serviceStatus(serviceErr.Code), body: h.errorBody(serviceErr.Code, serviceErr.Message, serviceErr.Details)}
	}
	return &opResult{status: http.StatusInternalServerError, body: h.errorBody("INTERNAL_SERVER_ERROR", "操作失败，请稍后重试", nil)}
}

// writeEnvelope 直接输出业务错误（占位/用户身份等前置错误不需要写入幂等存储）。
func (h *ScheduleHandler) writeEnvelope(c *gin.Context, serviceErr *scheduleservice.ServiceError) {
	h.writeError(c, serviceStatus(serviceErr.Code), serviceErr.Code, serviceErr.Message)
}

func (h *ScheduleHandler) writeError(c *gin.Context, status int, code, message string) {
	payload, _ := json.Marshal(h.errorBody(code, message, nil))
	c.Data(status, "application/json; charset=utf-8", payload)
}

func (h *ScheduleHandler) errorBody(code, message string, details scheduleservice.Details) map[string]any {
	if len(details) == 0 {
		return map[string]any{"code": code, "message": message}
	}
	return map[string]any{"code": code, "message": message, "details": details}
}

// planBody 序列化资源对象（POST/PATCH 成功响应体）。契约 11.3 的成功体不含 slots 字段，
// 因此仅当确实装载了 slots 时才输出，避免出现 "slots":null。
func (h *ScheduleHandler) planBody(plan *schedule.WorkPlan) map[string]any {
	if plan == nil {
		return map[string]any{}
	}
	body := map[string]any{
		"id":              plan.ID,
		"doctorId":        plan.DoctorID,
		"subdepartmentId": plan.SubdepartmentID,
		"date":            plan.Date,
		"maximum":         plan.Maximum,
		"used":            plan.Used,
		"remaining":       plan.Remaining,
	}
	if len(plan.Slots) > 0 {
		body["slots"] = plan.Slots
	}
	return body
}
func (h *ScheduleHandler) internal(c *gin.Context) {
	h.writeError(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "查询失败")
}

func (h *ScheduleHandler) idempotencyUnavailable(c *gin.Context) {
	h.writeError(c, http.StatusServiceUnavailable, "IDEMPOTENCY_STORE_UNAVAILABLE", "幂等存储不可用，请稍后重试")
}

// idempotencyProcessingCode 表示“相同幂等键的请求仍在处理中”，配合 503 让客户端可重试。
const idempotencyProcessingCode = "IDEMPOTENCY_PROCESSING"

// currentUser 从 access token claims 中提取用户 ID，用于幂等键隔离不同操作者。
func (h *ScheduleHandler) currentUser(c *gin.Context) (int64, *scheduleservice.ServiceError) {
	value, exists := c.Get(middleware.ClaimsKey)
	claims, ok := value.(*userservice.AccessClaims)
	if !exists || !ok || claims == nil {
		return 0, &scheduleservice.ServiceError{Code: "AUTH_INVALID_TOKEN", Message: "访问令牌无效或已过期"}
	}
	return claims.UserID, nil
}

// idempotencyKey 生成幂等存储键：sha256(userId|scope|key)。
// scope 由调用方拼“方法 + 实际 URL 路径”：同一用户同一方法同一资源路径同一 key 的重复
// 请求共享同一键（重放一致）；同一 key 用于不同资源（如不同 :planId）天然落不同键，
// 不会把 plan A 的结果重放到 plan B。
func idempotencyKey(userID int64, scope, key string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s", userID, scope, key)))
	return "medical:idem:schedule:plans:" + hex.EncodeToString(sum[:])
}

// planLocation 生成创建成功后的 Location 响应头。
func planLocation(planID int64) string {
	return fmt.Sprintf("/api/v1/schedule/plans/%d", planID)
}

// serviceStatus 把业务错误码映射为 HTTP 状态码。
func serviceStatus(code string) int {
	switch code {
	case scheduleservice.CodeValidationFailed:
		return http.StatusUnprocessableEntity
	case scheduleservice.CodePlanNotFound:
		return http.StatusNotFound
	case scheduleservice.CodePlanExists, scheduleservice.CodePlanLocked, scheduleservice.CodePlanConflict, scheduleservice.CodeHasRegistrations:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// marshalBody 把响应对象序列化为 JSON 字节（空对象不会出现，调用方仅在无 body 时传 nil）。
func marshalBody(body map[string]any) []byte {
	if len(body) == 0 {
		return nil
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil
	}
	return payload
}
