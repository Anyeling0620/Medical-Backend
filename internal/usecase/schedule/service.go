package schedule

import (
	"context"
	"time"

	"Medical-Web-Backend/internal/domain/schedule"
	"Medical-Web-Backend/internal/port"
)

// Service 编排排班时段相关的业务规则。
// 写操作只允许未来排班，当前日期统一由 Now 提供，便于测试与后续接入业务时钟。
type Service struct {
	repository port.ScheduleSlotRepository
	now        func() time.Time
}

// NewService 构造时段服务；now 缺省时使用系统当前时间（UTC 时钟）。
// 排班“是否已开始”的日历日按 Asia/Shanghai 在领域层 dateOnly 中归一（entity.go），
// 因此与注入时钟的时区无关，避免上海 00:00-08:00 的日界窗口误放行当天写操作。
func NewService(repository port.ScheduleSlotRepository, now func() time.Time) *Service {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{repository: repository, now: now}
}

// ListSlotsByPlan 返回某计划全部时段；计划不存在时返回 ErrPlanNotFound
// （由 repository 在同一查询内完成存在性判定与列表读取）。
func (s *Service) ListSlotsByPlan(ctx context.Context, planID int64) ([]schedule.ScheduleSlot, error) {
	if planID < 1 {
		return nil, schedule.ErrInvalidWorkPlan
	}
	if s == nil || s.repository == nil {
		return nil, schedule.ErrInvalidWorkPlan
	}
	items, err := s.repository.ListSlotsByPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Recalculate()
	}
	return items, nil
}

// CreateSlot 创建时段：计划不存在返回 ErrPlanNotFound；重复时段返回 ErrSlotExists；
// 已开始计划返回 ErrSlotLocked。成功返回含 remaining 的资源对象。
func (s *Service) CreateSlot(ctx context.Context, input schedule.ScheduleSlot) (*schedule.ScheduleSlot, error) {
	if s == nil || s.repository == nil {
		return nil, schedule.ErrInvalidWorkPlan
	}
	return s.repository.CreateSlot(ctx, input, s.now())
}

// UpdateSlotMaximum 修改时段容量。
// 时段不存在返回 ErrSlotNotFound；计划已开始或已有挂号返回 ErrSlotLocked；
// 容量小于已用返回 ErrMaximumBelowUsed。
func (s *Service) UpdateSlotMaximum(ctx context.Context, slotID int64, maximum int16) (*schedule.ScheduleSlot, error) {
	if slotID < 1 {
		return nil, schedule.ErrInvalidSlot
	}
	if s == nil || s.repository == nil {
		return nil, schedule.ErrInvalidWorkPlan
	}
	return s.repository.UpdateSlotMaximum(ctx, slotID, maximum, s.now())
}

// DeleteSlot 物理删除时段：仅允许无挂号且所属计划未开始的时段。
func (s *Service) DeleteSlot(ctx context.Context, slotID int64) error {
	if slotID < 1 {
		return schedule.ErrInvalidSlot
	}
	if s == nil || s.repository == nil {
		return schedule.ErrInvalidWorkPlan
	}
	return s.repository.DeleteSlot(ctx, slotID, s.now())
}
