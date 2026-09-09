package schedule

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"Medical-Web-Backend/internal/domain/schedule"
	"Medical-Web-Backend/internal/port"
)

// fakePlanRepository 是内存版 PlanRepository：事务方法直接操作同一份内存状态，
// 用于 service 单元测试（不连真实数据库）。
type fakePlanRepository struct {
	mu      sync.Mutex
	nextID  int64
	plans   map[int64]*schedule.WorkPlan
	regs    map[int64]bool  // 以 planID 记录是否已有挂号（模拟 medical_registration EXISTS）
	doctors map[int64]int16 // doctorID -> status（1=ACTIVE）
	subs    map[int64]bool  // subdepartmentID -> 存在
	links   map[string]bool // doctorID:subID -> 有关联
	// 记录最近一次 ListPlans 参数，便于断言过滤/分页透传。
	lastFilter schedule.PlanFilter
	lastOffset int
	lastLimit  int
	// createLocks 记录每次 CreatePlan 事务加锁的创建键（doctorID|subID|date）。
	createLocks []string
}

func newFakePlanRepository() *fakePlanRepository {
	return &fakePlanRepository{
		plans:   make(map[int64]*schedule.WorkPlan),
		regs:    make(map[int64]bool),
		doctors: make(map[int64]int16),
		subs:    make(map[int64]bool),
		links:   make(map[string]bool),
	}
}

// addDoctorActive 注册一个 ACTIVE 医生并关联子科室（模拟真实表数据）。
func (f *fakePlanRepository) addDoctorActive(doctorID int64, subIDs ...int64) {
	f.doctors[doctorID] = 1
	for _, sub := range subIDs {
		f.subs[sub] = true
		f.links[linkKey(doctorID, sub)] = true
	}
}

func linkKey(doctorID, subID int64) string { return "doc:" + itoa(doctorID) + ":sub:" + itoa(subID) }

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	digits := make([]byte, 0, 20)
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

func (f *fakePlanRepository) ListPlans(_ context.Context, filter schedule.PlanFilter, offset, limit int) ([]schedule.WorkPlan, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	f.lastOffset = offset
	f.lastLimit = limit
	items := make([]schedule.WorkPlan, 0)
	for _, p := range f.plans {
		if !matchPlan(p, filter) {
			continue
		}
		cp := *p
		cp.Recalculate()
		if filter.IncludeSlots {
			cp.Slots = append([]schedule.ScheduleSlot(nil), p.Slots...)
			for i := range cp.Slots {
				cp.Slots[i].Recalculate()
			}
		}
		items = append(items, cp)
	}
	// 按 id 升序（fake 只保证可断言性，排序语义由真实 SQL 承担）。
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].ID < items[j-1].ID; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	total := int64(len(items))
	if offset >= len(items) {
		return make([]schedule.WorkPlan, 0), total, nil
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	return items[offset:end], total, nil
}

func matchPlan(p *schedule.WorkPlan, f schedule.PlanFilter) bool {
	if f.DoctorID != nil && p.DoctorID != *f.DoctorID {
		return false
	}
	if f.SubdepartmentID != nil && p.SubdepartmentID != *f.SubdepartmentID {
		return false
	}
	// departmentId 过滤：存在某关联使 ds.dept_id 命中（fake 简化：无 dept 数据时按不过滤处理）。
	if f.DepartmentID != nil {
		matched := false
		for key := range map[string]bool{} {
			_ = key
		}
		// fake 未模拟科室维度，这里仅保证参数被记录、不过滤数据。
		_ = matched
	}
	if f.FromDate != "" && p.Date < f.FromDate {
		return false
	}
	if f.ToDate != "" && p.Date > f.ToDate {
		return false
	}
	return true
}

func (f *fakePlanRepository) FindPlan(_ context.Context, planID int64) (*schedule.WorkPlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.plans[planID]; ok {
		cp := *p
		cp.Slots = append([]schedule.ScheduleSlot(nil), p.Slots...)
		return &cp, nil
	}
	return nil, sql.ErrNoRows
}

func (f *fakePlanRepository) ExecTx(_ context.Context, fn func(tx port.ScheduleTx) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fn(f)
}

// --- ScheduleTx 实现（操作 fake 自身内存状态）---

func (f *fakePlanRepository) TxInsertPlan(_ context.Context, p schedule.WorkPlan) (int64, error) {
	if f.hasPlan(p.DoctorID, p.SubdepartmentID, p.Date) {
		return 0, schedule.ErrPlanExists
	}
	f.nextID++
	p.ID = f.nextID
	cp := p
	f.plans[p.ID] = &cp
	return p.ID, nil
}

func (f *fakePlanRepository) TxHasPlan(_ context.Context, doctorID, subID int64, date string) (bool, error) {
	return f.hasPlan(doctorID, subID, date), nil
}

// TxLockPlanCreate 内存 fake 的 ExecTx 已整体加互斥锁串行，无需真实咨询锁；仍记录创建键
// 以便断言 service 在事务内先执行加锁（顺序正确性）。
func (f *fakePlanRepository) TxLockPlanCreate(_ context.Context, doctorID, subID int64, date string) error {
	f.createLocks = append(f.createLocks, fmt.Sprintf("%d|%d|%s", doctorID, subID, date))
	return nil
}

func (f *fakePlanRepository) hasPlan(doctorID, subID int64, date string) bool {
	for _, p := range f.plans {
		if p.DoctorID == doctorID && p.SubdepartmentID == subID && p.Date == date {
			return true
		}
	}
	return false
}

func (f *fakePlanRepository) TxFindPlanLocked(_ context.Context, planID int64) (*schedule.WorkPlan, error) {
	if p, ok := f.plans[planID]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, sql.ErrNoRows
}

func (f *fakePlanRepository) TxUpdatePlanMaximum(_ context.Context, planID int64, maximum int16) error {
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

func (f *fakePlanRepository) TxHasRegistrations(_ context.Context, planID int64) (bool, error) {
	return f.regs[planID], nil
}

func (f *fakePlanRepository) TxDeletePlan(_ context.Context, planID int64) error {
	delete(f.regs, planID)
	delete(f.plans, planID)
	return nil
}

// TxDoctorAssociation 三态返回：医生 ACTIVE、子科室存在、存在关联，分别独立可判。
func (f *fakePlanRepository) TxDoctorAssociation(_ context.Context, doctorID, subID int64) (bool, bool, bool, error) {
	doctorActive := f.doctors[doctorID] == 1
	return doctorActive, f.subs[subID], doctorActive && f.subs[subID] && f.links[linkKey(doctorID, subID)], nil
}

// fakeClock 固定“业务当前日期”，便于构造过去/未来日期场景。
func fakeClock() time.Time {
	return time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
}

func newTestService(repo port.ScheduleRepository) *Service {
	return NewService(repo, fakeClock)
}

func TestServiceListPlansDefaultsAndPagination(t *testing.T) {
	repo := newFakePlanRepository()
	for i := 1; i <= 3; i++ {
		repo.addDoctorActive(1, 2)
		date := "2026-09-2" + itoa(int64(i))
		if _, err := repo.TxInsertPlan(context.Background(), schedule.WorkPlan{DoctorID: 1, SubdepartmentID: 2, Date: date, Maximum: 45}); err != nil {
			t.Fatal(err)
		}
	}
	svc := newTestService(repo)
	items, total, err := svc.ListPlans(context.Background(), schedule.PlanFilter{}, 1, 20)
	if err != nil {
		t.Fatalf("ListPlans: %v", err)
	}
	if total != 3 || len(items) != 3 {
		t.Fatalf("total/items = %d/%d, want 3/3", total, len(items))
	}
	if repo.lastOffset != 0 || repo.lastLimit != 20 {
		t.Errorf("offset/limit = %d/%d, want 0/20", repo.lastOffset, repo.lastLimit)
	}
	// 第二页 limit=1 从偏移 1 开始。
	if _, _, err := svc.ListPlans(context.Background(), schedule.PlanFilter{}, 2, 1); err != nil {
		t.Fatal(err)
	}
	if repo.lastOffset != 1 || repo.lastLimit != 1 {
		t.Errorf("page2 offset/limit = %d/%d, want 1/1", repo.lastOffset, repo.lastLimit)
	}
	// 空结果必须返回非 nil 切片（items:[]）。
	empty, count, err := svc.ListPlans(context.Background(), schedule.PlanFilter{DoctorID: int64ptr(999)}, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if empty == nil || len(empty) != 0 || count != 0 {
		t.Errorf("空列表应为空切片：len=%d total=%d", len(empty), count)
	}
}

func int64ptr(v int64) *int64 { return &v }

func TestServiceCreatePlanSuccess(t *testing.T) {
	repo := newFakePlanRepository()
	repo.addDoctorActive(16, 2)
	svc := newTestService(repo)
	result, err := svc.CreatePlan(context.Background(), schedule.WorkPlan{DoctorID: 16, SubdepartmentID: 2, Date: "2026-09-20", Maximum: 45})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if result.NewID < 1 {
		t.Fatalf("NewID = %d, want > 0", result.NewID)
	}
	plan := repo.plans[result.NewID]
	if plan == nil || plan.Maximum != 45 || plan.Used != 0 || plan.Date != "2026-09-20" {
		t.Errorf("插入计划状态错误: %+v", plan)
	}
}

func TestServiceCreatePlanRejectsDuplicate(t *testing.T) {
	repo := newFakePlanRepository()
	repo.addDoctorActive(16, 2)
	svc := newTestService(repo)
	body := schedule.WorkPlan{DoctorID: 16, SubdepartmentID: 2, Date: "2026-09-20", Maximum: 45}
	if _, err := svc.CreatePlan(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CreatePlan(context.Background(), body)
	if !isServiceError(err, CodePlanExists) {
		t.Fatalf("重复创建应返回 %s，got %v", CodePlanExists, err)
	}
}

// TestServiceCreatePlanTakesCreateLockFirst 验证 CreatePlan 事务内最先对“医生/子科室/日期”
// 创建键加咨询锁（在查重之前），保证并发创建在 READ COMMITTED 下也不会产生重复行。
func TestServiceCreatePlanTakesCreateLockFirst(t *testing.T) {
	repo := newFakePlanRepository()
	repo.addDoctorActive(16, 2)
	svc := newTestService(repo)
	body := schedule.WorkPlan{DoctorID: 16, SubdepartmentID: 2, Date: "2026-09-20", Maximum: 45}
	if _, err := svc.CreatePlan(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	want := "16|2|2026-09-20"
	if len(repo.createLocks) != 1 || repo.createLocks[0] != want {
		t.Fatalf("createLocks = %v, want 首次加锁 %s", repo.createLocks, want)
	}
	// 重复创建同样先加锁再查重返回 409，锁只加一次、校验顺序不变。
	if _, err := svc.CreatePlan(context.Background(), body); !isServiceError(err, CodePlanExists) {
		t.Fatalf("重复创建应返回 %s，got %v", CodePlanExists, err)
	}
	if len(repo.createLocks) != 2 || repo.createLocks[1] != want {
		t.Errorf("createLocks = %v, want 两次 %s", repo.createLocks, want)
	}
}

// TestMapServiceErrorDomainFallbacks 验证数据库层防御性兜底错误（未来唯一索引冲突/重读时
// used 超限）映射为契约 409 编码，而不是落入 500。
func TestMapServiceErrorDomainFallbacks(t *testing.T) {
	cases := []struct {
		name    string
		wrapped error
		code    string
		message string
	}{
		{"ErrPlanExists", fmt.Errorf("unique violated: %w", schedule.ErrPlanExists), CodePlanExists, "排班计划已存在"},
		{"ErrMaximumBelowUsed", fmt.Errorf("recheck below used: %w", schedule.ErrMaximumBelowUsed), CodePlanConflict, "最大号源不能小于已使用号源"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mapServiceError(tc.wrapped)
			var serviceErr *ServiceError
			if !errors.As(err, &serviceErr) {
				t.Fatalf("期望映射为 ServiceError，got %v", err)
			}
			if serviceErr.Code != tc.code || serviceErr.Message != tc.message {
				t.Errorf("code/message = %s/%s, want %s/%s", serviceErr.Code, serviceErr.Message, tc.code, tc.message)
			}
		})
	}
}

func TestServiceCreatePlanAssociationValidation(t *testing.T) {
	repo := newFakePlanRepository()
	repo.addDoctorActive(16, 2)
	svc := newTestService(repo)
	body := schedule.WorkPlan{DoctorID: 16, SubdepartmentID: 99, Date: "2026-09-20", Maximum: 45}
	_, err := svc.CreatePlan(context.Background(), body)
	if !isServiceError(err, CodeValidationFailed) {
		t.Fatalf("子科室不存在应 422，got %v", err)
	}
	// 医生不存在（此时 99 号子科室已存在）。
	repo.subs[99] = true
	_, err = svc.CreatePlan(context.Background(), schedule.WorkPlan{DoctorID: 999, SubdepartmentID: 99, Date: "2026-09-20", Maximum: 45})
	if err == nil || err.Error() != "医生不存在或未在出诊状态" {
		t.Fatalf("医生不存在应返回指定文案，got %v", err)
	}
	// 医生存在且 ACTIVE、子科室存在，但没有关联记录。
	_, err = svc.CreatePlan(context.Background(), schedule.WorkPlan{DoctorID: 16, SubdepartmentID: 99, Date: "2026-09-20", Maximum: 45})
	if err == nil || err.Error() != "医生未关联该子科室" {
		t.Fatalf("未关联应返回指定文案，got %v", err)
	}
}

func TestServiceCreatePlanPastDateAndInvalidMaximum(t *testing.T) {
	repo := newFakePlanRepository()
	repo.addDoctorActive(16, 2)
	svc := newTestService(repo)
	if _, err := svc.CreatePlan(context.Background(), schedule.WorkPlan{DoctorID: 16, SubdepartmentID: 2, Date: "2026-09-08", Maximum: 45}); err == nil || err.Error() != "排班日期不得早于业务当前日期" {
		t.Errorf("过去日期应 422，got %v", err)
	}
	if _, err := svc.CreatePlan(context.Background(), schedule.WorkPlan{DoctorID: 16, SubdepartmentID: 2, Date: "2026-09-20", Maximum: 0}); err == nil || err.Error() != "最大号源必须大于 0" {
		t.Errorf("maximum=0 应 422，got %v", err)
	}
}

func TestServiceUpdatePlanMaximumAndLock(t *testing.T) {
	repo := newFakePlanRepository()
	repo.addDoctorActive(16, 2)
	svc := newTestService(repo)
	created, err := svc.CreatePlan(context.Background(), schedule.WorkPlan{DoctorID: 16, SubdepartmentID: 2, Date: "2026-09-20", Maximum: 45})
	if err != nil {
		t.Fatal(err)
	}
	plan := repo.plans[created.NewID]
	plan.Used = 3 // 模拟已挂号 3 人
	// 正常更新。
	result, err := svc.UpdatePlan(context.Background(), created.NewID, 60)
	if err != nil {
		t.Fatalf("UpdatePlan: %v", err)
	}
	if result.Plan == nil || result.Plan.Maximum != 60 || result.Plan.Remaining != 57 {
		t.Errorf("更新后 maximum/remaining = %d/%d, want 60/57", result.Plan.Maximum, result.Plan.Remaining)
	}
	// 新值小于 used -> 409 + used details。
	_, err = svc.UpdatePlan(context.Background(), created.NewID, 2)
	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) || serviceErr.Code != CodePlanConflict {
		t.Fatalf("期望 SCHEDULE_CONFLICT，got %v", err)
	}
	if serviceErr.Details["used"] != int16(3) {
		t.Errorf("details.used = %v, want 3", serviceErr.Details["used"])
	}
	// 已开始（当天）的计划锁定。
	past, _ := svc.CreatePlan(context.Background(), schedule.WorkPlan{DoctorID: 16, SubdepartmentID: 2, Date: "2026-09-09", Maximum: 45})
	_ = past
	// 直接构造当天计划并更新应被锁定。
	repo.plans[999] = &schedule.WorkPlan{ID: 999, DoctorID: 16, SubdepartmentID: 2, Date: "2026-09-09", Maximum: 45}
	_, err = svc.UpdatePlan(context.Background(), 999, 50)
	if !isServiceError(err, CodePlanLocked) {
		t.Fatalf("当天计划应锁定，got %v", err)
	}
}

func TestServiceUpdatePlanNotFound(t *testing.T) {
	repo := newFakePlanRepository()
	repo.addDoctorActive(16, 2)
	svc := newTestService(repo)
	_, err := svc.UpdatePlan(context.Background(), 424242, 60)
	if !isServiceError(err, CodePlanNotFound) {
		t.Fatalf("不存在应 404，got %v", err)
	}
}

func TestServiceDeletePlanGuards(t *testing.T) {
	repo := newFakePlanRepository()
	repo.addDoctorActive(16, 2)
	svc := newTestService(repo)
	created, err := svc.CreatePlan(context.Background(), schedule.WorkPlan{DoctorID: 16, SubdepartmentID: 2, Date: "2026-09-20", Maximum: 45})
	if err != nil {
		t.Fatal(err)
	}
	id := created.NewID
	// 干净计划可删除。
	if err := svc.DeletePlan(context.Background(), id); err != nil {
		t.Fatalf("干净计划删除失败: %v", err)
	}
	if _, ok := repo.plans[id]; ok {
		t.Fatalf("计划 %d 应已物理删除", id)
	}
	// 有挂号记录的计划不可删除。
	created2, _ := svc.CreatePlan(context.Background(), schedule.WorkPlan{DoctorID: 16, SubdepartmentID: 2, Date: "2026-09-21", Maximum: 45})
	repo.regs[created2.NewID] = true
	err = svc.DeletePlan(context.Background(), created2.NewID)
	if !isServiceError(err, CodeHasRegistrations) {
		t.Fatalf("有挂号应 409，got %v", err)
	}
	// 计划 used>0（挂号数）也不可删除。
	created3, _ := svc.CreatePlan(context.Background(), schedule.WorkPlan{DoctorID: 16, SubdepartmentID: 2, Date: "2026-09-22", Maximum: 45})
	repo.plans[created3.NewID].Used = 1
	if err := svc.DeletePlan(context.Background(), created3.NewID); !isServiceError(err, CodeHasRegistrations) {
		t.Fatalf("used>0 应 409，got %v", err)
	}
	// 已开始计划不可删除。
	repo.plans[888] = &schedule.WorkPlan{ID: 888, DoctorID: 16, SubdepartmentID: 2, Date: "2026-09-09", Maximum: 45}
	if err := svc.DeletePlan(context.Background(), 888); !isServiceError(err, CodePlanLocked) {
		t.Fatalf("当天计划删除应锁定，got %v", err)
	}
	// 不存在 -> 404。
	if err := svc.DeletePlan(context.Background(), 555); !isServiceError(err, CodePlanNotFound) {
		t.Fatalf("不存在计划应 404，got %v", err)
	}
}

// isServiceError 断言错误是给定编码的 ServiceError。
func isServiceError(err error, code string) bool {
	var serviceErr *ServiceError
	return errors.As(err, &serviceErr) && serviceErr.Code == code
}

// memorySlotRepo 是 Service 单测使用的内存实现，方法集与 port.ScheduleSlotRepository 对齐。
// 写操作依据计划日期相对调用时传入的 now 判断是否允许，从而验证 Service 注入 now 是否传递到仓库层。
type memorySlotRepo struct {
	plans  map[int64]string        // planID -> 计划日期（YYYY-MM-DD）
	slots  []schedule.ScheduleSlot // 已持久化的时段记录
	nextID int64                   // 自增 ID，模拟数据库 sequence
	// lastNow 记录最近一次写操作收到的业务时间，供断言 now 注入使用。
	lastNow time.Time
}

// newMemorySlotRepo 构造空的排班时段内存仓库。
func newMemorySlotRepo() *memorySlotRepo {
	return &memorySlotRepo{plans: map[int64]string{}}
}

// ListSlotsByPlan 返回某计划全部时段并按 slot 升序（模拟 SQL ORDER BY slot, id）；
// 计划不存在时返回 ErrPlanNotFound（存在性判定与列表读取同语句完成）。
// 不在此处计算 remaining，交给 Service 统一重算以验证 usecase 行为。
func (s *memorySlotRepo) ListSlotsByPlan(_ context.Context, planID int64) ([]schedule.ScheduleSlot, error) {
	if _, ok := s.plans[planID]; !ok {
		return nil, schedule.ErrPlanNotFound
	}
	result := make([]schedule.ScheduleSlot, 0)
	for _, item := range s.slots {
		if item.WorkPlanID == planID {
			result = append(result, item)
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

// CreateSlot 创建时段：计划不存在/计划已开始/时段重复时返回对应领域错误。
func (s *memorySlotRepo) CreateSlot(_ context.Context, value schedule.ScheduleSlot, now time.Time) (*schedule.ScheduleSlot, error) {
	s.lastNow = now
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
	created := schedule.ScheduleSlot{ID: s.nextID, WorkPlanID: value.WorkPlanID, Slot: value.Slot, Maximum: value.Maximum}
	created.Recalculate()
	s.slots = append(s.slots, created)
	result := created
	return &result, nil
}

// UpdateSlotMaximum 更新时段容量：时段不存在或计划已开始时返回对应错误。
func (s *memorySlotRepo) UpdateSlotMaximum(_ context.Context, slotID int64, maximum int16, now time.Time) (*schedule.ScheduleSlot, error) {
	s.lastNow = now
	for i := range s.slots {
		if s.slots[i].ID != slotID {
			continue
		}
		date, ok := s.plans[s.slots[i].WorkPlanID]
		if !ok {
			return nil, schedule.ErrPlanNotFound
		}
		if !(schedule.WorkPlan{Date: date}).CanModify(now) {
			return nil, schedule.ErrSlotLocked
		}
		if err := schedule.ValidateMaximum(maximum, s.slots[i].Used); err != nil {
			return nil, err
		}
		s.slots[i].Maximum = maximum
		s.slots[i].Recalculate()
		result := s.slots[i]
		return &result, nil
	}
	return nil, schedule.ErrSlotNotFound
}

// DeleteSlot 删除时段：时段不存在或计划已开始时返回对应错误。
func (s *memorySlotRepo) DeleteSlot(_ context.Context, slotID int64, now time.Time) error {
	s.lastNow = now
	for i := range s.slots {
		if s.slots[i].ID != slotID {
			continue
		}
		date, ok := s.plans[s.slots[i].WorkPlanID]
		if !ok {
			return schedule.ErrPlanNotFound
		}
		if !(schedule.WorkPlan{Date: date}).CanModify(now) {
			return schedule.ErrSlotLocked
		}
		s.slots = append(s.slots[:i], s.slots[i+1:]...)
		return nil
	}
	return schedule.ErrSlotNotFound
}

// fixedUTCNow 返回单测统一的固定业务时间（2026-09-09 00:00 UTC）。
func fixedUTCNow() time.Time {
	return time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
}

// TestServiceListSlotsByPlanOK 计划存在时返回按 slot 升序且已计算 remaining 的时段列表。
func TestServiceListSlotsByPlanOK(t *testing.T) {
	repo := newMemorySlotRepo()
	repo.plans[4] = "2026-09-20"
	// 先插入 slot=2 再插入 slot=1，验证列表仍按 slot 升序输出。
	repo.slots = append(repo.slots,
		schedule.ScheduleSlot{ID: 13, WorkPlanID: 4, Slot: 2, Maximum: 3},
		schedule.ScheduleSlot{ID: 12, WorkPlanID: 4, Slot: 1, Maximum: 3, Used: 1},
	)
	svc := NewService(repo, fixedUTCNow)

	items, err := svc.ListSlotsByPlan(context.Background(), 4)
	if err != nil {
		t.Fatalf("ListSlotsByPlan error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len = %d, want 2", len(items))
	}
	if items[0].Slot != 1 || items[0].Remaining != 2 || items[0].Used != 1 {
		t.Errorf("first = %+v, want slot=1 remaining=2 used=1", items[0])
	}
	if items[1].Slot != 2 || items[1].Remaining != 3 {
		t.Errorf("second = %+v, want slot=2 remaining=3", items[1])
	}
}

// TestServiceListSlotsByPlanEmptyPlanOK 计划存在但尚无时段时返回空切片而非 nil（对应 200 []）。
func TestServiceListSlotsByPlanEmptyPlanOK(t *testing.T) {
	repo := newMemorySlotRepo()
	repo.plans[4] = "2026-09-20"
	svc := NewService(repo, fixedUTCNow)

	items, err := svc.ListSlotsByPlan(context.Background(), 4)
	if err != nil {
		t.Fatalf("ListSlotsByPlan error: %v", err)
	}
	if items == nil || len(items) != 0 {
		t.Fatalf("items = %v, want 空切片 []", items)
	}
}

// TestServiceListSlotsByPlanNotFound 计划不存在时返回 ErrPlanNotFound（对应 404）。
func TestServiceListSlotsByPlanNotFound(t *testing.T) {
	svc := NewService(newMemorySlotRepo(), fixedUTCNow)

	items, err := svc.ListSlotsByPlan(context.Background(), 999)
	if !errors.Is(err, schedule.ErrPlanNotFound) {
		t.Fatalf("err = %v, want ErrPlanNotFound", err)
	}
	if items != nil {
		t.Errorf("items = %v, want nil", items)
	}
}

// TestServiceListSlotsByPlanInvalidID 计划 ID 非正数时在 usecase 层直接拒绝，不触碰仓库。
func TestServiceListSlotsByPlanInvalidID(t *testing.T) {
	repo := newMemorySlotRepo()
	repo.plans[4] = "2026-09-20"
	svc := NewService(repo, fixedUTCNow)

	if _, err := svc.ListSlotsByPlan(context.Background(), 0); !errors.Is(err, schedule.ErrInvalidWorkPlan) {
		t.Fatalf("err = %v, want ErrInvalidWorkPlan", err)
	}
}

// TestServiceCreateSlotFuturePlanOK now 注入后，未来计划可正常创建时段；
// 同时断言仓库收到的 now 与注入值一致，证明 Service 正确透传业务时间。
func TestServiceCreateSlotFuturePlanOK(t *testing.T) {
	repo := newMemorySlotRepo()
	repo.plans[1] = "2026-09-20"
	svc := NewService(repo, fixedUTCNow)

	created, err := svc.CreateSlot(context.Background(), schedule.ScheduleSlot{WorkPlanID: 1, Slot: 1, Maximum: 3})
	if err != nil {
		t.Fatalf("CreateSlot error: %v", err)
	}
	if created == nil || created.ID != 1 || created.Maximum != 3 || created.Remaining != 3 {
		t.Fatalf("created = %+v, want id=1 maximum=3 remaining=3", created)
	}
	if !repo.lastNow.Equal(fixedUTCNow()) {
		t.Errorf("repo.now = %v, want %v（验证 Service now 注入）", repo.lastNow, fixedUTCNow())
	}
	// 同计划同 slot 再次创建应返回 ErrSlotExists。
	if _, err := svc.CreateSlot(context.Background(), schedule.ScheduleSlot{WorkPlanID: 1, Slot: 1, Maximum: 3}); !errors.Is(err, schedule.ErrSlotExists) {
		t.Errorf("duplicate err = %v, want ErrSlotExists", err)
	}
}

// TestServiceCreateSlotStartedPlanLocked 计划已开始（日期为 now 当天）时返回 ErrSlotLocked。
func TestServiceCreateSlotStartedPlanLocked(t *testing.T) {
	repo := newMemorySlotRepo()
	repo.plans[1] = "2026-09-09" // 与固定 now 同一天，视为已开始。
	svc := NewService(repo, fixedUTCNow)

	_, err := svc.CreateSlot(context.Background(), schedule.ScheduleSlot{WorkPlanID: 1, Slot: 1, Maximum: 3})
	if !errors.Is(err, schedule.ErrSlotLocked) {
		t.Fatalf("err = %v, want ErrSlotLocked", err)
	}
}

// TestServiceCreateSlotPlanNotFound 计划不存在时返回 ErrPlanNotFound。
func TestServiceCreateSlotPlanNotFound(t *testing.T) {
	svc := NewService(newMemorySlotRepo(), fixedUTCNow)

	_, err := svc.CreateSlot(context.Background(), schedule.ScheduleSlot{WorkPlanID: 99, Slot: 1, Maximum: 3})
	if !errors.Is(err, schedule.ErrPlanNotFound) {
		t.Fatalf("err = %v, want ErrPlanNotFound", err)
	}
}

// TestServiceUpdateAndDeleteRejectInvalidSlotID 非正数 slotID 在 usecase 层直接拒绝。
func TestServiceUpdateAndDeleteRejectInvalidSlotID(t *testing.T) {
	svc := NewService(newMemorySlotRepo(), fixedUTCNow)

	if _, err := svc.UpdateSlotMaximum(context.Background(), 0, 5); !errors.Is(err, schedule.ErrInvalidSlot) {
		t.Errorf("update err = %v, want ErrInvalidSlot", err)
	}
	if err := svc.DeleteSlot(context.Background(), 0); !errors.Is(err, schedule.ErrInvalidSlot) {
		t.Errorf("delete err = %v, want ErrInvalidSlot", err)
	}
}

// TestServiceUpdateSlotMaximumOK 更新成功后返回新容量，并透传注入的 now。
func TestServiceUpdateSlotMaximumOK(t *testing.T) {
	repo := newMemorySlotRepo()
	repo.plans[1] = "2026-09-20"
	repo.slots = append(repo.slots, schedule.ScheduleSlot{ID: 12, WorkPlanID: 1, Slot: 1, Maximum: 3})
	svc := NewService(repo, fixedUTCNow)

	updated, err := svc.UpdateSlotMaximum(context.Background(), 12, 5)
	if err != nil {
		t.Fatalf("UpdateSlotMaximum error: %v", err)
	}
	if updated == nil || updated.Maximum != 5 || updated.Remaining != 5 {
		t.Fatalf("updated = %+v, want maximum=5 remaining=5", updated)
	}
	if !repo.lastNow.Equal(fixedUTCNow()) {
		t.Errorf("repo.now = %v, want %v", repo.lastNow, fixedUTCNow())
	}
}

// TestServiceDeleteSlotOK 删除未来计划下的时段成功返回 nil。
func TestServiceDeleteSlotOK(t *testing.T) {
	repo := newMemorySlotRepo()
	repo.plans[1] = "2026-09-20"
	repo.slots = append(repo.slots, schedule.ScheduleSlot{ID: 12, WorkPlanID: 1, Slot: 1, Maximum: 3})
	svc := NewService(repo, fixedUTCNow)

	if err := svc.DeleteSlot(context.Background(), 12); err != nil {
		t.Fatalf("DeleteSlot error: %v", err)
	}
	if len(repo.slots) != 0 {
		t.Errorf("slot not removed: %v", repo.slots)
	}
	if !repo.lastNow.Equal(fixedUTCNow()) {
		t.Errorf("repo.now = %v, want %v", repo.lastNow, fixedUTCNow())
	}
}

// TestServiceDefaultNowUsesUTC now 为 nil 时 Service 应使用当前 UTC 时间。
func TestServiceDefaultNowUsesUTC(t *testing.T) {
	repo := newMemorySlotRepo()
	repo.plans[1] = "2030-01-01" // 远期计划，无论默认 now 取何值都可创建。
	svc := NewService(repo, nil)

	if _, err := svc.CreateSlot(context.Background(), schedule.ScheduleSlot{WorkPlanID: 1, Slot: 1, Maximum: 3}); err != nil {
		t.Fatalf("CreateSlot error: %v", err)
	}
	if repo.lastNow.Location() != time.UTC {
		t.Errorf("default now location = %v, want UTC", repo.lastNow.Location())
	}
	// 允许少量时钟偏差，避免测试在边界时间片抖动。
	if delta := time.Since(repo.lastNow); delta > 2*time.Minute || delta < -2*time.Minute {
		t.Errorf("default now = %v, 与当前时间偏差过大 delta=%v", repo.lastNow, delta)
	}
}

// --- ScheduleSlotRepository 适配桩（plans 单测不调用时段方法，仅满足组合接口）---
func (f *fakePlanRepository) ListSlotsByPlan(_ context.Context, planID int64) ([]schedule.ScheduleSlot, error) {
	return nil, schedule.ErrPlanNotFound
}
func (f *fakePlanRepository) CreateSlot(_ context.Context, slot schedule.ScheduleSlot, _ time.Time) (*schedule.ScheduleSlot, error) {
	return nil, schedule.ErrInvalidWorkPlan
}
func (f *fakePlanRepository) UpdateSlotMaximum(_ context.Context, slotID int64, maximum int16, _ time.Time) (*schedule.ScheduleSlot, error) {
	return nil, schedule.ErrSlotNotFound
}
func (f *fakePlanRepository) DeleteSlot(_ context.Context, slotID int64, _ time.Time) error {
	return schedule.ErrSlotNotFound
}

// --- PlanRepository 适配桩（slots 单测不调用计划方法，仅满足组合接口）---
func (s *memorySlotRepo) ListPlans(_ context.Context, f schedule.PlanFilter, _, _ int) ([]schedule.WorkPlan, int64, error) {
	return []schedule.WorkPlan{}, 0, nil
}
func (s *memorySlotRepo) FindPlan(_ context.Context, planID int64) (*schedule.WorkPlan, error) {
	return nil, sql.ErrNoRows
}
func (s *memorySlotRepo) ExecTx(_ context.Context, fn func(tx port.ScheduleTx) error) error {
	return errors.New("not implemented in memorySlotRepo")
}
