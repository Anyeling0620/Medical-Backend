package handler

import (
	"net/http"
	"testing"
)

// TestScheduleSlotCreateSlotCreated POST 成功返回 201、Location 头与完整资源对象，
// 并把首次结果保存进幂等存储。
func TestScheduleSlotCreateSlotCreated(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	store := newMemIdempotencyStore()
	e := newScheduleSlotEngine(t, stub, store, 7)

	w := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
		`{"slot":1,"maximum":3}`, map[string]string{"Idempotency-Key": "create-slot-1"})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != "/api/v1/schedule/slots/1" {
		t.Errorf("Location = %q, want /api/v1/schedule/slots/1", got)
	}
	body := decodeBody(t, w)
	if body["id"].(float64) != 1 || body["workPlanId"].(float64) != 1 || body["slot"].(float64) != 1 {
		t.Errorf("identity fields = %v", body)
	}
	if body["maximum"].(float64) != 3 || body["used"].(float64) != 0 || body["remaining"].(float64) != 3 {
		t.Errorf("capacity fields = %v", body)
	}
	if store.claimCalls != 1 || store.saveCalls != 1 || len(store.records) != 1 {
		t.Errorf("store calls = claim:%d save:%d records:%d, want 1/1/1",
			store.claimCalls, store.saveCalls, len(store.records))
	}
	if stub.createCalls != 1 {
		t.Errorf("repo.createCalls = %d, want 1", stub.createCalls)
	}
}

// TestScheduleSlotCreateSlotReplaysSavedResult 同用户同 fullPath 同 key 重试时，
// 即使 store 已存首次结果也必须原样重放首次 201，且不再次调用 repo.CreateSlot。
func TestScheduleSlotCreateSlotReplaysSavedResult(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	store := newMemIdempotencyStore()
	e := newScheduleSlotEngine(t, stub, store, 7)
	const key = "create-slot-retry"

	first := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
		`{"slot":1,"maximum":3}`, map[string]string{"Idempotency-Key": key})
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201; body=%s", first.Code, first.Body.String())
	}
	if stub.createCalls != 1 || store.saveCalls != 1 {
		t.Fatalf("first: createCalls=%d saveCalls=%d, want 1/1", stub.createCalls, store.saveCalls)
	}

	// 第二次携带相同 Idempotency-Key：Claim 抢不到占位，Load 命中记录后原样重放。
	second := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
		`{"slot":1,"maximum":3}`, map[string]string{"Idempotency-Key": key})
	if second.Code != http.StatusCreated {
		t.Fatalf("retry status = %d, want 201; body=%s", second.Code, second.Body.String())
	}
	if second.Body.String() != first.Body.String() {
		t.Errorf("retry body = %s, want 原样重放 %s", second.Body.String(), first.Body.String())
	}
	if got := second.Header().Get("Location"); got != first.Header().Get("Location") {
		t.Errorf("retry Location = %q, want %q", got, first.Header().Get("Location"))
	}
	if stub.createCalls != 1 {
		t.Errorf("重试不应再次调用 repo.CreateSlot，createCalls = %d", stub.createCalls)
	}
	// P1 修复后，首次请求在抢到占位时也会先 Load 一次历史结果；重试在未抢到占位时再 Load 一次并重放。
	if store.loadCalls != 2 {
		t.Errorf("store.loadCalls = %d, want 2（首次占位重查 + 重试 Load 重放）", store.loadCalls)
	}
}

// TestScheduleSlotCreateSlotIsolatedByUser 幂等键按用户隔离：不同用户同 key 互不影响。
func TestScheduleSlotCreateSlotIsolatedByUser(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	store := newMemIdempotencyStore()
	const key = "shared-key"

	userA := newScheduleSlotEngine(t, stub, store, 7)
	first := scheduleRequest(userA, http.MethodPost, "/api/v1/schedule/plans/1/slots",
		`{"slot":1,"maximum":3}`, map[string]string{"Idempotency-Key": key})
	if first.Code != http.StatusCreated {
		t.Fatalf("userA status = %d, want 201; body=%s", first.Code, first.Body.String())
	}

	// 用户 B 使用相同 key 提交 slot=2：因键含 userId 而互不影响，应独立执行并创建。
	userB := newScheduleSlotEngine(t, stub, store, 8)
	second := scheduleRequest(userB, http.MethodPost, "/api/v1/schedule/plans/1/slots",
		`{"slot":2,"maximum":3}`, map[string]string{"Idempotency-Key": key})
	if second.Code != http.StatusCreated {
		t.Fatalf("userB status = %d, want 201; body=%s", second.Code, second.Body.String())
	}
	if first.Header().Get("Location") == second.Header().Get("Location") {
		t.Errorf("不同用户不应重放同一结果：A=%q B=%q",
			first.Header().Get("Location"), second.Header().Get("Location"))
	}
	if stub.createCalls != 2 || len(store.records) != 2 {
		t.Errorf("createCalls = %d, records = %d, want 2/2", stub.createCalls, len(store.records))
	}
}

// TestScheduleSlotCreateSlotIsolatedByPlan 幂等键纳入实际 planId：
// 同一用户对两个不同计划使用同一 Idempotency-Key 时，两次请求必须各自执行
// repo.CreateSlot，第二次不得重放第一次计划的 Location/201。
func TestScheduleSlotCreateSlotIsolatedByPlan(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	stub.addPlan(2, "2026-09-21")
	store := newMemIdempotencyStore()
	e := newScheduleSlotEngine(t, stub, store, 7)
	const key = "same-key-two-plans"

	plan1 := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
		`{"slot":1,"maximum":3}`, map[string]string{"Idempotency-Key": key})
	if plan1.Code != http.StatusCreated {
		t.Fatalf("plan1 status = %d, want 201; body=%s", plan1.Code, plan1.Body.String())
	}
	plan2 := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/2/slots",
		`{"slot":1,"maximum":3}`, map[string]string{"Idempotency-Key": key})
	if plan2.Code != http.StatusCreated {
		t.Fatalf("plan2 status = %d, want 201; body=%s", plan2.Code, plan2.Body.String())
	}
	if plan2.Body.String() == plan1.Body.String() {
		t.Errorf("跨计划同 key 不应重放：plan1=%s plan2=%s", plan1.Body.String(), plan2.Body.String())
	}
	if got := plan2.Header().Get("Location"); got == plan1.Header().Get("Location") {
		t.Errorf("plan2 Location = %q 不应等于 plan1 %q", got, plan1.Header().Get("Location"))
	}
	if stub.createCalls != 2 || len(store.records) != 2 {
		t.Errorf("createCalls = %d, records = %d, want 2/2（两次各自执行）",
			stub.createCalls, len(store.records))
	}
}

// TestScheduleSlotCreateSlotDuplicateConflict 同一计划重复时段返回 409 SCHEDULE_SLOT_EXISTS。
func TestScheduleSlotCreateSlotDuplicateConflict(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-20")
	store := newMemIdempotencyStore()
	e := newScheduleSlotEngine(t, stub, store, 7)

	first := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
		`{"slot":1,"maximum":3}`, map[string]string{"Idempotency-Key": "dup-first"})
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201; body=%s", first.Code, first.Body.String())
	}
	second := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
		`{"slot":1,"maximum":3}`, map[string]string{"Idempotency-Key": "dup-second"})
	body := assertErrorStatus(t, second, http.StatusConflict, "SCHEDULE_SLOT_EXISTS")
	if body["message"] != "该时段已存在" {
		t.Errorf("message = %v, want 该时段已存在", body["message"])
	}
}

// TestScheduleSlotCreateSlotLockedPlan 计划已开始（日期为业务当日）返回 409 SCHEDULE_SLOT_LOCKED。
func TestScheduleSlotCreateSlotLockedPlan(t *testing.T) {
	stub := &slotRepoStub{}
	stub.addPlan(1, "2026-09-09") // 与固定 now 同属上海 09-09 业务日，已开始。
	store := newMemIdempotencyStore()
	e := newScheduleSlotEngine(t, stub, store, 7)

	w := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/1/slots",
		`{"slot":1,"maximum":3}`, map[string]string{"Idempotency-Key": "lock-key"})
	body := assertErrorStatus(t, w, http.StatusConflict, "SCHEDULE_SLOT_LOCKED")
	if body["message"] != "排班已开始或已结束，不能新增时段" {
		t.Errorf("message = %v", body["message"])
	}
}

// TestScheduleSlotCreateSlotPlanNotFound POST 创建对应计划不存在返回 404。
func TestScheduleSlotCreateSlotPlanNotFound(t *testing.T) {
	store := newMemIdempotencyStore()
	e := newScheduleSlotEngine(t, &slotRepoStub{}, store, 7)

	w := scheduleRequest(e, http.MethodPost, "/api/v1/schedule/plans/999/slots",
		`{"slot":1,"maximum":3}`, map[string]string{"Idempotency-Key": "no-plan"})
	body := assertErrorStatus(t, w, http.StatusNotFound, "SCHEDULE_PLAN_NOT_FOUND")
	if body["message"] != "排班计划不存在" {
		t.Errorf("message = %v, want 排班计划不存在", body["message"])
	}
}
