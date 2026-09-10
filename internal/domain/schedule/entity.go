package schedule

import (
	"errors"
	"fmt"
	"time"
)

const dateLayout = "2006-01-02"

var (
	ErrInvalidDate      = errors.New("schedule date must be YYYY-MM-DD")
	ErrDateInPast       = errors.New("schedule date cannot be in the past")
	ErrInvalidMaximum   = errors.New("schedule maximum must be between 1 and 32767")
	ErrMaximumBelowUsed = errors.New("schedule maximum cannot be below used capacity")
	ErrInvalidDoctor    = errors.New("schedule doctor id must be a positive integer")
	ErrInvalidSubdept   = errors.New("schedule subdepartment id must be a positive integer")
	ErrInvalidWorkPlan  = errors.New("schedule work plan id must be a positive integer")
	ErrInvalidSlot      = errors.New("schedule slot must be a positive integer")
	ErrPlanLocked       = errors.New("schedule plan has started or ended")
	ErrSlotLocked       = errors.New("schedule slot belongs to a plan that has started or ended")
	ErrHasRegistrations = errors.New("schedule resource has registrations and cannot be deleted")
	// ErrPlanExists 用于同一医生/子科室/日期重复创建排班计划。
	ErrPlanExists = errors.New("schedule plan already exists for the doctor, subdepartment and date")
	// ErrPlanNotFound 表示排班计划不存在（404 SCHEDULE_PLAN_NOT_FOUND）。
	ErrPlanNotFound = errors.New("schedule plan not found")
	// ErrSlotNotFound 表示时段不存在（404 SCHEDULE_SLOT_NOT_FOUND）。
	ErrSlotNotFound = errors.New("schedule slot not found")
	// ErrSlotExists 表示同一计划下时段编号重复（409 SCHEDULE_SLOT_EXISTS）。
	ErrSlotExists = errors.New("schedule slot already exists")
	// ErrDoctorInactive 表示排班所属医生已离职/退休或处于其他非在诊状态，
	// 不具备出诊资格，不得为其新增出诊时段（422 REQUEST_VALIDATION_FAILED）。
	ErrDoctorInactive = errors.New("schedule doctor is not active")
)

// PlanFilter 是排班计划列表的查询条件与排序参数。
// 所有过滤字段均可选；日期为 YYYY-MM-DD 字符串，由上层完成格式与先后校验。
type PlanFilter struct {
	DoctorID        *int64
	DepartmentID    *int64
	SubdepartmentID *int64
	FromDate        string
	ToDate          string
	IncludeSlots    bool
	Sort            string
	Order           string
}

// WorkPlan maps hospital.doctor_work_plan. Date is kept as a date-only string
// because the schema stores a PostgreSQL date and the API serializes it likewise.
type WorkPlan struct {
	ID              int64          `json:"id"`
	DoctorID        int64          `json:"doctorId"`
	SubdepartmentID int64          `json:"subdepartmentId"`
	Date            string         `json:"date"`
	Maximum         int16          `json:"maximum"`
	Used            int16          `json:"used"`
	Remaining       int16          `json:"remaining"`
	Slots           []ScheduleSlot `json:"slots,omitempty"`
}

// ScheduleSlot maps hospital.doctor_work_plan_schedule. Slot is an existing
// integer period identifier; this entity intentionally does not assign a time.
type ScheduleSlot struct {
	ID         int64 `json:"id"`
	WorkPlanID int64 `json:"workPlanId"`
	Slot       int16 `json:"slot"`
	Maximum    int16 `json:"maximum"`
	Used       int16 `json:"used"`
	Remaining  int16 `json:"remaining"`
}

func (p *WorkPlan) Recalculate() {
	if p == nil {
		return
	}
	p.Remaining = p.Maximum - p.Used
	if p.Remaining < 0 {
		p.Remaining = 0
	}
}

func (s *ScheduleSlot) Recalculate() {
	if s == nil {
		return
	}
	s.Remaining = s.Maximum - s.Used
	if s.Remaining < 0 {
		s.Remaining = 0
	}
}

func (p WorkPlan) ParsedDate() (time.Time, error) {
	date, err := time.ParseInLocation(dateLayout, p.Date, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %v", ErrInvalidDate, err)
	}
	return date, nil
}

// ValidateNew checks invariants for creating a plan. The current day is accepted
// because the API contract forbids only dates earlier than today.
func (p WorkPlan) ValidateNew(now time.Time) error {
	if p.DoctorID < 1 {
		return ErrInvalidDoctor
	}
	if p.SubdepartmentID < 1 {
		return ErrInvalidSubdept
	}
	date, err := p.ParsedDate()
	if err != nil {
		return err
	}
	if date.Before(dateOnly(now)) {
		return ErrDateInPast
	}
	return ValidateMaximum(p.Maximum, p.Used)
}

func ValidateMaximum(maximum, used int16) error {
	if maximum < 1 || maximum > 32767 {
		return ErrInvalidMaximum
	}
	if used < 0 || maximum < used {
		return ErrMaximumBelowUsed
	}
	return nil
}

func (s ScheduleSlot) ValidateNew() error {
	if s.WorkPlanID < 1 {
		return ErrInvalidWorkPlan
	}
	if s.Slot < 1 {
		return ErrInvalidSlot
	}
	return ValidateMaximum(s.Maximum, s.Used)
}

// CanModify reports whether a plan is editable at the beginning of the given day.
// A plan is locked once its date has arrived or passed.
func (p WorkPlan) CanModify(now time.Time) bool {
	date, err := p.ParsedDate()
	return err == nil && date.After(dateOnly(now))
}

func (s ScheduleSlot) CanModify(plan WorkPlan, now time.Time) bool {
	return plan.CanModify(now)
}

func (p WorkPlan) HasRegistrations() bool {
	if p.Used > 0 {
		return true
	}
	for _, slot := range p.Slots {
		if slot.Used > 0 {
			return true
		}
	}
	return false
}

func (s ScheduleSlot) HasRegistrations() bool {
	return s.Used > 0
}

func (p *WorkPlan) UpdateMaximum(maximum int16, now time.Time) error {
	if p == nil || !p.CanModify(now) {
		return ErrPlanLocked
	}
	if err := ValidateMaximum(maximum, p.Used); err != nil {
		return err
	}
	p.Maximum = maximum
	p.Recalculate()
	return nil
}

func (s *ScheduleSlot) UpdateMaximum(maximum int16, plan WorkPlan, now time.Time) error {
	if s == nil || !s.CanModify(plan, now) {
		return ErrSlotLocked
	}
	if err := ValidateMaximum(maximum, s.Used); err != nil {
		return err
	}
	s.Maximum = maximum
	s.Recalculate()
	return nil
}

func (p WorkPlan) ValidateDelete(now time.Time) error {
	if !p.CanModify(now) {
		return ErrPlanLocked
	}
	if p.HasRegistrations() {
		return ErrHasRegistrations
	}
	return nil
}

func (s ScheduleSlot) ValidateDelete(plan WorkPlan, now time.Time) error {
	if !plan.CanModify(now) {
		return ErrSlotLocked
	}
	if s.HasRegistrations() {
		return ErrHasRegistrations
	}
	return nil
}

// MaximumBelowUsedError 表示新容量小于当前已用号源，携带 used 供 409 响应 details 展示。
type MaximumBelowUsedError struct{ Used int16 }

func (e *MaximumBelowUsedError) Error() string {
	return ErrMaximumBelowUsed.Error()
}
func (e *MaximumBelowUsedError) Unwrap() error { return ErrMaximumBelowUsed }

// businessLocation 是排班业务的日历时区。排班日期按医院所在地（Asia/Shanghai）的
// 日历日判定是否“已开始/已结束”，不能使用 UTC 日界：若按 UTC 截断，
// 上海每天 00:00-08:00 会把“当天已开始的计划”误判为未来计划而放行写操作。
// 使用固定 +08:00 偏移，避免依赖部署环境是否安装时区数据库。
var businessLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

// dateOnly 把任意时刻归一到“业务日期”对应的 UTC 零点：
// 先按 Asia/Shanghai 取日历日，再构造 UTC 零点便于与 ParsedDate（UTC 零点）比较。
func dateOnly(value time.Time) time.Time {
	year, month, day := value.In(businessLocation).Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}
