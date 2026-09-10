package patient

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 以下身份证号均按 GB 11643 校验位算法手工构造，保证单测不依赖随机数据：
//   - testPIDMale   ：1990-01-01，第 17 位 3（奇，男），校验位 7
//   - testPIDFemale ：1990-01-01，第 17 位 0（偶，女），校验位 2
//   - testPIDLowerX ：与 testPIDMale 同日期同性别，校验位为 X，用于验证小写 x 规范化
//   - testPIDFuture ：2099-01-01，校验位正确，用于验证“出生日期不得晚于业务日期”
//   - testPIDBadDay ：2026-02-30（不存在），校验位正确，用于验证日期真实性校验
const (
	testPIDMale   = "110101199001011237"
	testPIDFemale = "110101199001011202"
	testPIDLowerX = "11010119900101127x"
	testPIDFuture = "11010120990101013X"
	testPIDBadDay = "110101202602301234"
)

// testNow 是单测使用的固定时刻，让“业务日期”与“未来出生日期”断言保持稳定。
var testNow = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

// validCardInput 返回一份全字段合法的建卡入参，用例只需覆盖要验证的字段。
func validCardInput() CardInput {
	return CardInput{
		Name:           "张三",
		Sex:            SexMale,
		PID:            testPIDMale,
		Tel:            "13800138000",
		MedicalHistory: []string{"高血压", "糖尿病"},
		InsuranceType:  "社会基本医疗保险",
	}
}

// firstErr 丢弃校验函数的返回值，只取错误，便于表驱动用例收集期望错误。
func firstErr[T any](_ T, err error) error {
	return err
}

// containsString 报告切片中是否包含目标字符串。
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestParsePID 覆盖合法号推导、大小写与空白规范化，以及各类非法输入。
func TestParsePID(t *testing.T) {
	cases := []struct {
		name    string
		pid     string
		want    PIDInfo
		wantErr error
	}{
		{"合法男号", testPIDMale, PIDInfo{Birthday: "1990-01-01", Sex: SexMale}, nil},
		{"合法女号", testPIDFemale, PIDInfo{Birthday: "1990-01-01", Sex: SexFemale}, nil},
		{"末位小写x规范化", testPIDLowerX, PIDInfo{Birthday: "1990-01-01", Sex: SexMale}, nil},
		{"首尾空白可容忍", "  " + testPIDMale + "  ", PIDInfo{Birthday: "1990-01-01", Sex: SexMale}, nil},
		{"空串", "", PIDInfo{}, ErrPIDInvalid},
		{"长度不足", "11010119900101123", PIDInfo{}, ErrPIDInvalid},
		{"长度超长", testPIDMale + "1", PIDInfo{}, ErrPIDInvalid},
		{"含非数字", "11010119900101A237", PIDInfo{}, ErrPIDInvalid},
		{"校验位错误", "110101199001011238", PIDInfo{}, ErrPIDInvalid},
		{"地址码全零", "000000199001011237", PIDInfo{}, ErrPIDInvalid},
		{"日期不存在", testPIDBadDay, PIDInfo{}, ErrPIDInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePID(tc.pid)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParsePID(%q) 错误 = %v，期望 %v", tc.pid, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePID(%q) 意外错误：%v", tc.pid, err)
			}
			if got != tc.want {
				t.Fatalf("ParsePID(%q) = %+v，期望 %+v", tc.pid, got, tc.want)
			}
		})
	}
}

// TestMaskPID 验证脱敏格式，并确认非 18 位输入不会泄露原文。
func TestMaskPID(t *testing.T) {
	cases := []struct {
		name string
		pid  string
		want string
	}{
		{"18位脱敏", testPIDMale, "110101********1237"},
		{"空串返回空串", "", ""},
		{"17位等长星号", "11010119900101123", strings.Repeat("*", 17)},
		{"非数字等长星号", "abc", "***"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MaskPID(tc.pid)
			if got != tc.want {
				t.Fatalf("MaskPID(%q) = %q，期望 %q", tc.pid, got, tc.want)
			}
			if tc.pid != "" && !strings.Contains(got, "*") {
				t.Fatalf("MaskPID(%q) = %q，脱敏结果必须包含星号", tc.pid, got)
			}
			if tc.pid != "" && got == tc.pid {
				t.Fatalf("MaskPID(%q) 原样返回，未脱敏", tc.pid)
			}
		})
	}
}

// TestValidateName 验证必填，以及长度上限按“字符数”而非“字节数”计算。
func TestValidateName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{"正常中文", "张三", "张三", nil},
		{"去除首尾空白", "  张三  ", "张三", nil},
		{"恰好20个中文字符", strings.Repeat("张", 20), strings.Repeat("张", 20), nil},
		{"21个中文字符", strings.Repeat("张", 21), "", ErrNameTooLong},
		{"空串", "", "", ErrNameRequired},
		{"全空白", "   ", "", ErrNameRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateName(tc.input)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ValidateName(%q) 错误 = %v，期望 %v", tc.input, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateName(%q) 意外错误：%v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("ValidateName(%q) = %q，期望 %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestValidateTel 验证大陆手机号规则（11 位、1 开头、第二位 3-9）。
func TestValidateTel(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{"合法号码", "13800138000", "13800138000", nil},
		{"去除首尾空白", " 13800138000 ", "13800138000", nil},
		{"第二位为2", "12345678901", "", ErrTelInvalid},
		{"第二位为1", "11800138000", "", ErrTelInvalid},
		{"第二位为0", "10800138000", "", ErrTelInvalid},
		{"10位", "1380013800", "", ErrTelInvalid},
		{"12位", "138001380000", "", ErrTelInvalid},
		{"中间含空格", "138 00138000", "", ErrTelInvalid},
		{"含字母", "1380013800a", "", ErrTelInvalid},
		{"空串", "", "", ErrTelInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateTel(tc.input)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ValidateTel(%q) 错误 = %v，期望 %v", tc.input, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateTel(%q) 意外错误：%v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("ValidateTel(%q) = %q，期望 %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestValidateSex 验证性别只接受“男/女”。
func TestValidateSex(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{"男", "男", "男", nil},
		{"女", "女", "女", nil},
		{"去除首尾空白", " 男 ", "男", nil},
		{"英文M", "M", "", ErrSexInvalid},
		{"未知取值", "未知", "", ErrSexInvalid},
		{"空串", "", "", ErrSexInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateSex(tc.input)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ValidateSex(%q) 错误 = %v，期望 %v", tc.input, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateSex(%q) 意外错误：%v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("ValidateSex(%q) = %q，期望 %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestValidateMedicalHistory 覆盖必填、“无”互斥、非法取值与去重。
func TestValidateMedicalHistory(t *testing.T) {
	cases := []struct {
		name    string
		input   []string
		want    []string
		wantErr error
	}{
		{"仅无", []string{"无"}, []string{"无"}, nil},
		{"多种疾病", []string{"高血压", "糖尿病"}, []string{"高血压", "糖尿病"}, nil},
		{"无与疾病互斥", []string{"无", "高血压"}, nil, ErrMedicalHistoryNoneConflict},
		{"未知取值", []string{"口吃"}, nil, ErrMedicalHistoryInvalid},
		{"空字符串项", []string{""}, nil, ErrMedicalHistoryInvalid},
		{"重复项去重", []string{"高血压", "高血压"}, []string{"高血压"}, nil},
		{"去重前先去除空白", []string{"高血压", " 高血压 "}, []string{"高血压"}, nil},
		{"无重复仍合法", []string{"无", "无"}, []string{"无"}, nil},
		{"nil", nil, nil, ErrMedicalHistoryRequired},
		{"空切片", []string{}, nil, ErrMedicalHistoryRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateMedicalHistory(tc.input)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ValidateMedicalHistory(%v) 错误 = %v，期望 %v", tc.input, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateMedicalHistory(%v) 意外错误：%v", tc.input, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ValidateMedicalHistory(%v) = %v，期望 %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestValidateInsuranceType 验证医保类型的合法全集与错误分支。
func TestValidateInsuranceType(t *testing.T) {
	for _, option := range InsuranceTypeOptions() {
		t.Run("合法_"+option, func(t *testing.T) {
			got, err := ValidateInsuranceType(option)
			if err != nil {
				t.Fatalf("ValidateInsuranceType(%q) 意外错误：%v", option, err)
			}
			if got != option {
				t.Fatalf("ValidateInsuranceType(%q) = %q", option, got)
			}
		})
	}
	cases := []struct {
		name    string
		input   string
		wantErr error
	}{
		{"空串必填", "", ErrInsuranceTypeRequired},
		{"全空白必填", "   ", ErrInsuranceTypeRequired},
		{"非全集取值", "商业保险", ErrInsuranceTypeInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ValidateInsuranceType(tc.input); !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateInsuranceType(%q) 错误 = %v，期望 %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

// TestFieldErrorContract 验证每个校验错误都能被 errors.Is 命中，
// 且 FieldsOf 能取到 API 契约使用的 JSON 字段名。
func TestFieldErrorContract(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantField string
		wantErr   error
	}{
		{"姓名必填", firstErr(ValidateName("")), "name", ErrNameRequired},
		{"姓名超长", firstErr(ValidateName(strings.Repeat("张", 21))), "name", ErrNameTooLong},
		{"性别非法", firstErr(ValidateSex("M")), "sex", ErrSexInvalid},
		{"手机号非法", firstErr(ValidateTel("123")), "tel", ErrTelInvalid},
		{"疾病史必填", firstErr(ValidateMedicalHistory(nil)), "medicalHistory", ErrMedicalHistoryRequired},
		{"疾病史非法", firstErr(ValidateMedicalHistory([]string{"口吃"})), "medicalHistory", ErrMedicalHistoryInvalid},
		{"疾病史互斥", firstErr(ValidateMedicalHistory([]string{"无", "高血压"})), "medicalHistory", ErrMedicalHistoryNoneConflict},
		{"医保类型必填", firstErr(ValidateInsuranceType("")), "insuranceType", ErrInsuranceTypeRequired},
		{"医保类型非法", firstErr(ValidateInsuranceType("商业保险")), "insuranceType", ErrInsuranceTypeInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.err, tc.wantErr) {
				t.Fatalf("errors.Is(%v, %v) = false", tc.err, tc.wantErr)
			}
			fields := FieldsOf(tc.err)
			if !reflect.DeepEqual(fields, []string{tc.wantField}) {
				t.Fatalf("FieldsOf(%v) = %v，期望 [%s]", tc.err, fields, tc.wantField)
			}
		})
	}
	if got := FieldsOf(nil); got != nil {
		t.Fatalf("FieldsOf(nil) = %v，期望 nil", got)
	}
}

// TestNewCardCollectsEveryFieldError 是本包的关键设计断言：
// 建卡时必须一次性汇总全部字段问题，details.fields 才能同时列出多个字段。
func TestNewCardCollectsEveryFieldError(t *testing.T) {
	_, err := NewCard(7, "", CardInput{Name: "张三"}, testNow)
	if err == nil {
		t.Fatal("只填姓名时应当返回字段校验错误")
	}
	for _, want := range []error{
		ErrSexInvalid, ErrPIDInvalid, ErrTelInvalid,
		ErrMedicalHistoryRequired, ErrInsuranceTypeRequired,
	} {
		if !errors.Is(err, want) {
			t.Errorf("errors.Is(err, %v) = false，实际错误：%v", want, err)
		}
	}
	fields := FieldsOf(err)
	if len(fields) != 5 {
		t.Fatalf("FieldsOf(err) = %v，期望恰好 5 个去重字段", fields)
	}
	for _, want := range []string{"sex", "pid", "tel", "medicalHistory", "insuranceType"} {
		if !containsString(fields, want) {
			t.Errorf("FieldsOf(err) = %v，缺少字段 %q", fields, want)
		}
	}
}

// TestNewCardSexMismatch 验证性别与身份证号第 17 位奇偶性不一致时被拒绝。
func TestNewCardSexMismatch(t *testing.T) {
	input := validCardInput()
	input.Sex = SexFemale // 身份证号第 17 位为 3（奇，男）
	_, err := NewCard(7, "", input, testNow)
	if !errors.Is(err, ErrSexMismatch) {
		t.Fatalf("错误 = %v，期望命中 ErrSexMismatch", err)
	}
	if fields := FieldsOf(err); !reflect.DeepEqual(fields, []string{"sex"}) {
		t.Fatalf("FieldsOf(err) = %v，期望 [sex]", fields)
	}
}

// TestNewCardBirthdayInFuture 验证校验位正确但日期在未来的身份证号被拒绝。
func TestNewCardBirthdayInFuture(t *testing.T) {
	input := validCardInput()
	input.PID = testPIDFuture
	input.Sex = SexMale
	_, err := NewCard(7, "", input, testNow)
	if !errors.Is(err, ErrBirthdayInFuture) {
		t.Fatalf("错误 = %v，期望命中 ErrBirthdayInFuture", err)
	}
	if fields := FieldsOf(err); !reflect.DeepEqual(fields, []string{"pid"}) {
		t.Fatalf("FieldsOf(err) = %v，期望 [pid]", fields)
	}
}

// TestNewCardSuccess 验证成功建卡：出生日期由身份证号推导、uuid 自动生成。
func TestNewCardSuccess(t *testing.T) {
	card, err := NewCard(7, "", validCardInput(), testNow)
	if err != nil {
		t.Fatalf("NewCard 意外错误：%v", err)
	}
	if card.UserID != 7 {
		t.Errorf("UserID = %d，期望 7", card.UserID)
	}
	if card.Name != "张三" {
		t.Errorf("Name = %q，期望 张三", card.Name)
	}
	if card.Sex != SexMale {
		t.Errorf("Sex = %q，期望 %q", card.Sex, SexMale)
	}
	if card.PID != testPIDMale {
		t.Errorf("PID = %q，期望保留完整号码 %q", card.PID, testPIDMale)
	}
	if card.Birthday != "1990-01-01" {
		t.Errorf("Birthday = %q，期望由身份证号推导为 1990-01-01", card.Birthday)
	}
	if card.Tel != "13800138000" {
		t.Errorf("Tel = %q", card.Tel)
	}
	if !reflect.DeepEqual(card.MedicalHistory, []string{"高血压", "糖尿病"}) {
		t.Errorf("MedicalHistory = %v", card.MedicalHistory)
	}
	if card.InsuranceType != "社会基本医疗保险" {
		t.Errorf("InsuranceType = %q", card.InsuranceType)
	}
	if card.ExistFaceModel {
		t.Error("新建卡 ExistFaceModel 必须为 false")
	}
	if len(card.UUID) != 32 {
		t.Errorf("UUID 长度 = %d，期望 32：%q", len(card.UUID), card.UUID)
	}
	if strings.Contains(card.UUID, "-") {
		t.Errorf("UUID 不应包含横线：%q", card.UUID)
	}
}

// TestNewCardKeepsProvidedUUID 验证显式传入 uuid 时不被覆盖（测试与迁移场景需要确定性）。
func TestNewCardKeepsProvidedUUID(t *testing.T) {
	const provided = "abcdef0123456789abcdef0123456789"
	card, err := NewCard(7, provided, validCardInput(), testNow)
	if err != nil {
		t.Fatalf("NewCard 意外错误：%v", err)
	}
	if card.UUID != provided {
		t.Fatalf("UUID = %q，期望保留传入值 %q", card.UUID, provided)
	}
}

// TestNewCardUUIDFormat 验证自动生成的 uuid 固定为 32 位无横线且不重复。
func TestNewCardUUIDFormat(t *testing.T) {
	first := NewCardUUID()
	if len(first) != 32 {
		t.Fatalf("NewCardUUID() 长度 = %d，期望 32：%q", len(first), first)
	}
	if strings.Contains(first, "-") {
		t.Fatalf("NewCardUUID() 不应包含横线：%q", first)
	}
	if second := NewCardUUID(); second == first {
		t.Fatalf("两次生成的 uuid 相同：%q", first)
	}
}

// mustCard 构造一张合法就诊卡，供改卡与投影用例复用。
func mustCard(t *testing.T) Card {
	t.Helper()
	card, err := NewCard(7, "", validCardInput(), testNow)
	if err != nil {
		t.Fatalf("构造就诊卡失败：%v", err)
	}
	return card
}

// TestApplyUpdateImmutables 验证提交不可修改字段时返回对应的领域错误。
func TestApplyUpdateImmutables(t *testing.T) {
	base := mustCard(t)

	pid := testPIDFemale
	userID := int64(99)
	birthday := "2000-01-01"
	cases := []struct {
		name    string
		update  CardUpdate
		wantErr error
	}{
		{"pid 不可修改", CardUpdate{PID: &pid}, ErrPIDImmutable},
		{"userId 不可修改", CardUpdate{UserID: &userID}, ErrUserIDImmutable},
		{"birthday 不可修改", CardUpdate{Birthday: &birthday}, ErrBirthdayImmutable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := base.ApplyUpdate(tc.update)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("错误 = %v，期望 %v", err, tc.wantErr)
			}
			if !reflect.DeepEqual(got, Card{}) {
				t.Fatalf("出错时不应返回半成品卡：%+v", got)
			}
			if fields := FieldsOf(err); len(fields) != 1 {
				t.Fatalf("FieldsOf(err) = %v，期望恰好 1 个字段", fields)
			}
		})
	}
}

// TestApplyUpdatePartial 验证部分更新只改指定字段，其余字段保持不变。
func TestApplyUpdatePartial(t *testing.T) {
	base := mustCard(t)
	newTel := "13900139000"

	updated, err := base.ApplyUpdate(CardUpdate{Tel: &newTel})
	if err != nil {
		t.Fatalf("ApplyUpdate 意外错误：%v", err)
	}
	if updated.Tel != newTel {
		t.Fatalf("Tel = %q，期望 %q", updated.Tel, newTel)
	}
	if updated.ID != base.ID || updated.UserID != base.UserID || updated.UUID != base.UUID ||
		updated.Name != base.Name || updated.Sex != base.Sex || updated.PID != base.PID ||
		updated.Birthday != base.Birthday || updated.InsuranceType != base.InsuranceType ||
		updated.ExistFaceModel != base.ExistFaceModel {
		t.Fatalf("未提交的字段发生了变化：%+v vs %+v", updated, base)
	}
	if !reflect.DeepEqual(updated.MedicalHistory, base.MedicalHistory) {
		t.Fatalf("未提交疾病史时不应修改：%v", updated.MedicalHistory)
	}
}

// TestApplyUpdateReturnsCopy 验证 ApplyUpdate 不修改原实体（含底层切片）。
func TestApplyUpdateReturnsCopy(t *testing.T) {
	base := mustCard(t)
	before := base
	beforeHistory := append([]string(nil), base.MedicalHistory...)

	newName := "李四"
	newHistory := []string{"癫痫"}
	if _, err := base.ApplyUpdate(CardUpdate{Name: &newName, MedicalHistory: &newHistory}); err != nil {
		t.Fatalf("ApplyUpdate 意外错误：%v", err)
	}
	if !reflect.DeepEqual(base, before) {
		t.Fatalf("原卡被修改：%+v，期望 %+v", base, before)
	}
	if !reflect.DeepEqual(base.MedicalHistory, beforeHistory) {
		t.Fatalf("原卡疾病史被修改：%v，期望 %v", base.MedicalHistory, beforeHistory)
	}
}

// TestApplyUpdateInvalidValue 验证非法取值返回字段错误且不产生半成品。
func TestApplyUpdateInvalidValue(t *testing.T) {
	base := mustCard(t)
	badTel := "123"
	got, err := base.ApplyUpdate(CardUpdate{Tel: &badTel})
	if !errors.Is(err, ErrTelInvalid) {
		t.Fatalf("错误 = %v，期望 %v", err, ErrTelInvalid)
	}
	if fields := FieldsOf(err); !reflect.DeepEqual(fields, []string{"tel"}) {
		t.Fatalf("FieldsOf(err) = %v，期望 [tel]", fields)
	}
	if !reflect.DeepEqual(got, Card{}) {
		t.Fatalf("出错时不应返回半成品卡：%+v", got)
	}
}

// TestApplyUpdateEmpty 验证空更新不报错且返回与输入等值的卡。
func TestApplyUpdateEmpty(t *testing.T) {
	base := mustCard(t)
	got, err := base.ApplyUpdate(CardUpdate{})
	if err != nil {
		t.Fatalf("空更新不应报错：%v", err)
	}
	if !reflect.DeepEqual(got, base) {
		t.Fatalf("空更新返回 %+v，期望与输入等值 %+v", got, base)
	}
}

// TestApplyUpdateNameOnlyKeepsIdentity 验证只改姓名时身份相关字段全部不变。
func TestApplyUpdateNameOnlyKeepsIdentity(t *testing.T) {
	base := mustCard(t)
	newName := "李四"
	got, err := base.ApplyUpdate(CardUpdate{Name: &newName})
	if err != nil {
		t.Fatalf("ApplyUpdate 意外错误：%v", err)
	}
	if got.Name != newName {
		t.Fatalf("Name = %q，期望 %q", got.Name, newName)
	}
	if got.PID != base.PID {
		t.Errorf("PID 不应变化：%q -> %q", base.PID, got.PID)
	}
	if got.Birthday != base.Birthday {
		t.Errorf("Birthday 不应变化：%q -> %q", base.Birthday, got.Birthday)
	}
	if got.UserID != base.UserID {
		t.Errorf("UserID 不应变化：%d -> %d", base.UserID, got.UserID)
	}
	if got.UUID != base.UUID {
		t.Errorf("UUID 不应变化：%q -> %q", base.UUID, got.UUID)
	}
	if got.ExistFaceModel != base.ExistFaceModel {
		t.Errorf("ExistFaceModel 不应变化：%v -> %v", base.ExistFaceModel, got.ExistFaceModel)
	}
}

// TestMedicalHistoryCodecRoundTrip 验证 JSON 编解码往返，空集合固定编码为 []。
func TestMedicalHistoryCodecRoundTrip(t *testing.T) {
	values := []string{"高血压", "糖尿病"}
	raw, err := EncodeMedicalHistory(values)
	if err != nil {
		t.Fatalf("EncodeMedicalHistory 意外错误：%v", err)
	}
	if raw != `["高血压","糖尿病"]` {
		t.Fatalf("编码结果 = %q", raw)
	}
	if got := ParseMedicalHistory(raw); !reflect.DeepEqual(got, values) {
		t.Fatalf("往返解析 = %v，期望 %v", got, values)
	}
	for _, empty := range [][]string{nil, {}} {
		encoded, err := EncodeMedicalHistory(empty)
		if err != nil {
			t.Fatalf("EncodeMedicalHistory(%v) 意外错误：%v", empty, err)
		}
		if encoded != "[]" {
			t.Fatalf("空集合应编码为 []，实际 %q", encoded)
		}
	}
}

// TestParseMedicalHistoryMalformed 验证非法 JSON 统一降级为空数组（非 nil）。
func TestParseMedicalHistoryMalformed(t *testing.T) {
	for _, raw := range []string{"", "   ", "not-json", "null", "[1,2]", "{}"} {
		got := ParseMedicalHistory(raw)
		if got == nil {
			t.Errorf("ParseMedicalHistory(%q) 返回 nil，期望非 nil 空数组", raw)
			continue
		}
		if len(got) != 0 {
			t.Errorf("ParseMedicalHistory(%q) = %v，期望长度 0", raw, got)
		}
	}
}

// TestBusinessDate 验证业务日历日按 Asia/Shanghai(+08:00) 归一，而不是 UTC 日界。
func TestBusinessDate(t *testing.T) {
	cases := []struct {
		name string
		when time.Time
		want string
	}{
		{"UTC 00:30 即北京时间 08:30", time.Date(2026, 9, 10, 0, 30, 0, 0, time.UTC), "2026-09-10"},
		{"UTC 前一日 17:00 即北京时间次日 01:00", time.Date(2026, 9, 9, 17, 0, 0, 0, time.UTC), "2026-09-10"},
		{"UTC 15:30 即北京时间 23:30", time.Date(2026, 9, 9, 15, 30, 0, 0, time.UTC), "2026-09-09"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BusinessDate(tc.when); got != tc.want {
				t.Fatalf("BusinessDate(%s) = %q，期望 %q", tc.when, got, tc.want)
			}
		})
	}
}

// TestOptionsAreDefensiveCopies 验证取值枚举返回副本，调用方修改不影响包内状态。
func TestOptionsAreDefensiveCopies(t *testing.T) {
	history := MedicalHistoryOptions()
	if len(history) != 11 {
		t.Fatalf("疾病史取值数量 = %d，期望 11", len(history))
	}
	originalHistory := history[0]
	history[0] = "被篡改"
	if got := MedicalHistoryOptions()[0]; got != originalHistory {
		t.Fatalf("疾病史取值被外部修改：%q，期望 %q", got, originalHistory)
	}

	insurance := InsuranceTypeOptions()
	if len(insurance) != 8 {
		t.Fatalf("医保类型取值数量 = %d，期望 8", len(insurance))
	}
	originalInsurance := insurance[0]
	insurance[0] = "被篡改"
	if got := InsuranceTypeOptions()[0]; got != originalInsurance {
		t.Fatalf("医保类型取值被外部修改：%q，期望 %q", got, originalInsurance)
	}
}
