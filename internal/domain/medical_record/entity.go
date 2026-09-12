// Package medical_record 定义病历域（hospital.doctor_prescription）的实体与业务规则。
//
// 病历是医生为「自己负责的挂号记录」书写的就诊记录：诊断结论落在 diagnosis，
// 正文落在 rp，正文的内容与格式完全由医生自行整理，本域不做结构化拆分。
// patient_card_id、doctor_id、sub_dept_id 一律由服务端从挂号记录推导，客户端只能提交
// registrationId 与病历内容，避免把病历写到他人名下（spec/04-api-contract.md 病历一节）。
//
// 本包只做纯领域计算，不依赖数据库、HTTP 或第三方 SDK；持久化由 internal/repo
// 实现 internal/port 中声明的接口。
package medical_record

import (
	"errors"
	"strings"

	"github.com/google/uuid"
)

// 诊断与正文的长度上限，按字符数（rune）计算，中文与 emoji 各算 1 个字符。
// doctor_prescription.diagnosis / rp 是无长度限制的 varchar，上限属于业务约定，
// 用于挡住异常大的正文把响应体与日志撑爆。
const (
	MaxDiagnosisRunes = 200
	MaxContentRunes   = 20000
)

// uuidPrefix 是病历业务标识前缀：doctor_prescription.uuid 是 VARCHAR(32)（库中确认），
// 契约示例形如 RX000000000000000000000000000001，本实现保持同形态（RX + 30 位大写十六进制）。
const uuidPrefix = "RX"

// 领域错误。repository 只返回这些错误，由 use case 翻译成稳定业务错误码。
var (
	// ErrNotFound 表示病历不存在，或存在但不属于调用者负责的挂号记录。
	// 两种情形共用同一错误：越权不能通过状态码枚举他人病历，与挂号域的越权隐藏口径一致。
	ErrNotFound = errors.New("medical record not found")
	// ErrRegistrationNotFound 表示挂号不存在，或该挂号不由调用者负责。
	ErrRegistrationNotFound = errors.New("medical record registration not found")
	// ErrDuplicate 表示同一挂号已经有病历：同一挂号只允许一份病历。
	ErrDuplicate = errors.New("medical record already exists for this registration")
	// ErrDiagnosisRequired 表示诊断为空。
	ErrDiagnosisRequired = errors.New("medical record diagnosis is required")
	// ErrDiagnosisTooLong 表示诊断超过 MaxDiagnosisRunes。
	ErrDiagnosisTooLong = errors.New("medical record diagnosis is too long")
	// ErrContentRequired 表示病历正文为空。
	ErrContentRequired = errors.New("medical record content is required")
	// ErrContentTooLong 表示病历正文超过 MaxContentRunes。
	ErrContentTooLong = errors.New("medical record content is too long")
)

// MedicalRecord 映射 hospital.doctor_prescription（病历的存储表）。
//
// 实体不带 json tag，也不直接序列化：对外字段集合是契约的一部分，
// 统一由 transport/http/response 的 DTO 投影，避免领域结构体调整时静默改变接口形状。
type MedicalRecord struct {
	ID              int64
	UUID            string
	RegistrationID  int64
	PatientCardID   int64
	DoctorID        int64
	SubdepartmentID int64
	Diagnosis       string
	// Content 对应 doctor_prescription.rp，是医生自行整理的病历正文。
	Content string
}

// RegistrationOwner 是书写病历前必须确认的挂号归属信息，全部取自 medical_registration。
//
// 归属以挂号记录的 doctor_id 为准，而不是 doctor_prescription.doctor_id：
// 历史数据可能不一致，且「自己负责的挂号记录」才是本功能的授权边界。
type RegistrationOwner struct {
	RegistrationID  int64
	PatientCardID   int64
	DoctorID        int64
	SubdepartmentID int64
}

// Filter 是病历列表的查询条件。
//
// OwnerDoctorID 由调用者身份（mis_user.ref_id 解析出的医生编号）强制推导，客户端不可指定；
// 仓储把它当必填条件使用（fail closed），其余字段是客户端可选过滤条件，取值与格式由请求层校验。
type Filter struct {
	OwnerDoctorID  int64
	RegistrationID *int64
	PatientCardID  *int64
	DoctorID       *int64
}

// UpdateInput 是病历修改（PATCH）入参：nil 表示该字段未提交，只更新提交的字段。
// 两个字段都为 nil 的请求由请求层拦截为 422，不进入 repository。
type UpdateInput struct {
	Diagnosis *string
	Content   *string
}

// NewUUID 生成病历业务标识：RX 前缀 + 30 位大写十六进制，总长度 32，与 uuid 列宽一致。
func NewUUID() string {
	raw := strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))
	return uuidPrefix + raw[:30]
}

// NormalizeDiagnosis 归一化并校验诊断结论：去除首尾空白后必须非空且不超过 MaxDiagnosisRunes。
func NormalizeDiagnosis(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", ErrDiagnosisRequired
	}
	if len([]rune(value)) > MaxDiagnosisRunes {
		return "", ErrDiagnosisTooLong
	}
	return value, nil
}

// NormalizeContent 归一化并校验病历正文：去除首尾空白后必须非空且不超过 MaxContentRunes。
// 仅裁剪首尾空白，正文内部的换行与排版原样保留，医生可自行整理格式。
func NormalizeContent(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", ErrContentRequired
	}
	if len([]rune(value)) > MaxContentRunes {
		return "", ErrContentTooLong
	}
	return value, nil
}
