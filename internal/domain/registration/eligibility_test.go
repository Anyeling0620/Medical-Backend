package registration

import (
	"reflect"
	"testing"

	"Medical-Web-Backend/internal/domain/patient"
)

// 本文件覆盖挂号资格校验的领域规则（spec/04-api-contract.md §6.1、§12.4）：
// 就诊卡资料完整性判定、reasons 词表取值，以及资格结果的组装语义。
// 资格不通过不是错误，因此这里只断言领域结果，不涉及 HTTP 映射。

// completeRegistrationCard 返回一张六项必填齐全、疾病史非空的就诊卡，
// 作为「资料完整」的基准；各用例只改动一个字段以定位缺失项。
func completeRegistrationCard() patient.Card {
	return patient.Card{
		ID:             10,
		UserID:         20,
		Name:           "张三",
		Sex:            patient.SexMale,
		PID:            "110101199001011237",
		Tel:            "13800138000",
		Birthday:       "1990-01-01",
		MedicalHistory: []string{"高血压"},
		InsuranceType:  "社会基本医疗保险",
	}
}

// TestProfileCompleteAcceptsFullyFilledCard 六项必填齐全且疾病史非空时资料完整。
func TestProfileCompleteAcceptsFullyFilledCard(t *testing.T) {
	if !ProfileComplete(completeRegistrationCard()) {
		t.Error("完整就诊卡应判定为资料完整")
	}
}

// TestProfileCompleteRejectsMissingRequiredFields 姓名、性别、身份证号、手机号、
// 出生日期、医保类型任一为空（含纯空白）都判定为资料不完整
// （契约 §6.1 的 PATIENT_PROFILE_INCOMPLETE）。
func TestProfileCompleteRejectsMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(card *patient.Card)
	}{
		{"缺姓名", func(card *patient.Card) { card.Name = "" }},
		{"缺性别", func(card *patient.Card) { card.Sex = "" }},
		{"缺身份证号", func(card *patient.Card) { card.PID = "" }},
		{"缺手机号", func(card *patient.Card) { card.Tel = "" }},
		{"缺出生日期", func(card *patient.Card) { card.Birthday = "" }},
		{"缺医保类型", func(card *patient.Card) { card.InsuranceType = "" }},
		{"姓名仅空白", func(card *patient.Card) { card.Name = "   " }},
		{"手机号仅空白", func(card *patient.Card) { card.Tel = "\t " }},
		{"疾病史为空切片", func(card *patient.Card) { card.MedicalHistory = []string{} }},
		{"疾病史为 nil", func(card *patient.Card) { card.MedicalHistory = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := completeRegistrationCard()
			tc.mutate(&card)
			if ProfileComplete(card) {
				t.Errorf("资料不完整（%s）时不应判定为完整：%+v", tc.name, card)
			}
		})
	}
}

// TestEligibilityReasonVocabulary reasons 是独立词表（不是 HTTP 错误码），
// 取值必须与契约 §6.1 完全一致，前端按词表展示原因。
func TestEligibilityReasonVocabulary(t *testing.T) {
	want := map[string]string{
		"ReasonCardInvalid":       "PATIENT_CARD_INVALID",
		"ReasonProfileIncomplete": "PATIENT_PROFILE_INCOMPLETE",
		"ReasonPatientDisabled":   "PATIENT_DISABLED",
		"ReasonScheduleNotFound":  "SCHEDULE_NOT_FOUND",
		"ReasonScheduleStarted":   "SCHEDULE_STARTED",
		"ReasonSoldOut":           "SCHEDULE_SOLD_OUT",
		"ReasonDuplicate":         "REGISTRATION_DUPLICATE",
	}
	got := map[string]string{
		"ReasonCardInvalid":       ReasonCardInvalid,
		"ReasonProfileIncomplete": ReasonProfileIncomplete,
		"ReasonPatientDisabled":   ReasonPatientDisabled,
		"ReasonScheduleNotFound":  ReasonScheduleNotFound,
		"ReasonScheduleStarted":   ReasonScheduleStarted,
		"ReasonSoldOut":           ReasonSoldOut,
		"ReasonDuplicate":         ReasonDuplicate,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reasons 词表 = %#v, want %#v", got, want)
	}
}

// TestNewEligibilityWithoutReasons reasons 为空即资格通过，且必须输出空切片而不是 null
// （契约 §1.3、§12.4：reasons: []）。
func TestNewEligibilityWithoutReasons(t *testing.T) {
	amount := "80.00"
	result := NewEligibility(2, &amount, nil)

	if !result.Eligible {
		t.Error("reasons 为空时 eligible 必须为 true")
	}
	if result.Reasons == nil {
		t.Fatal("reasons 不允许为 nil（响应会输出 null）")
	}
	if len(result.Reasons) != 0 {
		t.Errorf("reasons = %#v, want 空切片", result.Reasons)
	}
	if result.Remaining != 2 {
		t.Errorf("remaining = %d, want 2", result.Remaining)
	}
	if result.Amount == nil || *result.Amount != amount {
		t.Errorf("amount = %v, want %q", result.Amount, amount)
	}
}

// TestNewEligibilityWithReasons 有原因时 eligible=false，且原因顺序与传入一致
// （调用方按固定顺序追加，保证同一输入下结果稳定可比）。
func TestNewEligibilityWithReasons(t *testing.T) {
	reasons := []string{ReasonProfileIncomplete, ReasonSoldOut}
	result := NewEligibility(0, nil, reasons)

	if result.Eligible {
		t.Error("reasons 非空时 eligible 必须为 false")
	}
	if !reflect.DeepEqual(result.Reasons, reasons) {
		t.Errorf("reasons = %#v, want %#v", result.Reasons, reasons)
	}
	if result.Amount != nil {
		t.Errorf("amount = %v, want nil（无法给出金额时输出 null）", *result.Amount)
	}
}
