package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/domain/schedule"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/middleware"
	misuser "Medical-Web-Backend/internal/usecase/misuser"
	scheduleservice "Medical-Web-Backend/internal/usecase/schedule"
)

// 本文件覆盖“离职或退休医生不能新增出诊时段”在 HTTP 层的契约表现：
// 仓储返回 schedule.ErrDoctorInactive 时，POST /api/v1/schedule/plans/{planId}/slots
// 必须返回 422 REQUEST_VALIDATION_FAILED 与专用文案，且不影响其它错误映射与前置校验。
// 复用同包已提交的 helper 与桩（memIdempotencyStore、scheduleRequest、assertErrorStatus、
// scheduleSlotTestNow），不修改既有测试文件。

// slotRepoErrorStub 是“仅 CreateSlot 可注入错误”的排班仓储桩：嵌入 port.ScheduleRepository
// 以满足接口（本用例只走创建路径，其余方法不会被调用），覆写 CreateSlot 直接返回预置错误。
type slotRepoErrorStub struct {
	port.ScheduleRepository
	createErr   error
	createCalls int
}

// CreateSlot 记录调用次数后返回预置错误，用于验证 handler 的错误码与文案映射。
func (s *slotRepoErrorStub) CreateSlot(_ context.Context, _ schedule.ScheduleSlot, _ time.Time) (*schedule.ScheduleSlot, error) {
	s.createCalls++
	return nil, s.createErr
}

// newSlotRepoErrorEngine 用“可注入错误的仓储桩 + 内存幂等 store + 固定业务时间”装配
// 创建时段路由；claims 预置方式与同包 newScheduleSlotEngine 保持一致（realm=mis）。
func newSlotRepoErrorEngine(t *testing.T, repo port.ScheduleRepository, store *memIdempotencyStore, userID int64) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	service := scheduleservice.NewService(repo, func() time.Time { return scheduleSlotTestNow })
	slotHandler := NewScheduleSlotHandler(service, store)
	e := gin.New()
	e.Use(func(c *gin.Context) {
		c.Set(middleware.ClaimsKey, &misuser.AccessClaims{UserID: userID, Realm: domainauth.RealmMis})
	})
	e.POST("/api/v1/schedule/plans/:planId/slots", slotHandler.CreateSlot)
	return e
}

// TestScheduleSlotCreateSlotInactiveDoctorRejected 验证仓储返回 ErrDoctorInactive 时：
// 状态码 422、错误码 REQUEST_VALIDATION_FAILED、文案“医生已离职、退休或不在出诊状态，不能新增出诊时段”，
// 且响应体不携带 details；同一幂等键重试必须原样重放该结果而不重复调用仓储。
func TestScheduleSlotCreateSlotInactiveDoctorRejected(t *testing.T) {
	stub := &slotRepoErrorStub{createErr: schedule.ErrDoctorInactive}
	store := newMemIdempotencyStore()
	e := newSlotRepoErrorEngine(t, stub, store, 7)
	const key = "inactive-doctor-key"

	first := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
		`{"slot":1,"maximum":3}`, map[string]string{"Idempotency-Key": key})
	body := assertErrorStatus(t, first, http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED")
	if body["message"] != "医生已离职、退休或不在出诊状态，不能新增出诊时段" {
		t.Fatalf("message = %v，期望 医生已离职、退休或不在出诊状态，不能新增出诊时段", body["message"])
	}
	// 资格失败的 422 错误体保持 {code, message} 简约形态（不携带 details）。
	if details, exists := body["details"]; exists {
		t.Fatalf("医生非在诊的 422 不应携带 details：%v", details)
	}
	if stub.createCalls != 1 {
		t.Fatalf("createCalls = %d，期望 1", stub.createCalls)
	}

	// 同一幂等键重试：必须重放第一次的 422 结果，且不再触达仓储。
	replay := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
		`{"slot":1,"maximum":3}`, map[string]string{"Idempotency-Key": key})
	replayBody := assertErrorStatus(t, replay, http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED")
	if replayBody["message"] != "医生已离职、退休或不在出诊状态，不能新增出诊时段" {
		t.Fatalf("重放 message = %v，期望与首次一致", replayBody["message"])
	}
	if stub.createCalls != 1 {
		t.Fatalf("幂等重放不应重复调用仓储：createCalls = %d", stub.createCalls)
	}
}

// TestScheduleSlotCreateSlotOtherErrorMappingsUnchanged 验证新增 ErrDoctorInactive 分支
// 没有影响其它领域错误的既有映射：计划不存在 404、时段重复/计划已开始 409、未知错误 500。
func TestScheduleSlotCreateSlotOtherErrorMappingsUnchanged(t *testing.T) {
	cases := []struct {
		name    string
		create  error
		status  int
		code    string
		message string
	}{
		{"计划不存在", schedule.ErrPlanNotFound, http.StatusNotFound, "SCHEDULE_PLAN_NOT_FOUND", "排班计划不存在"},
		{"时段重复", schedule.ErrSlotExists, http.StatusConflict, "SCHEDULE_SLOT_EXISTS", "该时段已存在"},
		{"计划已开始", schedule.ErrSlotLocked, http.StatusConflict, "SCHEDULE_SLOT_LOCKED", "排班已开始或已结束，不能新增时段"},
		{"未知错误", errors.New("unexpected repository failure"), http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "时段创建失败"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &slotRepoErrorStub{createErr: tc.create}
			e := newSlotRepoErrorEngine(t, stub, newMemIdempotencyStore(), 7)
			w := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
				`{"slot":1,"maximum":3}`, map[string]string{"Idempotency-Key": "mapping-key"})
			body := assertErrorStatus(t, w, tc.status, tc.code)
			if body["message"] != tc.message {
				t.Fatalf("message = %v，期望 %s", body["message"], tc.message)
			}
		})
	}
}

// TestScheduleSlotCreateSlotInactiveDoctorKeepsPrevalidation 验证医生资格分支不改变
// 前置校验行为：即使仓储会返回 ErrDoctorInactive，缺键/非法键仍先返回原有 422 文案，
// 并且不会触达仓储。
func TestScheduleSlotCreateSlotInactiveDoctorKeepsPrevalidation(t *testing.T) {
	stub := &slotRepoErrorStub{createErr: schedule.ErrDoctorInactive}
	store := newMemIdempotencyStore()
	e := newSlotRepoErrorEngine(t, stub, store, 7)

	cases := []struct {
		name    string
		headers map[string]string
		message string
	}{
		{"缺少幂等键", nil, "Idempotency-Key 请求头必填"},
		{"幂等键含控制字符", map[string]string{"Idempotency-Key": "ab\tcd"}, "Idempotency-Key 只能包含可打印 ASCII 字符"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
				`{"slot":1,"maximum":3}`, tc.headers)
			body := assertErrorStatus(t, w, http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED")
			if body["message"] != tc.message {
				t.Fatalf("message = %v，期望 %s", body["message"], tc.message)
			}
		})
	}
	if stub.createCalls != 0 {
		t.Fatalf("前置校验失败不应触达仓储：createCalls = %d", stub.createCalls)
	}
}
