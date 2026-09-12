package response

import (
	"encoding/json"
	"strings"
	"testing"

	domaindoctorpatient "Medical-Web-Backend/internal/domain/doctorpatient"
)

// 本文件覆盖医生工作台「我的患者」的响应投影
// （internal/transport/http/response/doctor_patient.go，契约 §6.10）。
// 重点是缺值字段的收敛形态：疾病史必须输出 [] 而不是 null，空列表必须是 [] 而不是 null。

// TestNewDoctorPatientItemsMedicalHistoryNotNull 仓储（或领域实体）返回 nil 疾病史时，
// 投影必须收敛成空数组，保证前端只需处理一种形态。
func TestNewDoctorPatientItemsMedicalHistoryNotNull(t *testing.T) {
	items := NewDoctorPatientItems([]domaindoctorpatient.Patient{
		{PatientCardID: 10, Name: "张三"},                             // MedicalHistory 为 nil
		{PatientCardID: 11, Name: "李四", MedicalHistory: []string{}}, // 空切片
		{PatientCardID: 12, Name: "王五", MedicalHistory: []string{"高血压"}},
	})
	if len(items) != 3 {
		t.Fatalf("投影条数 = %d, want 3", len(items))
	}
	for i, item := range items {
		if item.MedicalHistory == nil {
			t.Errorf("第 %d 项 MedicalHistory = nil, want 非 nil 切片（必须输出 []）", i)
		}
	}

	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("序列化响应失败：%v", err)
	}
	if strings.Contains(string(raw), `"medicalHistory":null`) {
		t.Errorf("响应 JSON 不得出现 null 疾病史：%s", raw)
	}
	if !strings.Contains(string(raw), `"medicalHistory":[]`) {
		t.Errorf("缺值疾病史必须输出 []：%s", raw)
	}
}

// TestNewDoctorPatientItemsEmptyInputReturnsEmptySlice 空输入返回空切片，
// 序列化为 items:[] 而不是 null（契约 §1.4）。
func TestNewDoctorPatientItemsEmptyInputReturnsEmptySlice(t *testing.T) {
	for name, input := range map[string][]domaindoctorpatient.Patient{
		"nil 输入": nil,
		"空切片输入":  {},
	} {
		t.Run(name, func(t *testing.T) {
			items := NewDoctorPatientItems(input)
			if items == nil {
				t.Fatal("空结果必须返回空切片而不是 nil（否则 JSON 会输出 null）")
			}
			raw, err := json.Marshal(items)
			if err != nil {
				t.Fatalf("序列化响应失败：%v", err)
			}
			if string(raw) != "[]" {
				t.Errorf("序列化 = %s, want []", raw)
			}
		})
	}
}
