package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/domain/schedule"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/middleware"
	misuser "Medical-Web-Backend/internal/usecase/misuser"
	scheduleservice "Medical-Web-Backend/internal/usecase/schedule"
)

// scheduleSlotTestNow 是排班时段 HTTP 契约测试使用的固定业务时间（2026-09-09 UTC，
// 即上海时间 2026-09-09 08:00，业务日 09-09）。计划日期选未来 2026-09-20 允许写，
// 日期 2026-09-09（当天）用于模拟已开始计划。
var scheduleSlotTestNow = time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)

// memIdempotencyStore 是 port.IdempotencyStore 的内存 fake：
// Claim 等价 SET NX（键已存在返回 false），Save/Load 保存并读取可重放结果，
// Release 删除占位。handler 用 sha256(userId|fullPath|key) 生成键，天然按用户隔离。
type memIdempotencyStore struct {
	mu           sync.Mutex
	placeholders map[string]bool
	records      map[string]port.IdempotencyRecord

	claimErr error
	saveErr  error
	loadErr  error

	claimCalls   int
	saveCalls    int
	loadCalls    int
	releaseCalls int
}

func newMemIdempotencyStore() *memIdempotencyStore {
	return &memIdempotencyStore{
		placeholders: map[string]bool{},
		records:      map[string]port.IdempotencyRecord{},
	}
}

// Claim 原子占位：键已存在（在途或已完成）返回 false。
func (m *memIdempotencyStore) Claim(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claimCalls++
	if m.claimErr != nil {
		return false, m.claimErr
	}
	if m.placeholders[key] {
		return false, nil
	}
	m.placeholders[key] = true
	return true, nil
}

// Save 保存首次执行的可重放结果。
func (m *memIdempotencyStore) Save(_ context.Context, key string, record port.IdempotencyRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveCalls++
	if m.saveErr != nil {
		return m.saveErr
	}
	m.records[key] = record
	return nil
}

// Load 读取已保存结果；无记录返回 (nil, nil)。
func (m *memIdempotencyStore) Load(_ context.Context, key string) (*port.IdempotencyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadCalls++
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	record, ok := m.records[key]
	if !ok {
		return nil, nil
	}
	return &record, nil
}

// Release 释放占位（5xx / Save 失败等未落地可重放结果的场景）。
func (m *memIdempotencyStore) Release(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseCalls++
	delete(m.placeholders, key)
	delete(m.records, key)
	return nil
}

// slotRepoStub 是 port.ScheduleSlotRepository 的内存实现，行为对齐 Postgres 实现：
// ListSlotsByPlan 在计划缺失时返回 ErrPlanNotFound；创建前校验计划存在与日期未到；
// 更新时校验挂号与容量下限；删除时校验挂号与计划状态。
type slotRepoStub struct {
	plans       map[int64]string        // planID -> 计划日期（YYYY-MM-DD）
	slots       []schedule.ScheduleSlot // 时段持久化数据（不含 remaining）
	nextID      int64                   // 自增 ID，模拟数据库 sequence
	registered  map[int64]bool          // 时段是否已产生挂号记录（medical_registration 行）
	createCalls int                     // CreateSlot 调用次数，用于幂等重放断言
}

// addPlan 预置一个排班计划，date 形如 2026-09-20。
func (s *slotRepoStub) addPlan(id int64, date string) {
	if s.plans == nil {
		s.plans = map[int64]string{}
	}
	s.plans[id] = date
}

// addSlot 预置一个时段记录（ID 由测试显式给出，便于构造路径参数）。
func (s *slotRepoStub) addSlot(slot schedule.ScheduleSlot) {
	s.slots = append(s.slots, slot)
}

// markRegistered 标记某时段已有挂号记录（对应 medical_registration 存在关联行）。
func (s *slotRepoStub) markRegistered(slotID int64) {
	if s.registered == nil {
		s.registered = map[int64]bool{}
	}
	s.registered[slotID] = true
}

// ListSlotsByPlan 返回某计划全部时段并按 slot 升序；计划不存在返回 ErrPlanNotFound，
// 计划存在但无时段返回空切片（对应 200 []）。
func (s *slotRepoStub) ListSlotsByPlan(_ context.Context, planID int64) ([]schedule.ScheduleSlot, error) {
	if _, ok := s.plans[planID]; !ok {
		return nil, schedule.ErrPlanNotFound
	}
	result := make([]schedule.ScheduleSlot, 0)
	for _, item := range s.slots {
		if item.WorkPlanID == planID {
			copyItem := item
			copyItem.Recalculate()
			result = append(result, copyItem)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Slot != result[j].Slot {
			return result[i].Slot < result[j].Slot
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

// CreateSlot 创建时段：先实体校验，再检查计划存在、计划未开始与时段唯一。
func (s *slotRepoStub) CreateSlot(_ context.Context, value schedule.ScheduleSlot, now time.Time) (*schedule.ScheduleSlot, error) {
	s.createCalls++
	if err := value.ValidateNew(); err != nil {
		return nil, err
	}
	date, ok := s.plans[value.WorkPlanID]
	if !ok {
		return nil, schedule.ErrPlanNotFound
	}
	if !(schedule.WorkPlan{Date: date}).CanModify(now) {
		return nil, schedule.ErrSlotLocked
	}
	for _, item := range s.slots {
		if item.WorkPlanID == value.WorkPlanID && item.Slot == value.Slot {
			return nil, schedule.ErrSlotExists
		}
	}
	s.nextID++
	created := schedule.ScheduleSlot{
		ID:         s.nextID,
		WorkPlanID: value.WorkPlanID,
		Slot:       value.Slot,
		Maximum:    value.Maximum,
		Used:       value.Used,
	}
	created.Recalculate()
	s.slots = append(s.slots, created)
	result := created
	return &result, nil
}

// UpdateSlotMaximum 按 port 注释顺序校验：时段存在 -> 计划未开始 -> 无挂号 ->
// 容量不小于已用。
func (s *slotRepoStub) UpdateSlotMaximum(_ context.Context, slotID int64, maximum int16, now time.Time) (*schedule.ScheduleSlot, error) {
	index := -1
	for i, item := range s.slots {
		if item.ID == slotID {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, schedule.ErrSlotNotFound
	}
	slot := s.slots[index]
	date, ok := s.plans[slot.WorkPlanID]
	if !ok {
		return nil, schedule.ErrPlanNotFound
	}
	if !(schedule.WorkPlan{Date: date}).CanModify(now) {
		return nil, schedule.ErrSlotLocked
	}
	if s.registered[slotID] {
		return nil, schedule.ErrSlotLocked
	}
	if err := schedule.ValidateMaximum(maximum, slot.Used); err != nil {
		if errors.Is(err, schedule.ErrMaximumBelowUsed) {
			return nil, &schedule.MaximumBelowUsedError{Used: slot.Used}
		}
		return nil, err
	}
	slot.Maximum = maximum
	slot.Recalculate()
	s.slots[index] = slot
	result := slot
	return &result, nil
}

// DeleteSlot 物理删除时段：仅无挂号且所属计划尚未开始时允许。
func (s *slotRepoStub) DeleteSlot(_ context.Context, slotID int64, now time.Time) error {
	index := -1
	for i, item := range s.slots {
		if item.ID == slotID {
			index = i
			break
		}
	}
	if index < 0 {
		return schedule.ErrSlotNotFound
	}
	slot := s.slots[index]
	date, ok := s.plans[slot.WorkPlanID]
	if !ok {
		return schedule.ErrPlanNotFound
	}
	if !(schedule.WorkPlan{Date: date}).CanModify(now) {
		return schedule.ErrSlotLocked
	}
	if s.registered[slotID] {
		return schedule.ErrHasRegistrations
	}
	s.slots = append(s.slots[:index], s.slots[index+1:]...)
	return nil
}

// newScheduleSlotEngine 用真实 Service + 内存 repo + 固定 now + 幂等 store fake 构造
// 四个接口路由；userID > 0 时预置 access claims 上下文（handler 从 claims 取 UserID）。
func newScheduleSlotEngine(t *testing.T, stub *slotRepoStub, store *memIdempotencyStore, userID int64) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	service := scheduleservice.NewService(stub, func() time.Time { return scheduleSlotTestNow })
	slotHandler := NewScheduleSlotHandler(service, store)
	e := gin.New()
	if userID > 0 {
		e.Use(func(c *gin.Context) {
			c.Set(middleware.ClaimsKey, &misuser.AccessClaims{UserID: userID})
		})
	}
	e.GET("/api/v1/schedule/plans/:planId/slots", slotHandler.ListSlots)
	e.POST("/api/v1/schedule/plans/:planId/slots", slotHandler.CreateSlot)
	e.PATCH("/api/v1/schedule/slots/:slotId", slotHandler.UpdateSlotMaximum)
	e.DELETE("/api/v1/schedule/slots/:slotId", slotHandler.DeleteSlot)
	return e
}

// scheduleRequest 发起排班接口请求；headers 用于附加 Idempotency-Key 等头。
func scheduleRequest(e *gin.Engine, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

// assertErrorStatus 断言状态码与统一错误体 code，并返回解码后的 body 供进一步断言。
func assertErrorStatus(t *testing.T, w *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, status, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["code"] != code {
		t.Fatalf("code = %v, want %s; body=%s", body["code"], code, w.Body.String())
	}
	return body
}

// decodeBody 解码响应体为 map；响应为空或非 JSON 时以 t.Fatal 中断。
func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if w.Body.Len() == 0 {
		t.Fatalf("response body is empty; status=%d", w.Code)
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON body: %v; body=%s", err, w.Body.String())
	}
	return m
}

// --- PlanRepository 适配桩（slots handler 单测不调用计划方法，仅满足组合接口）---

func (s *slotRepoStub) ListPlans(_ context.Context, f schedule.PlanFilter, _, _ int) ([]schedule.WorkPlan, int64, error) {
	return []schedule.WorkPlan{}, 0, nil
}

func (s *slotRepoStub) FindPlan(_ context.Context, planID int64) (*schedule.WorkPlan, error) {
	return nil, errors.New("not found")
}

func (s *slotRepoStub) ExecTx(_ context.Context, fn func(tx port.ScheduleTx) error) error {
	return errors.New("not implemented")
}
