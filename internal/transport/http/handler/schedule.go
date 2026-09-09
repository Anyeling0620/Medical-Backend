package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/domain/schedule"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/request"
	scheduleservice "Medical-Web-Backend/internal/usecase/schedule"
)

// ScheduleHandler 提供排班计划的四个 HTTP 接口。
// 幂等协调（Claim 原子占位 + 结果重放）位于 handler 层；业务规则在 use case/domain 层。
type ScheduleHandler struct {
	service *scheduleservice.Service
	// 幂等协调（Claim 原子占位 + 结果重放）复用 schedule_idem.go 的共享实现。
	*scheduleIdem
}

// NewScheduleHandler 构造排班计划处理器；store 为创建/写入接口的幂等存储（Redis）。
func NewScheduleHandler(service *scheduleservice.Service, store port.IdempotencyStore) *ScheduleHandler {
	return &ScheduleHandler{service: service, scheduleIdem: newScheduleIdem(store)}
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
	userID, authErr := currentUserID(c)
	if authErr != nil {
		h.writeError(c, http.StatusUnauthorized, "AUTH_INVALID_TOKEN", authErr.Error())
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
			return h.bindResult(c, bindErr)
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
			return h.bindResult(c, bindErr)
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
	userID, authErr := currentUserID(c)
	if authErr != nil {
		h.writeError(c, http.StatusUnauthorized, "AUTH_INVALID_TOKEN", authErr.Error())
		return
	}
	// scope 用“方法 + 实际 URL 路径”：模板 :planId 展开成真实 plan 编号，同一 key 对不同
	// 计划（以及同一计划的 PATCH/DELETE）落在不同幂等键，避免重放串到别的资源。
	h.claimAndRun(c, idempotencyKey(userID, c.Request.Method+"|"+c.Request.URL.Path, key), run)
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

// bindResult 按仓库 auth 约定区分请求体绑定错误：JSON 语法错误、字段类型错误与截断的
// JSON（io.ErrUnexpectedEOF）返回 400 REQUEST_INVALID_JSON；空请求体、未知字段与语义
// 错误仍走 422。与 slots 分支共用 schedule_idem.go 的实现，避免泄漏英文内部错误。
func (h *ScheduleHandler) bindResult(c *gin.Context, e error) *opResult {
	return bindRequestResult(e, func(err error) *opResult { return h.validationResult(c, err) })
}

// serviceErrorResult 把 use case 返回的错误映射为 HTTP opResult。
func (h *ScheduleHandler) serviceErrorResult(c *gin.Context, err error) *opResult {
	var serviceErr *scheduleservice.ServiceError
	if errors.As(err, &serviceErr) {
		return &opResult{status: serviceStatus(serviceErr.Code), body: h.errorBody(serviceErr.Code, serviceErr.Message, serviceErr.Details)}
	}
	return &opResult{status: http.StatusInternalServerError, body: h.errorBody("INTERNAL_SERVER_ERROR", "操作失败，请稍后重试", nil)}
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
