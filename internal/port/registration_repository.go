package port

import (
	"context"
	"time"

	"Medical-Web-Backend/internal/domain/registration"
)

// RegistrationRepository 描述挂号（medical_registration）持久化所需的读写能力。
//
// 号源扣减、占用判重与状态校验属于业务规则：repository 只在事务内原子执行，
// 并返回领域错误（号源不足、重复挂号、时段已开始），不替 use case 决定 HTTP 语义；
// 调用方必须先完成就诊卡归属与患者状态校验（见 usecase/registration）。
type RegistrationRepository interface {
	// FindScheduleSnapshot 读取时段及其所属计划、医生与价目快照。
	// 时段不存在、或所属计划/医生/子科室关联缺失（脏数据）时返回
	// registration.ErrScheduleNotFound。
	FindScheduleSnapshot(ctx context.Context, scheduleID int64) (*registration.ScheduleSnapshot, error)
	// HasOccupyingRegistration 判断同一身份证号在同一时段是否已有占用中的挂号。
	//
	// 判定跨账号生效：medical_registration 关联 patient_user_info_card 后按 pid 过滤，
	// 且使用 EXISTS 语义——同一 pid 可能对应多张就诊卡，不能把 JOIN 结果当成唯一行计数
	// （契约 §6.1）。占用状态为 PAID 与未超期的 UNPAID，EXPIRED/REFUNDED 不占用。
	HasOccupyingRegistration(ctx context.Context, pid string, scheduleID int64, now time.Time) (bool, error)
	// CreateRegistration 在单事务内完成「重新读取关联与金额 -> 校验时段与号源 ->
	// 同步递增时段级与计划级 num -> 插入挂号记录」，返回已创建的挂号。
	//
	// 任一环节失败整单回滚，不会留下半条记录或永久吞号；号源不足返回
	// *registration.SlotSoldOutError（携带实时余量），重复挂号返回
	// registration.ErrDuplicate，时段已开始返回 registration.ErrScheduleStarted，
	// 医生不在诊返回 registration.ErrDoctorInactive。
	CreateRegistration(ctx context.Context, input registration.CreateInput) (*registration.Registration, error)
	// ListRegistrations 按过滤条件分页返回挂号记录，并按 f.Sort/f.Order 排序，返回总数。
	// 调用方必须先按契约 §1.4 校验 page/pageSize 再换算 offset/limit。
	ListRegistrations(ctx context.Context, f registration.Filter, offset, limit int) ([]registration.Registration, int64, error)
	// FindRegistrationDetail 读取挂号详情（含医生/科室摘要与时段容量）。
	//
	// ownerPatientID > 0 时同时限定「该挂号属于此患者名下的就诊卡」，用于患者端的
	// 越权隐藏：他人记录与不存在的记录统一返回 registration.ErrRegistrationNotFound，
	// 调用方无法据此枚举患者数据；管理端传 0 表示不限定归属（契约 §6.4、§1.2）。
	FindRegistrationDetail(ctx context.Context, registrationID int64, ownerPatientID int64) (*registration.Detail, error)
}
