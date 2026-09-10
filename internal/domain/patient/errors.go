package patient

import "errors"

// 患者域的错误集合。这些错误是领域与上层之间的稳定契约：use case 用 errors.Is
// 判定后映射为 spec/04-api-contract.md 中的错误码，repository 只用它们表达
// “业务上不成立”，不把数据库错误（sql.ErrNoRows、唯一冲突）泄露给上层。
var (
	// ErrPatientNotFound 表示 patient_user 不存在。
	// 上层在鉴权路径上映射为 401 AUTH_INVALID_TOKEN（不区分“不存在”和“令牌无效”）。
	ErrPatientNotFound = errors.New("patient not found")
	// ErrCardNotFound 表示就诊卡不存在或不属于当前患者。
	// 契约要求他人就诊卡与不存在的卡统一返回 404 PATIENT_CARD_NOT_FOUND，
	// 避免调用方通过状态码枚举他人卡号，因此两者共用同一个错误。
	ErrCardNotFound = errors.New("patient card not found")
	// ErrCardExists 表示该账号已存在就诊卡，映射为 409 PATIENT_CARD_EXISTS。
	// 每个患者账号最多一张就诊卡，唯一性由事务内串行化保证。
	ErrCardExists = errors.New("patient card already exists")
	// ErrOpenIDRequired 表示微信登录返回的 openId 为空，无法建立患者账号。
	ErrOpenIDRequired = errors.New("open id is required")
	// ErrInvalidPagination 表示 offset/limit 不是合法分页区间（offset < 0 或 limit < 1）。
	// repository 主动判定，而不是把 PostgreSQL 的 2201W/2201X 原始错误冒泡成 500；
	// 上层应据此返回 422 REQUEST_VALIDATION_FAILED（spec/04-api-contract.md §1.4）。
	ErrInvalidPagination = errors.New("invalid pagination window")

	// 以下为建卡/改卡的字段级校验错误。字段名由 FieldError 承载，
	// 供响应 details.fields 使用；这里只表达“哪条规则不成立”。
	ErrNameRequired = errors.New("name is required")
	ErrNameTooLong  = errors.New("name exceeds the column length")
	ErrSexInvalid   = errors.New("sex must be 男 or 女")
	ErrPIDInvalid   = errors.New("pid is not a valid 18-digit resident id")
	// ErrSexMismatch 表示性别与身份证号第 17 位的奇偶性不一致。
	ErrSexMismatch = errors.New("sex contradicts the 17th digit of pid")
	// ErrBirthdayInFuture 表示身份证号推导出的出生日期晚于当前业务日期。
	ErrBirthdayInFuture = errors.New("birthday derived from pid is in the future")
	ErrTelInvalid       = errors.New("tel is not an 11-digit mainland mobile number")
	// ErrMedicalHistoryRequired 表示疾病史缺失或为空数组。
	ErrMedicalHistoryRequired = errors.New("medicalHistory is required")
	// ErrMedicalHistoryInvalid 表示疾病史含有约定取值之外的项。
	ErrMedicalHistoryInvalid = errors.New("medicalHistory contains an unsupported value")
	// ErrMedicalHistoryNoneConflict 表示“无”与其他疾病史同时提交（两者互斥）。
	ErrMedicalHistoryNoneConflict = errors.New("medicalHistory 无 excludes any other value")
	ErrInsuranceTypeRequired      = errors.New("insuranceType is required")
	ErrInsuranceTypeInvalid       = errors.New("insuranceType is not supported")

	// 以下为不可修改字段：PATCH 提交这些字段时按契约返回 422，
	// 而不是静默忽略（静默忽略会让客户端误以为修改已生效）。
	ErrPIDImmutable      = errors.New("pid is immutable")
	ErrUserIDImmutable   = errors.New("userId is immutable")
	ErrBirthdayImmutable = errors.New("birthday is immutable")
)

// FieldError 表示某个请求字段未通过领域校验。
//
// 它同时承载字段名与原因：字段名用于组装 422 响应中的 details.fields，
// 原因是稳定的领域错误，供上层 errors.Is 判定后生成用户可见文案。
// 文案本身不在这里拼装，避免领域层产出包含敏感信息的提示。
type FieldError struct {
	Field  string
	Reason error
}

// Error 实现 error 接口；字段名与原因是稳定标识，不含用户数据。
func (e *FieldError) Error() string {
	if e == nil {
		return ""
	}
	if e.Reason == nil {
		// 导出类型允许被零值构造，不能因为原因缺失就 panic。
		return e.Field
	}
	return e.Field + ": " + e.Reason.Error()
}

// Unwrap 让 errors.Is/errors.As 能定位到具体的领域错误。
func (e *FieldError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Reason
}

// FieldsOf 收集 err 链上全部 FieldError 的字段名，按出现顺序去重。
//
// 建卡与改卡校验会一次性收集所有字段问题并用 errors.Join 汇总，
// 因此这里必须遍历 Join 树，而不能只取第一个匹配项：契约的
// details.fields 允许同时列出多个问题字段（如 pid 与 tel）。
func FieldsOf(err error) []string {
	if err == nil {
		return nil
	}
	seen := make(map[string]bool, 4)
	fields := make([]string, 0, 4)
	collectFields(err, seen, &fields)
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// collectFields 递归遍历 errors.Join 产生的多叉错误树以及普通包装链。
func collectFields(err error, seen map[string]bool, out *[]string) {
	if err == nil {
		return
	}
	if fieldErr, ok := err.(*FieldError); ok && fieldErr.Field != "" {
		if !seen[fieldErr.Field] {
			seen[fieldErr.Field] = true
			*out = append(*out, fieldErr.Field)
		}
	}
	// errors.Join 返回的错误实现了 Unwrap() []error；必须先处理它，
	// 否则只会看到树的第一个分支。
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			collectFields(child, seen, out)
		}
		return
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		collectFields(wrapped.Unwrap(), seen, out)
	}
}
