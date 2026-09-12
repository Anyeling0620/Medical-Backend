package response

import (
	domainmedicalrecord "Medical-Web-Backend/internal/domain/medical_record"
)

// 本文件是病历域（/api/v1/medical-records*）的响应 DTO（契约病历接口一节）。
//
// 显式定义而不直接序列化领域实体：字段集合本身是契约的一部分，
// 领域结构体调整不应静默改变接口形状。病历的归属字段（patientCardId/doctorId/subdepartmentId）
// 由服务端从挂号记录写入，响应中原样返回，便于前端展示与核对。

// MedicalRecordResource 是病历资源（创建与详情响应，也是列表项）。
//
// content 是医生自行整理的病历正文，对应 doctor_prescription.rp；
// uuid 是病历的业务标识（RX + 30 位大写十六进制），在排障与对账时替代自增主键对外使用。
type MedicalRecordResource struct {
	ID              int64  `json:"id"`
	UUID            string `json:"uuid"`
	RegistrationID  int64  `json:"registrationId"`
	PatientCardID   int64  `json:"patientCardId"`
	DoctorID        int64  `json:"doctorId"`
	SubdepartmentID int64  `json:"subdepartmentId"`
	Diagnosis       string `json:"diagnosis"`
	Content         string `json:"content"`
}

// NewMedicalRecordResource 把病历实体投影为响应 DTO。
func NewMedicalRecordResource(item domainmedicalrecord.MedicalRecord) MedicalRecordResource {
	return MedicalRecordResource{
		ID:              item.ID,
		UUID:            item.UUID,
		RegistrationID:  item.RegistrationID,
		PatientCardID:   item.PatientCardID,
		DoctorID:        item.DoctorID,
		SubdepartmentID: item.SubdepartmentID,
		Diagnosis:       item.Diagnosis,
		Content:         item.Content,
	}
}

// NewMedicalRecordItems 把病历实体列表投影为列表项；空输入返回空切片，
// 保证响应输出 items: [] 而不是 null（契约 §1.4）。
func NewMedicalRecordItems(items []domainmedicalrecord.MedicalRecord) []MedicalRecordResource {
	result := make([]MedicalRecordResource, 0, len(items))
	for _, item := range items {
		result = append(result, NewMedicalRecordResource(item))
	}
	return result
}
