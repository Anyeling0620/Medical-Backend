// 病历响应投影单测：MedicalRecordResource 是创建/详情与列表项共用的 DTO，
// 本文件锁定它的字段映射与空列表输出（契约 §1.4：空列表输出 items: [] 而不是 null）。
package response

import (
	"encoding/json"
	"testing"

	domainmedicalrecord "Medical-Web-Backend/internal/domain/medical_record"
)

// medicalRecordResponseSample 返回字段齐全的病历实体，便于逐字段核对投影。
func medicalRecordResponseSample() domainmedicalrecord.MedicalRecord {
	return domainmedicalrecord.MedicalRecord{
		ID:              1001,
		UUID:            "RX000000000000000000000000000001",
		RegistrationID:  2001,
		PatientCardID:   501,
		DoctorID:        16,
		SubdepartmentID: 2,
		Diagnosis:       "牙髓炎",
		Content:         "主诉：牙痛\n处理：根管治疗",
	}
}

// TestNewMedicalRecordResourceMapsAllFields 投影必须逐字段映射，
// 且不得把领域内部结构（如 RegistrationID 之外的字段）意外暴露或丢失。
func TestNewMedicalRecordResourceMapsAllFields(t *testing.T) {
	item := medicalRecordResponseSample()
	got := NewMedicalRecordResource(item)

	if got.ID != item.ID || got.UUID != item.UUID || got.RegistrationID != item.RegistrationID {
		t.Errorf("标识字段投影错误：%+v", got)
	}
	if got.PatientCardID != item.PatientCardID || got.DoctorID != item.DoctorID ||
		got.SubdepartmentID != item.SubdepartmentID {
		t.Errorf("归属字段投影错误：%+v", got)
	}
	if got.Diagnosis != item.Diagnosis || got.Content != item.Content {
		t.Errorf("病历内容投影错误：%+v", got)
	}

	// JSON 字段名必须与契约一致（content 对应 doctor_prescription.rp）。
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	for _, key := range []string{
		"id", "uuid", "registrationId", "patientCardId", "doctorId", "subdepartmentId", "diagnosis", "content",
	} {
		if _, exists := body[key]; !exists {
			t.Errorf("响应缺少字段 %q：%s", key, raw)
		}
	}
	if len(body) != 8 {
		t.Errorf("响应字段数量 = %d, want 8：%s", len(body), raw)
	}
}

// TestNewMedicalRecordItemsNilBecomesEmptySlice 空输入（含 nil）必须投影为空切片，
// 保证 JSON 输出 items: [] 而不是 null。
func TestNewMedicalRecordItemsNilBecomesEmptySlice(t *testing.T) {
	for _, items := range [][]domainmedicalrecord.MedicalRecord{nil, {}} {
		got := NewMedicalRecordItems(items)
		if got == nil {
			t.Fatal("空输入应返回非 nil 空切片")
		}
		if len(got) != 0 {
			t.Fatalf("空输入应返回空切片，实际长度 %d", len(got))
		}
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("序列化失败：%v", err)
		}
		if string(raw) != "[]" {
			t.Errorf("JSON = %s, want []", raw)
		}
	}
}

// TestNewMedicalRecordItemsProjectsEachRecord 列表项逐条投影，顺序保持不变。
func TestNewMedicalRecordItemsProjectsEachRecord(t *testing.T) {
	first := medicalRecordResponseSample()
	second := medicalRecordResponseSample()
	second.ID = 1002
	second.Diagnosis = "慢性牙髓炎"

	got := NewMedicalRecordItems([]domainmedicalrecord.MedicalRecord{first, second})
	if len(got) != 2 {
		t.Fatalf("items 长度 = %d, want 2", len(got))
	}
	if got[0].ID != first.ID || got[0].Diagnosis != first.Diagnosis {
		t.Errorf("items[0] = %+v", got[0])
	}
	if got[1].ID != second.ID || got[1].Diagnosis != second.Diagnosis {
		t.Errorf("items[1] = %+v", got[1])
	}
}
