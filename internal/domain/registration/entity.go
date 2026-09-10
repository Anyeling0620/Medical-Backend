// Package registration 定义挂号域（hospital.medical_registration）的实体与业务规则。
//
// 挂号是双 realm 共享业务资源：管理端凭 REGISTRATION:SELECT/WRITE 代查代建，
// 患者端只能操作本人就诊卡名下的记录，越权一律按「资源不存在」处理
// （spec/04-api-contract.md §1.2、§6）。本包只做纯领域计算，不依赖数据库、
// HTTP 或第三方 SDK；持久化由 internal/repo 实现 internal/port 中声明的接口。
package registration

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// 支付状态编码与对外字符串语义的映射依据 medical_registration 建表注释：
// 1 未付款、2 已付款、3 已退款、4 已过期（契约 §6.8）。
const (
	PaymentCodeUnpaid   int16 = 1
	PaymentCodePaid     int16 = 2
	PaymentCodeRefunded int16 = 3
	PaymentCodeExpired  int16 = 4
)

// 支付状态对外字符串语义：UNPAID、PAID、REFUNDED、EXPIRED。
const (
	PaymentStatusUnpaid   = "UNPAID"
	PaymentStatusPaid     = "PAID"
	PaymentStatusRefunded = "REFUNDED"
	PaymentStatusExpired  = "EXPIRED"
)

// 列表排序白名单（契约 §6.3、§1.4：sort 必须从白名单选择，禁止拼接 SQL）。
const (
	SortCreateDate = "createDate"
	SortDate       = "date"
	SortID         = "id"
)

const dateLayout = "2006-01-02"

// businessLocation 是挂号业务的日历时区。挂号日期与创建日期都按医院所在地
// （Asia/Shanghai）的日历日判定，不能使用 UTC 日界；使用固定 +08:00 偏移，
// 避免依赖部署环境是否安装时区数据库（与排班域、患者域保持同一口径）。
var businessLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

// BusinessDate 把时刻归一到业务日历日，格式 YYYY-MM-DD。
func BusinessDate(t time.Time) string {
	return t.In(businessLocation).Format(dateLayout)
}

// BusinessToday 返回业务时区当天日期（UTC 零点的 date-only），用于与数据库 date 列比较。
func BusinessToday(t time.Time) time.Time {
	year, month, day := t.In(businessLocation).Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// Started 报告排班日期是否已开始或已过期：当天与更早的日期都算已开始
// （与排班域 WorkPlan.CanModify 的判定一致，契约 §6.1 的 SCHEDULE_STARTED）。
// 日期解析失败属于脏数据，向「不可挂号」一侧收敛。
func Started(date string, now time.Time) bool {
	parsed, err := time.ParseInLocation(dateLayout, date, time.UTC)
	if err != nil {
		return true
	}
	return !parsed.After(BusinessToday(now))
}

// PaymentStatusLabel 把数据库编码映射为对外字符串。
//
// 未知编码（脏数据或后续新增状态）统一按未付款返回：该映射决定列表与详情中
// 的展示语义，向「仍占用号源」一侧收敛不会把已占用的时段误报为可再挂，
// 也不会让响应出现空字符串。
func PaymentStatusLabel(code int16) string {
	switch code {
	case PaymentCodePaid:
		return PaymentStatusPaid
	case PaymentCodeRefunded:
		return PaymentStatusRefunded
	case PaymentCodeExpired:
		return PaymentStatusExpired
	case PaymentCodeUnpaid:
		return PaymentStatusUnpaid
	default:
		return PaymentStatusUnpaid
	}
}

// PaymentStatusCode 把对外字符串语义解析为数据库编码；取值不合法时返回 false，
// 由请求层映射为 422（契约 §12.4：paymentStatus=WAITING 返回 REQUEST_VALIDATION_FAILED）。
func PaymentStatusCode(label string) (int16, bool) {
	switch label {
	case PaymentStatusUnpaid:
		return PaymentCodeUnpaid, true
	case PaymentStatusPaid:
		return PaymentCodePaid, true
	case PaymentStatusRefunded:
		return PaymentCodeRefunded, true
	case PaymentStatusExpired:
		return PaymentCodeExpired, true
	default:
		return 0, false
	}
}

// Registration 映射 hospital.medical_registration。
//
// 本实体不带 json tag，也不用于直接序列化：契约对不同接口规定了不同的字段集合
// （列表不含 workPlanId/scheduleId，详情含医生与容量摘要，prepayId 只在受保护的
// 支付详情中返回），因此对外响应统一由 transport/http/response 的 DTO 投影，
// 避免领域结构体调整时静默改变接口形状。
type Registration struct {
	ID              int64
	PatientCardID   int64
	WorkPlanID      int64
	ScheduleID      int64
	DoctorID        int64
	SubdepartmentID int64
	Date            string
	Slot            int16
	Amount          string
	OutTradeNo      string
	PaymentStatus   string
	CreateDate      string
}

// ScheduleSnapshot 是资格校验与建单所需的排班快照：
// 由 scheduleSlot -> workPlan -> doctor/subdepartment 一次读取，
// 应付金额取该医生的价目（doctor_price），客户端提交的值一律不可信（契约 §6.2）。
type ScheduleSnapshot struct {
	ScheduleID        int64
	WorkPlanID        int64
	Slot              int16
	SlotMaximum       int16
	SlotUsed          int16
	PlanMaximum       int16
	PlanUsed          int16
	Date              string
	DoctorID          int64
	DoctorName        string
	DoctorJob         string
	DoctorActive      bool
	SubdepartmentID   int64
	SubdepartmentName string
	Amount            string
}

// Remaining 返回真正可挂的剩余号源。
//
// 建单时时段级（doctor_work_plan_schedule.num < maximum）与计划级
// （doctor_work_plan.num < maximum）上限同时生效（契约 §6.2），因此这里取两者
// 剩余量的较小值：资格校验据此给出 SCHEDULE_SOLD_OUT，避免出现「资格通过但建单被拒」。
func (s ScheduleSnapshot) Remaining() int16 {
	remaining := s.SlotMaximum - s.SlotUsed
	if planRemaining := s.PlanMaximum - s.PlanUsed; planRemaining < remaining {
		remaining = planRemaining
	}
	if remaining < 0 {
		return 0
	}
	return remaining
}

// Eligibility 是资格校验结果（契约 §6.1）：不满足资格仍返回成功响应，
// 以 eligible=false 与稳定的 reasons 词表表达。
//
// Amount 为 nil 表示无法给出应付金额（时段不存在等），响应体输出 null；
// Remaining 在无法计算时输出 0。
type Eligibility struct {
	Eligible  bool
	Remaining int16
	Amount    *string
	Reasons   []string
}

// Filter 是挂号列表的查询条件。
//
// OwnerPatientID 非空时只返回该患者名下就诊卡的记录（患者端强制条件，越权不可见）；
// 其余字段为可选过滤条件，由请求层完成取值与格式校验。
type Filter struct {
	OwnerPatientID  *int64
	PatientCardID   *int64
	DoctorID        *int64
	SubdepartmentID *int64
	FromDate        string
	ToDate          string
	PaymentStatus   *int16
	Sort            string
	Order           string
}

// DoctorSummary 是挂号详情内嵌的医生摘要（契约 §12.4 详情示例：id、name、job）。
type DoctorSummary struct {
	ID   int64
	Name string
	Job  string
}

// SubdepartmentSummary 是挂号详情内嵌的子科室摘要。
type SubdepartmentSummary struct {
	ID   int64
	Name string
}

// Capacity 是挂号详情内嵌的时段容量（contract §6.4：返回时段容量）。
type Capacity struct {
	Maximum   int16
	Used      int16
	Remaining int16
}

// Detail 是挂号详情（契约 §6.4、§12.4）：挂号字段 + 医生/科室摘要 + 时段容量。
type Detail struct {
	ID            int64
	PatientCardID int64
	Doctor        DoctorSummary
	Subdepartment SubdepartmentSummary
	Date          string
	Slot          int16
	Capacity      Capacity
	Amount        string
	OutTradeNo    string
	PaymentStatus string
}

// CreateInput 是创建挂号的领域入参：关联、金额与状态都由服务端从排班快照推导，
// 客户端只能提交就诊卡与时段（契约 §6.2：客户端提交的 amount、doctorId、outTradeNo 不可信）。
//
// PID 取自已校验归属的就诊卡，用于「同一身份证号同一时段」的占用判重；
// OutTradeNo 由服务端生成，作为本单的外部交易标识。
type CreateInput struct {
	PatientCardID int64
	PID           string
	ScheduleID    int64
	OutTradeNo    string
	Now           time.Time
}

// 挂号域的可识别错误。映射关系（契约 §10 错误码目录）：
// ErrCardNotFound -> 404 PATIENT_CARD_NOT_FOUND，
// ErrScheduleNotFound/ErrScheduleStarted -> 404 SCHEDULE_SLOT_NOT_FOUND，
// ErrSlotSoldOut -> 409 REGISTRATION_SLOT_SOLD_OUT，
// ErrDuplicate -> 409 REGISTRATION_DUPLICATE，
// ErrRegistrationNotFound -> 404 REGISTRATION_NOT_FOUND。
var (
	ErrCardNotFound         = errors.New("registration patient card not found")
	ErrScheduleNotFound     = errors.New("registration schedule not found")
	ErrScheduleStarted      = errors.New("registration schedule has already started")
	ErrDoctorInactive       = errors.New("registration doctor is not active")
	ErrSlotSoldOut          = errors.New("registration schedule slot is sold out")
	ErrDuplicate            = errors.New("registration already occupies the schedule for this pid")
	ErrRegistrationNotFound = errors.New("registration not found")
)

// SlotSoldOutError 表示号源不足，携带 scheduleId 与实时余量供 409 响应 details 使用
// （契约 §12.4 的 details 为 { scheduleId, remaining }）。
type SlotSoldOutError struct {
	ScheduleID int64
	Remaining  int16
}

func (e *SlotSoldOutError) Error() string { return ErrSlotSoldOut.Error() }
func (e *SlotSoldOutError) Unwrap() error { return ErrSlotSoldOut }

// NewOutTradeNo 生成挂号的外部交易号。
//
// medical_registration.out_trade_no 是 CHAR(32)，库中只有普通索引、没有唯一约束，
// 因此唯一性由服务端保证：业务时间戳（14 位）+ UUID 前 12 位十六进制，
// 同一毫秒内的并发建单也不会撞号，长度 26 位不会超出列宽。
func NewOutTradeNo(now time.Time) string {
	timestamp := now.In(businessLocation).Format("20060102150405")
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	if len(suffix) > 12 {
		suffix = suffix[:12]
	}
	return timestamp + suffix
}

// ValidateOutTradeNo 校验外部交易号可用于落库：非空且不超过 CHAR(32) 列宽。
func ValidateOutTradeNo(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return errors.New("outTradeNo is required")
	}
	if len(trimmed) > 32 {
		return fmt.Errorf("outTradeNo exceeds 32 characters: %d", len(trimmed))
	}
	return nil
}
