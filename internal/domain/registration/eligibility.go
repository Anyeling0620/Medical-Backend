package registration

import (
	"strings"

	"Medical-Web-Backend/internal/domain/patient"
)

// 资格校验 reasons 词表（spec/04-api-contract.md §6.1）。
//
// 这是独立的词表，不是 HTTP 错误码：不满足资格仍返回 200，用 eligible=false 与
// 本词表中的稳定取值表达原因；前端只按词表展示，不依赖返回顺序。
// 同名 token（如 REGISTRATION_DUPLICATE 同时出现在 HTTP 错误码目录）表达的是
// 另一种语境（建单冲突而不是资格原因），实现与测试都不得混用。
const (
	// ReasonCardInvalid 表示就诊卡不存在或不属于当前患者。
	ReasonCardInvalid = "PATIENT_CARD_INVALID"
	// ReasonProfileIncomplete 表示就诊卡资料不完整（建卡必填项在库中为空）。
	ReasonProfileIncomplete = "PATIENT_PROFILE_INCOMPLETE"
	// ReasonPatientDisabled 表示持卡患者账号已被禁用。
	ReasonPatientDisabled = "PATIENT_DISABLED"
	// ReasonScheduleNotFound 表示时段不存在或已不可挂号（医生不在诊、关联缺失）。
	ReasonScheduleNotFound = "SCHEDULE_NOT_FOUND"
	// ReasonScheduleStarted 表示时段已开始或已过期。
	ReasonScheduleStarted = "SCHEDULE_STARTED"
	// ReasonSoldOut 表示剩余号源为 0。
	ReasonSoldOut = "SCHEDULE_SOLD_OUT"
	// ReasonDuplicate 表示同一身份证号已占用该时段。
	ReasonDuplicate = "REGISTRATION_DUPLICATE"
)

// ProfileComplete 判定就诊卡资料是否完整（契约 §6.1 的 PATIENT_PROFILE_INCOMPLETE）。
//
// 建卡接口一次性要求姓名、性别、身份证号、手机号、疾病史、医保类型六项必填
// （契约 §7.4），其中出生日期由身份证号推导；因此「不完整」意味着库里存在
// 建档时被绕过或历史导入留下的空值。这里按业务必需的六项判定，
// 任一为空即视为资料不完整，不能放行挂号（否则挂号单无法支撑就诊流程）。
func ProfileComplete(card patient.Card) bool {
	if strings.TrimSpace(card.Name) == "" ||
		strings.TrimSpace(card.Sex) == "" ||
		strings.TrimSpace(card.PID) == "" ||
		strings.TrimSpace(card.Tel) == "" ||
		strings.TrimSpace(card.Birthday) == "" ||
		strings.TrimSpace(card.InsuranceType) == "" {
		return false
	}
	// 疾病史以 JSON 数组存放，空数组或解析失败都视为缺失。
	return len(card.MedicalHistory) > 0
}

// NewEligibility 组装资格校验结果：reasons 为空即资格通过。
//
// reasons 按固定顺序追加（就诊卡 -> 资料 -> 患者状态 -> 时段 -> 余量 -> 判重），
// 保证同一输入下 reasons 稳定可比；空值统一输出 [] 而不是 null。
func NewEligibility(remaining int16, amount *string, reasons []string) *Eligibility {
	if reasons == nil {
		reasons = make([]string, 0)
	}
	return &Eligibility{
		Eligible:  len(reasons) == 0,
		Remaining: remaining,
		Amount:    amount,
		Reasons:   reasons,
	}
}
