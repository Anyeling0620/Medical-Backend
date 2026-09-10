package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/domain/schedule"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/request"
	scheduleservice "Medical-Web-Backend/internal/usecase/schedule"
)

// ScheduleSlotHandler 处理排班时段四个契约接口：
// GET/POST /api/v1/schedule/plans/{planId}/slots 与 PATCH/DELETE /api/v1/schedule/slots/{slotId}。
// POST 创建按契约 1.5 做幂等协调（Claim 原子占位 + 结果重放，与 plans 分支一致）；
// 状态规则（开始/结束锁、挂号保护、容量下限）在 use case 调用链与
// repository 事务内完成，handler 只负责参数解析与错误码/文案映射。
type ScheduleSlotHandler struct {
	service *scheduleservice.Service
	// 幂等协调（Claim 原子占位 + 结果重放）复用 schedule_idem.go 的共享实现。
	*scheduleIdem
}

// NewScheduleSlotHandler 构造排班时段处理器；store 为创建接口的幂等存储（Redis）。
func NewScheduleSlotHandler(service *scheduleservice.Service, store port.IdempotencyStore) *ScheduleSlotHandler {
	return &ScheduleSlotHandler{service: service, scheduleIdem: newScheduleIdem(store)}
}

// ListSlots GET /api/v1/schedule/plans/{planId}/slots：返回按 slot 升序的时段数组。
func (h *ScheduleSlotHandler) ListSlots(c *gin.Context) {
	planID, err := parsePositiveID(c.Param("planId"), "planId")
	if err != nil {
		h.writeResult(c, h.validationResult(err))
		return
	}
	items, err := h.service.ListSlotsByPlan(c.Request.Context(), planID)
	if err != nil {
		if errors.Is(err, schedule.ErrPlanNotFound) {
			h.writeResult(c, h.opFromError(err, "排班计划不存在"))
			return
		}
		h.writeResult(c, h.internalResult())
		return
	}
	c.JSON(http.StatusOK, items)
}

// CreateSlot POST /api/v1/schedule/plans/{planId}/slots：创建时段。
// Idempotency-Key 必填；同用户同路径同 key 的重试（含网络超时后的重试）
// 原样重放第一次结果，Redis 不可用时返回 503 而不是降级为无保护创建。
func (h *ScheduleSlotHandler) CreateSlot(c *gin.Context) {
	planID, err := parsePositiveID(c.Param("planId"), "planId")
	if err != nil {
		h.writeResult(c, h.validationResult(err))
		return
	}
	userID, authErr := currentUserID(c)
	if authErr != nil {
		h.writeResult(c, h.authResult())
		return
	}
	key, err := request.BindIdempotencyKey(c)
	if err != nil {
		h.writeResult(c, h.validationResult(err))
		return
	}
	idemKey := slotIdempotencyKey(userID, planID, c.FullPath(), key)
	h.claimAndRun(c, idemKey, func(ctx context.Context) *opResult {
		body, bindErr := request.BindCreateSlotRequest(c)
		if bindErr != nil {
			return h.bindResult(bindErr)
		}
		created, createErr := h.service.CreateSlot(ctx, schedule.ScheduleSlot{
			WorkPlanID: planID,
			Slot:       int16(body.Slot),
			Maximum:    int16(body.Maximum),
		})
		if createErr != nil {
			return h.opFromError(createErr, createSlotLockedMessage(createErr))
		}
		return &opResult{
			status:  http.StatusCreated,
			headers: map[string]string{"Location": slotLocation(created.ID)},
			body:    h.slotBody(created),
		}
	})
}

// UpdateSlotMaximum PATCH /api/v1/schedule/slots/{slotId}：修改时段容量。
func (h *ScheduleSlotHandler) UpdateSlotMaximum(c *gin.Context) {
	slotID, err := parsePositiveID(c.Param("slotId"), "slotId")
	if err != nil {
		h.writeResult(c, h.validationResult(err))
		return
	}
	body, err := request.BindUpdateSlotRequest(c)
	if err != nil {
		h.writeResult(c, h.bindResult(err))
		return
	}
	updated, err := h.service.UpdateSlotMaximum(c.Request.Context(), slotID, int16(body.Maximum))
	if err != nil {
		h.writeResult(c, h.opFromError(err, updateSlotLockedMessage(err)))
		return
	}
	h.writeResult(c, &opResult{status: http.StatusOK, body: h.slotBody(updated)})
}

// DeleteSlot DELETE /api/v1/schedule/slots/{slotId}：仅物理删除无挂号且计划未开始的时段。
func (h *ScheduleSlotHandler) DeleteSlot(c *gin.Context) {
	slotID, err := parsePositiveID(c.Param("slotId"), "slotId")
	if err != nil {
		h.writeResult(c, h.validationResult(err))
		return
	}
	if err := h.service.DeleteSlot(c.Request.Context(), slotID); err != nil {
		h.writeResult(c, h.opFromError(err, deleteSlotLockedMessage(err)))
		return
	}
	h.writeResult(c, &opResult{status: http.StatusNoContent})
}

// parsePositiveID 解析路径参数中的正整数 ID。
func parsePositiveID(raw string, label string) (int64, error) {
	id, err := request.ParsePositiveID(raw)
	if err != nil {
		return 0, errors.New(label + " 必须为正整数")
	}
	return id, nil
}

// idempotencyKey 生成幂等存储键：sha256(userId|planId|fullPath|key)。
// c.FullPath() 只含路由模板（不含实际 planId），因此必须显式加入 planId：
// 否则同一用户对不同计划的 POST slots 复用同一 Idempotency-Key 时，
// 后到的请求会把前一计划的 201（含 Location）误重放给当前计划。
func slotIdempotencyKey(userID, planID int64, fullPath, key string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%d|%s|%s", userID, planID, fullPath, key)))
	return "medical:idem:schedule:slots:" + hex.EncodeToString(sum[:])
}

// slotLocation 生成创建成功后的 Location 响应头。
func slotLocation(slotID int64) string {
	return fmt.Sprintf("/api/v1/schedule/slots/%d", slotID)
}

// validationResult 生成 422 参数错误的 opResult（绑定失败发生在占位之后，需一并重放）。
func (h *ScheduleSlotHandler) validationResult(e error) *opResult {
	return h.errResult(http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED", e.Error())
}

// bindResult 按仓库 auth 约定区分 JSON 错误：语法错误、字段类型错误与
// 截断的 JSON（io.ErrUnexpectedEOF）返回 400 REQUEST_INVALID_JSON；
// 空请求体、未知字段与语义错误仍走 422。复用 schedule_idem.go 的共享实现。
func (h *ScheduleSlotHandler) bindResult(e error) *opResult {
	return bindRequestResult(e, h.validationResult)
}

// opFromError 把 use case/repository 返回的领域错误映射为 HTTP 结果；
// message 按具体操作传入（锁定/挂号保护的文案因操作而异）。
func (h *ScheduleSlotHandler) opFromError(err error, message string) *opResult {
	status, code, details := resolveSlotError(err)
	if details == nil {
		return h.errResult(status, code, message)
	}
	body := map[string]any{"code": code, "message": message, "details": details}
	return &opResult{status: status, body: body}
}

// resolveSlotError 把领域错误映射为契约错误码与 HTTP 状态（错误码目录第 9 章）。
func resolveSlotError(err error) (int, string, map[string]any) {
	var belowErr *schedule.MaximumBelowUsedError
	switch {
	case errors.Is(err, schedule.ErrPlanNotFound):
		return http.StatusNotFound, "SCHEDULE_PLAN_NOT_FOUND", nil
	case errors.Is(err, schedule.ErrSlotNotFound):
		return http.StatusNotFound, "SCHEDULE_SLOT_NOT_FOUND", nil
	case errors.Is(err, schedule.ErrSlotExists):
		return http.StatusConflict, "SCHEDULE_SLOT_EXISTS", nil
	case errors.Is(err, schedule.ErrSlotLocked):
		return http.StatusConflict, "SCHEDULE_SLOT_LOCKED", nil
	case errors.Is(err, schedule.ErrHasRegistrations):
		return http.StatusConflict, "SCHEDULE_HAS_REGISTRATIONS", nil
	case errors.As(err, &belowErr) || errors.Is(err, schedule.ErrMaximumBelowUsed):
		details := map[string]any{"used": 0}
		if errors.As(err, &belowErr) {
			details["used"] = belowErr.Used
		}
		return http.StatusConflict, "SCHEDULE_CONFLICT", details
	case errors.Is(err, schedule.ErrDoctorInactive):
		// 医生已离职/退休或非在诊属于资格失败，按错误码目录归入 422 REQUEST_VALIDATION_FAILED。
		return http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED", nil
	case errors.Is(err, schedule.ErrInvalidSlot):
		return http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED", nil
	case errors.Is(err, schedule.ErrInvalidWorkPlan):
		return http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED", nil
	case errors.Is(err, schedule.ErrInvalidMaximum):
		return http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED", nil
	default:
		return http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", nil
	}
}

// createSlotLockedMessage 选择创建接口的业务文案。
func createSlotLockedMessage(err error) string {
	switch {
	case errors.Is(err, schedule.ErrPlanNotFound):
		return "排班计划不存在"
	case errors.Is(err, schedule.ErrSlotExists):
		return "该时段已存在"
	case errors.Is(err, schedule.ErrSlotLocked):
		return "排班已开始或已结束，不能新增时段"
	case errors.Is(err, schedule.ErrDoctorInactive):
		return "医生已离职、退休或不在出诊状态，不能新增出诊时段"
	default:
		return "时段创建失败"
	}
}

// updateSlotLockedMessage 选择更新接口的业务文案。
func updateSlotLockedMessage(err error) string {
	switch {
	case errors.Is(err, schedule.ErrSlotNotFound):
		return "时段不存在"
	case errors.Is(err, schedule.ErrSlotLocked):
		return "时段所属排班已开始、已结束或已有挂号，不能修改"
	case errors.Is(err, schedule.ErrMaximumBelowUsed):
		return "最大号源不能小于已使用号源"
	default:
		return "时段更新失败"
	}
}

// deleteSlotLockedMessage 选择删除接口的业务文案。
func deleteSlotLockedMessage(err error) string {
	switch {
	case errors.Is(err, schedule.ErrSlotNotFound):
		return "时段不存在"
	case errors.Is(err, schedule.ErrSlotLocked):
		return "时段所属排班已开始或已结束，不能删除"
	case errors.Is(err, schedule.ErrHasRegistrations):
		return "已有挂号记录，不能删除时段"
	default:
		return "时段删除失败"
	}
}

// slotBody 序列化时段资源对象（POST/PATCH 成功响应体），字段与契约示例一致。
func (h *ScheduleSlotHandler) slotBody(slot *schedule.ScheduleSlot) map[string]any {
	if slot == nil {
		return map[string]any{}
	}
	return map[string]any{
		"id":         slot.ID,
		"workPlanId": slot.WorkPlanID,
		"slot":       slot.Slot,
		"maximum":    slot.Maximum,
		"used":       slot.Used,
		"remaining":  slot.Remaining,
	}
}

// errResult 构造统一错误 opResult（第 1.3 节格式；requestId 属仓库基线待统一项）。
// details 为可选项，仅 409 SCHEDULE_CONFLICT（容量小于已用）等场景携带 used。
func (h *ScheduleSlotHandler) errResult(status int, code, message string, details ...map[string]any) *opResult {
	body := map[string]any{"code": code, "message": message}
	if len(details) > 0 && len(details[0]) > 0 {
		body["details"] = details[0]
	}
	return &opResult{status: status, body: body}
}

// authResult 返回 401 令牌无效结果。
func (h *ScheduleSlotHandler) authResult() *opResult {
	return h.errResult(http.StatusUnauthorized, "AUTH_INVALID_TOKEN", "访问令牌无效或已过期")
}

// internalResult 返回 500 内部错误结果。
func (h *ScheduleSlotHandler) internalResult() *opResult {
	return h.errResult(http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "服务器内部错误")
}
