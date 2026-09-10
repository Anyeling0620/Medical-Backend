package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/domain/schedule"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/middleware"
	userservice "Medical-Web-Backend/internal/usecase/misuser"
	scheduleservice "Medical-Web-Backend/internal/usecase/schedule"
)

// fakeSchedulePlanRepo 复用 usecase 测试的内存仓库实现会引入循环依赖，因此 handler
// 测试内嵌一个极简内存 PlanRepository：已存在性/锁定由 service 层判定，这里只负责落数据。
type fakeSchedulePlanRepo struct {
	nextID  int64
	plans   map[int64]*schedule.WorkPlan
	slots   map[int64][]schedule.ScheduleSlot
	regs    map[int64]bool
	doctors map[int64]bool
	assoc   map[string]bool
	subs    map[int64]bool
}

func newFakeSchedulePlanRepo() *fakeSchedulePlanRepo {
	return &fakeSchedulePlanRepo{
		plans:   make(map[int64]*schedule.WorkPlan),
		slots:   make(map[int64][]schedule.ScheduleSlot),
		regs:    make(map[int64]bool),
		doctors: make(map[int64]bool),
		assoc:   make(map[string]bool),
		subs:    make(map[int64]bool),
	}
}

// seedDoctor 注册 ACTIVE 医生并建立其与子科室的关联。
func (f *fakeSchedulePlanRepo) seedDoctor(doctorID, subID int64) {
	f.doctors[doctorID] = true
	f.subs[subID] = true
	f.assoc[docKey(doctorID, subID)] = true
}

func docKey(a, b int64) string {
	return idStr(a) + ":" + idStr(b)
}

func idStr(v int64) string {
	if v == 0 {
		return "0"
	}
	buf := make([]byte, 0, 20)
	for v > 0 {
		buf = append([]byte{byte('0' + v%10)}, buf...)
		v /= 10
	}
	return string(buf)
}

func (f *fakeSchedulePlanRepo) ListPlans(_ context.Context, filter schedule.PlanFilter, _, _ int) ([]schedule.WorkPlan, int64, error) {
	out := make([]schedule.WorkPlan, 0)
	for _, p := range f.plans {
		if filter.DoctorID != nil && p.DoctorID != *filter.DoctorID {
			continue
		}
		cp := *p
		cp.Recalculate()
		cp.Slots = append([]schedule.ScheduleSlot(nil), f.slots[p.ID]...)
		for i := range cp.Slots {
			cp.Slots[i].Recalculate()
		}
		out = append(out, cp)
	}
	return out, int64(len(out)), nil
}

func (f *fakeSchedulePlanRepo) FindPlan(_ context.Context, planID int64) (*schedule.WorkPlan, error) {
	if p, ok := f.plans[planID]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, sql.ErrNoRows
}

func (f *fakeSchedulePlanRepo) ExecTx(_ context.Context, fn func(tx port.ScheduleTx) error) error {
	return fn(f)
}

func (f *fakeSchedulePlanRepo) TxInsertPlan(_ context.Context, p schedule.WorkPlan) (int64, error) {
	for _, plan := range f.plans {
		if plan.DoctorID == p.DoctorID && plan.SubdepartmentID == p.SubdepartmentID && plan.Date == p.Date {
			return 0, schedule.ErrPlanExists
		}
	}
	f.nextID++
	p.ID = f.nextID
	p.Recalculate()
	cp := p
	f.plans[p.ID] = &cp
	return p.ID, nil
}

func (f *fakeSchedulePlanRepo) TxHasPlan(_ context.Context, doctorID, subID int64, date string) (bool, error) {
	for _, plan := range f.plans {
		if plan.DoctorID == doctorID && plan.SubdepartmentID == subID && plan.Date == date {
			return true, nil
		}
	}
	return false, nil
}

// TxLockPlanCreate 内存 fake 由 ExecTx 直执行且单协程驱动，无需真正咨询锁。
func (f *fakeSchedulePlanRepo) TxLockPlanCreate(_ context.Context, _ int64, _ int64, _ string) error {
	return nil
}

func (f *fakeSchedulePlanRepo) TxFindPlanLocked(_ context.Context, planID int64) (*schedule.WorkPlan, error) {
	if p, ok := f.plans[planID]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, sql.ErrNoRows
}

func (f *fakeSchedulePlanRepo) TxUpdatePlanMaximum(_ context.Context, planID int64, maximum int16) error {
	p, ok := f.plans[planID]
	if !ok {
		return sql.ErrNoRows
	}
	if maximum < p.Used {
		return schedule.ErrMaximumBelowUsed
	}
	p.Maximum = maximum
	return nil
}

func (f *fakeSchedulePlanRepo) TxHasRegistrations(_ context.Context, planID int64) (bool, error) {
	return f.regs[planID], nil
}

func (f *fakeSchedulePlanRepo) TxDeletePlan(_ context.Context, planID int64) error {
	delete(f.plans, planID)
	delete(f.regs, planID)
	return nil
}

// TxDoctorAssociation 三态返回（医生 ACTIVE、子科室存在、存在关联），与 port 契约一致。
func (f *fakeSchedulePlanRepo) TxDoctorAssociation(_ context.Context, doctorID, subID int64) (bool, bool, bool, error) {
	doctorActive := f.doctors[doctorID]
	return doctorActive, f.subs[subID], doctorActive && f.subs[subID] && f.assoc[docKey(doctorID, subID)], nil
}

// fakeIdemStore 是内存幂等存储：支持“保存即返回”“不可用”“已保存重放”三种行为。
type fakeIdemStore struct {
	released    map[string]bool
	claimed     map[string]bool
	results     map[string]*port.IdempotencyRecord
	unavailable bool
	// slowResults 记录每个 key 在 Load 时先返回“无结果”的次数，用于模拟首请求仍在途。
	slowResults map[string]int
	// saveFailsBeforeOK 记录每个 key 的 Save 在成功前先失败几次，用于模拟结果落库瞬时故障。
	saveFailsBeforeOK map[string]int
	claims            int
	saves             int
	// saveCalls 记录 Save 被调用的总次数（含失败尝试），与 saves（成功次数）配合断言重试。
	saveCalls int
}

func newFakeIdemStore() *fakeIdemStore {
	return &fakeIdemStore{
		claimed:           make(map[string]bool),
		results:           make(map[string]*port.IdempotencyRecord),
		released:          make(map[string]bool),
		slowResults:       make(map[string]int),
		saveFailsBeforeOK: make(map[string]int),
	}
}

func (f *fakeIdemStore) Claim(_ context.Context, key string) (bool, error) {
	if f.unavailable {
		return false, port.ErrIdempotencyStoreUnavailable
	}
	f.claims++
	if f.claimed[key] {
		return false, nil
	}
	f.claimed[key] = true
	return true, nil
}

func (f *fakeIdemStore) Save(_ context.Context, key string, record port.IdempotencyRecord) error {
	if f.unavailable {
		return port.ErrIdempotencyStoreUnavailable
	}
	f.saveCalls++
	if n := f.saveFailsBeforeOK[key]; n > 0 {
		f.saveFailsBeforeOK[key] = n - 1
		return port.ErrIdempotencyStoreUnavailable
	}
	f.saves++
	f.results[key] = &record
	return nil
}

func (f *fakeIdemStore) Load(_ context.Context, key string) (*port.IdempotencyRecord, error) {
	if f.unavailable {
		return nil, port.ErrIdempotencyStoreUnavailable
	}
	// 模拟首请求仍在途：指定的前几次轮询返回空，之后正常读到已保存结果。
	if n := f.slowResults[key]; n > 0 {
		f.slowResults[key] = n - 1
		return nil, nil
	}
	if rec, ok := f.results[key]; ok {
		return rec, nil
	}
	return nil, nil
}

func (f *fakeIdemStore) Release(_ context.Context, key string) error {
	f.released[key] = true
	delete(f.claimed, key)
	return nil
}

// scheduleHandlerEnv 组装 handler 测试环境（固定业务日期 2026-09-09）。
type scheduleHandlerEnv struct {
	engine *gin.Engine
	repo   *fakeSchedulePlanRepo
	store  *fakeIdemStore
	// handler 暴露给测试，便于调小在途轮询次数/间隔以加速超时分支断言。
	handler *ScheduleHandler
}

func newScheduleEnv(t *testing.T) *scheduleHandlerEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := newFakeSchedulePlanRepo()
	store := newFakeIdemStore()
	service := scheduleservice.NewService(repo, func() time.Time {
		return time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	})
	h := NewScheduleHandler(service, store)
	engine := gin.New()
	// 模拟 RequireAccessToken(RealmMis) 设置的 claims（handler 只依赖 claims.UserID，
	// 但 realm 必须为 mis 才是合法状态，避免夹具与中间件语义不一致）。
	engine.Use(func(c *gin.Context) {
		c.Set(middleware.ClaimsKey, &userservice.AccessClaims{
			UserID: 1,
			Realm:  domainauth.RealmMis,
		})
		c.Next()
	})
	engine.GET("/api/v1/schedule/plans", h.ListPlans)
	engine.POST("/api/v1/schedule/plans", h.CreatePlan)
	engine.PATCH("/api/v1/schedule/plans/:planId", h.UpdatePlan)
	engine.DELETE("/api/v1/schedule/plans/:planId", h.DeletePlan)
	return &scheduleHandlerEnv{engine: engine, repo: repo, store: store, handler: h}
}

// performSchedule 发起带可选 body 与头部的排班请求。
func performSchedule(e *gin.Engine, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

func mustJSON(t *testing.T, payload string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	return m
}

func TestScheduleListOKAndEmptyItems(t *testing.T) {
	env := newScheduleEnv(t)
	repo := env.repo
	planSeed := &schedule.WorkPlan{ID: 4, DoctorID: 16, SubdepartmentID: 2, Date: "2026-09-20", Maximum: 45, Used: 3}
	planSeed.Recalculate()
	repo.plans[4] = planSeed
	repo.slots[4] = []schedule.ScheduleSlot{{ID: 12, WorkPlanID: 4, Slot: 1, Maximum: 3, Used: 1}}

	w := performSchedule(env.engine, http.MethodGet, "/api/v1/schedule/plans?includeSlots=true", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := mustJSON(t, w.Body.String())
	if body["total"].(float64) != 1 || body["page"].(float64) != 1 || body["pageSize"].(float64) != 20 {
		t.Errorf("page fields = %v", body)
	}
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", items)
	}
	plan := items[0].(map[string]any)
	if plan["id"].(float64) != 4 || plan["doctorId"].(float64) != 16 || plan["maximum"].(float64) != 45 || plan["used"].(float64) != 3 || plan["remaining"].(float64) != 42 {
		t.Errorf("plan = %v", plan)
	}
	slots := plan["slots"].([]any)
	if len(slots) != 1 {
		t.Fatalf("slots = %v", plan["slots"])
	}
	slot := slots[0].(map[string]any)
	if slot["slot"].(float64) != 1 || slot["remaining"].(float64) != 2 {
		t.Errorf("slot = %v", slot)
	}

	// 空结果 items 必须是 []。
	w = performSchedule(env.engine, http.MethodGet, "/api/v1/schedule/plans?doctorId=999", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Errorf("空列表必须序列化为 items:[]，got %s", w.Body.String())
	}
}

func TestScheduleListValidation(t *testing.T) {
	env := newScheduleEnv(t)
	for _, query := range []string{"doctorId=0", "page=0", "pageSize=101", "sort=name", "order=up", "includeSlots=xx", "fromDate=2026-13-01"} {
		w := performSchedule(env.engine, http.MethodGet, "/api/v1/schedule/plans?"+query, "", nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("query %s status = %d, want 422", query, w.Code)
			continue
		}
		body := mustJSON(t, w.Body.String())
		if body["code"] != "REQUEST_VALIDATION_FAILED" {
			t.Errorf("query %s code = %v", query, body["code"])
		}
	}
	// fromDate 晚于 toDate。
	w := performSchedule(env.engine, http.MethodGet, "/api/v1/schedule/plans?fromDate=2026-10-01&toDate=2026-09-01", "", nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("date range status = %d, want 422", w.Code)
	}
	body := mustJSON(t, w.Body.String())
	if body["message"] != "日期范围无效" {
		t.Errorf("message = %v, want 日期范围无效", body["message"])
	}
}

func TestScheduleCreateSuccess(t *testing.T) {
	env := newScheduleEnv(t)
	env.repo.seedDoctor(16, 2)
	w := performSchedule(env.engine, http.MethodPost, "/api/v1/schedule/plans",
		`{"doctorId":16,"subdepartmentId":2,"date":"2026-09-20","maximum":45}`,
		map[string]string{"Idempotency-Key": "create-plan-1"})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/api/v1/schedule/plans/1" {
		t.Errorf("Location = %q, want /api/v1/schedule/plans/1", loc)
	}
	body := mustJSON(t, w.Body.String())
	if body["id"].(float64) != 1 || body["doctorId"].(float64) != 16 || body["subdepartmentId"].(float64) != 2 ||
		body["date"] != "2026-09-20" || body["maximum"].(float64) != 45 || body["used"].(float64) != 0 || body["remaining"].(float64) != 45 {
		t.Errorf("create body = %v", body)
	}
}

func TestScheduleCreateValidationAndAssociationErrors(t *testing.T) {
	env := newScheduleEnv(t)
	env.repo.seedDoctor(16, 2)
	// 医生 17 ACTIVE 且关联子科室 3：可用于“医生有效、子科室存在但未关联”用例。
	env.repo.seedDoctor(17, 3)
	cases := []struct {
		name    string
		body    string
		hasKey  bool
		code    string
		message string
		status  int
	}{
		{"缺幂等键", `{"doctorId":16,"subdepartmentId":2,"date":"2026-09-20","maximum":45}`, false, "REQUEST_VALIDATION_FAILED", "Idempotency-Key 请求头必填", 422},
		{"maximum=0", `{"doctorId":16,"subdepartmentId":2,"date":"2026-09-20","maximum":0}`, true, "REQUEST_VALIDATION_FAILED", "最大号源必须大于 0", 422},
		{"过去日期", `{"doctorId":16,"subdepartmentId":2,"date":"2026-09-08","maximum":45}`, true, "REQUEST_VALIDATION_FAILED", "排班日期不得早于业务当前日期", 422},
		{"子科室不存在", `{"doctorId":16,"subdepartmentId":99,"date":"2026-09-20","maximum":45}`, true, "REQUEST_VALIDATION_FAILED", "子科室不存在", 422},
		{"医生不存在", `{"doctorId":999,"subdepartmentId":2,"date":"2026-09-20","maximum":45}`, true, "REQUEST_VALIDATION_FAILED", "医生不存在或未在出诊状态", 422},
		{"医生未关联该子科室", `{"doctorId":17,"subdepartmentId":2,"date":"2026-09-20","maximum":45}`, true, "REQUEST_VALIDATION_FAILED", "医生未关联该子科室", 422},
	}
	for ci := range cases {
		t.Run(cases[ci].name, func(t *testing.T) {
			tc := cases[ci]
			var headers map[string]string
			if tc.hasKey {
				headers = map[string]string{"Idempotency-Key": "create-plan-err-" + idStr(int64(ci))}
			}
			w := performSchedule(env.engine, http.MethodPost, "/api/v1/schedule/plans", tc.body, headers)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.status, w.Body.String())
			}
			body := mustJSON(t, w.Body.String())
			if body["code"] != tc.code || body["message"] != tc.message {
				t.Errorf("body = %v, want code=%s message=%s", body, tc.code, tc.message)
			}
		})
	}
}

func TestScheduleCreateIdempotencyReplay(t *testing.T) {
	env := newScheduleEnv(t)
	env.repo.seedDoctor(16, 2)
	const body = `{"doctorId":16,"subdepartmentId":2,"date":"2026-09-20","maximum":45}`
	headers := map[string]string{"Idempotency-Key": "create-replay"}
	first := performSchedule(env.engine, http.MethodPost, "/api/v1/schedule/plans", body, headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("首次 status = %d; body=%s", first.Code, first.Body.String())
	}
	second := performSchedule(env.engine, http.MethodPost, "/api/v1/schedule/plans", body, headers)
	if second.Code != http.StatusCreated {
		t.Fatalf("重放 status = %d, want 201; body=%s", second.Code, second.Body.String())
	}
	if second.Body.String() != first.Body.String() {
		t.Errorf("重放 body 与首次不一致:\n%s\n%s", first.Body.String(), second.Body.String())
	}
	if second.Header().Get("Location") != first.Header().Get("Location") {
		t.Errorf("重放 Location = %q, 首次 = %q", second.Header().Get("Location"), first.Header().Get("Location"))
	}
	if env.store.saves != 1 {
		t.Errorf("saves = %d, want 1（重放不应再次执行）", env.store.saves)
	}
	if len(env.repo.plans) != 1 {
		t.Errorf("plans = %d, want 1（不能重复创建）", len(env.repo.plans))
	}
}

// TestScheduleCreateReplayBusinessError 契约 1.5：同一用户/路径/key 的重复请求原样返回
// 第一次结果，业务错误（重复计划 409）同样重放，不能再次执行校验或创建。
func TestScheduleCreateReplayBusinessError(t *testing.T) {
	env := newScheduleEnv(t)
	env.repo.seedDoctor(16, 2)
	const body = `{"doctorId":16,"subdepartmentId":2,"date":"2026-09-20","maximum":45}`

	first := performSchedule(env.engine, http.MethodPost, "/api/v1/schedule/plans", body,
		map[string]string{"Idempotency-Key": "create-biz-1"})
	if first.Code != http.StatusCreated {
		t.Fatalf("首次 status = %d, want 201; body=%s", first.Code, first.Body.String())
	}
	// 第二个 key 撞上重复计划：业务错误 409。
	second := performSchedule(env.engine, http.MethodPost, "/api/v1/schedule/plans", body,
		map[string]string{"Idempotency-Key": "create-biz-2"})
	if second.Code != http.StatusConflict {
		t.Fatalf("重复计划 status = %d, want 409; body=%s", second.Code, second.Body.String())
	}
	// 第三个 key 与第二次相同：必须原样重放 409，不再次创建或改动。
	third := performSchedule(env.engine, http.MethodPost, "/api/v1/schedule/plans", body,
		map[string]string{"Idempotency-Key": "create-biz-2"})
	if third.Code != http.StatusConflict {
		t.Fatalf("重放业务错误 status = %d, want 409; body=%s", third.Code, third.Body.String())
	}
	if third.Body.String() != second.Body.String() {
		t.Errorf("业务错误重放不一致:\n%s\n%s", second.Body.String(), third.Body.String())
	}
	if len(env.repo.plans) != 1 {
		t.Errorf("plans = %d, want 1（业务错误重放不得重复创建）", len(env.repo.plans))
	}
}

func TestScheduleCreateStoreUnavailable(t *testing.T) {
	env := newScheduleEnv(t)
	env.repo.seedDoctor(16, 2)
	env.store.unavailable = true
	w := performSchedule(env.engine, http.MethodPost, "/api/v1/schedule/plans",
		`{"doctorId":16,"subdepartmentId":2,"date":"2026-09-20","maximum":45}`,
		map[string]string{"Idempotency-Key": "create-unavail"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	body := mustJSON(t, w.Body.String())
	if body["code"] != "IDEMPOTENCY_STORE_UNAVAILABLE" || body["message"] != "幂等存储不可用，请稍后重试" {
		t.Errorf("body = %v", body)
	}
	if len(env.repo.plans) != 0 {
		t.Errorf("Redis 不可用时不得静默降级创建，plans = %d", len(env.repo.plans))
	}
}

func seedFuturePlan(t *testing.T, env *scheduleHandlerEnv, maximum int16, used int16) *schedule.WorkPlan {
	t.Helper()
	env.repo.seedDoctor(16, 2)
	env.repo.nextID++
	p := &schedule.WorkPlan{ID: env.repo.nextID, DoctorID: 16, SubdepartmentID: 2, Date: "2026-09-20", Maximum: maximum, Used: used}
	p.Recalculate()
	env.repo.plans[p.ID] = p
	return p
}

func TestScheduleUpdateSuccess(t *testing.T) {
	env := newScheduleEnv(t)
	plan := seedFuturePlan(t, env, 45, 3)
	headers := map[string]string{"Idempotency-Key": "update-ok"}
	w := performSchedule(env.engine, http.MethodPatch, "/api/v1/schedule/plans/"+idStr(plan.ID), `{"maximum":60}`, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := mustJSON(t, w.Body.String())
	if body["maximum"].(float64) != 60 || body["used"].(float64) != 3 || body["remaining"].(float64) != 57 {
		t.Errorf("update body = %v", body)
	}

}

func TestScheduleUpdateErrors(t *testing.T) {
	env := newScheduleEnv(t)
	plan := seedFuturePlan(t, env, 45, 3)
	planID := idStr(plan.ID)

	cases := []struct {
		name       string
		idem       string
		body       string
		status     int
		code       string
		message    string
		wantDetail bool
	}{
		{"新值低于 used", "u3", `{"maximum":2}`, 409, "SCHEDULE_CONFLICT", "最大号源不能小于已使用号源", true},
		{"unknown 字段", "u4", `{"maximum":60,"doctorId":1}`, 422, "REQUEST_VALIDATION_FAILED", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{"Idempotency-Key": tc.idem}
			w := performSchedule(env.engine, http.MethodPatch, "/api/v1/schedule/plans/"+planID, tc.body, headers)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.status, w.Body.String())
			}
			body := mustJSON(t, w.Body.String())
			if body["code"] != tc.code {
				t.Errorf("code = %v, want %s", body["code"], tc.code)
			}
			if tc.message != "" && body["message"] != tc.message {
				t.Errorf("message = %v, want %s", body["message"], tc.message)
			}
			if tc.wantDetail {
				details, ok := body["details"].(map[string]any)
				if !ok || details["used"].(float64) != 3 {
					t.Errorf("details = %v, want used=3", body["details"])
				}
			}
		})
	}
}

func TestScheduleUpdateLockedAndNotFound(t *testing.T) {
	env := newScheduleEnv(t)
	env.repo.seedDoctor(16, 2)
	env.repo.nextID++
	locked := &schedule.WorkPlan{ID: env.repo.nextID, DoctorID: 16, SubdepartmentID: 2, Date: "2026-09-09", Maximum: 45}
	locked.Recalculate()
	env.repo.plans[locked.ID] = locked

	w := performSchedule(env.engine, http.MethodPatch, "/api/v1/schedule/plans/"+idStr(locked.ID), `{"maximum":60}`,
		map[string]string{"Idempotency-Key": "update-locked"})
	if w.Code != http.StatusConflict {
		t.Fatalf("锁定计划 status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	body := mustJSON(t, w.Body.String())
	if body["code"] != "SCHEDULE_PLAN_LOCKED" {
		t.Errorf("code = %v, want SCHEDULE_PLAN_LOCKED", body["code"])
	}

	w = performSchedule(env.engine, http.MethodPatch, "/api/v1/schedule/plans/424242", `{"maximum":60}`,
		map[string]string{"Idempotency-Key": "update-missing"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("不存在计划 status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	body = mustJSON(t, w.Body.String())
	if body["code"] != "SCHEDULE_PLAN_NOT_FOUND" || body["message"] != "排班计划不存在" {
		t.Errorf("body = %v", body)
	}
}

func TestScheduleDeleteSuccessAndGuards(t *testing.T) {
	env := newScheduleEnv(t)
	plan := seedFuturePlan(t, env, 45, 0)
	w := performSchedule(env.engine, http.MethodDelete, "/api/v1/schedule/plans/"+idStr(plan.ID), "",
		map[string]string{"Idempotency-Key": "delete-ok"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("删除 status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Errorf("204 不应有响应体: %q", w.Body.String())
	}
	if _, ok := env.repo.plans[plan.ID]; ok {
		t.Errorf("计划 %d 应已删除", plan.ID)
	}

	// 有挂号记录。
	withReg := seedFuturePlan(t, env, 45, 0)
	env.repo.regs[withReg.ID] = true
	w = performSchedule(env.engine, http.MethodDelete, "/api/v1/schedule/plans/"+idStr(withReg.ID), "",
		map[string]string{"Idempotency-Key": "delete-reg"})
	if w.Code != http.StatusConflict {
		t.Fatalf("有挂号 status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	body := mustJSON(t, w.Body.String())
	if body["code"] != "SCHEDULE_HAS_REGISTRATIONS" || body["message"] != "已有挂号记录，不能删除排班" {
		t.Errorf("body = %v", body)
	}

	// 已开始计划。
	env.repo.nextID++
	started := &schedule.WorkPlan{ID: env.repo.nextID, DoctorID: 16, SubdepartmentID: 2, Date: "2026-09-09", Maximum: 45}
	started.Recalculate()
	env.repo.plans[started.ID] = started
	w = performSchedule(env.engine, http.MethodDelete, "/api/v1/schedule/plans/"+idStr(started.ID), "",
		map[string]string{"Idempotency-Key": "delete-started"})
	if w.Code != http.StatusConflict {
		t.Fatalf("已开始 status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	body = mustJSON(t, w.Body.String())
	if body["code"] != "SCHEDULE_PLAN_LOCKED" {
		t.Errorf("code = %v, want SCHEDULE_PLAN_LOCKED", body["code"])
	}

	// 不存在。
	w = performSchedule(env.engine, http.MethodDelete, "/api/v1/schedule/plans/424242", "",
		map[string]string{"Idempotency-Key": "delete-missing"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("不存在 status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestScheduleDeleteReplay(t *testing.T) {
	env := newScheduleEnv(t)
	plan := seedFuturePlan(t, env, 45, 0)
	headers := map[string]string{"Idempotency-Key": "delete-replay"}
	target := "/api/v1/schedule/plans/" + idStr(plan.ID)
	first := performSchedule(env.engine, http.MethodDelete, target, "", headers)
	if first.Code != http.StatusNoContent {
		t.Fatalf("首次删除 status = %d; body=%s", first.Code, first.Body.String())
	}
	second := performSchedule(env.engine, http.MethodDelete, target, "", headers)
	if second.Code != http.StatusNoContent {
		t.Fatalf("重放删除 status = %d, want 204; body=%s", second.Code, second.Body.String())
	}
}

// TestScheduleUpdateIdempotencyKeyIsolatesPerPlan 验证幂等键 scope 用真实资源路径隔离：
// 同一用户对 plan1 与 plan2 用同一 Idempotency-Key 做 PATCH 时，第二次必须真实执行 plan2
// 的更新，而不是重放 plan1 的结果（旧实现用路由模板 :planId 会让两者共键）。
func TestScheduleUpdateIdempotencyKeyIsolatesPerPlan(t *testing.T) {
	env := newScheduleEnv(t)
	p1 := seedFuturePlan(t, env, 45, 0)
	p2 := seedFuturePlan(t, env, 45, 0)
	const rawKey = "shared-plan-key"
	for _, plan := range []*schedule.WorkPlan{p1, p2} {
		w := performSchedule(env.engine, http.MethodPatch, "/api/v1/schedule/plans/"+idStr(plan.ID), `{"maximum":60}`,
			map[string]string{"Idempotency-Key": rawKey})
		if w.Code != http.StatusOK {
			t.Fatalf("plan %d PATCH status = %d, want 200; body=%s", plan.ID, w.Code, w.Body.String())
		}
	}
	if p1.Maximum != 60 || p2.Maximum != 60 {
		t.Errorf("两个计划都应真实变更为 60：p1=%d p2=%d", p1.Maximum, p2.Maximum)
	}
	if env.store.saves != 2 {
		t.Errorf("saves = %d, want 2（不同计划同一 key 不得相互重放）", env.store.saves)
	}
}

// TestScheduleDeleteIdempotencyKeyIsolatesPerPlan 与 PATCH 用例同理：同一 key 删除 plan1 后，
// 删除 plan2 必须真实执行，plan2 不能因共键被 plan1 的 204 重放而残留。
func TestScheduleDeleteIdempotencyKeyIsolatesPerPlan(t *testing.T) {
	env := newScheduleEnv(t)
	p1 := seedFuturePlan(t, env, 45, 0)
	p2 := seedFuturePlan(t, env, 45, 0)
	const rawKey = "shared-delete-key"
	for _, plan := range []*schedule.WorkPlan{p1, p2} {
		w := performSchedule(env.engine, http.MethodDelete, "/api/v1/schedule/plans/"+idStr(plan.ID), "",
			map[string]string{"Idempotency-Key": rawKey})
		if w.Code != http.StatusNoContent {
			t.Fatalf("plan %d DELETE status = %d, want 204; body=%s", plan.ID, w.Code, w.Body.String())
		}
	}
	if _, ok := env.repo.plans[p1.ID]; ok {
		t.Errorf("plan1 应已删除")
	}
	if _, ok := env.repo.plans[p2.ID]; ok {
		t.Errorf("plan2 应已真实删除（不能被 plan1 的 204 重放挡住）")
	}
	if env.store.saves != 2 {
		t.Errorf("saves = %d, want 2", env.store.saves)
	}
}

// TestScheduleIdempotencyInFlightTimeoutReturns503 验证同 key 首请求仍在途且轮询期内未产生结果时，
// 重复请求返回 503 IDEMPOTENCY_PROCESSING（契约可重试），而不是不可盲目重试的 409。
func TestScheduleIdempotencyInFlightTimeoutReturns503(t *testing.T) {
	env := newScheduleEnv(t)
	plan := seedFuturePlan(t, env, 45, 0)
	env.handler.pollAttempts = 3
	env.handler.pollInterval = time.Millisecond
	key := idempotencyKey(1, http.MethodPatch+"|/api/v1/schedule/plans/"+idStr(plan.ID), "inflight-timeout")
	env.store.claimed[key] = true // 模拟首个请求已抢到占位但始终未保存结果
	w := performSchedule(env.engine, http.MethodPatch, "/api/v1/schedule/plans/"+idStr(plan.ID), `{"maximum":60}`,
		map[string]string{"Idempotency-Key": "inflight-timeout"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	body := mustJSON(t, w.Body.String())
	if body["code"] != "IDEMPOTENCY_PROCESSING" || body["message"] != "相同幂等键的请求正在处理中，请稍后重试" {
		t.Errorf("body = %v", body)
	}
	if env.repo.plans[plan.ID].Maximum != 45 {
		t.Errorf("超时分支不应执行更新，plan maximum = %d", env.repo.plans[plan.ID].Maximum)
	}
}

// TestScheduleIdempotencyInFlightWaitsAndReplays 验证轮询期间第一次结果一出现即重放，
// 无需等待到 3 秒超时。
func TestScheduleIdempotencyInFlightWaitsAndReplays(t *testing.T) {
	env := newScheduleEnv(t)
	plan := seedFuturePlan(t, env, 45, 0)
	env.handler.pollAttempts = 50
	env.handler.pollInterval = time.Millisecond
	target := "/api/v1/schedule/plans/" + idStr(plan.ID)
	rawKey := "inflight-replay"
	headers := map[string]string{"Idempotency-Key": rawKey}
	// 先真实执行一次产生可重放结果（成功后 fake 占位仍保留，模拟首请求完成）。
	first := performSchedule(env.engine, http.MethodPatch, target, `{"maximum":60}`, headers)
	if first.Code != http.StatusOK {
		t.Fatalf("首次 PATCH status = %d; body=%s", first.Code, first.Body.String())
	}
	// 让前 5 次轮询模拟“仍在途无结果”，之后返回已保存结果触发重放。
	key := idempotencyKey(1, http.MethodPatch+"|"+target, rawKey)
	env.store.slowResults[key] = 5
	second := performSchedule(env.engine, http.MethodPatch, target, `{"maximum":60}`, headers)
	if second.Code != http.StatusOK {
		t.Fatalf("重放 PATCH status = %d, want 200; body=%s", second.Code, second.Body.String())
	}
	if second.Body.String() != first.Body.String() {
		t.Errorf("在途等待后应原样重放第一次结果:\n%s\n%s", first.Body.String(), second.Body.String())
	}
	if env.store.saves != 1 {
		t.Errorf("saves = %d, want 1（等待到结果后重放，不应再次执行）", env.store.saves)
	}
}

// TestScheduleIdempotencyReplaysWhenClaimExpiredResultRemains 验证：旧 claim 按短 TTL（10 分钟）
// 过期而 24h 结果仍在时，新请求抢到占位后必须直接重放历史结果，不得重跑业务（否则同 key
// 重复 PATCH/DELETE/POST 会产生二次副作用）。此处第二次请求故意换成不同请求体，若被重跑会
// 留下不同结果（saves=2），重放路径则原样返回第一次结果（saves=1）。
func TestScheduleIdempotencyReplaysWhenClaimExpiredResultRemains(t *testing.T) {
	env := newScheduleEnv(t)
	plan := seedFuturePlan(t, env, 45, 0)
	target := "/api/v1/schedule/plans/" + idStr(plan.ID)
	rawKey := "stale-claim-replay"
	firstHeaders := map[string]string{"Idempotency-Key": rawKey}
	first := performSchedule(env.engine, http.MethodPatch, target, `{"maximum":60}`, firstHeaders)
	if first.Code != http.StatusOK {
		t.Fatalf("首次 PATCH status = %d; body=%s", first.Code, first.Body.String())
	}
	key := idempotencyKey(1, http.MethodPatch+"|"+target, rawKey)
	// 模拟占位过期：删除 claim（真实 Redis 中为 TTL 到期），结果键仍保留。
	delete(env.store.claimed, key)
	if _, ok := env.store.results[key]; !ok {
		t.Fatalf("测试前置：第一次结果应已落库")
	}
	second := performSchedule(env.engine, http.MethodPatch, target, `{"maximum":70}`, firstHeaders)
	if second.Code != http.StatusOK {
		t.Fatalf("重放 PATCH status = %d, want 200; body=%s", second.Code, second.Body.String())
	}
	if second.Body.String() != first.Body.String() {
		t.Errorf("应原样重放第一次结果（maximum=60），got:\n%s\nwant:\n%s", second.Body.String(), first.Body.String())
	}
	if env.store.saves != 1 {
		t.Errorf("saves = %d, want 1（claim 过期但结果仍在时不得重跑业务）", env.store.saves)
	}
	// 本次新占位应被释放，不残留阻塞后续同 key 请求。
	if env.store.claimed[key] {
		t.Errorf("重放后本次新 claim 应已释放")
	}
	if !env.store.released[key] {
		t.Errorf("重放路径应调用 Release 释放本次占位")
	}
}

// TestScheduleIdempotencySaveRetriesThenSucceeds 验证 Save 先失败若干次后成功的路径：
// handler 对 Save 做有限重试（此处前 2 次失败、第 3 次成功），最终返回真实结果且结果落库，
// 占位不被提前释放、业务只执行一次。
func TestScheduleIdempotencySaveRetriesThenSucceeds(t *testing.T) {
	env := newScheduleEnv(t)
	env.repo.seedDoctor(16, 2)
	env.handler.saveAttempts = 3
	env.handler.saveInterval = time.Millisecond
	const body = `{"doctorId":16,"subdepartmentId":2,"date":"2026-09-20","maximum":45}`
	key := idempotencyKey(1, http.MethodPost+"|/api/v1/schedule/plans", "save-retry")
	env.store.saveFailsBeforeOK[key] = 2
	w := performSchedule(env.engine, http.MethodPost, "/api/v1/schedule/plans", body,
		map[string]string{"Idempotency-Key": "save-retry"})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if _, ok := env.store.results[key]; !ok {
		t.Fatalf("Save 重试成功后结果应最终落库")
	}
	if env.store.saveCalls != 3 {
		t.Errorf("saveCalls = %d, want 3（两次失败 + 一次成功）", env.store.saveCalls)
	}
	if env.store.saves != 1 {
		t.Errorf("成功保存次数 = %d, want 1", env.store.saves)
	}
	if !env.store.claimed[key] {
		t.Errorf("Save 成功路径应保留占位（占位供同 key 重放，直到 claim TTL 到期）")
	}
	if len(env.repo.plans) != 1 {
		t.Errorf("业务只应执行一次创建，plans = %d", len(env.repo.plans))
	}
}

// TestScheduleUpdateDeleteWithoutIdempotencyKey 验证契约合规：PATCH/DELETE 不强制要求
// Idempotency-Key，两者不带键时也应正常执行（幂等键对它们是可选增强）。
func TestScheduleUpdateDeleteWithoutIdempotencyKey(t *testing.T) {
	env := newScheduleEnv(t)
	plan := seedFuturePlan(t, env, 45, 0)

	w := performSchedule(env.engine, http.MethodPatch, "/api/v1/schedule/plans/"+idStr(plan.ID), `{"maximum":60}`,
		nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH 不带幂等键 status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if body := mustJSON(t, w.Body.String()); body["maximum"].(float64) != 60 {
		t.Errorf("PATCH 后 maximum = %v, want 60", body["maximum"])
	}

	plan2 := seedFuturePlan(t, env, 45, 0)
	w = performSchedule(env.engine, http.MethodDelete, "/api/v1/schedule/plans/"+idStr(plan2.ID), "", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE 不带幂等键 status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Errorf("204 不应有响应体: %q", w.Body.String())
	}
}

// --- ScheduleSlotRepository 适配桩（plans handler 单测不调用时段方法，仅满足组合接口）---

func (f *fakeSchedulePlanRepo) ListSlotsByPlan(_ context.Context, planID int64) ([]schedule.ScheduleSlot, error) {
	return nil, schedule.ErrPlanNotFound
}

func (f *fakeSchedulePlanRepo) CreateSlot(_ context.Context, slot schedule.ScheduleSlot, _ time.Time) (*schedule.ScheduleSlot, error) {
	return nil, schedule.ErrInvalidWorkPlan
}

func (f *fakeSchedulePlanRepo) UpdateSlotMaximum(_ context.Context, slotID int64, maximum int16, _ time.Time) (*schedule.ScheduleSlot, error) {
	return nil, schedule.ErrSlotNotFound
}

func (f *fakeSchedulePlanRepo) DeleteSlot(_ context.Context, slotID int64, _ time.Time) error {
	return schedule.ErrSlotNotFound
}
