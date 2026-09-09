package handler

import (
	"net/http"
	"testing"

	"Medical-Web-Backend/internal/domain/schedule"
)

// TestScheduleSlotUpdateMaximumOK PATCH 更新时段容量直接成功返回 200。
func TestScheduleSlotUpdateMaximumOK(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	stub.addSlot(schedule.ScheduleSlot{ID: 12, WorkPlanID: 1, Slot: 1, Maximum: 3})
	e := newScheduleSlotEngine(t, stub, newMemIdempotencyStore(), 0)

	w := scheduleRequest(e, http.MethodPatch, "/api/v1/schedule/slots/12",
		`{"maximum":5}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["id"].(float64) != 12 || body["maximum"].(float64) != 5 || body["remaining"].(float64) != 5 {
		t.Errorf("body = %v, want id=12 maximum=5 remaining=5", body)
	}
}
