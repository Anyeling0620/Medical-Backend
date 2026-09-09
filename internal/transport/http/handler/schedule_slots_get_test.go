package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"Medical-Web-Backend/internal/domain/schedule"
)

// TestScheduleSlotListSlotsOK GET 列表返回 200，按 slot 升序并计算 remaining。
func TestScheduleSlotListSlotsOK(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(4, "2026-09-20")
	// 故意先插 slot=2 再插 slot=1，验证服务端排序输出。
	stub.addSlot(schedule.ScheduleSlot{ID: 13, WorkPlanID: 4, Slot: 2, Maximum: 3})
	stub.addSlot(schedule.ScheduleSlot{ID: 12, WorkPlanID: 4, Slot: 1, Maximum: 3, Used: 1})
	e := newScheduleSlotEngine(t, stub, newMemIdempotencyStore(), 0)

	w := scheduleRequest(e, http.MethodGet, "/api/v1/schedule/plans/4/slots", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var items []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, w.Body.String())
	}
	if len(items) != 2 {
		t.Fatalf("len = %d, want 2; body=%s", len(items), w.Body.String())
	}
	if items[0]["id"].(float64) != 12 || items[0]["slot"].(float64) != 1 || items[0]["remaining"].(float64) != 2 {
		t.Errorf("first item = %v, want id=12 slot=1 remaining=2", items[0])
	}
	if items[1]["id"].(float64) != 13 || items[1]["slot"].(float64) != 2 || items[1]["remaining"].(float64) != 3 {
		t.Errorf("second item = %v, want id=13 slot=2 remaining=3", items[1])
	}
}

// TestScheduleSlotListEmptySlotsOK 计划存在但无时段时返回 200 []。
func TestScheduleSlotListEmptySlotsOK(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(4, "2026-09-20")
	e := newScheduleSlotEngine(t, stub, newMemIdempotencyStore(), 0)

	w := scheduleRequest(e, http.MethodGet, "/api/v1/schedule/plans/4/slots", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Errorf("body = %s, want []", got)
	}
}

// TestScheduleSlotListPlanNotFound GET 列表对应计划不存在时返回 404 SCHEDULE_PLAN_NOT_FOUND。
func TestScheduleSlotListPlanNotFound(t *testing.T) {
	e := newScheduleSlotEngine(t, &slotRepoStub{}, newMemIdempotencyStore(), 0)

	w := scheduleRequest(e, http.MethodGet, "/api/v1/schedule/plans/999/slots", "", nil)
	body := assertErrorStatus(t, w, http.StatusNotFound, "SCHEDULE_PLAN_NOT_FOUND")
	if body["message"] != "排班计划不存在" {
		t.Errorf("message = %v, want 排班计划不存在", body["message"])
	}
}

// TestScheduleSlotListInvalidPlanID 路径参数 planId 非正整数返回 422。
func TestScheduleSlotListInvalidPlanID(t *testing.T) {
	e := newScheduleSlotEngine(t, &slotRepoStub{}, newMemIdempotencyStore(), 0)

	w := scheduleRequest(e, http.MethodGet, "/api/v1/schedule/plans/abc/slots", "", nil)
	body := assertErrorStatus(t, w, http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED")
	if body["message"] != "planId 必须为正整数" {
		t.Errorf("message = %v, want planId 必须为正整数", body["message"])
	}
}
