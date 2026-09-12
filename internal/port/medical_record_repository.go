package port

import (
	"context"

	"Medical-Web-Backend/internal/domain/medical_record"
)

// MedicalRecordRepository 描述病历（hospital.doctor_prescription）持久化所需的读写能力。
//
// ownerDoctorID 是必填的归属条件：所有读写都要求病历所属挂号满足
// medical_registration.doctor_id = ownerDoctorID，不满足一律按资源不存在处理
// （medical_record.ErrNotFound / ErrRegistrationNotFound），避免用状态码枚举他人数据。
// 该值由 use case 从 mis_user.ref_id 解析，客户端不可提交；误传 0 只会匹配不到记录
// （fail closed），不存在「0 表示不限定归属」的全量通道（契约 §1.2、§6.10）。
//
// 归属判定始终以挂号记录的 doctor_id 为准，不采信 doctor_prescription.doctor_id：
// 该列由本域写入，但历史数据可能与挂号不一致，授权边界必须是「自己负责的挂号记录」。
type MedicalRecordRepository interface {
	// FindDoctorIDByUserID 读取 mis_user.ref_id，即管理端账号绑定的医生编号。
	// 账号不存在或未绑定医生身份（ref_id 为空或非正数）时返回 0，由 use case 按 403 拒绝。
	FindDoctorIDByUserID(ctx context.Context, userID int64) (int64, error)
	// FindRegistrationOwner 读取挂号归属信息（患者就诊卡、医生、子科室）。
	// 挂号不存在、或 doctor_id 不等于 ownerDoctorID 时返回
	// medical_record.ErrRegistrationNotFound。
	FindRegistrationOwner(ctx context.Context, registrationID, ownerDoctorID int64) (*medical_record.RegistrationOwner, error)
	// CreateMedicalRecord 在单事务内完成「锁定挂号行 -> 校验同一挂号尚无病历 -> 插入病历」。
	//
	// 挂号行加锁（SELECT ... FOR UPDATE）保证同一挂号并发书写只有一个请求成功，
	// 另一个返回 medical_record.ErrDuplicate；病历的 patient_card_id、doctor_id、
	// sub_dept_id 全部取自挂号行，调用方无法把病历写到他人名下。
	CreateMedicalRecord(ctx context.Context, owner medical_record.RegistrationOwner, recordUUID, diagnosis, content string) (*medical_record.MedicalRecord, error)
	// ListMedicalRecords 按过滤条件分页返回病历并统计总数。
	// doctor_prescription 没有时间列，排序固定为 id 倒序（最近书写的病历在前）。
	ListMedicalRecords(ctx context.Context, f medical_record.Filter, offset, limit int) ([]medical_record.MedicalRecord, int64, error)
	// FindMedicalRecord 读取病历详情；病历不存在或不属于 ownerDoctorID 负责的挂号时
	// 返回 medical_record.ErrNotFound。
	FindMedicalRecord(ctx context.Context, id, ownerDoctorID int64) (*medical_record.MedicalRecord, error)
	// UpdateMedicalRecord 只更新提交的字段并返回更新后的病历；
	// 病历不存在或不属于 ownerDoctorID 负责的挂号时返回 medical_record.ErrNotFound。
	UpdateMedicalRecord(ctx context.Context, id, ownerDoctorID int64, input medical_record.UpdateInput) (*medical_record.MedicalRecord, error)
	// DeleteMedicalRecord 删除病历；病历不存在或不属于 ownerDoctorID 负责的挂号时
	// 返回 medical_record.ErrNotFound（重复删除同样按不存在处理，由调用方映射 404）。
	DeleteMedicalRecord(ctx context.Context, id, ownerDoctorID int64) error
}
