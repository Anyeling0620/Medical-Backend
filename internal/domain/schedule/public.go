package schedule

import "time"

// 本文件是匿名公开查询域可挂号时段（契约 §8.1 GET /api/v1/public/schedules）的资源模型。
// 该域只暴露挂号所需的最小字段集合，不返回 planId、workPlanId 等内部管理字段。

// DateLayout 是公开域日期参数与响应的统一布局（YYYY-MM-DD），与数据库 date 列一致。
const DateLayout = dateLayout

// BusinessToday 返回业务时区（Asia/Shanghai）当天日期（UTC 零点的 date-only）。
// 公开排班查询的缺省日期范围以它为起点：若按部署机时区取当天，跨零点时会算错窗口。
func BusinessToday(now time.Time) time.Time {
	return dateOnly(now)
}

// PublicScheduleDoctor 是公开时段内嵌的医生摘要。
type PublicScheduleDoctor struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Job      string `json:"job"`
	Degree   string `json:"degree"`
	PhotoURL string `json:"photoUrl"`
}

// PublicScheduleSubdepartment 是公开时段内嵌的子科室摘要。
type PublicScheduleSubdepartment struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// PublicSchedule 是可挂号时段；ScheduleID 为 doctor_work_plan_schedule 主键，
// Date 取自所属排班计划（doctor_work_plan.date）。
type PublicSchedule struct {
	ScheduleID    int64                       `json:"scheduleId"`
	Date          string                      `json:"date"`
	Slot          int16                       `json:"slot"`
	Maximum       int16                       `json:"maximum"`
	Remaining     int16                       `json:"remaining"`
	Amount        string                      `json:"amount"`
	Doctor        PublicScheduleDoctor        `json:"doctor"`
	Subdepartment PublicScheduleSubdepartment `json:"subdepartment"`
}

// Recalculate 按时段级 maximum-num 计算剩余号源，并保证不出现负数
// （与管理端 WorkPlan/ScheduleSlot 的口径一致）。
func (p *PublicSchedule) Recalculate(used int16) {
	if p == nil {
		return
	}
	p.Remaining = p.Maximum - used
	if p.Remaining < 0 {
		p.Remaining = 0
	}
}

// PublicScheduleFilter 是公开排班查询的过滤条件。
// SubdepartmentID 与 DoctorID 同时给出时取交集，两者至少一个非空（由 use case 校验）。
type PublicScheduleFilter struct {
	SubdepartmentID *int64
	DoctorID        *int64
	FromDate        string
	ToDate          string
}
