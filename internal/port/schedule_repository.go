package port

import (
	"context"

	"Medical-Web-Backend/internal/domain/schedule"
	"time"
)

// ScheduleSlotRepository 封装排班时段（doctor_work_plan_schedule）的持久化操作。
// 写操作必须满足：计划存在且日期未到（未来排班）、时段容量校验、
// 无重复时段、已有挂号/已开始计划的时段不可修改或删除。
type ScheduleSlotRepository interface {
	// ListSlotsByPlan 返回某计划的时段列表，按 slot 升序，并计算 remaining；
	// 计划不存在返回 schedule.ErrPlanNotFound（存在性判定与列表读取同语句完成，
	// 避免先查存在再查列表的并发删除窗口返回 200 []）。
	ListSlotsByPlan(ctx context.Context, planID int64) ([]schedule.ScheduleSlot, error)
	// CreateSlot 事务内创建时段：计划不存在返回 ErrPlanNotFound；
	// 同计划时段重复返回 ErrSlotExists；计划已开始返回 ErrSlotLocked。
	CreateSlot(ctx context.Context, slot schedule.ScheduleSlot, now time.Time) (*schedule.ScheduleSlot, error)
	// UpdateSlotMaximum 事务内更新 maximum：
	// 时段不存在返回 ErrSlotNotFound；计划已开始或已有挂号返回 ErrSlotLocked；
	// 容量小于已用返回 ErrMaximumBelowUsed。
	UpdateSlotMaximum(ctx context.Context, slotID int64, maximum int16, now time.Time) (*schedule.ScheduleSlot, error)
	// DeleteSlot 物理删除时段：仅当无挂号且所属计划未开始时允许。
	DeleteSlot(ctx context.Context, slotID int64, now time.Time) error
}
