package patient

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// 本文件锁定 Slice 4 复审后的修复项：字段校验顺序、FieldError 零值安全、
// 疾病史空集合编码，以及实体与对外投影的敏感字段序列化边界。
// 全部为新增测试用例，不修改任何生产代码。

// TestNewCardFieldOrderMatchesContract 验证 NewCard 汇总的 details.fields
// 顺序与契约 §7.4 的字段顺序一致
// （name、sex、pid、tel、medicalHistory、insuranceType）。
func TestNewCardFieldOrderMatchesContract(t *testing.T) {
	cases := []struct {
		name  string
		input CardInput
		want  []string
	}{
		{
			name: "pid与tel同时非法",
			input: func() CardInput {
				in := validCardInput()
				in.PID = "110101199001011238" // 校验位错误，身份证号非法
				in.Tel = "123"
				return in
			}(),
			want: []string{"pid", "tel"},
		},
		{
			name: "疾病史与医保类型同时缺失",
			input: func() CardInput {
				in := validCardInput()
				in.MedicalHistory = nil
				in.InsuranceType = ""
				return in
			}(),
			want: []string{"medicalHistory", "insuranceType"},
		},
		{
			name:  "全部字段非法时严格按契约顺序",
			input: CardInput{},
			want:  []string{"name", "sex", "pid", "tel", "medicalHistory", "insuranceType"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCard(7, "", tc.input, testNow)
			if err == nil {
				t.Fatal("非法输入应返回错误")
			}
			if got := FieldsOf(err); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("FieldsOf(err) = %v，期望 %v（顺序必须与契约 §7.4 一致）", got, tc.want)
			}
		})
	}
}

// TestFieldErrorZeroValueIsSafe 验证 FieldError 的 Reason 为 nil 时 Error()
// 不会 panic（导出类型允许被零值构造）。
func TestFieldErrorZeroValueIsSafe(t *testing.T) {
	if got := (&FieldError{Field: "name"}).Error(); got != "name" {
		t.Fatalf("FieldError{Field:name}.Error() = %q，期望 %q", got, "name")
	}
	if got := (&FieldError{}).Error(); got != "" {
		t.Fatalf("空 FieldError.Error() = %q，期望空串", got)
	}

	var nilFieldErr *FieldError
	if got := nilFieldErr.Error(); got != "" {
		t.Fatalf("nil *FieldError.Error() = %q，期望空串", got)
	}
	if err := nilFieldErr.Unwrap(); err != nil {
		t.Fatalf("nil *FieldError.Unwrap() = %v，期望 nil", err)
	}
	if got := (&FieldError{Field: "name"}).Unwrap(); got != nil {
		t.Fatalf("Reason 为 nil 时 Unwrap() = %v，期望 nil", got)
	}
	if got := FieldsOf(&FieldError{Field: "tel"}); !reflect.DeepEqual(got, []string{"tel"}) {
		t.Fatalf("FieldsOf = %v，期望 [tel]", got)
	}
}

// TestEncodeMedicalHistoryEmptyIsArray 验证空集合（nil 与 len=0）固定编码为
// "[]" 而不是 "null"，且解析回来一定是非 nil 空切片。
func TestEncodeMedicalHistoryEmptyIsArray(t *testing.T) {
	for _, empty := range [][]string{nil, {}} {
		encoded, err := EncodeMedicalHistory(empty)
		if err != nil {
			t.Fatalf("EncodeMedicalHistory(%v) 意外错误：%v", empty, err)
		}
		if encoded != "[]" {
			t.Fatalf("EncodeMedicalHistory(%v) = %q，期望 []", empty, encoded)
		}
		parsed := ParseMedicalHistory(encoded)
		if parsed == nil {
			t.Fatalf("ParseMedicalHistory(%q) 返回 nil，期望非 nil 空切片", encoded)
		}
		if len(parsed) != 0 {
			t.Fatalf("ParseMedicalHistory(%q) = %v，期望长度 0", encoded, parsed)
		}
	}

	parsed := ParseMedicalHistory("[]")
	if parsed == nil {
		t.Fatal("ParseMedicalHistory 解析字面量 [] 返回 nil，期望非 nil 空切片")
	}
	if len(parsed) != 0 {
		t.Fatalf("ParseMedicalHistory 解析字面量 [] = %v，期望长度 0", parsed)
	}
}

// TestCardJSONHidesPIDAndTel 验证直接序列化实体时既不出现完整 pid，
// 也不出现明文 tel（Tel 已改为 json:"-"）。
func TestCardJSONHidesPIDAndTel(t *testing.T) {
	card := mustCard(t)
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("序列化 Card 失败：%v", err)
	}
	text := string(raw)
	if card.PID != "" && strings.Contains(text, card.PID) {
		t.Fatalf("Card JSON 泄露完整 pid：%s", text)
	}
	if card.Tel != "" && strings.Contains(text, card.Tel) {
		t.Fatalf("Card JSON 泄露明文 tel：%s", text)
	}
	if strings.Contains(text, "\"tel\"") {
		t.Fatalf("Card JSON 不应包含 tel 字段：%s", text)
	}
	if strings.Contains(text, "\"pid\"") {
		t.Fatalf("Card JSON 不应包含 pid 字段：%s", text)
	}
}

// TestCardViewJSONKeepsMaskedPIDAndTel 验证对外投影仍按契约返回脱敏 pid 与明文 tel。
func TestCardViewJSONKeepsMaskedPIDAndTel(t *testing.T) {
	card := mustCard(t)
	view := card.View()
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("序列化 CardView 失败：%v", err)
	}
	text := string(raw)
	if masked := MaskPID(card.PID); !strings.Contains(text, masked) {
		t.Fatalf("CardView JSON 缺少脱敏 pid %q：%s", masked, text)
	}
	if card.PID != "" && strings.Contains(text, card.PID) {
		t.Fatalf("CardView JSON 泄露完整 pid：%s", text)
	}
	if card.Tel != "" && !strings.Contains(text, card.Tel) {
		t.Fatalf("CardView JSON 缺少明文 tel：%s", text)
	}
}
