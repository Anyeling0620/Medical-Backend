package handler

import (
	"net/http"
	"testing"

	"Medical-Web-Backend/internal/domain/schedule"
)

// TestScheduleSlotUpdateMaximumBodyValidation PATCH 请求体非法返回固定中文文案的 422。
func TestScheduleSlotUpdateMaximumBodyValidation(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	stub.addSlot(schedule.ScheduleSlot{ID: 12, WorkPlanID: 1, Slot: 1, Maximum: 3})
	e := newScheduleSlotEngine(t, stub, newMemIdempotencyStore(), 0)

	cases := map[string]struct {
		body    string
		message string
	}{
		"empty body":      {body: "", message: "请求体不能为空"},
		"maximum zero":    {body: `{"maximum":0}`, message: "时段最大号源必须大于 0"},
		"maximum too big": {body: `{"maximum":32768}`, message: "时段最大号源不能超过 32767"},
		"unknown field":   {body: `{"maximum":5,"slot":1}`, message: "请求体格式不正确（包含未知字段）"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := scheduleRequest(e, http.MethodPatch, "/api/v1/schedule/slots/12",
				tc.body, nil)
			body := assertErrorStatus(t, w, http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED")
			if body["message"] != tc.message {
				t.Errorf("message = %v, want %s", body["message"], tc.message)
			}
		})
	}
}

// TestScheduleSlotUpdateMaximumInvalidJSON PATCH 请求体 JSON 语法/字段类型错误
// 以及截断的 JSON（io.ErrUnexpectedEOF，如 {"maximum":5）统一返回 400
// REQUEST_INVALID_JSON 与固定文案“请求体不是合法的 JSON”（与仓库 auth 约定一致）。
func TestScheduleSlotUpdateMaximumInvalidJSON(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	stub.addSlot(schedule.ScheduleSlot{ID: 12, WorkPlanID: 1, Slot: 1, Maximum: 3})
	e := newScheduleSlotEngine(t, stub, newMemIdempotencyStore(), 0)

	cases := map[string]string{
		"syntax error":       `{"maximum":5,}`,
		"truncated json":     `{"maximum":5`,
		"field type error":   `{"maximum":"x"}`,
		"maximum fractional": `{"maximum":5.5}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			w := scheduleRequest(e, http.MethodPatch, "/api/v1/schedule/slots/12",
				body, nil)
			resp := assertErrorStatus(t, w, http.StatusBadRequest, "REQUEST_INVALID_JSON")
			if resp["message"] != "请求体不是合法的 JSON" {
				t.Errorf("message = %v, want 请求体不是合法的 JSON", resp["message"])
			}
		})
	}
}

// TestScheduleSlotUpdateMaximumBelowUsedConflict 新容量小于已用量返回 409 SCHEDULE_CONFLICT，
// 且 details.used 携带实际已用量。
func TestScheduleSlotUpdateMaximumBelowUsedConflict(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	usedSlot := schedule.ScheduleSlot{ID: 12, WorkPlanID: 1, Slot: 1, Maximum: 3, Used: 3}
	stub.addSlot(usedSlot)
	e := newScheduleSlotEngine(t, stub, newMemIdempotencyStore(), 0)

	w := scheduleRequest(e, http.MethodPatch, "/api/v1/schedule/slots/12",
		`{"maximum":2}`, nil)
	body := assertErrorStatus(t, w, http.StatusConflict, "SCHEDULE_CONFLICT")
	if body["message"] != "最大号源不能小于已使用号源" {
		t.Errorf("message = %v, want 最大号源不能小于已使用号源", body["message"])
	}
	details, ok := body["details"].(map[string]any)
	if !ok || details["used"].(float64) != 3 {
		t.Errorf("details = %v, want {used:3}", body["details"])
	}
}

// TestScheduleSlotUpdateMaximumSlotNotFound 更新不存在的时段返回 404 SCHEDULE_SLOT_NOT_FOUND。
func TestScheduleSlotUpdateMaximumSlotNotFound(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	e := newScheduleSlotEngine(t, stub, newMemIdempotencyStore(), 0)

	w := scheduleRequest(e, http.MethodPatch, "/api/v1/schedule/slots/999",
		`{"maximum":5}`, nil)
	body := assertErrorStatus(t, w, http.StatusNotFound, "SCHEDULE_SLOT_NOT_FOUND")
	if body["message"] != "时段不存在" {
		t.Errorf("message = %v, want 时段不存在", body["message"])
	}
}

// TestScheduleSlotUpdateMaximumLockedPlan 已开始计划下的时段不可更新（409 SCHEDULE_SLOT_LOCKED）。
func TestScheduleSlotUpdateMaximumLockedPlan(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-09") // 业务当日，计划已开始。
	stub.addSlot(schedule.ScheduleSlot{ID: 12, WorkPlanID: 1, Slot: 1, Maximum: 3})
	e := newScheduleSlotEngine(t, stub, newMemIdempotencyStore(), 0)

	w := scheduleRequest(e, http.MethodPatch, "/api/v1/schedule/slots/12",
		`{"maximum":5}`, nil)
	body := assertErrorStatus(t, w, http.StatusConflict, "SCHEDULE_SLOT_LOCKED")
	if body["message"] != "时段所属排班已开始、已结束或已有挂号，不能修改" {
		t.Errorf("message = %v", body["message"])
	}
}

// TestScheduleSlotUpdateMaximumHasRegistrations 已有挂号的时段返回 409 SCHEDULE_SLOT_LOCKED
//（契约：已开始/已结束/已有挂号统一该错误码）。
func TestScheduleSlotUpdateMaximumHasRegistrations(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	stub.addSlot(schedule.ScheduleSlot{ID: 12, WorkPlanID: 1, Slot: 1, Maximum: 3})
	stub.markRegistered(12)
	e := newScheduleSlotEngine(t, stub, newMemIdempotencyStore(), 0)

	w := scheduleRequest(e, http.MethodPatch, "/api/v1/schedule/slots/12",
		`{"maximum":5}`, nil)
	body := assertErrorStatus(t, w, http.StatusConflict, "SCHEDULE_SLOT_LOCKED")
	if body["message"] != "时段所属排班已开始、已结束或已有挂号，不能修改" {
		t.Errorf("message = %v", body["message"])
	}
}
