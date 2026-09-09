package port

import (
	"context"

	"Medical-Web-Backend/internal/domain/schedule"
)

// PlanRepository 描述排班计划持久化所需的读操作；写操作必须通过 ExecTx 提交，
// 以便 use case 在单事务内完成存在性/状态/并发校验后再落库，repository 不绕过规则。
type PlanRepository interface {
	// ListPlans 按过滤条件分页返回计划；f.IncludeSlots 为 true 时同时装载 slots。
	ListPlans(ctx context.Context, f schedule.PlanFilter, offset, limit int) ([]schedule.WorkPlan, int64, error)
	// FindPlan 返回单个计划（含 slots），用于读取场景；不存在返回 sql.ErrNoRows。
	FindPlan(ctx context.Context, planID int64) (*schedule.WorkPlan, error)
	// ExecTx 在单数据库事务内执行 fn；提交/回滚由实现负责，fn 返回错误时回滚。
	ExecTx(ctx context.Context, fn func(tx ScheduleTx) error) error
}

// ScheduleTx 是事务作用域内的排班持久化操作集合，只暴露写计划需要的原语。
// PostgresScheduleRepository 用真实事务实现；测试用内存 fake 实现。
type ScheduleTx interface {
	// TxInsertPlan 插入排班计划（num=0）并返回自增 id；存在唯一冲突时返回领域错误
	// schedule.ErrPlanExists（并发兜底）。使用 INSERT ... RETURNING id 获取主键，
	// 避免依赖驱动层 LastInsertId。
	TxInsertPlan(ctx context.Context, p schedule.WorkPlan) (int64, error)
	// TxLockPlanCreate 事务内按创建键（doctorID|subdepartmentID|date）加咨询锁，串行化
	// 并发创建：表没有唯一约束（库结构不可改），同一创建键的两个事务必须排队，后到者等
	// 先行事务提交后再查重/插入，保证 409 判定可靠，避免 READ COMMITTED 下的重复行。
	TxLockPlanCreate(ctx context.Context, doctorID, subdepartmentID int64, date string) error
	// TxHasPlan 判断同一医生/子科室/日期是否已有计划。
	TxHasPlan(ctx context.Context, doctorID, subdepartmentID int64, date string) (bool, error)
	// TxFindPlanLocked 事务内 FOR UPDATE 重新读取计划（含 slots），未找到返回 sql.ErrNoRows。
	TxFindPlanLocked(ctx context.Context, planID int64) (*schedule.WorkPlan, error)
	// TxUpdatePlanMaximum 条件更新 maximum；新值低于已用量时返回领域错误 ErrMaximumBelowUsed。
	TxUpdatePlanMaximum(ctx context.Context, planID int64, maximum int16) error
	// TxHasRegistrations 检查计划或它的任一 slots 是否已产生挂号记录。
	TxHasRegistrations(ctx context.Context, planID int64) (bool, error)
	// TxDeletePlan 物理删除计划（级联删除其 slots）。
	TxDeletePlan(ctx context.Context, planID int64) error
	// TxDoctorAssociation 事务内校验医生与子科室创建排班的资格，三态独立返回便于精确报错：
	// doctorActive=true 表示医生存在且 status=1（ACTIVE）；
	// subdepartmentExists=true 表示子科室存在；
	// associated=true 表示 medical_dept_sub_and_doctor 存在该医生-子科室关联。
	TxDoctorAssociation(ctx context.Context, doctorID, subdepartmentID int64) (doctorActive bool, subdepartmentExists bool, associated bool, err error)
}
