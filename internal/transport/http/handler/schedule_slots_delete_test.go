package handler

import (
	"net/http"
	"testing"

	"Medical-Web-Backend/internal/domain/schedule"
)

// TestScheduleSlotDeleteSlotNoContent DELETE 成功返回 204 且时段被物理删除。
func TestScheduleSlotDeleteSlotNoContent(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	stub.addSlot(schedule.ScheduleSlot{ID: 12, WorkPlanID: 1, Slot: 1, Maximum: 3})
	e := newScheduleSlotEngine(t, stub, newMemIdempotencyStore(), 0)

	w := scheduleRequest(e, http.MethodDelete, "/api/v1/schedule/slots/12", "", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if len(stub.slots) != 0 {
		t.Errorf("slot still exists in stub: %v", stub.slots)
	}
}

// TestScheduleSlotDeleteSlotNotFound 删除不存在的时段返回 404 SCHEDULE_SLOT_NOT_FOUND。
func TestScheduleSlotDeleteSlotNotFound(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	e := newScheduleSlotEngine(t, stub, newMemIdempotencyStore(), 0)

	w := scheduleRequest(e, http.MethodDelete, "/api/v1/schedule/slots/999", "", nil)
	body := assertErrorStatus(t, w, http.StatusNotFound, "SCHEDULE_SLOT_NOT_FOUND")
	if body["message"] != "时段不存在" {
		t.Errorf("message = %v, want 时段不存在", body["message"])
	}
}

// TestScheduleSlotDeleteSlotLockedPlan 删除已开始计划的时段返回 409 SCHEDULE_SLOT_LOCKED。
func TestScheduleSlotDeleteSlotLockedPlan(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-09") // 业务当日，计划已开始。
	stub.addSlot(schedule.ScheduleSlot{ID: 12, WorkPlanID: 1, Slot: 1, Maximum: 3})
	e := newScheduleSlotEngine(t, stub, newMemIdempotencyStore(), 0)

	w := scheduleRequest(e, http.MethodDelete, "/api/v1/schedule/slots/12", "", nil)
	body := assertErrorStatus(t, w, http.StatusConflict, "SCHEDULE_SLOT_LOCKED")
	if body["message"] != "时段所属排班已开始或已结束，不能删除" {
		t.Errorf("message = %v", body["message"])
	}
}

// TestScheduleSlotDeleteSlotHasRegistrations 删除已有挂号的时段返回 409 SCHEDULE_HAS_REGISTRATIONS。
func TestScheduleSlotDeleteSlotHasRegistrations(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	stub.addSlot(schedule.ScheduleSlot{ID: 12, WorkPlanID: 1, Slot: 1, Maximum: 3})
	stub.markRegistered(12)
	e := newScheduleSlotEngine(t, stub, newMemIdempotencyStore(), 0)

	w := scheduleRequest(e, http.MethodDelete, "/api/v1/schedule/slots/12", "", nil)
	body := assertErrorStatus(t, w, http.StatusConflict, "SCHEDULE_HAS_REGISTRATIONS")
	if body["message"] != "已有挂号记录，不能删除时段" {
		t.Errorf("message = %v, want 已有挂号记录，不能删除时段", body["message"])
	}
}
