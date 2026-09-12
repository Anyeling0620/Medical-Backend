// 病历领域（internal/domain/medical_record）纯函数单测：覆盖输入归一化、长度上限与业务标识格式。
//
// 全部用例只做纯计算，不依赖 PostgreSQL、Redis 或 HTTP，
// 与 internal/domain/registration、internal/domain/schedule 的领域单测保持同一风格
// （表驱动 + 中文断言消息）。
package medical_record

import (
	"errors"
	"strings"
	"testing"
)

// TestNormalizeDiagnosis 覆盖诊断结论的归一化与校验：
// 空值/纯空白 → ErrDiagnosisRequired；按 rune 计的超长 → ErrDiagnosisTooLong；首尾空白被裁剪。
func TestNormalizeDiagnosis(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr error
	}{
		{name: "空字符串", raw: "", wantErr: ErrDiagnosisRequired},
		{name: "纯半角空格", raw: "   ", wantErr: ErrDiagnosisRequired},
		{name: "制表符与换行", raw: "\t\n\r ", wantErr: ErrDiagnosisRequired},
		{name: "全角空白", raw: "\u3000\u3000", wantErr: ErrDiagnosisRequired},
		{name: "裁剪首尾空白", raw: "  牙髓炎  ", want: "牙髓炎"},
		{name: "保留正文内部空格", raw: " 慢性 牙髓炎 ", want: "慢性 牙髓炎"},
		{name: "恰好等于长度上限", raw: strings.Repeat("诊", MaxDiagnosisRunes), want: strings.Repeat("诊", MaxDiagnosisRunes)},
		{name: "超过长度上限 1 个字符", raw: strings.Repeat("诊", MaxDiagnosisRunes+1), wantErr: ErrDiagnosisTooLong},
		{name: "长度按 rune 而非字节计算", raw: strings.Repeat("😀", MaxDiagnosisRunes), want: strings.Repeat("😀", MaxDiagnosisRunes)},
		{name: "多字节字符超长", raw: strings.Repeat("😀", MaxDiagnosisRunes+1), wantErr: ErrDiagnosisTooLong},
		{
			name: "首尾空白不计入长度",
			raw:  " " + strings.Repeat("诊", MaxDiagnosisRunes) + " ",
			want: strings.Repeat("诊", MaxDiagnosisRunes),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeDiagnosis(tc.raw)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("NormalizeDiagnosis(%q) 错误 = %v, want %v", tc.raw, err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if got != "" {
					t.Errorf("校验失败时应返回空串，实际 %q", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("NormalizeDiagnosis(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestNormalizeContent 覆盖病历正文的归一化与校验，并确认只裁剪首尾空白、
// 内部换行与排版原样保留（医生自行整理格式，领域层不得改写正文中间内容）。
func TestNormalizeContent(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr error
	}{
		{name: "空字符串", raw: "", wantErr: ErrContentRequired},
		{name: "纯半角空格", raw: "     ", wantErr: ErrContentRequired},
		{name: "换行与制表符", raw: "\n\t\r", wantErr: ErrContentRequired},
		{name: "全角空白", raw: "\u3000", wantErr: ErrContentRequired},
		{name: "裁剪首尾空白", raw: "  主诉：牙痛  ", want: "主诉：牙痛"},
		{
			name: "保留内部换行与缩进",
			raw:  "\n主诉：牙痛\n\n处理：根管治疗\n  ",
			want: "主诉：牙痛\n\n处理：根管治疗",
		},
		{name: "恰好等于长度上限", raw: strings.Repeat("疗", MaxContentRunes), want: strings.Repeat("疗", MaxContentRunes)},
		{name: "超过长度上限 1 个字符", raw: strings.Repeat("疗", MaxContentRunes+1), wantErr: ErrContentTooLong},
		{name: "长度按 rune 而非字节计算", raw: strings.Repeat("😀", MaxContentRunes), want: strings.Repeat("😀", MaxContentRunes)},
		{name: "多字节字符超长", raw: strings.Repeat("😀", MaxContentRunes+1), wantErr: ErrContentTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeContent(tc.raw)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("NormalizeContent(长度=%d) 错误 = %v, want %v", len([]rune(tc.raw)), err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if got != "" {
					t.Errorf("校验失败时应返回空串，实际长度 %d", len([]rune(got)))
				}
				return
			}
			if got != tc.want {
				t.Errorf("NormalizeContent = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNewUUIDFormat 业务标识固定为「RX + 30 位大写十六进制」：总长 32 与
// doctor_prescription.uuid 的 CHAR(32) 列宽一致，且不允许出现小写字母或分隔符。
func TestNewUUIDFormat(t *testing.T) {
	const sample = 20
	for i := 0; i < sample; i++ {
		got := NewUUID()
		if len(got) != 32 {
			t.Fatalf("NewUUID() = %q 长度 = %d, want 32", got, len(got))
		}
		if got[:2] != "RX" {
			t.Fatalf("NewUUID() = %q 前缀 = %q, want RX", got, got[:2])
		}
		for pos, r := range got[2:] {
			// 只允许 0-9A-F：排除小写十六进制与 UUID 的连字符。
			if !strings.ContainsRune("0123456789ABCDEF", r) {
				t.Fatalf("NewUUID() = %q 第 %d 位 %q 不是大写十六进制字符", got, pos+2, r)
			}
		}
	}
}

// TestNewUUIDUnique 至少采样 1000 次确认不碰撞：uuid 列没有唯一约束，
// 病历业务标识的唯一性完全由服务端保证。
func TestNewUUIDUnique(t *testing.T) {
	const count = 1000
	seen := make(map[string]struct{}, count)
	for i := 0; i < count; i++ {
		got := NewUUID()
		if _, exists := seen[got]; exists {
			t.Fatalf("第 %d 次生成重复的业务标识：%q", i, got)
		}
		seen[got] = struct{}{}
	}
	if len(seen) != count {
		t.Fatalf("去重后数量 = %d, want %d", len(seen), count)
	}
}
