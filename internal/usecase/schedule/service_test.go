package schedule

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	domainschedule "Medical-Web-Backend/internal/domain/schedule"
)

// memorySlotRepo 是 Service 单测使用的内存实现，方法集与 port.ScheduleSlotRepository 对齐。
// 写操作依据计划日期相对调用时传入的 now 判断是否允许，从而验证 Service 注入 now 是否传递到仓库层。
type memorySlotRepo struct {
	plans  map[int64]string              // planID -> 计划日期（YYYY-MM-DD）
	slots  []domainschedule.ScheduleSlot // 已持久化的时段记录
	nextID int64                         // 自增 ID，模拟数据库 sequence
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
func (s *memorySlotRepo) ListSlotsByPlan(_ context.Context, planID int64) ([]domainschedule.ScheduleSlot, error) {
	if _, ok := s.plans[planID]; !ok {
		return nil, domainschedule.ErrPlanNotFound
	}
	result := make([]domainschedule.ScheduleSlot, 0)
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
func (s *memorySlotRepo) CreateSlot(_ context.Context, value domainschedule.ScheduleSlot, now time.Time) (*domainschedule.ScheduleSlot, error) {
	s.lastNow = now
	date, ok := s.plans[value.WorkPlanID]
	if !ok {
		return nil, domainschedule.ErrPlanNotFound
	}
	if !(domainschedule.WorkPlan{Date: date}).CanModify(now) {
		return nil, domainschedule.ErrSlotLocked
	}
	for _, item := range s.slots {
		if item.WorkPlanID == value.WorkPlanID && item.Slot == value.Slot {
			return nil, domainschedule.ErrSlotExists
		}
	}
	s.nextID++
	created := domainschedule.ScheduleSlot{ID: s.nextID, WorkPlanID: value.WorkPlanID, Slot: value.Slot, Maximum: value.Maximum}
	created.Recalculate()
	s.slots = append(s.slots, created)
	result := created
	return &result, nil
}

// UpdateSlotMaximum 更新时段容量：时段不存在或计划已开始时返回对应错误。
func (s *memorySlotRepo) UpdateSlotMaximum(_ context.Context, slotID int64, maximum int16, now time.Time) (*domainschedule.ScheduleSlot, error) {
	s.lastNow = now
	for i := range s.slots {
		if s.slots[i].ID != slotID {
			continue
		}
		date, ok := s.plans[s.slots[i].WorkPlanID]
		if !ok {
			return nil, domainschedule.ErrPlanNotFound
		}
		if !(domainschedule.WorkPlan{Date: date}).CanModify(now) {
			return nil, domainschedule.ErrSlotLocked
		}
		if err := domainschedule.ValidateMaximum(maximum, s.slots[i].Used); err != nil {
			return nil, err
		}
		s.slots[i].Maximum = maximum
		s.slots[i].Recalculate()
		result := s.slots[i]
		return &result, nil
	}
	return nil, domainschedule.ErrSlotNotFound
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
			return domainschedule.ErrPlanNotFound
		}
		if !(domainschedule.WorkPlan{Date: date}).CanModify(now) {
			return domainschedule.ErrSlotLocked
		}
		s.slots = append(s.slots[:i], s.slots[i+1:]...)
		return nil
	}
	return domainschedule.ErrSlotNotFound
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
		domainschedule.ScheduleSlot{ID: 13, WorkPlanID: 4, Slot: 2, Maximum: 3},
		domainschedule.ScheduleSlot{ID: 12, WorkPlanID: 4, Slot: 1, Maximum: 3, Used: 1},
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
	if !errors.Is(err, domainschedule.ErrPlanNotFound) {
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

	if _, err := svc.ListSlotsByPlan(context.Background(), 0); !errors.Is(err, domainschedule.ErrInvalidWorkPlan) {
		t.Fatalf("err = %v, want ErrInvalidWorkPlan", err)
	}
}

// TestServiceCreateSlotFuturePlanOK now 注入后，未来计划可正常创建时段；
// 同时断言仓库收到的 now 与注入值一致，证明 Service 正确透传业务时间。
func TestServiceCreateSlotFuturePlanOK(t *testing.T) {
	repo := newMemorySlotRepo()
	repo.plans[1] = "2026-09-20"
	svc := NewService(repo, fixedUTCNow)

	created, err := svc.CreateSlot(context.Background(), domainschedule.ScheduleSlot{WorkPlanID: 1, Slot: 1, Maximum: 3})
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
	if _, err := svc.CreateSlot(context.Background(), domainschedule.ScheduleSlot{WorkPlanID: 1, Slot: 1, Maximum: 3}); !errors.Is(err, domainschedule.ErrSlotExists) {
		t.Errorf("duplicate err = %v, want ErrSlotExists", err)
	}
}

// TestServiceCreateSlotStartedPlanLocked 计划已开始（日期为 now 当天）时返回 ErrSlotLocked。
func TestServiceCreateSlotStartedPlanLocked(t *testing.T) {
	repo := newMemorySlotRepo()
	repo.plans[1] = "2026-09-09" // 与固定 now 同一天，视为已开始。
	svc := NewService(repo, fixedUTCNow)

	_, err := svc.CreateSlot(context.Background(), domainschedule.ScheduleSlot{WorkPlanID: 1, Slot: 1, Maximum: 3})
	if !errors.Is(err, domainschedule.ErrSlotLocked) {
		t.Fatalf("err = %v, want ErrSlotLocked", err)
	}
}

// TestServiceCreateSlotPlanNotFound 计划不存在时返回 ErrPlanNotFound。
func TestServiceCreateSlotPlanNotFound(t *testing.T) {
	svc := NewService(newMemorySlotRepo(), fixedUTCNow)

	_, err := svc.CreateSlot(context.Background(), domainschedule.ScheduleSlot{WorkPlanID: 99, Slot: 1, Maximum: 3})
	if !errors.Is(err, domainschedule.ErrPlanNotFound) {
		t.Fatalf("err = %v, want ErrPlanNotFound", err)
	}
}

// TestServiceUpdateAndDeleteRejectInvalidSlotID 非正数 slotID 在 usecase 层直接拒绝。
func TestServiceUpdateAndDeleteRejectInvalidSlotID(t *testing.T) {
	svc := NewService(newMemorySlotRepo(), fixedUTCNow)

	if _, err := svc.UpdateSlotMaximum(context.Background(), 0, 5); !errors.Is(err, domainschedule.ErrInvalidSlot) {
		t.Errorf("update err = %v, want ErrInvalidSlot", err)
	}
	if err := svc.DeleteSlot(context.Background(), 0); !errors.Is(err, domainschedule.ErrInvalidSlot) {
		t.Errorf("delete err = %v, want ErrInvalidSlot", err)
	}
}

// TestServiceUpdateSlotMaximumOK 更新成功后返回新容量，并透传注入的 now。
func TestServiceUpdateSlotMaximumOK(t *testing.T) {
	repo := newMemorySlotRepo()
	repo.plans[1] = "2026-09-20"
	repo.slots = append(repo.slots, domainschedule.ScheduleSlot{ID: 12, WorkPlanID: 1, Slot: 1, Maximum: 3})
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
	repo.slots = append(repo.slots, domainschedule.ScheduleSlot{ID: 12, WorkPlanID: 1, Slot: 1, Maximum: 3})
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

	if _, err := svc.CreateSlot(context.Background(), domainschedule.ScheduleSlot{WorkPlanID: 1, Slot: 1, Maximum: 3}); err != nil {
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
