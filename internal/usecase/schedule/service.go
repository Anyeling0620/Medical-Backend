package schedule

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"Medical-Web-Backend/internal/domain/schedule"
	"Medical-Web-Backend/internal/port"
)

// 业务错误码：handler 依据这些稳定编码映射 HTTP 响应（见 spec 错误码目录与第 11.3 节示例）。
const (
	CodeValidationFailed = "REQUEST_VALIDATION_FAILED"
	CodePlanNotFound     = "SCHEDULE_PLAN_NOT_FOUND"
	CodePlanExists       = "SCHEDULE_PLAN_EXISTS"
	CodePlanLocked       = "SCHEDULE_PLAN_LOCKED"
	CodePlanConflict     = "SCHEDULE_CONFLICT"
	CodeHasRegistrations = "SCHEDULE_HAS_REGISTRATIONS"
)

// Details 是可选的错误细节（仅非敏感字段），例如 PATCH 时返回当前已用量 used。
type Details map[string]any

// ServiceError 是 use case 返回给 handler 的业务错误，携带稳定 code 与面向用户的 message。
type ServiceError struct {
	Code    string
	Message string
	Details Details
}

func (e *ServiceError) Error() string { return e.Message }

// Result 是写操作的结果：成功时携带资源对象与新建 ID（供 Location）；
// 失败时携带业务错误（供 handler 转换为错误响应并写入幂等存储以便原样重放）。
type Result struct {
	Plan  *schedule.WorkPlan
	NewID int64
	Error *ServiceError
}

// Service 编排排班计划与排班时段的业务规则。所有写操作的状态规则（开始/结束锁、
// 已用量约束、挂号保护）都在本层/领域层判定，repository 不绕过规则。
type Service struct {
	repo port.ScheduleRepository
	now  func() time.Time
	// versioner 是公开排班读缓存的失效端口（T4b）；为 nil 表示未启用缓存，
	// 所有失效调用退化为空操作，行为与改动前一致。
	versioner port.ScheduleCacheVersioner
}

// Option 是构造排班服务的可选配置项（保持既有调用方不必改动签名）。
type Option func(*Service)

// WithScheduleCacheVersioner 注入公开排班读缓存的版本号递增器（T4b）。
// 传入 nil 等价于不启用缓存。
func WithScheduleCacheVersioner(versioner port.ScheduleCacheVersioner) Option {
	return func(s *Service) { s.versioner = versioner }
}

// NewService 构造排班服务；now 为空时回退到当前 UTC 时间。
func NewService(repo port.ScheduleRepository, now func() time.Time, options ...Option) *Service {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	service := &Service{repo: repo, now: now}
	for _, option := range options {
		if option != nil {
			option(service)
		}
	}
	return service
}

// invalidatePublicSchedules 在写操作提交后使公开排班缓存立即失效。
//
// 失败只记日志、不让业务失败：写操作已经提交，缓存是**性能依赖而不是正确性依赖**，
// 这里不能照抄幂等存储「不可用就 5xx」的取舍。漏失效的代价是前端可能短暂显示可挂、
// 下单时拿到 409 REGISTRATION_SLOT_SOLD_OUT（错误码已有），且陈旧窗口不超过缓存 TTL。
func (s *Service) invalidatePublicSchedules(ctx context.Context, subdepartmentID, doctorID int64) {
	if s == nil || s.versioner == nil {
		return
	}
	if err := s.versioner.BumpSchedules(ctx, subdepartmentID, doctorID); err != nil {
		log.Printf("告警：公开排班缓存失效失败 subdepartment_id=%d doctor_id=%d: %v",
			subdepartmentID, doctorID, err)
	}
}

// invalidatePublicSchedulesByPlan 用于只有 planId、拿不到作用域的时段写路径：
// 读一次计划定位到「子科室 + 医生」两个维度；计划读不到（例如已被并发删除）时
// 退化为全局失效——宁可多失效一次回源，也不能漏失效留下脏余量。
func (s *Service) invalidatePublicSchedulesByPlan(ctx context.Context, planID int64) {
	if s == nil || s.versioner == nil || planID < 1 {
		return
	}
	plan, err := s.repo.FindPlan(ctx, planID)
	if err != nil {
		log.Printf("提示：公开排班缓存按计划失效时读取计划失败 plan_id=%d，退化为全局失效: %v", planID, err)
		s.invalidatePublicSchedules(ctx, 0, 0)
		return
	}
	s.invalidatePublicSchedules(ctx, plan.SubdepartmentID, plan.DoctorID)
}

// ListPlans 分页列出排班计划；空结果保证返回非 nil 切片以输出 items:[]。
func (s *Service) ListPlans(ctx context.Context, f schedule.PlanFilter, page, pageSize int) ([]schedule.WorkPlan, int64, error) {
	if s.repo == nil {
		return make([]schedule.WorkPlan, 0), 0, errors.New("排班仓库未配置")
	}
	items, total, err := s.repo.ListPlans(ctx, f, (page-1)*pageSize, pageSize)
	if items == nil {
		items = make([]schedule.WorkPlan, 0)
	}
	return items, total, err
}

// CreatePlan 创建排班计划：领域校验与关联校验完成后，在单事务内检查重复并插入，
// 避免并发创建绕过唯一规则（契约 1.5：状态规则在 use case/domain 层完成）。
func (s *Service) CreatePlan(ctx context.Context, p schedule.WorkPlan) (*Result, error) {
	if s.repo == nil {
		return nil, errors.New("排班仓库未配置")
	}
	// maximum 越界校验：先于领域校验给出 <1 与 >32767 的区分文案（int16 域内仅 <1 可达，
	// 保留上界分支以防御上层转换溢出，契约要求两种文案不同）。
	if err := maximumServiceError(p.Maximum); err != nil {
		return nil, err
	}
	// 领域校验：日期格式、不得早于业务当前日期、maximum 1..32767 等。
	if err := p.ValidateNew(s.now()); err != nil {
		return nil, validationError(err)
	}
	err := s.repo.ExecTx(ctx, func(tx port.ScheduleTx) error {
		// 事务最先对“医生/子科室/日期”创建键加咨询锁：该表没有唯一约束，READ COMMITTED 下
		// 并发“先查后插”会同时通过检查，先用事务级锁把同创建键的插入串行化（见 ScheduleTx
		// 的 TxLockPlanCreate 说明），再执行关联校验与查重，保证 409 判定可靠。
		if err := tx.TxLockPlanCreate(ctx, p.DoctorID, p.SubdepartmentID, p.Date); err != nil {
			return err
		}
		// 医生 ACTIVE、子科室存在且医生关联该子科室（复用目录表 medical_dept_sub_and_doctor），
		// 三态独立校验以便给出契约要求的三种精确文案。
		doctorActive, subExists, associated, err := tx.TxDoctorAssociation(ctx, p.DoctorID, p.SubdepartmentID)
		if err != nil {
			return err
		}
		if !doctorActive {
			return &ServiceError{Code: CodeValidationFailed, Message: "医生不存在或未在出诊状态"}
		}
		if !subExists {
			return &ServiceError{Code: CodeValidationFailed, Message: "子科室不存在"}
		}
		if !associated {
			return &ServiceError{Code: CodeValidationFailed, Message: "医生未关联该子科室"}
		}
		// 同医生/子科室/日期重复计划返回 409。
		duplicate, err := tx.TxHasPlan(ctx, p.DoctorID, p.SubdepartmentID, p.Date)
		if err != nil {
			return err
		}
		if duplicate {
			return &ServiceError{Code: CodePlanExists, Message: "排班计划已存在"}
		}
		insertedID, err := tx.TxInsertPlan(ctx, p)
		if err != nil {
			return err
		}
		p.ID = insertedID
		p.Recalculate()
		return nil
	})
	if err != nil {
		return nil, mapServiceError(err)
	}
	// 计划创建会新增可挂号时段：立即失效该子科室与医生的公开排班缓存，
	// 避免「空结果缓存」把新排班挡住（T4b）。
	s.invalidatePublicSchedules(ctx, p.SubdepartmentID, p.DoctorID)
	// 插入成功：返回生成的 ID 与资源对象，handler 据此输出 201 资源与 Location。
	return &Result{Plan: &p, NewID: p.ID}, nil
}

// UpdatePlan 更新 maximum：事务内 FOR UPDATE 重读计划，依次判定锁状态与
// “新值不得小于已用量”，最后条件更新并返回最新资源。
func (s *Service) UpdatePlan(ctx context.Context, planID int64, maximum int16) (*Result, error) {
	if s.repo == nil {
		return nil, errors.New("排班仓库未配置")
	}
	if err := maximumServiceError(maximum); err != nil {
		return nil, err
	}
	var updated *schedule.WorkPlan
	// scopeSubdepartment / scopeDoctor 从事务内读到的计划快照里取出：
	// 失效需要的「子科室 + 医生」两个维度在 planID 上是查不到的。
	var scopeSubdepartment, scopeDoctor int64
	err := s.repo.ExecTx(ctx, func(tx port.ScheduleTx) error {
		plan, err := tx.TxFindPlanLocked(ctx, planID)
		if err != nil {
			return err
		}
		scopeSubdepartment, scopeDoctor = plan.SubdepartmentID, plan.DoctorID
		// 已开始/已结束的计划锁定，不允许修改。
		if !plan.CanModify(s.now()) {
			return &ServiceError{Code: CodePlanLocked, Message: "排班计划已开始或已结束，不能修改"}
		}
		// 新值不得小于已用量，details 携带 used 供前端展示。
		if maximum < plan.Used {
			return &ServiceError{Code: CodePlanConflict, Message: "最大号源不能小于已使用号源", Details: Details{"used": plan.Used}}
		}
		if err := tx.TxUpdatePlanMaximum(ctx, planID, maximum); err != nil {
			return err
		}
		plan.Maximum = maximum
		plan.Recalculate()
		updated = plan
		return nil
	})
	if err != nil {
		return nil, mapServiceError(err)
	}
	// 计划容量变化会改变公开余量：提交后立即失效（T4b）。
	s.invalidatePublicSchedules(ctx, scopeSubdepartment, scopeDoctor)
	return &Result{Plan: updated}, nil
}

// DeletePlan 物理删除排班计划：已开始/已结束或计划及其 slots 存在挂号记录时拒绝删除。
func (s *Service) DeletePlan(ctx context.Context, planID int64) error {
	if s.repo == nil {
		return errors.New("排班仓库未配置")
	}
	var scopeSubdepartment, scopeDoctor int64
	err := s.repo.ExecTx(ctx, func(tx port.ScheduleTx) error {
		plan, err := tx.TxFindPlanLocked(ctx, planID)
		if err != nil {
			return err
		}
		scopeSubdepartment, scopeDoctor = plan.SubdepartmentID, plan.DoctorID
		// 已开始/已结束不允许删除（表无状态列，不允许软删除）。
		if !plan.CanModify(s.now()) {
			return &ServiceError{Code: CodePlanLocked, Message: "排班计划已开始或已结束，不能删除"}
		}
		// 挂号保护：先按实体 used 判断，再以挂号表 EXISTS 兜底并发的历史关联。
		if plan.HasRegistrations() {
			return &ServiceError{Code: CodeHasRegistrations, Message: "已有挂号记录，不能删除排班"}
		}
		hasRegistrations, err := tx.TxHasRegistrations(ctx, planID)
		if err != nil {
			return err
		}
		if hasRegistrations {
			return &ServiceError{Code: CodeHasRegistrations, Message: "已有挂号记录，不能删除排班"}
		}
		return tx.TxDeletePlan(ctx, planID)
	})
	if err != nil {
		return mapServiceError(err)
	}
	// 计划删除后其对下的时段不再可挂：提交后立即失效（T4b）。
	s.invalidatePublicSchedules(ctx, scopeSubdepartment, scopeDoctor)
	return nil
}

// maximumServiceError 把 maximum 越界映射为契约要求的两种文案：
// <1 与 >32767 分别提示，供请求绑定与 use case 层复用。
func maximumServiceError(maximum int16) error {
	switch {
	case maximum < 1:
		return &ServiceError{Code: CodeValidationFailed, Message: "最大号源必须大于 0"}
	case maximum > 32767:
		return &ServiceError{Code: CodeValidationFailed, Message: "最大号源不能超过 32767"}
	default:
		return nil
	}
}

// validationError 把领域层校验错误映射为 422 参数错误，携带契约要求的文案。
func validationError(err error) error {
	var serviceErr *ServiceError
	if errors.As(err, &serviceErr) {
		return err
	}
	switch {
	case errors.Is(err, schedule.ErrDateInPast):
		return &ServiceError{Code: CodeValidationFailed, Message: "排班日期不得早于业务当前日期"}
	case errors.Is(err, schedule.ErrInvalidDate):
		return &ServiceError{Code: CodeValidationFailed, Message: "日期格式必须为 YYYY-MM-DD"}
	case errors.Is(err, schedule.ErrInvalidMaximum):
		return &ServiceError{Code: CodeValidationFailed, Message: "最大号源必须大于 0"}
	case errors.Is(err, schedule.ErrInvalidDoctor):
		return &ServiceError{Code: CodeValidationFailed, Message: "医生编号必须为正整数"}
	case errors.Is(err, schedule.ErrInvalidSubdept):
		return &ServiceError{Code: CodeValidationFailed, Message: "子科室编号必须为正整数"}
	default:
		return &ServiceError{Code: CodeValidationFailed, Message: "请求参数不正确"}
	}
}
func mapServiceError(err error) error {
	if err == nil {
		return nil
	}
	var serviceErr *ServiceError
	if errors.As(err, &serviceErr) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return &ServiceError{Code: CodePlanNotFound, Message: "排班计划不存在"}
	}
	// 兜底映射：未来数据库层若增加唯一索引或防御性重读触发领域错误，仍按契约返回 409
	// 而不是落入 500（serviceErr 上面的分支只在直接返回 ServiceError 时命中）。
	if errors.Is(err, schedule.ErrPlanExists) {
		return &ServiceError{Code: CodePlanExists, Message: "排班计划已存在"}
	}
	if errors.Is(err, schedule.ErrMaximumBelowUsed) {
		return &ServiceError{Code: CodePlanConflict, Message: "最大号源不能小于已使用号源"}
	}
	// TxDeletePlan 在加锁后重跑挂号检查命中时返回领域错误，按契约仍映射为 409 而非 500。
	if errors.Is(err, schedule.ErrHasRegistrations) {
		return &ServiceError{Code: CodeHasRegistrations, Message: "已有挂号记录，不能删除排班"}
	}
	return err
}

// ListSlotsByPlan 返回某计划全部时段；计划不存在时返回 ErrPlanNotFound
// （由 repository 在同一查询内完成存在性判定与列表读取）。
func (s *Service) ListSlotsByPlan(ctx context.Context, planID int64) ([]schedule.ScheduleSlot, error) {
	if planID < 1 {
		return nil, schedule.ErrInvalidWorkPlan
	}
	if s == nil || s.repo == nil {
		return nil, schedule.ErrInvalidWorkPlan
	}
	items, err := s.repo.ListSlotsByPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Recalculate()
	}
	return items, nil
}

// CreateSlot 创建时段：计划不存在返回 ErrPlanNotFound；重复时段返回 ErrSlotExists；
// 已开始计划返回 ErrSlotLocked；计划所属医生已离职/退休等非在诊状态返回 ErrDoctorInactive。
// 医生资格在仓储事务内与计划状态一起校验（读取医生行快照），
// 把“先查后写”之间医生状态被改动的窗口缩小到本事务提交前。
// 成功返回含 remaining 的资源对象。
func (s *Service) CreateSlot(ctx context.Context, input schedule.ScheduleSlot) (*schedule.ScheduleSlot, error) {
	if s == nil || s.repo == nil {
		return nil, schedule.ErrInvalidWorkPlan
	}
	created, err := s.repo.CreateSlot(ctx, input, s.now())
	if err != nil {
		return nil, err
	}
	// 新增时段会新增可挂号号源：按计划定位作用域后失效（T4b）。
	s.invalidatePublicSchedulesByPlan(ctx, input.WorkPlanID)
	return created, nil
}

// UpdateSlotMaximum 修改时段容量。
// 时段不存在返回 ErrSlotNotFound；计划已开始或已有挂号返回 ErrSlotLocked；
// 容量小于已用返回 ErrMaximumBelowUsed。
func (s *Service) UpdateSlotMaximum(ctx context.Context, slotID int64, maximum int16) (*schedule.ScheduleSlot, error) {
	if slotID < 1 {
		return nil, schedule.ErrInvalidSlot
	}
	if s == nil || s.repo == nil {
		return nil, schedule.ErrInvalidWorkPlan
	}
	updated, err := s.repo.UpdateSlotMaximum(ctx, slotID, maximum, s.now())
	if err != nil {
		return nil, err
	}
	// 时段容量变化会改变公开余量：按计划定位作用域后失效（T4b）。
	s.invalidatePublicSchedulesByPlan(ctx, updated.WorkPlanID)
	return updated, nil
}

// DeleteSlot 物理删除时段：仅允许无挂号且所属计划未开始的时段。
func (s *Service) DeleteSlot(ctx context.Context, slotID int64) error {
	if slotID < 1 {
		return schedule.ErrInvalidSlot
	}
	if s == nil || s.repo == nil {
		return schedule.ErrInvalidWorkPlan
	}
	if err := s.repo.DeleteSlot(ctx, slotID, s.now()); err != nil {
		return err
	}
	// 删除时段后无法再读到「时段 -> 计划」的映射（记录已消失），拿不到子科室/医生作用域，
	// 因此退化为全局失效：多一次回源，换「绝不漏失效」。
	s.invalidatePublicSchedules(ctx, 0, 0)
	return nil
}
