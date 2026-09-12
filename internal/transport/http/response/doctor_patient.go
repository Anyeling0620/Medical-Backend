package response

import (
	domaindoctorpatient "Medical-Web-Backend/internal/domain/doctorpatient"
)

// 本文件是医生工作台（/api/v1/mis/doctor/*）的响应 DTO（契约 §6.10）。
// 字段集合即数据可见范围：不含身份证号与就诊卡 user_id，患者账号信息不出现在
// 医生工作台响应中。

// DoctorPatientItem 是「我的患者」列表项。
//
// birthday/lastVisitDate 在库中缺值时输出空串、medicalHistory 输出空数组（契约 §2），
// lastPaymentStatus 缺值时按未付款（UNPAID）收敛——与挂号域的展示口径一致，
// 保证前端不需要为 null 与空串写两套分支。
type DoctorPatientItem struct {
	PatientCardID     int64    `json:"patientCardId"`
	Name              string   `json:"name"`
	Sex               string   `json:"sex"`
	Tel               string   `json:"tel"`
	Birthday          string   `json:"birthday"`
	MedicalHistory    []string `json:"medicalHistory"`
	InsuranceType     string   `json:"insuranceType"`
	RegistrationCount int64    `json:"registrationCount"`
	LastVisitDate     string   `json:"lastVisitDate"`
	LastPaymentStatus string   `json:"lastPaymentStatus"`
}

// NewDoctorPatientItems 把领域实体列表投影为响应 DTO；空输入返回空切片，
// 保证响应输出 items: [] 而不是 null（契约 §1.4）。
func NewDoctorPatientItems(items []domaindoctorpatient.Patient) []DoctorPatientItem {
	result := make([]DoctorPatientItem, 0, len(items))
	for _, item := range items {
		// 防御性收敛：任何仓储实现返回 nil 时都必须输出 []，不能输出 null（契约 §6.10）。
		history := item.MedicalHistory
		if history == nil {
			history = []string{}
		}
		result = append(result, DoctorPatientItem{
			PatientCardID:     item.PatientCardID,
			Name:              item.Name,
			Sex:               item.Sex,
			Tel:               item.Tel,
			Birthday:          item.Birthday,
			MedicalHistory:    history,
			InsuranceType:     item.InsuranceType,
			RegistrationCount: item.RegistrationCount,
			LastVisitDate:     item.LastVisitDate,
			LastPaymentStatus: item.LastPaymentStatus,
		})
	}
	return result
}
